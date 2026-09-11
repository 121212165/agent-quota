# agent-quota — Agent 额度管家

让 AI agent 能**看到自己的剩余额度与消耗次数**，并**规定每次任务的消耗上限**。Go 实现的单二进制工具，零运行时依赖，提供 **MCP Server** 与 **CLI/Skill** 两种接入形态。

## 功能特性

| 能力 | 说明 |
|------|------|
| 💰 剩余额度感知 | 实时查询服务商余额（DeepSeek / OpenRouter / 自定义 API），或用本地自管账本（先设总额、逐次扣减） |
| 🔢 双计量口径 | 按计费（按模型单价折算金额）+ 按次数计费（调用次数），同时统计、同时设限 |
| 📋 任务预算申报制 | 任务开始前申报金额/token/次数三维度预算，超支立即硬阻断 |
| 🚧 硬阻断 | 额度不足时 `begin`/`check`/`usage` 一律拒绝（CLI 退出码 2，MCP 返回 isError），agent 必须停手 |
| 📊 消耗分布统计 | 按任务 / 工具 / 模型 / 日期分组，找出最烧额度的环节 |
| 🔒 并发安全 | flock 文件锁 + 原子写入，多进程（MCP server 与 CLI 同时跑）记账不丢笔 |
| 📦 单二进制 | 无 Python/Node 运行时，交叉编译支持 Linux / macOS / Windows |

## 快速开始

### 1. 获取二进制

直接用 `dist/` 里预编译产物，或自行构建：

```bash
go build -o agent-quota .
```

把它放到 PATH 里（下文统一用 `quota` 指代）：

```bash
sudo install -m755 agent-quota /usr/local/bin/quota    # Linux/macOS
```

### 2. 初始化配置

```bash
# 本地自管账本模式：总额 100 元
quota set --total 100 --currency CNY \
  --input-price 4 --output-price 16    # 默认单价：元/百万token

# 查看状态
quota status
```

### 3. 接入你的 Agent

三种方式任选（可叠加，状态共享）：

**方式 A：MCP Server（Claude Code / Cursor / Cline 等）**

```bash
# Claude Code
claude mcp add quota-keeper -- /path/to/agent-quota serve

# 或手动编辑 mcp.json（见 examples/mcp.json）
{
  "mcpServers": {
    "quota-keeper": {
      "command": "/path/to/agent-quota",
      "args": ["serve"]
    }
  }
}
```

agent 将获得 7 个工具：`quota_status` / `begin_task` / `end_task` / `report_usage` / `check_gate` / `usage_stats` / `set_config`。

**方式 B：Agent Skill（Claude Code skills）**

```bash
mkdir -p ~/.claude/skills/quota-keeper
cp SKILL.md ~/.claude/skills/quota-keeper/SKILL.md
# 把 SKILL.md 中的 quota 路径换成实际路径
```

**方式 C：让 agent 直接跑命令（任何支持 Bash 工具的 agent）**

把下面这段贴进 agent 的系统提示 / CLAUDE.md：

```
## 额度管理规范（强制）
1. 任何任务开始前：quota begin -n "<任务名>" --cost <预估金额> --tokens <预估token>
   （被拒绝/退出码2 = 额度不足，立即向用户报告，不得继续）
2. 每次 LLM/工具调用后：quota usage -t "<来源>" --model <模型> --in <输入token> --out <输出token>
3. 大开销动作前：quota check --tokens <预估> ；退出码 2 则放弃或缩小动作
4. 任务结束：quota end -s "<一句话总结>"
5. 随时：quota status（看余额）、quota stats --by tool（看消耗分布）
```

## 余额来源配置

```bash
# DeepSeek（环境变量 DEEPSEEK_API_KEY 或 --token）
quota set --provider deepseek

# OpenRouter（环境变量 OPENROUTER_API_KEY）
quota set --provider openrouter

# 自定义任意余额 API（如中转站/聚合网关）
quota set --provider custom --provider-url "https://api.example.com/v1/me/balance" \
  --json-path "data.total_balance" --provider-token "sk-xxx"

# 回到本地账本模式
quota set --provider local --total 100
```

- 服务商余额结果缓存 60 秒（`--refresh` / `{"refresh":true}` 强制实时）。
- 查询失败时自动回退 24h 内的缓存值，不影响阻断决策。
- provider 模式下若同时设置 `--total`，取两者较小值作为本地总额双保险。
- `min_reserve`：最低保留金额，余额低于该值时全部动作被阻断（防止最后一分钱被打爆）。

## 计费公式

```
单笔成本 = input_tokens/1e6 × 输入单价 + output_tokens/1e6 × 输出单价
```

单价优先级：`model_pricing[模型名]` > `default_pricing`。也可以在记账时用 `--cost` 显式指定金额跳过折算（适合按次数收费的工具）。

## 硬阻断语义

| 场景 | 行为 |
|------|------|
| `begin` 时可用额度 < 申报金额预算 | 任务拒绝创建，`tasks_blocked+1` |
| `check` 预估消耗 > 可用额度/任务余量 | 返回 denied，`denied_calls+1`，CLI 退出码 2 |
| `usage` 使任务突破任一预算维度 | 照常记账（消耗已发生），但返回 QUOTA_EXCEEDED，CLI 退出码 2，MCP isError=true —— agent 必须立即停止该任务 |
| 任务已 breached 后继续 usage | 持续返回 QUOTA_EXCEEDED |

注意：记账类工具在超支时仍会记账——因为消耗已经真实发生，隐瞒不记只会让账本失真；阻断体现在"返回错误迫使 agent 停手"。

## 数据文件

默认 `~/.agent-quota/state.json`（0600 权限），包含配置、累计计数、进行中任务、最近 500 条任务归档、最近 2000 条消耗事件。用 `AGENT_QUOTA_HOME` 环境变量或 `--home` 参数可重定向（例如按项目隔离账本）。

## 从源码构建

```bash
go build -o agent-quota .        # 需要 Go 1.25+
# 交叉编译示例
GOOS=darwin GOARCH=arm64 go build -o agent-quota-darwin-arm64 .
```

## 目录说明

- `SKILL.md` — Agent Skill 形态的定义文件（复制到 `~/.claude/skills/quota-keeper/`）
- `examples/mcp.json` — MCP 客户端接入配置示例
- `examples/state-config-fields.json` — `state.json` 中 config 段的字段参考（配置通过 `quota set` 修改，无需手编文件）
- `dist/` — 预编译二进制（linux-amd64 / darwin-amd64 / darwin-arm64 / windows-amd64）

## 设计局限（诚实声明）

- 记账依赖 agent 主动调用 `usage`/`report_usage`，agent 若恶意漏报则账本失真（这是所有客户端侧额度管理的共同边界；要强制管控需在网关层做）。
- 服务商余额查询目前内置 DeepSeek 与 OpenRouter 的官方接口；其他服务商用 `custom` 模式对接任意 JSON API 即可。
- MCP 实现为 stdio 传输的 tools 子集，未含 resources/prompts —— 额度管理场景不需要。
