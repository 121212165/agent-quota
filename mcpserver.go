package main

// MCP (Model Context Protocol) stdio 服务端。
// 传输: 每行一个 JSON-RPC 2.0 消息（newline-delimited），零第三方依赖。
// 只声明 tools 能力，暴露 7 个额度管理工具。

import (
        "bufio"
        "encoding/json"
        "fmt"
        "io"
        "os"
)

const (
        serverName     = "agent-quota"
        serverVersion  = "1.0.0"
        mcpProtoLatest = "2024-11-05" // 兼容回退版本
)

type jsonRPCRequest struct {
        JSONRPC string          `json:"jsonrpc"`
        ID      json.RawMessage `json:"id,omitempty"`
        Method  string          `json:"method"`
        Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
        JSONRPC string          `json:"jsonrpc"`
        ID      json.RawMessage `json:"id,omitempty"`
        Result  any             `json:"result,omitempty"`
        Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
        Code    int    `json:"code"`
        Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

var (
        errMethodNotFound = &rpcError{Code: -32601, Message: "method not found"}
        errInvalidParams  = &rpcError{Code: -32602, Message: "invalid params"}
        errParse          = &rpcError{Code: -32700, Message: "parse error"}
)

// mcpTool 工具定义
type mcpTool struct {
        Name        string          `json:"name"`
        Description string          `json:"description"`
        InputSchema json.RawMessage `json:"inputSchema"`
}

// textContent 工具返回内容
type textContent struct {
        Type string `json:"type"`
        Text string `json:"text"`
}

type toolCallResult struct {
        Content []textContent `json:"content"`
        IsError bool          `json:"isError,omitempty"`
}

func schema(properties string, required string) json.RawMessage {
        if required == "" {
                return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%s}}`, properties))
        }
        return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%s},"required":[%s]}`, properties, required))
}

var mcpTools = []mcpTool{
        {
                Name: "quota_status",
                Description: "查询当前额度状态：剩余金额（实时读取服务商余额或本地账本）、累计消耗次数/token/金额、进行中任务的预算余量。开始任务前后都应调用它了解额度。",
                InputSchema: schema(`"refresh":{"type":"boolean","description":"强制绕过60秒余额缓存，实时查询服务商","default":false}`, ""),
        },
        {
                Name:        "begin_task",
                Description: "任务开始前申报预算（硬阻断）。声明本任务允许的最大消耗（金额/token/调用次数任选其一或组合）。若可用额度不足以覆盖金额预算，任务将被拒绝创建。成功后所有消耗自动计入该任务。",
                InputSchema: schema(
                        `"name":{"type":"string","description":"任务名，如 '搜索资料'、'重写README'"},
 "description":{"type":"string","description":"任务说明（可选）"},
 "budget_cost":{"type":"number","description":"金额预算，如 0.5；0/缺省=不限此维度"},
 "budget_tokens":{"type":"integer","description":"token 预算（输入+输出合计），如 50000；0/缺省=不限"},
 "max_calls":{"type":"integer","description":"调用次数上限，如 20；0/缺省=不限"}`,
                        `"name"`),
        },
        {
                Name:        "end_task",
                Description: "结束当前任务并结算：归档任务记录，返回预算使用率汇总。之后消耗不再计入该任务。",
                InputSchema: schema(
                        `"task_id":{"type":"string","description":"任务ID（可选，默认取当前唯一活动任务）"},
 "status":{"type":"string","enum":["completed","aborted"],"description":"completed=正常完成（默认）；aborted=中途放弃"},
 "summary":{"type":"string","description":"一句话总结（可选，写入归档）"}`,
                        ""),
        },
        {
                Name:        "report_usage",
                Description: "记账一笔消耗：每次 LLM 调用或工具调用后上报 token 用量，按模型单价自动折算金额并扣减（全局+当前任务）。若本笔导致任务预算突破，返回 isError=true，此时必须立即停止任务。",
                InputSchema: schema(
                        `"tool":{"type":"string","description":"消耗来源，如 'llm:glm-4.6'、'web-search'、'code-interpreter'"},
 "model":{"type":"string","description":"模型名（用于匹配单价表，可选）"},
 "input_tokens":{"type":"integer","description":"本次输入 token 数"},
 "output_tokens":{"type":"integer","description":"本次输出 token 数"},
 "calls":{"type":"integer","description":"调用次数（默认1）"},
 "cost":{"type":"number","description":"显式指定金额（提供时跳过单价折算，可选）"},
 "task_id":{"type":"string","description":"任务ID（可选）"}`,
                        `"tool"`),
        },
        {
                Name:        "check_gate",
                Description: "硬阻断闸门：在执行任何大开销动作（大文档LLM调用、批量任务）前调用。传入预估消耗，额度不足时返回 allowed=false，此时必须放弃或缩小该动作。",
                InputSchema: schema(
                        `"estimate_cost":{"type":"number","description":"预估金额消耗"},
 "estimate_tokens":{"type":"integer","description":"预估 token 消耗"},
 "calls":{"type":"integer","description":"预估调用次数（默认1）"},
 "refresh":{"type":"boolean","description":"绕过余额缓存实时查询（默认false）"}`,
                        ""),
        },
        {
                Name:        "usage_stats",
                Description: "消耗分布统计：按任务/工具/模型/日期分组，找出最烧额度的环节。",
                InputSchema: schema(
                        `"group_by":{"type":"string","enum":["task","tool","model","day"],"description":"分组维度（默认task）"},
 "top":{"type":"integer","description":"返回前N条（默认10）"}`,
                        ""),
        },
        {
                Name:        "set_config",
                Description: "修改配置：单价表（input_per_1m/output_per_1m 或 model_price）、总额度 local_total、余额来源 provider、最低保留 min_reserve 等。只更新传入的字段。",
                InputSchema: schema(
                        `"provider":{"type":"string","enum":["local","deepseek","openrouter","custom"],"description":"余额来源"},
 "local_total":{"type":"number","description":"local 模式总额度"},
 "currency":{"type":"string","description":"币种，如 CNY/USD"},
 "input_per_1m":{"type":"number","description":"默认输入单价/百万token"},
 "output_per_1m":{"type":"number","description":"默认输出单价/百万token"},
 "min_reserve":{"type":"number","description":"最低保留金额，低于此值硬阻断"},
 "model_price":{"type":"object","description":"设置某模型单价","properties":{"model":{"type":"string"},"input_per_1m":{"type":"number"},"output_per_1m":{"type":"number"}},"required":["model"]},
 "remove_model_price":{"type":"string","description":"删除某模型单价"},
 "provider_url":{"type":"string","description":"custom 余额接口URL"},
 "provider_token":{"type":"string","description":"custom 余额接口token"},
 "json_path":{"type":"string","description":"custom 余额提取路径，如 data.total_balance"}`,
                        ""),
        },
}

// ServeMCP 启动 stdio MCP 服务。home 为状态目录（可为空）。
func ServeMCP(home string) error {
        reader := bufio.NewReaderSize(os.Stdin, 1<<20)
        enc := json.NewEncoder(os.Stdout)
        logf := func(format string, args ...any) {
                fmt.Fprintf(os.Stderr, "[agent-quota] "+format+"\n", args...)
        }
        for {
                line, err := readLine(reader)
                if err == io.EOF {
                        return nil
                }
                if err != nil {
                        return err
                }
                if len(trimSpace(line)) == 0 {
                        continue
                }
                var req jsonRPCRequest
                if err := json.Unmarshal(line, &req); err != nil {
                        _ = enc.Encode(jsonRPCResponse{JSONRPC: "2.0", ID: nil, Error: errParse})
                        continue
                }
                if req.Method == "" || req.ID == nil { // notification
                        continue
                }
                resp := dispatchMCP(&req, home, logf)
                if err := enc.Encode(resp); err != nil {
                        return err
                }
        }
}

func readLine(r *bufio.Reader) ([]byte, error) {
        var buf []byte
        for {
                chunk, err := r.ReadSlice('\n')
                if err == bufio.ErrBufferFull {
                        buf = append(buf, chunk...)
                        continue
                }
                if err != nil {
                        if len(buf) > 0 {
                                return append(buf, chunk...), err
                        }
                        return nil, err
                }
                if buf == nil {
                        return chunk, nil
                }
                return append(buf, chunk...), nil
        }
}

func trimSpace(b []byte) []byte {
        start := 0
        for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
                start++
        }
        end := len(b)
        for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
                end--
        }
        return b[start:end]
}

// callToolParams tools/call 的参数
type callToolParams struct {
        Name      string          `json:"name"`
        Arguments json.RawMessage `json:"arguments"`
}

func dispatchMCP(req *jsonRPCRequest, home string, logf func(string, ...any)) *jsonRPCResponse {
        switch req.Method {
        case "initialize":
                var p struct {
                        ProtocolVersion string `json:"protocolVersion"`
                }
                _ = json.Unmarshal(req.Params, &p)
                ver := p.ProtocolVersion
                if ver == "" {
                        ver = mcpProtoLatest
                }
                return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
                        "protocolVersion": ver,
                        "capabilities":    map[string]any{"tools": map[string]any{}},
                        "serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
                }}
        case "ping":
                return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
        case "tools/list":
                return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": mcpTools}}
        case "tools/call":
                var p callToolParams
                if err := json.Unmarshal(req.Params, &p); err != nil {
                        return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: errInvalidParams}
                }
                result := callTool(&p, home)
                logf("tools/call %s -> isError=%v", p.Name, result.IsError)
                return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
        default:
                return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: errMethodNotFound}
        }
}

// callTool 工具分发：业务错误返回 isError=true（agent 可读），不中断会话。
func callTool(p *callToolParams, home string) *toolCallResult {
        ok := func(v any) *toolCallResult {
                data, _ := json.MarshalIndent(v, "", "  ")
                return &toolCallResult{Content: []textContent{{Type: "text", Text: string(data)}}}
        }
        fail := func(format string, args ...any) *toolCallResult {
                return &toolCallResult{
                        Content: []textContent{{Type: "text", Text: fmt.Sprintf(format, args...)}},
                        IsError: true,
                }
        }
        var err error
        switch p.Name {
        case "quota_status":
                var in struct {
                        Refresh bool `json:"refresh"`
                }
                _ = json.Unmarshal(p.Arguments, &in)
                var qs *QuotaStatus
                err = withState(home, true, func(st *State) error { // 总是保存，持久化余额缓存
                        qs, err = statusOf(st, in.Refresh)
                        return err
                })
                if err != nil {
                        return fail("查询状态失败: %v", err)
                }
                return ok(qs)

        case "begin_task":
                var in BeginTaskInput
                if err := json.Unmarshal(p.Arguments, &in); err != nil {
                        return fail("参数错误: %v", err)
                }
                var t *ActiveTask
                err = withState(home, true, func(st *State) error {
                        t, err = beginTask(st, &in)
                        return err
                })
                if err != nil {
                        return fail("%v", err) // 硬阻断走这里
                }
                return ok(map[string]any{
                        "ok": true, "task": t,
                        "hint": "任务已开启，后续 report_usage 自动计入。结束请调用 end_task。",
                })

        case "end_task":
                var in endTaskInput
                _ = json.Unmarshal(p.Arguments, &in)
                var rec *TaskRecord
                err = withState(home, true, func(st *State) error {
                        rec, err = endTask(st, &in)
                        return err
                })
                if err != nil {
                        return fail("%v", err)
                }
                return ok(map[string]any{"ok": true, "record": rec})

        case "report_usage":
                var in usageInput
                if err := json.Unmarshal(p.Arguments, &in); err != nil {
                        return fail("参数错误: %v", err)
                }
                var res *usageResult
                err = withState(home, true, func(st *State) error {
                        res, err = reportUsage(st, &in)
                        return err
                })
                if err != nil {
                        return fail("%v", err)
                }
                if res.Breached {
                        return fail("%s 任务必须立即停止，如需继续请向用户申请追加预算。", res.Breach)
                }
                return ok(res)

        case "check_gate":
                var in struct {
                        EstimateCost   float64 `json:"estimate_cost"`
                        EstimateTokens float64 `json:"estimate_tokens"`
                        Calls          int64   `json:"calls"`
                        Refresh        bool    `json:"refresh"`
                }
                _ = json.Unmarshal(p.Arguments, &in)
                var g *gateResult
                err = withState(home, true, func(st *State) error {
                        g, err = checkGate(st, in.EstimateTokens, in.EstimateCost, in.Calls, in.Refresh)
                        return err
                })
                if err != nil {
                        return fail("闸门检查失败: %v", err)
                }
                if !g.Allowed {
                        return fail("%v", g.Reason)
                }
                return ok(g)

        case "usage_stats":
                var in struct {
                        GroupBy string `json:"group_by"`
                        Top     int    `json:"top"`
                }
                _ = json.Unmarshal(p.Arguments, &in)
                var res *statsResult
                err = withState(home, false, func(st *State) error {
                        res, err = usageStats(st, in.GroupBy, in.Top)
                        return err
                })
                if err != nil {
                        return fail("%v", err)
                }
                return ok(res)

        case "set_config":
                var in setConfigInput
                if err := json.Unmarshal(p.Arguments, &in); err != nil {
                        return fail("参数错误: %v", err)
                }
                var cfg *Config
                err = withState(home, true, func(st *State) error {
                        cfg, err = setConfig(st, &in)
                        return err
                })
                if err != nil {
                        return fail("%v", err)
                }
                return ok(map[string]any{"ok": true, "config": cfg})

        default:
                return fail("未知工具: %s", p.Name)
        }
}
