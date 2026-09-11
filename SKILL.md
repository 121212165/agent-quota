---
name: quota-keeper
description: Agent 额度管家。让 agent 感知自己的剩余额度与消耗次数，任务开始前申报预算、消耗后记账，额度不足时硬阻断。当用户要求"控制消耗/预算/额度"、"先看看还剩多少额度"、执行大开销任务（长文档、批量调用、多轮搜索）之前，或对话涉及 token 成本管理时使用。
---

# Agent 额度管家 (quota-keeper)

## 核心职责

你是额度的第一责任人。本 skill 通过 `quota` 命令行工具（即 agent-quota 二进制）为每次任务做预算管理和记账。你的三条铁律：

1. **先申报，后开工**：任何任务开始前必须用 `begin` 申报预算，未申报预算的大开销动作不得执行。
2. **诚实记账**：每次 LLM 调用、工具调用产生 token 消耗后，立刻用 `usage` 上报真实用量，不得漏报、少报。
3. **硬阻断必须服从**：任何命令退出码为 2（QUOTA_EXCEEDED）时，立即停止当前动作，向用户说明剩余额度并请求决策；不得绕过、重试或缩小单位规避。

## 首次使用

确认二进制位置（本文以 `quota` 代指 agent-quota 可执行文件路径），并初始化配置（仅需一次）：

```bash
# 设定总额度与单价（示例：总额 100 元，输入 4 元/百万token，输出 16 元/百万token）
quota set --total 100 --currency CNY --input-price 4 --output-price 16
# 接入服务商真实余额（可选，二选一或叠加）
quota set --provider deepseek        # 读取 DEEPSEEK_API_KEY 查询真实余额
quota status --refresh               # 验证连通
```

## 标准工作流

### 1. 任务开始：申报预算

```bash
# 三种维度任选：金额 --cost / token --tokens / 次数 --calls
quota begin -n "重构登录模块" --cost 0.8 --tokens 200000 --calls 30
```

- 若可用额度 < 申报预算，命令拒绝创建（退出码 2）→ 把真实余额告诉用户，协商缩小任务或充值。
- 申报量应基于任务复杂度预估：简单问答 ≈ 0.05 元/1万token；代码任务 ≈ 0.3-1 元；深度研究 ≈ 1-3 元。

### 2. 执行中：记账与闸门

每次 LLM / 工具调用后立即记账（金额按单价表自动折算）：

```bash
quota usage -t "llm" --model glm-4.6 --in 3200 --out 850   # 挂到当前任务
quota usage -t "web-search" --calls 1                       # 只记次数
```

执行大开销动作（读大文件、批量处理、长上下文调用）**之前**先过闸门：

```bash
quota check --tokens 50000 --cost 0.3
# 退出码 0 = 放行；2 = 阻断（必须放弃或缩小该动作）
```

### 3. 任务结束：结算

```bash
quota end -s "完成登录重构，消耗 0.62 元"
```

### 4. 随时查看

```bash
quota status              # 剩余额度、累计消耗、当前任务余量
quota stats --by tool     # 找出最烧钱的工具
quota stats --by task     # 找出最烧钱的任务类型
```

## 阻断时的汇报话术

收到 QUOTA_EXCEEDED 时，向用户如实报告，模板：

> ⚠ 额度已触顶：[原因，如"本任务预算 0.8 元已用完"]。
> 当前剩余额度 [X 元]，建议：a) 追加任务预算 b) 缩小任务范围 c) 停止任务。
> 请告诉我如何继续。

## 配置速查

```bash
quota set --total 100                          # local 模式总额度
quota set --provider deepseek                  # deepseek|openrouter|custom|local
quota set --model-price "glm-4.6,4,16"         # 模型单价: 名,输入,输出 (元/百万token)
quota set --min-reserve 1                      # 最低保留 1 元，低于即全线阻断
```

状态数据保存在 `~/.agent-quota/state.json`（可用 `AGENT_QUOTA_HOME` 或 `--home` 重定向）。
