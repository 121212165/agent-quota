package main

// 状态文件读写：JSON 持久化 + 跨进程文件锁（flock）。
// CLI 与 MCP server 可能同时运行，锁保证记账不丢。

import (
        "encoding/json"
        "errors"
        "fmt"
        "os"
        "path/filepath"
)

// stateDir 返回状态目录。优先级: --home 参数 > AGENT_QUOTA_HOME > ~/.agent-quota
func stateDir(home string) string {
        if home != "" {
                return home
        }
        if env := os.Getenv("AGENT_QUOTA_HOME"); env != "" {
                return env
        }
        usr, err := os.UserHomeDir()
        if err != nil {
                usr = "."
        }
        return filepath.Join(usr, ".agent-quota")
}

const stateFileName = "state.json"

func statePath(home string) string {
        return filepath.Join(stateDir(home), stateFileName)
}

// LoadState 读取状态；文件不存在时返回带默认配置的空状态。
func LoadState(home string) (*State, error) {
        p := statePath(home)
        data, err := os.ReadFile(p)
        if err != nil {
                if errors.Is(err, os.ErrNotExist) {
                        return defaultState(), nil
                }
                return nil, fmt.Errorf("读取状态文件失败: %w", err)
        }
        st := &State{}
        if err := json.Unmarshal(data, st); err != nil {
                return nil, fmt.Errorf("状态文件损坏(%s): %w", p, err)
        }
        if st.Version == 0 {
                st.Version = stateVersion
        }
        // 补齐缺省配置
        if st.Config.Provider == "" {
                st.Config.Provider = "local"
        }
        if st.Config.Currency == "" {
                st.Config.Currency = "CNY"
        }
        if st.Config.LocalTotal == 0 && st.Config.Provider == "local" {
                st.Config.LocalTotal = defLocalTotal
        }
        if st.Config.DefaultPrice.InputPer1M == 0 && st.Config.DefaultPrice.OutputPer1M == 0 {
                st.Config.DefaultPrice = Price{InputPer1M: defInputPer1M, OutputPer1M: defOutputPer1M}
        }
        return st, nil
}

func defaultState() *State {
        return &State{
                Version: stateVersion,
                Config: Config{
                        Provider:     "local",
                        LocalTotal:   defLocalTotal,
                        Currency:     "CNY",
                        DefaultPrice: Price{InputPer1M: defInputPer1M, OutputPer1M: defOutputPer1M},
                        MinReserve:   0,
                },
        }
}

// SaveState 原子写入（临时文件 + rename），并确保目录与权限。
func SaveState(home string, st *State) error {
        dir := stateDir(home)
        if err := os.MkdirAll(dir, 0o755); err != nil {
                return err
        }
        data, err := json.MarshalIndent(st, "", "  ")
        if err != nil {
                return err
        }
        p := statePath(home)
        tmp := p + ".tmp"
        if err := os.WriteFile(tmp, data, 0o600); err != nil {
                return err
        }
        return os.Rename(tmp, p)
}

// bizErr 业务类错误（额度不足、参数非法等）。这类错误发生时
// 状态中已累积的计数器（如 tasks_blocked）需要被持久化。
type bizErr struct{ error }

func bizError(format string, args ...any) error {
        return bizErr{fmt.Errorf(format, args...)}
}

// withState 在持有文件锁的情况下执行 fn，自动加载与保存。
func withState(home string, save bool, fn func(st *State) error) error {
        dir := stateDir(home)
        if err := os.MkdirAll(dir, 0o755); err != nil {
                return err
        }
        lockPath := filepath.Join(dir, "lock")
        lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
        if err != nil {
                return err
        }
        defer lf.Close()
        if err := lockFile(lf); err != nil {
                return fmt.Errorf("获取文件锁失败: %w", err)
        }
        defer unlockFile(lf)

        st, err := LoadState(home)
        if err != nil {
                return err
        }
        if err := fn(st); err != nil {
                // 业务错误也保存，确保计数器/缓存变更不丢失
                if _, isBiz := err.(bizErr); isBiz {
                        if serr := SaveState(home, st); serr != nil {
                                return serr
                        }
                }
                return err
        }
        if save {
                return SaveState(home, st)
        }
        return nil
}
