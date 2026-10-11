// Package agentconfig 把"配置文件的 llm 段"翻译成运行时对象。
//
// 为什么单独一层：admin 与 node 都要建网关（admin 做自然语言起草，node 做诊断与巡检），
// 如果两边各写一份，会出现"admin 认为已配置、node 认为未配置"这种最难查的不一致。
// 这里同时是**唯一**的"模型到底配没配"判定点：
// 未配置时返回 llm.ErrNotConfigured，调用方必须把它转成明确的业务错误码，
// 而不是让 Agent 拿着空 provider 跑出一个看似成功的假结论。
package agentconfig

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agent"
	"github.com/tmnhs/crony/common/pkg/llm"
)

// ProviderOffline 离线 provider 名（确定性演示/测试用）。
const ProviderOffline = "offline"

// ProviderOpenAI 默认的 OpenAI 兼容 provider 名。
const ProviderOpenAI = "openai"

// Runtime 一次 Agent 会话所需的全部运行时对象。
type Runtime struct {
	Gateway *llm.Gateway
	Config  agent.Config
	Budget  llm.Budget
	// ProviderName 实际生效的 provider 名（UI 必须展示它，不允许写死宣传文案）。
	ProviderName string
	// Offline 是否为离线模式：离线模式的结论**不能**当真实模型能力宣传。
	Offline bool
}

// Configured 判断配置里是否声明了可用的模型。
func Configured(cfg models.LLM) bool {
	if !cfg.Enabled {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case ProviderOpenAI, ProviderOffline:
		return true
	default:
		return false
	}
}

// FromConfig 构造运行时。未配置时返回 llm.ErrNotConfigured。
func FromConfig(cfg models.LLM) (*Runtime, error) {
	if !Configured(cfg) {
		return nil, fmt.Errorf("%w: llm.enabled=false 或 llm.provider 为空（支持 openai / offline）", llm.ErrNotConfigured)
	}

	budget := budgetFrom(cfg)
	agentCfg := agentConfigFrom(cfg)

	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	var (
		p       llm.Provider
		offline bool
		name    string
	)

	switch provider {
	case ProviderOffline:
		offline = true
		name = ProviderOffline
		p = offlineProvider(cfg, budget)

	case ProviderOpenAI:
		name = strings.TrimSpace(cfg.Provider)
		if base := strings.TrimSpace(cfg.BaseURL); base != "" {
			name = base
		}
		op, err := llm.NewOpenAICompatProvider(llm.OpenAICompatConfig{
			Name:    "openai",
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
			Timeout: timeoutFrom(cfg),
		})
		if err != nil {
			return nil, err
		}
		p = op
	}

	gateway := llm.NewGateway(
		llm.WithProvider(p),
		llm.WithBudget(budget),
	)

	return &Runtime{
		Gateway:      gateway,
		Config:       agentCfg,
		Budget:       budget,
		ProviderName: name,
		Offline:      offline,
	}, nil
}

// budgetFrom 会话预算：留空即默认值，显式配置才有毛刺。
func budgetFrom(cfg models.LLM) llm.Budget {
	b := llm.DefaultBudget()
	if cfg.MaxCallsPerSession > 0 {
		b.MaxCalls = cfg.MaxCallsPerSession
	}
	if cfg.MaxTokensPerSession > 0 {
		b.MaxTokens = cfg.MaxTokensPerSession
	}
	if cfg.MaxDurationSeconds > 0 {
		b.MaxDuration = time.Duration(cfg.MaxDurationSeconds) * time.Second
	}
	return b.Normalize()
}

// agentConfigFrom Agent 循环上限。
func agentConfigFrom(cfg models.LLM) agent.Config {
	c := agent.DefaultConfig()
	if cfg.MaxSteps > 0 {
		c.MaxSteps = cfg.MaxSteps
	}
	if cfg.MaxToolCalls > 0 {
		c.MaxToolCalls = cfg.MaxToolCalls
	}
	if m := strings.TrimSpace(cfg.Model); m != "" {
		c.Model = m
	}
	if cfg.Temperature > 0 {
		temp := cfg.Temperature
		c.Temperature = &temp
	}
	return c.Normalize()
}

// timeoutFrom 单次模型调用超时。
func timeoutFrom(cfg models.LLM) time.Duration {
	if cfg.TimeoutSeconds > 0 {
		return time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return 60 * time.Second
}

// offlineProvider 构造确定性离线 provider。
//
// 用途：本机没有模型凭据时，仍然能把"工具调用 → 证据 → 结构化结论 → 轨迹落库"
// 整条链路跑通（演示、端到端测试、回归）。配置里给了 offline-tool-call/
// offline-response 就按脚本走，否则走 llm 自带的"已无脚本"收尾回答。
//
// 它**不是**模型：轨迹里 provider 恒为 offline，前端必须如实展示。
func offlineProvider(cfg models.LLM, budget llm.Budget) llm.Provider {
	script := make([]llm.Completion, 0, 2)
	if call, ok := parseOfflineToolCall(cfg.OfflineToolCall); ok {
		script = append(script, call)
	}
	if resp := strings.TrimSpace(cfg.OfflineResponse); resp != "" {
		script = append(script, llm.Completion{
			Content:      resp,
			FinishReason: "stop",
		})
	}
	if len(script) == 0 {
		return llm.NewFakeProvider(ProviderOffline)
	}
	return llm.NewFakeProvider(ProviderOffline, script...)
}

// parseOfflineToolCall 解析 `{"name":"log_search","arguments":{"keyword":"error"}}`。
func parseOfflineToolCall(raw string) (llm.Completion, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return llm.Completion{}, false
	}
	var spec struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(raw), &spec); err != nil || strings.TrimSpace(spec.Name) == "" {
		return llm.Completion{}, false
	}
	args, err := json.Marshal(spec.Arguments)
	if err != nil {
		args = []byte("{}")
	}
	if len(spec.Arguments) == 0 {
		args = []byte("{}")
	}
	return llm.Completion{
		ToolCalls:    []llm.ToolCall{{ID: "offline_" + spec.Name, Name: spec.Name, Arguments: string(args)}},
		FinishReason: "tool_calls",
	}, true
}
