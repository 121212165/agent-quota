package main

// 核心业务引擎：额度状态 / 任务预算申报 / 用量记账 / 硬阻断闸门 / 统计。
// 所有函数只操作 *State，不负责持久化 —— 持久化由 withState 统一管理。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// QuotaStatus 状态总览
type QuotaStatus struct {
	Remaining     *BalanceCache `json:"remaining"`
	Counters      *Counters     `json:"counters"`
	CurrentTask   *ActiveTask   `json:"current_task,omitempty"`
	TaskRemaining float64       `json:"task_remaining_cost"` // 当前任务剩余预算（无任务或未设金额预算时为 -1）
	TaskRemainTok int64         `json:"task_remaining_tokens"`
	TaskRemainCalls int64       `json:"task_remaining_calls"`
	MinReserve    float64       `json:"min_reserve"`
	Blocking      float64       `json:"blocking_remaining"` // 实际可用于决策的金额 = max(remaining - min_reserve, 0)
}

func statusOf(st *State, refresh bool) (*QuotaStatus, error) {
	bc, err := getRemaining(st, refresh)
	if err != nil {
		return nil, err
	}
	qs := &QuotaStatus{
		Remaining:     bc,
		Counters:      &st.Counters,
		CurrentTask:   st.CurrentTask,
		TaskRemaining: -1,
		TaskRemainTok: -1,
		TaskRemainCalls: -1,
		MinReserve:    st.Config.MinReserve,
	}
	blocking := bc.Amount
	if st.Config.MinReserve > 0 {
		blocking = bc.Amount - st.Config.MinReserve
	}
	if blocking < 0 {
		blocking = 0
	}
	qs.Blocking = blocking
	if t := st.CurrentTask; t != nil {
		if t.BudgetCost > 0 {
			qs.TaskRemaining = t.BudgetCost - t.UsedCost
		}
		if t.BudgetTok > 0 {
			qs.TaskRemainTok = t.BudgetTok - t.UsedTok
		}
		if t.MaxCalls > 0 {
			qs.TaskRemainCalls = t.MaxCalls - t.UsedCalls
		}
	}
	return qs, nil
}

func newTaskID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "t-" + hex.EncodeToString(b)
}

// BeginTaskInput 任务申报
type BeginTaskInput struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	BudgetTokens int64  `json:"budget_tokens,omitempty"`  // token 预算，0=不限
	BudgetCost  float64 `json:"budget_cost,omitempty"`    // 金额预算，0=不限
	MaxCalls    int64   `json:"max_calls,omitempty"`      // 调用次数上限，0=不限
}

// beginTask 开启任务。硬阻断规则：
//   - 已有进行中任务 → 报错（先 end）
//   - 申报了金额预算且可用额度(Blocking)不足 → 拒绝，任务不创建
func beginTask(st *State, in *BeginTaskInput) (*ActiveTask, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, bizError("必须提供任务名 name")
	}
	if st.CurrentTask != nil {
		return nil, bizError("已有进行中任务 %q(id=%s)，请先调用 end_task 结束", st.CurrentTask.Name, st.CurrentTask.ID)
	}
	if in.BudgetCost < 0 || in.BudgetTokens < 0 || in.MaxCalls < 0 {
		return nil, bizError("预算不能为负数")
	}
	if in.BudgetCost == 0 && in.BudgetTokens == 0 && in.MaxCalls == 0 {
		return nil, bizError("至少申报一种预算: budget_cost / budget_tokens / max_calls（0 表示不限的维度请勿申报）")
	}
	if in.BudgetCost > 0 {
		bc, err := getRemaining(st, false)
		if err != nil {
			return nil, bizError("查询可用额度失败，无法校验预算: %v", err)
		}
		avail := bc.Amount - st.Config.MinReserve
		if avail < in.BudgetCost {
			st.Counters.TasksBlocked++
			return nil, bizError("QUOTA_EXCEEDED: 可用额度 %.4f %s < 任务申报预算 %.4f，任务被硬阻断。请缩小预算或充值", avail, bc.Currency, in.BudgetCost)
		}
	}
	t := &ActiveTask{
		ID:         newTaskID(),
		Name:       in.Name,
		Desc:       in.Description,
		StartedTS:  nowUnix(),
		BudgetTok:  in.BudgetTokens,
		BudgetCost: in.BudgetCost,
		MaxCalls:   in.MaxCalls,
	}
	st.CurrentTask = t
	st.Counters.TasksStarted++
	return t, nil
}

// endTaskInput 结束任务
type endTaskInput struct {
	TaskID  string `json:"task_id,omitempty"` // 只有一个活动任务时可省略
	Status  string `json:"status,omitempty"`  // completed(默认) | aborted
	Summary string `json:"summary,omitempty"`
}

func endTask(st *State, in *endTaskInput) (*TaskRecord, error) {
	t := st.CurrentTask
	if t == nil {
		return nil, bizError("没有进行中的任务")
	}
	if in.TaskID != "" && in.TaskID != t.ID {
		return nil, bizError("task_id 不匹配: 当前任务是 %s", t.ID)
	}
	status := in.Status
	if status == "" {
		status = "completed"
	}
	if status != "completed" && status != "aborted" {
		return nil, bizError("status 只能是 completed 或 aborted")
	}
	rec := TaskRecord{ActiveTask: *t, EndedTS: nowUnix(), EndStatus: status, Summary: in.Summary}
	st.History = append(st.History, rec)
	if len(st.History) > maxHistory {
		st.History = st.History[len(st.History)-maxHistory:]
	}
	st.CurrentTask = nil
	return &rec, nil
}

// usageInput 记一笔消耗
type usageInput struct {
	TaskID string `json:"task_id,omitempty"`
	Tool   string `json:"tool"`
	Model  string `json:"model,omitempty"`
	In     int64  `json:"input_tokens"`
	Out    int64  `json:"output_tokens"`
	Calls  int64  `json:"calls"`
	Cost   float64 `json:"cost,omitempty"` // 显式指定金额则跳过单价折算
}

// usageResult 记账结果，可能携带 breach 信息
type usageResult struct {
	Event    *UsageEvent `json:"event"`
	Breached bool        `json:"breached"`
	Breach   string      `json:"breach_reason,omitempty"`
	Task     *ActiveTask `json:"task,omitempty"`
}

func priceFor(cfg *Config, model string) Price {
	if model != "" {
		if p, ok := cfg.ModelPrices[model]; ok {
			return p
		}
	}
	return cfg.DefaultPrice
}

// reportUsage 记账。若当前任务预算被突破：照样记账（消耗已发生），
// 但标记 breached 并返回原因 —— agent 收到后必须停止任务（硬阻断语义）。
func reportUsage(st *State, in *usageInput) (*usageResult, error) {
	if strings.TrimSpace(in.Tool) == "" && in.In == 0 && in.Out == 0 && in.Calls == 0 {
		return nil, bizError("至少提供 tool / input_tokens / output_tokens / calls 之一")
	}
	calls := in.Calls
	if calls == 0 {
		calls = 1
	}
	cost := in.Cost
	if cost == 0 && (in.In > 0 || in.Out > 0) {
		p := priceFor(&st.Config, in.Model)
		cost = float64(in.In)/1e6*p.InputPer1M + float64(in.Out)/1e6*p.OutputPer1M
	}
	ev := &UsageEvent{
		TS: nowUnix(), Tool: in.Tool, Model: in.Model,
		In: in.In, Out: in.Out, Cost: cost, Calls: calls,
	}
	if t := st.CurrentTask; t != nil {
		if in.TaskID != "" && in.TaskID != t.ID {
			return nil, bizError("task_id 不匹配: 当前任务是 %s", t.ID)
		}
		ev.Task = t.Name
		ev.TaskID = t.ID
	}

	// 全局记账
	st.Counters.TotalCalls += calls
	st.Counters.TotalIn += in.In
	st.Counters.TotalOut += in.Out
	st.Counters.TotalCost += cost
	st.Events = append(st.Events, *ev)
	if len(st.Events) > maxEvents {
		st.Events = st.Events[len(st.Events)-maxEvents:]
	}

	// 任务维度记账 + 预算检查
	res := &usageResult{Event: ev}
	if t := st.CurrentTask; t != nil {
		t.UsedTok += in.In + in.Out
		t.UsedCost += cost
		t.UsedCalls += calls
		res.Task = t
		var reasons []string
		if t.BudgetTok > 0 && t.UsedTok > t.BudgetTok {
			reasons = append(reasons, fmt.Sprintf("token 超支: 已用 %d > 预算 %d", t.UsedTok, t.BudgetTok))
		}
		if t.BudgetCost > 0 && t.UsedCost > t.BudgetCost {
			reasons = append(reasons, fmt.Sprintf("金额超支: 已用 %.4f > 预算 %.4f", t.UsedCost, t.BudgetCost))
		}
		if t.MaxCalls > 0 && t.UsedCalls > t.MaxCalls {
			reasons = append(reasons, fmt.Sprintf("次数超限: 已用 %d > 上限 %d", t.UsedCalls, t.MaxCalls))
		}
		if len(reasons) > 0 {
			t.Breached = true
			res.Breached = true
			res.Breach = "QUOTA_EXCEEDED: " + strings.Join(reasons, "; ")
		}
	}
	return res, nil
}

// gateResult 硬阻断闸门判定
type gateResult struct {
	Allowed    bool    `json:"allowed"`
	Reason     string  `json:"reason,omitempty"`
	Remaining  float64 `json:"remaining_cost"`
	Currency   string  `json:"currency"`
	TaskRemain float64 `json:"task_remaining_cost"` // -1 = 无任务或未设金额预算
}

// checkGate 事前检查：预估消耗是否能通过。任一口径不足即 deny。
func checkGate(st *State, estTokens, estCost float64, calls int64, refresh bool) (*gateResult, error) {
	bc, err := getRemaining(st, refresh)
	if err != nil {
		return nil, err
	}
	avail := bc.Amount - st.Config.MinReserve
	g := &gateResult{Allowed: true, Remaining: avail, Currency: bc.Currency, TaskRemain: -1}

	if estCost > 0 && avail < estCost {
		g.Allowed = false
		g.Reason = fmt.Sprintf("QUOTA_EXCEEDED: 可用额度 %.4f %s < 预估消耗 %.4f", avail, bc.Currency, estCost)
	}
	if t := st.CurrentTask; t != nil && g.Allowed {
		if t.BudgetCost > 0 {
			tr := t.BudgetCost - t.UsedCost
			g.TaskRemain = tr
			if estCost > 0 && tr < estCost {
				g.Allowed = false
				g.Reason = fmt.Sprintf("TASK_BUDGET_EXCEEDED: 任务剩余预算 %.4f < 预估消耗 %.4f（任务 %q）", tr, estCost, t.Name)
			}
		}
		if t.BudgetTok > 0 && estTokens > 0 {
			tr := float64(t.BudgetTok - t.UsedTok)
			if tr < estTokens {
				g.Allowed = false
				g.Reason = fmt.Sprintf("TASK_BUDGET_EXCEEDED: 任务剩余 token %.0f < 预估 %.0f（任务 %q）", tr, estTokens, t.Name)
			}
		}
		if t.MaxCalls > 0 && calls > 0 {
			tr := t.MaxCalls - t.UsedCalls
			if tr < calls {
				g.Allowed = false
				g.Reason = fmt.Sprintf("TASK_BUDGET_EXCEEDED: 任务剩余次数 %d < 申请 %d（任务 %q）", tr, calls, t.Name)
			}
		}
	}
	if !g.Allowed {
		st.Counters.DeniedCalls++
	}
	return g, nil
}

// statRow 统计行
type statRow struct {
	Key      string  `json:"key"`
	Calls    int64   `json:"calls"`
	In       int64   `json:"input_tokens"`
	Out      int64   `json:"output_tokens"`
	Cost     float64 `json:"cost"`
	CostPct  float64 `json:"cost_pct"`
}

type statsResult struct {
	GroupBy string    `json:"group_by"`
	Rows    []statRow `json:"rows"`
	Total   statRow   `json:"total"`
}

// usageStats 按维度聚合消耗分布
func usageStats(st *State, groupBy string, topN int) (*statsResult, error) {
	switch groupBy {
	case "task", "tool", "model", "day":
	case "":
		groupBy = "task"
	default:
		return nil, bizError("group_by 只支持 task|tool|model|day")
	}
	if topN <= 0 {
		topN = 10
	}
	idx := map[string]*statRow{}
	var total statRow
	for _, e := range st.Events {
		key := ""
		switch groupBy {
		case "task":
			if e.Task != "" {
				key = e.Task
			} else {
				key = "(无任务)"
			}
		case "tool":
			if e.Tool == "" {
				key = "(未标注)"
			} else {
				key = e.Tool
			}
		case "model":
			if e.Model == "" {
				key = "(默认单价)"
			} else {
				key = e.Model
			}
		case "day":
			key = dayOfTS(e.TS)
		}
		row, ok := idx[key]
		if !ok {
			row = &statRow{Key: key}
			idx[key] = row
		}
		row.Calls += e.Calls
		row.In += e.In
		row.Out += e.Out
		row.Cost += e.Cost
		total.Calls += e.Calls
		total.In += e.In
		total.Out += e.Out
		total.Cost += e.Cost
	}
	rows := make([]statRow, 0, len(idx))
	for _, r := range idx {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Cost > rows[j].Cost })
	if len(rows) > topN {
		rows = rows[:topN]
	}
	if total.Cost > 0 {
		for i := range rows {
			rows[i].CostPct = rows[i].Cost / total.Cost * 100
		}
	}
	return &statsResult{GroupBy: groupBy, Rows: rows, Total: total}, nil
}

func dayOfTS(ts int64) string {
	return unixDay(ts)
}

// setConfigInput 运行时配置变更（只更新非零/显式提供的字段，用指针区分）
type setConfigInput struct {
	Provider      *string  `json:"provider,omitempty"`
	ProviderURL   *string  `json:"provider_url,omitempty"`
	ProviderToken *string  `json:"provider_token,omitempty"`
	JSONPath      *string  `json:"json_path,omitempty"`
	LocalTotal    *float64 `json:"local_total,omitempty"`
	Currency      *string  `json:"currency,omitempty"`
	InputPer1M    *float64 `json:"input_per_1m,omitempty"`
	OutputPer1M   *float64 `json:"output_per_1m,omitempty"`
	MinReserve    *float64 `json:"min_reserve,omitempty"`
	ModelPrice    *struct {
		Model       string  `json:"model"`
		InputPer1M  float64 `json:"input_per_1m"`
		OutputPer1M float64 `json:"output_per_1m"`
	} `json:"model_price,omitempty"`
	// 清除某个模型单价
	RemoveModelPrice string `json:"remove_model_price,omitempty"`
}

func setConfig(st *State, in *setConfigInput) (*Config, error) {
	c := &st.Config
	if in.Provider != nil {
		switch *in.Provider {
		case "local", "deepseek", "openrouter", "custom":
			c.Provider = *in.Provider
		default:
			return nil, bizError("provider 只支持 local|deepseek|openrouter|custom")
		}
	}
	if in.ProviderURL != nil {
		c.ProviderURL = *in.ProviderURL
	}
	if in.ProviderToken != nil {
		c.ProviderToken = *in.ProviderToken
	}
	if in.JSONPath != nil {
		c.JSONPath = *in.JSONPath
	}
	if in.LocalTotal != nil {
		if *in.LocalTotal < 0 {
			return nil, bizError("local_total 不能为负")
		}
		c.LocalTotal = *in.LocalTotal
	}
	if in.Currency != nil && *in.Currency != "" {
		c.Currency = *in.Currency
	}
	if in.InputPer1M != nil {
		c.DefaultPrice.InputPer1M = *in.InputPer1M
	}
	if in.OutputPer1M != nil {
		c.DefaultPrice.OutputPer1M = *in.OutputPer1M
	}
	if in.MinReserve != nil {
		if *in.MinReserve < 0 {
			return nil, bizError("min_reserve 不能为负")
		}
		c.MinReserve = *in.MinReserve
	}
	if in.ModelPrice != nil {
		if in.ModelPrice.Model == "" {
			return nil, bizError("model_price.model 不能为空")
		}
		if c.ModelPrices == nil {
			c.ModelPrices = map[string]Price{}
		}
		c.ModelPrices[in.ModelPrice.Model] = Price{InputPer1M: in.ModelPrice.InputPer1M, OutputPer1M: in.ModelPrice.OutputPer1M}
	}
	if in.RemoveModelPrice != "" {
		delete(c.ModelPrices, in.RemoveModelPrice)
	}
	return c, nil
}

// formatAmount 人性化金额
func formatAmount(v float64, currency string) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if currency != "" {
		return s + " " + currency
	}
	return s
}
