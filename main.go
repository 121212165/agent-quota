package main

// agent-quota — Agent 额度管家
//
// 双形态:
//   MCP server : agent-quota serve               (stdio JSON-RPC, 配进 mcp.json)
//   CLI / Skill: agent-quota status|begin|end|usage|check|stats|set
//
// 硬阻断约定: CLI 在额度不足时退出码为 2，方便脚本与 skill 判断。

import (
        "encoding/json"
        "flag"
        "fmt"
        "os"
        "strconv"
        "strings"
)

const usageText = `agent-quota — Agent 额度管家 (MCP + CLI 双形态)

用法:
  agent-quota serve                        以 MCP stdio 服务运行（配置到 mcp.json）
  agent-quota status [--refresh] [--json]  查看剩余额度与累计消耗
  agent-quota begin -n 任务名 [--cost 0.5] [--tokens 50000] [--calls 20] [-d 说明]
                                           任务开始前申报预算（超额拒绝创建）
  agent-quota end [--status completed|aborted] [-s 总结]
                                           结束当前任务并结算
  agent-quota usage -t 工具名 [--model glm-4.6] [--in 1200] [--out 800] [--calls 1] [--cost 0.01]
                                           记一笔消耗（自动挂到当前任务）
  agent-quota check [--cost 0.5] [--tokens 8000] [--calls 1]
                                           大开销前检查（不足时退出码 2）
  agent-quota stats [--by task|tool|model|day] [--top 10] [--json]
                                           消耗分布统计
  agent-quota set ...                      修改配置（--total/--currency/--input-price/
                                           --output-price/--provider/--min-reserve/
                                           --model-price "模型,入,出" 等）
  agent-quota config                       查看当前配置

全局参数:
  --home 目录    状态目录（默认 ~/.agent-quota，或环境变量 AGENT_QUOTA_HOME）
  --json         以 JSON 输出（status/stats/check）

硬阻断语义: begin / check / usage 在额度不足时输出 QUOTA_EXCEEDED 并以退出码 2 结束。
`

func main() {
        if len(os.Args) < 2 {
                fmt.Fprint(os.Stderr, usageText)
                os.Exit(1)
        }
        cmd := os.Args[1]
        fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
        home := fs.String("home", "", "状态目录覆盖")
        if cmd == "serve" {
                // serve 子命令无需其他参数
                if err := fs.Parse(os.Args[2:]); err != nil {
                        os.Exit(1)
                }
                if err := ServeMCP(*home); err != nil {
                        fmt.Fprintf(os.Stderr, "agent-quota serve 异常退出: %v\n", err)
                        os.Exit(1)
                }
                return
        }

        var errCode int
        switch cmd {
        case "status":
                errCode = cmdStatus(fs, *home)
        case "begin":
                errCode = cmdBegin(fs, *home)
        case "end":
                errCode = cmdEnd(fs, *home)
        case "usage":
                errCode = cmdUsage(fs, *home)
        case "check":
                errCode = cmdCheck(fs, *home)
        case "stats":
                errCode = cmdStats(fs, *home)
        case "set":
                errCode = cmdSet(fs, *home)
        case "config":
                errCode = cmdConfig(fs, *home)
        case "help", "-h", "--help":
                fmt.Print(usageText)
        default:
                fmt.Fprintf(os.Stderr, "未知命令 %q\n\n%s", cmd, usageText)
                errCode = 1
        }
        os.Exit(errCode)
}

// ---- helpers ----

func printJSON(v any) {
        data, _ := json.MarshalIndent(v, "", "  ")
        fmt.Println(string(data))
}

func printStatus(qs *QuotaStatus) {
        fmt.Printf("余额: %.4f %s (来源: %s)\n", qs.Remaining.Amount, qs.Remaining.Currency, qs.Remaining.Source)
        if qs.MinReserve > 0 {
                fmt.Printf("可用额度(扣除保留 %.4f): %.4f\n", qs.MinReserve, qs.Blocking)
        }
        c := qs.Counters
        fmt.Printf("累计: %d 次调用 | 输入 %d tok | 输出 %d tok | 消耗 %.4f\n", c.TotalCalls, c.TotalIn, c.TotalOut, c.TotalCost)
        fmt.Printf("任务: 已开启 %d 个, 被阻断 %d 次, 闸门拒绝 %d 次\n", c.TasksStarted, c.TasksBlocked, c.DeniedCalls)
        if t := qs.CurrentTask; t != nil {
                fmt.Printf("进行中: %s (id=%s)\n", t.Name, t.ID)
                if t.BudgetCost > 0 {
                        fmt.Printf("  金额预算: %.4f 已用 %.4f 余 %.4f\n", t.BudgetCost, t.UsedCost, t.BudgetCost-t.UsedCost)
                }
                if t.BudgetTok > 0 {
                        fmt.Printf("  token预算: %d 已用 %d 余 %d\n", t.BudgetTok, t.UsedTok, t.BudgetTok-t.UsedTok)
                }
                if t.MaxCalls > 0 {
                        fmt.Printf("  次数上限: %d 已用 %d 余 %d\n", t.MaxCalls, t.UsedCalls, t.MaxCalls-t.UsedCalls)
                }
                if t.Breached {
                        fmt.Printf("  ⚠ 任务已超支(breached)，必须立即停止！\n")
                }
        } else {
                fmt.Println("进行中: (无任务)")
        }
}

func exitDenied(msg string) int {
        fmt.Fprintln(os.Stderr, msg)
        return 2
}

// ---- 子命令实现 ----

func cmdStatus(fs *flag.FlagSet, home string) int {
        refresh := fs.Bool("refresh", false, "绕过缓存实时查询服务商余额")
        asJSON := fs.Bool("json", false, "JSON 输出")
        if err := fs.Parse(os.Args[2:]); err != nil {
                return 1
        }
        var qs *QuotaStatus
        var err error
        err = withState(home, true, func(st *State) error {
                qs, err = statusOf(st, *refresh)
                return err
        })
        if err != nil {
                fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
                return 1
        }
        if *asJSON {
                printJSON(qs)
        } else {
                printStatus(qs)
        }
        return 0
}

func cmdBegin(fs *flag.FlagSet, home string) int {
        name := fs.String("n", "", "任务名")
        desc := fs.String("d", "", "任务说明")
        cost := fs.Float64("cost", 0, "金额预算")
        tokens := fs.Int64("tokens", 0, "token 预算")
        calls := fs.Int64("calls", 0, "调用次数上限")
        if err := fs.Parse(os.Args[2:]); err != nil {
                return 1
        }
        if fs.NArg() > 0 && *name == "" {
                *name = strings.Join(fs.Args(), " ")
        }
        in := &BeginTaskInput{Name: *name, Description: *desc, BudgetCost: *cost, BudgetTokens: *tokens, MaxCalls: *calls}
        var t *ActiveTask
        var err error
        err = withState(home, true, func(st *State) error {
                t, err = beginTask(st, in)
                return err
        })
        if err != nil {
                return exitDenied(fmt.Sprintf("✗ 任务被拒绝: %v", err))
        }
        fmt.Printf("✓ 任务已开启: %s (id=%s)\n", t.Name, t.ID)
        if t.BudgetCost > 0 {
                fmt.Printf("  金额预算: %.4f\n", t.BudgetCost)
        }
        if t.BudgetTok > 0 {
                fmt.Printf("  token预算: %d\n", t.BudgetTok)
        }
        if t.MaxCalls > 0 {
                fmt.Printf("  次数上限: %d\n", t.MaxCalls)
        }
        return 0
}

func cmdEnd(fs *flag.FlagSet, home string) int {
        taskID := fs.String("task", "", "任务ID(可选)")
        status := fs.String("status", "completed", "completed|aborted")
        summary := fs.String("s", "", "总结")
        if err := fs.Parse(os.Args[2:]); err != nil {
                return 1
        }
        in := &endTaskInput{TaskID: *taskID, Status: *status, Summary: *summary}
        var rec *TaskRecord
        var err error
        err = withState(home, true, func(st *State) error {
                rec, err = endTask(st, in)
                return err
        })
        if err != nil {
                fmt.Fprintf(os.Stderr, "✗ %v\n", err)
                return 1
        }
        fmt.Printf("✓ 任务已结束: %s (%s)\n", rec.Name, rec.EndStatus)
        fmt.Printf("  消耗: %d tok | %.4f 金额 | %d 次调用\n", rec.UsedTok, rec.UsedCost, rec.UsedCalls)
        if rec.BudgetCost > 0 {
                pct := rec.UsedCost / rec.BudgetCost * 100
                fmt.Printf("  预算使用率: %.1f%% (预算 %.4f)\n", pct, rec.BudgetCost)
        }
        return 0
}

func cmdUsage(fs *flag.FlagSet, home string) int {
        tool := fs.String("t", "", "工具/来源名")
        model := fs.String("model", "", "模型名")
        inTok := fs.Int64("in", 0, "输入 token")
        outTok := fs.Int64("out", 0, "输出 token")
        calls := fs.Int64("calls", 1, "调用次数")
        cost := fs.Float64("cost", 0, "显式金额(跳过折算)")
        taskID := fs.String("task", "", "任务ID(可选)")
        if err := fs.Parse(os.Args[2:]); err != nil {
                return 1
        }
        in := &usageInput{Tool: *tool, Model: *model, In: *inTok, Out: *outTok, Calls: *calls, Cost: *cost, TaskID: *taskID}
        var res *usageResult
        var err error
        err = withState(home, true, func(st *State) error {
                res, err = reportUsage(st, in)
                return err
        })
        if err != nil {
                fmt.Fprintf(os.Stderr, "✗ %v\n", err)
                return 1
        }
        ev := res.Event
        fmt.Printf("✓ 已记账: %s | in %d + out %d tok | %.6f 金额\n", ev.Tool, ev.In, ev.Out, ev.Cost)
        if t := res.Task; t != nil {
                if t.BudgetCost > 0 {
                        fmt.Printf("  任务 %s 金额: 已用 %.4f / 预算 %.4f\n", t.Name, t.UsedCost, t.BudgetCost)
                }
                if t.BudgetTok > 0 {
                        fmt.Printf("  任务 %s token: 已用 %d / 预算 %d\n", t.Name, t.UsedTok, t.BudgetTok)
                }
        }
        if res.Breached {
                return exitDenied(fmt.Sprintf("✗ %s", res.Breach))
        }
        return 0
}

func cmdCheck(fs *flag.FlagSet, home string) int {
        cost := fs.Float64("cost", 0, "预估金额")
        tokens := fs.Float64("tokens", 0, "预估 token")
        calls := fs.Int64("calls", 1, "预估次数")
        refresh := fs.Bool("refresh", false, "实时查询")
        asJSON := fs.Bool("json", false, "JSON 输出")
        if err := fs.Parse(os.Args[2:]); err != nil {
                return 1
        }
        var g *gateResult
        var err error
        err = withState(home, true, func(st *State) error {
                g, err = checkGate(st, *tokens, *cost, *calls, *refresh)
                return err
        })
        if err != nil {
                fmt.Fprintf(os.Stderr, "✗ 检查失败: %v\n", err)
                return 1
        }
        if *asJSON {
                printJSON(g)
        }
        if !g.Allowed {
                if !*asJSON {
                        return exitDenied("✗ "+g.Reason)
                }
                return exitDenied("")
        }
        if !*asJSON {
                fmt.Printf("✓ 放行: 可用 %.4f %s\n", g.Remaining, g.Currency)
        }
        return 0
}

func cmdStats(fs *flag.FlagSet, home string) int {
        by := fs.String("by", "task", "task|tool|model|day")
        top := fs.Int("top", 10, "前 N 条")
        asJSON := fs.Bool("json", false, "JSON 输出")
        if err := fs.Parse(os.Args[2:]); err != nil {
                return 1
        }
        var res *statsResult
        var err error
        err = withState(home, false, func(st *State) error {
                res, err = usageStats(st, *by, *top)
                return err
        })
        if err != nil {
                fmt.Fprintf(os.Stderr, "✗ %v\n", err)
                return 1
        }
        if *asJSON {
                printJSON(res)
                return 0
        }
        fmt.Printf("消耗分布 (按 %s, 共 %d 次 / %.4f 金额):\n", res.GroupBy, res.Total.Calls, res.Total.Cost)
        if len(res.Rows) == 0 {
                fmt.Println("  (暂无记录)")
                return 0
        }
        fmt.Printf("%-28s %8s %12s %12s %8s\n", "名称", "次数", "tokens", "金额", "占比")
        for _, r := range res.Rows {
                fmt.Printf("%-28s %8d %12d %12.4f %7.1f%%\n", r.Key, r.Calls, r.In+r.Out, r.Cost, r.CostPct)
        }
        return 0
}

func cmdSet(fs *flag.FlagSet, home string) int {
        total := fs.Float64("total", -1, "local 总额度")
        currency := fs.String("currency", "", "币种")
        inputPrice := fs.Float64("input-price", -1, "默认输入单价/百万token")
        outputPrice := fs.Float64("output-price", -1, "默认输出单价/百万token")
        provider := fs.String("provider", "", "local|deepseek|openrouter|custom")
        minReserve := fs.Float64("min-reserve", -1, "最低保留金额")
        modelPrice := fs.String("model-price", "", `设置模型单价 "模型名,输入单价,输出单价"`)
        removeModel := fs.String("remove-model", "", "删除模型单价")
        if err := fs.Parse(os.Args[2:]); err != nil {
                return 1
        }
        in := &setConfigInput{}
        if *total >= 0 {
                in.LocalTotal = total
        }
        if *currency != "" {
                in.Currency = currency
        }
        if *inputPrice >= 0 {
                in.InputPer1M = inputPrice
        }
        if *outputPrice >= 0 {
                in.OutputPer1M = outputPrice
        }
        if *provider != "" {
                in.Provider = provider
        }
        if *minReserve >= 0 {
                in.MinReserve = minReserve
        }
        if *modelPrice != "" {
                parts := strings.Split(*modelPrice, ",")
                if len(parts) != 3 {
                        fmt.Fprintln(os.Stderr, `--model-price 格式: "模型名,输入单价,输出单价"`)
                        return 1
                }
                ip, e1 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
                op, e2 := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
                if e1 != nil || e2 != nil {
                        fmt.Fprintln(os.Stderr, "--model-price 单价必须是数字")
                        return 1
                }
                in.ModelPrice = &struct {
                        Model       string  `json:"model"`
                        InputPer1M  float64 `json:"input_per_1m"`
                        OutputPer1M float64 `json:"output_per_1m"`
                }{Model: strings.TrimSpace(parts[0]), InputPer1M: ip, OutputPer1M: op}
        }
        if *removeModel != "" {
                in.RemoveModelPrice = *removeModel
        }
        var cfg *Config
        var err error
        err = withState(home, true, func(st *State) error {
                cfg, err = setConfig(st, in)
                return err
        })
        if err != nil {
                fmt.Fprintf(os.Stderr, "✗ %v\n", err)
                return 1
        }
        fmt.Println("✓ 配置已更新:")
        printJSON(cfg)
        return 0
}

func cmdConfig(fs *flag.FlagSet, home string) int {
        if err := fs.Parse(os.Args[2:]); err != nil {
                return 1
        }
        var st *State
        if err := withState(home, false, func(s *State) error { st = s; return nil }); err != nil {
                fmt.Fprintf(os.Stderr, "✗ %v\n", err)
                return 1
        }
        cfg := st.Config // 拷贝，脱敏 token
        if cfg.ProviderToken != "" {
                cfg.ProviderToken = "***"
        }
        printJSON(cfg)
        return 0
}
