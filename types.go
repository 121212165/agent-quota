package main

// 数据模型定义：账本状态、配置、任务、用量事件。
// 全部持久化在单个 JSON 文件中（默认 ~/.agent-quota/state.json）。

import "time"

// Price 模型单价（每百万 token 的金额）
type Price struct {
        InputPer1M  float64 `json:"input_per_1m"`
        OutputPer1M float64 `json:"output_per_1m"`
}

// Config 全局配置（随状态文件一起持久化）
type Config struct {
        // 余额来源: "local" | "deepseek" | "openrouter" | "custom"
        Provider string `json:"provider"`
        // custom provider 专用
        ProviderURL   string `json:"provider_url,omitempty"`
        ProviderToken string `json:"provider_token,omitempty"` // 也可用环境变量
        JSONPath      string `json:"json_path,omitempty"`      // 例如 "data.total_balance"
        // local 模式总额度；provider 模式下若 >0 则同时作为一道本地总额上限
        LocalTotal float64 `json:"local_total"`
        Currency   string  `json:"currency"`
        // 默认单价（未匹配到 model_pricing 时使用）
        DefaultPrice Price `json:"default_pricing"`
        // 按模型覆盖单价
        ModelPrices map[string]Price `json:"model_pricing,omitempty"`
        // 最低保留金额：剩余低于该值即硬阻断（0 = 不启用）
        MinReserve float64 `json:"min_reserve"`
}

// UsageEvent 一次消耗记录（LLM 调用 / 工具调用）
type UsageEvent struct {
        TS    int64  `json:"ts"`
        Task  string `json:"task,omitempty"` // 所属任务名（无任务时为空）
        TaskID string `json:"task_id,omitempty"`
        Tool  string `json:"tool"`
        Model string `json:"model,omitempty"`
        In    int64  `json:"input_tokens"`
        Out   int64  `json:"output_tokens"`
        Cost  float64 `json:"cost"`
        Calls int64  `json:"calls"`
}

// ActiveTask 进行中的任务（同一时刻最多一个）
type ActiveTask struct {
        ID         string  `json:"id"`
        Name       string  `json:"name"`
        Desc       string  `json:"description,omitempty"`
        StartedTS  int64   `json:"started_ts"`
        BudgetTok  int64   `json:"budget_tokens"`  // 0 = 不限
        BudgetCost float64 `json:"budget_cost"`    // 0 = 不限
        MaxCalls   int64   `json:"max_calls"`      // 0 = 不限
        UsedTok    int64   `json:"used_tokens"`
        UsedCost   float64 `json:"used_cost"`
        UsedCalls  int64   `json:"used_calls"`
        Breached   bool    `json:"breached"` // 任一预算维度被突破
}

// TaskRecord 已结束任务的归档
type TaskRecord struct {
        ActiveTask
        EndedTS   int64  `json:"ended_ts"`
        EndStatus string `json:"end_status"` // completed | aborted
        Summary   string `json:"summary,omitempty"`
}

// BalanceCache 服务商余额查询缓存
type BalanceCache struct {
        Amount    float64 `json:"amount"`
        Currency  string  `json:"currency"`
        CheckedAt int64   `json:"checked_at"`
        Source    string  `json:"source"` // provider 名或 "local"
}

// Counters 全局累计
type Counters struct {
        TotalCalls   int64   `json:"total_calls"`
        TotalIn      int64   `json:"total_input_tokens"`
        TotalOut     int64   `json:"total_output_tokens"`
        TotalCost    float64 `json:"total_cost"`
        TasksStarted int64   `json:"tasks_started"`
        TasksBlocked int64   `json:"tasks_blocked"`
        DeniedCalls  int64   `json:"denied_calls"`
}

// State 顶层状态
type State struct {
        Version     int          `json:"version"`
        Config      Config       `json:"config"`
        Counters    Counters     `json:"counters"`
        CurrentTask *ActiveTask  `json:"current_task,omitempty"`
        History     []TaskRecord `json:"history,omitempty"`
        Events      []UsageEvent `json:"events,omitempty"`
        LastBalance *BalanceCache `json:"last_balance,omitempty"`
}

const (
        stateVersion    = 1
        maxEvents       = 2000
        maxHistory      = 500
        balanceTTL      = 60 // 余额缓存秒数
        defLocalTotal   = 100.0
        defInputPer1M   = 4.0
        defOutputPer1M  = 16.0
)

func nowUnix() int64 { return time.Now().Unix() }

func unixDay(ts int64) string { return time.Unix(ts, 0).Format("2006-01-02") }

func nowDay() string { return time.Now().Format("2006-01-02") }
