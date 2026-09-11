package main

// 服务商余额查询：deepseek / openrouter / custom(任意 JSON API) / local。
// 查询结果缓存 balanceTTL 秒，避免每次状态查询都打 API。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// lookupPath 从 map 中按 "a.b.0.c" 点分路径取值（数字段表示数组下标）。
func lookupPath(v any, path string) (any, error) {
	cur := v
	if path == "" {
		return cur, nil
	}
	for _, seg := range strings.Split(path, ".") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		switch node := cur.(type) {
		case map[string]any:
			nv, ok := node[seg]
			if !ok {
				return nil, fmt.Errorf("路径 %q 在对象中不存在字段 %q", path, seg)
			}
			cur = nv
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("路径 %q 段 %q 不是有效数组下标", path, seg)
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("路径 %q 在标量 %T 上无法继续", path, seg)
		}
	}
	return cur, nil
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(n), 64)
	case int64:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	default:
		return 0, fmt.Errorf("无法将 %T 转为数值", v)
	}
}

// resolveToken 按优先级解析 API token: 配置 > 环境变量。
func resolveToken(cfg *Config) string {
	if cfg.ProviderToken != "" {
		return cfg.ProviderToken
	}
	candidates := []string{"AGENT_QUOTA_API_KEY"}
	switch cfg.Provider {
	case "deepseek":
		candidates = append(candidates, "DEEPSEEK_API_KEY")
	case "openrouter":
		candidates = append(candidates, "OPENROUTER_API_KEY")
	case "custom":
		candidates = append(candidates, "CUSTOM_PROVIDER_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY", "OPENAI_API_KEY")
	}
	for _, k := range candidates {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// queryProviderBalance 实时查询服务商余额。
func queryProviderBalance(cfg *Config) (*BalanceCache, error) {
	token := resolveToken(cfg)
	switch cfg.Provider {
	case "local":
		return nil, fmt.Errorf("local 模式无需查询服务商")
	case "deepseek":
		return fetchBalance("https://api.deepseek.com/user/balance", token, "balance_infos.0.total_balance", "balance_infos.0.currency", cfg)
	case "openrouter":
		bc, err := fetchBalance("https://openrouter.ai/api/v1/credits", token, "", "data.total_credits_usage.currency", cfg)
		if err != nil {
			return nil, err
		}
		// openrouter: 剩余 = total_credits - total_usage，在 fetch 内已处理特殊逻辑
		return bc, nil
	case "custom":
		if cfg.ProviderURL == "" {
			return nil, fmt.Errorf("custom provider 未配置 provider_url")
		}
		if cfg.JSONPath == "" {
			return nil, fmt.Errorf("custom provider 未配置 json_path")
		}
		return fetchBalance(cfg.ProviderURL, token, cfg.JSONPath, "", cfg)
	default:
		return nil, fmt.Errorf("未知 provider: %q", cfg.Provider)
	}
}

// fetchBalance 请求 URL 并按 jsonPath 提取余额数值。
// openrouter 特殊：路径为空时自动计算 data.total_credits - data.total_usage。
func fetchBalance(url, token, jsonPath, currencyPath string, cfg *Config) (*BalanceCache, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求余额接口失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("余额接口返回 HTTP %d（检查 API key 与网络）", resp.StatusCode)
	}
	var body any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("余额接口响应解析失败: %w", err)
	}

	var amount float64
	if cfg.Provider == "openrouter" {
		credits, err1 := toFloat(mustLookup(body, "data.total_credits"))
		usage, err2 := toFloat(mustLookup(body, "data.total_usage"))
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("openrouter 响应缺少 data.total_credits/total_usage")
		}
		amount = credits - usage
	} else {
		v, err := lookupPath(body, jsonPath)
		if err != nil {
			return nil, err
		}
		if amount, err = toFloat(v); err != nil {
			return nil, fmt.Errorf("余额字段非数值: %w", err)
		}
	}

	currency := cfg.Currency
	if currencyPath != "" {
		if cv, err := lookupPath(body, currencyPath); err == nil {
			if cs, ok := cv.(string); ok && cs != "" {
				currency = cs
			}
		}
	}
	return &BalanceCache{
		Amount:    amount,
		Currency:  currency,
		CheckedAt: nowUnix(),
		Source:    cfg.Provider,
	}, nil
}

func mustLookup(v any, path string) any {
	out, err := lookupPath(v, path)
	if err != nil {
		return nil
	}
	return out
}

// getRemaining 返回当前可用额度（金额口径）。
// local 模式: local_total - total_cost
// provider 模式: 服务商余额（缓存 60s），失败时回退缓存旧值
// 若 local_total > 0 且非 local 模式，取两者较小值（本地总额作为双保险）。
func getRemaining(st *State, refresh bool) (*BalanceCache, error) {
	cfg := &st.Config
	if cfg.Provider == "local" {
		return &BalanceCache{
			Amount:    cfg.LocalTotal - st.Counters.TotalCost,
			Currency:  cfg.Currency,
			CheckedAt: nowUnix(),
			Source:    "local",
		}, nil
	}

	// provider 模式
	var bc *BalanceCache
	var err error
	now := nowUnix()
	if !refresh && st.LastBalance != nil && now-st.LastBalance.CheckedAt < balanceTTL {
		bc = st.LastBalance
	} else {
		bc, err = queryProviderBalance(cfg)
		if err != nil {
			// 回退旧缓存
			if st.LastBalance != nil && now-st.LastBalance.CheckedAt < 24*3600 {
				fallback := *st.LastBalance
				fallback.Source += " (缓存,实时查询失败)"
				bc = &fallback
			} else {
				return nil, err
			}
		}
		st.LastBalance = bc
	}

	amount := bc.Amount
	if cfg.LocalTotal > 0 {
		local := cfg.LocalTotal - st.Counters.TotalCost
		if local < amount {
			bc = &BalanceCache{Amount: local, Currency: cfg.Currency, CheckedAt: now, Source: "local_total(取较小值)"}
			return bc, nil
		}
	}
	return &BalanceCache{Amount: amount, Currency: bc.Currency, CheckedAt: bc.CheckedAt, Source: bc.Source}, nil
}
