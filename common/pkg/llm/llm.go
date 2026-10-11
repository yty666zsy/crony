// Package llm 是阶段 2 的 **LLM 网关**：全项目与模型交互的唯一入口。
//
// 设计取舍（对应方案 §4 D3「绝不散落在业务代码里」）：
//   - Provider 接口只做"把消息发出去、把结果收回来"，不管预算、校验、记账；
//   - Gateway 统一负责：provider 路由、预算熔断、token 记账、输出 Schema 校验、
//     审计回调——业务代码（Agent Runtime）只认识 Gateway；
//   - 错误必须带"可重试/不可重试"语义（复用 common/pkg/retry 的标记），
//     否则一次 401 会被重试 3 次、一次 429 反而直接失败；
//   - **默认离线可用**：内置 FakeProvider，没有真实模型也能跑通全链路与测试。
//
// 为什么先做单 provider：多模型路由的价值依赖真实配额与成本数据，
// 在没有真机验证之前把"路由"做厚只会掩盖问题（阶段 3 再补配额与降级）。
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
)

// Role 消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool 工具执行结果回灌（OpenAI 兼容协议里必须带 tool_call_id）。
	RoleTool Role = "tool"
)

// ToolCall 模型请求调用某个工具。
type ToolCall struct {
	// ID 模型给出的调用 ID，回灌结果时必须原样带回。
	ID string
	// Name 工具名（必须能在工具注册表里找到，否则按"幻觉工具"处理）。
	Name string
	// Arguments 原始 JSON 文本（**不做**隐式修复，交给工具层的 Schema 校验）。
	Arguments string
}

// DecodeArgs 把参数 JSON 解成对象。
func (c ToolCall) DecodeArgs() (map[string]interface{}, error) {
	trimmed := strings.TrimSpace(c.Arguments)
	if trimmed == "" {
		return map[string]interface{}{}, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("工具 %s 的参数不是合法 JSON: %w", c.Name, err)
	}
	return out, nil
}

// ToolSpec 提供给模型的工具声明。
type ToolSpec struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Parameters  *jsonschema.Schema `json:"parameters,omitempty"`
}

// Message 一条对话消息。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// System 构造 system 消息。
func System(content string) Message { return Message{Role: RoleSystem, Content: content} }

// User 构造 user 消息。
func User(content string) Message { return Message{Role: RoleUser, Content: content} }

// Assistant 构造 assistant 消息。
func Assistant(content string) Message { return Message{Role: RoleAssistant, Content: content} }

// ToolResult 构造工具结果消息。
func ToolResult(callID, name, content string) Message {
	return Message{Role: RoleTool, Content: content, ToolCallID: callID, Name: name}
}

// Usage token 用量。字段名对齐 OpenAI，便于直接落库与做成本统计。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Add 累加用量。
func (u Usage) Add(o Usage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens + o.PromptTokens,
		CompletionTokens: u.CompletionTokens + o.CompletionTokens,
		TotalTokens:      u.TotalTokens + o.TotalTokens,
	}
}

// IsZero 是否为 0（部分自建端点不返回 usage）。
func (u Usage) IsZero() bool { return u == Usage{} }

// Normalize 补齐 total（有些端点只给 prompt/completion）。
func (u Usage) Normalize() Usage {
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u
}

// Request 一次模型调用请求。
type Request struct {
	// Model 显式指定模型；为空则用 provider 默认模型。
	Model    string
	Messages []Message
	Tools    []ToolSpec
	// Temperature 为空表示用端点默认值（不同厂商默认值差异很大，不要替用户决定）。
	Temperature *float64
	MaxTokens   int
	// ResponseSchema 非空 → 要求模型输出 JSON 并**强校验**；
	// 校验不通过返回 ErrSchemaViolation（可重试，因为可能是随机性导致的格式跑偏）。
	ResponseSchema *jsonschema.Schema
	// PromptVersion 提示词版本号，落库后用于"效果回滚"与 A/B（阶段 2 只记录不切换）。
	PromptVersion string
	// Provider 指定使用哪个 provider（为空用默认）。
	Provider string
	// Metadata 透传标记（如 session_id / run_id），用于审计与关联。
	Metadata map[string]string
}

// Completion 一次模型调用的结果。
type Completion struct {
	Content      string
	ToolCalls    []ToolCall
	Usage        Usage
	Provider     string
	Model        string
	FinishReason string
	Latency      time.Duration
	// Structured 当 Request.ResponseSchema 非空且校验通过时的结构化结果。
	Structured map[string]interface{}
}

// HasToolCalls 模型是否要求调用工具。
func (c Completion) HasToolCalls() bool { return len(c.ToolCalls) > 0 }

// Provider 模型供应商抽象。
type Provider interface {
	// Name provider 名称（落库与路由用）。
	Name() string
	// Complete 执行一次调用。约定：不 panic；失败时返回 error（并带重试语义标记）。
	Complete(ctx context.Context, req Request) (Completion, error)
}

// 错误语义：全部用 errors.Is 判定，避免调用方比对字符串。
var (
	// ErrNotConfigured 未配置 provider / 鉴权失败（重试无意义）。
	ErrNotConfigured = errors.New("llm: provider not configured")
	// ErrRateLimited 被限流（可重试）。
	ErrRateLimited = errors.New("llm: rate limited")
	// ErrTimeout 调用超时（可重试）。
	ErrTimeout = errors.New("llm: request timeout")
	// ErrInvalidResponse 端点返回了无法解析/缺少必要字段的响应。
	ErrInvalidResponse = errors.New("llm: invalid response")
	// ErrSchemaViolation 输出不满足 ResponseSchema（可重试一次，之后应降级为"证据不足"）。
	ErrSchemaViolation = errors.New("llm: output violates response schema")
	// ErrBudgetExceeded 会话预算耗尽（**不可重试**，必须转人工）。
	ErrBudgetExceeded = errors.New("llm: budget exceeded")
	// ErrNoProvider 请求指定的 provider 不存在。
	ErrNoProvider = errors.New("llm: provider not found")
)

// 凭据形态（保守但线性）：
//   - `Bearer <token>`：token 至少 8 个 [A-Za-z0-9._-]
//   - `sk-<token>`：**要求词边界**，否则 "task-list"、"disk-space" 这类正常英文会被误伤
//
// 阈值（8 个字符）是刻意的：宁可漏掉一个超短假 key，也不要把
// "task-runner failed" 这类诊断文本涂成 "task-*** failed"，
// 那会让轨迹彻底失去可读性。
var (
	bearerPattern = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._\-]{8,}`)
	apiKeyPattern = regexp.MustCompile(`\b(sk-)[A-Za-z0-9_\-]{8,}`)
)

// Redact 把可能出现的凭据从文本里抹掉（用于错误信息与日志）。
//
// 为什么必须有：HTTP 错误里经常整段回显请求头，一旦把 `Authorization: Bearer sk-...`
// 写进 job_run / agent_step，凭据就永久躺在数据库里了。
//
// 实现说明：这里用正则而不是手写循环。手写版本（按前缀反复 Index + 替换）
// 存在一个真实踩过的坑：替换时保留了前缀（`sk-` → `sk-***`），
// 下一次 Index 又会命中同一个前缀，字符串不再收缩 → **死循环**。
// 正则替换一次成型，天然线性。
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = bearerPattern.ReplaceAllString(s, "${1}***")
	return apiKeyPattern.ReplaceAllString(s, "${1}***")
}

// Truncate 截断长文本（提示词/响应可能很大，落库前统一收口）。
//
// limit 是**字节**预算，但必须回退到 UTF-8 字符边界：
// 直接 `s[:limit]` 会把中文/emoji 从中间砍断，产生非法 UTF-8 字节序列。
// 后果不是"看起来乱码"而是**整行写不进去**——MySQL 的 utf8mb4 列会直接
// `Error 1366 Incorrect string value` 拒绝插入（真实事故：Agent 轨迹的
// prompt_preview 被砍断，导致每次诊断都"会话落库成功但步骤全失败"，
// 前端只表现为回放里没有步骤）。内存单测不校验字符集，只有真库能抓出来。
func Truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	suffix := fmt.Sprintf("…[已截断 %d 字节]", len(s)-cut)
	if cut <= 0 {
		// 预算连一个字符都装不下：宁可不留原文，也不能留半个字符。
		return suffix
	}
	return s[:cut] + suffix
}
