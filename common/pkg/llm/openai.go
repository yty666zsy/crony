package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// defaultMaxResponseBytes 单次响应体上限（1 MiB）。
// 与阶段 1 执行器的输出上限同理：一个坏掉/被劫持的端点不该把内存打满。
const defaultMaxResponseBytes = 1 << 20

// OpenAICompatConfig OpenAI 兼容端点配置。
//
// 之所以做成"OpenAI 兼容"而不是给每家写一个适配器：
// 火山方舟、DeepSeek、通义、vLLM、Ollama、LiteLLM 等绝大多数端点都实现了
// `/chat/completions`，先把这一条路走通，比同时维护五套适配器更可靠。
type OpenAICompatConfig struct {
	// Name provider 名（落库用，如 "ark" / "deepseek" / "vllm"）。
	Name string
	// BaseURL 例：https://ark.cn-beijing.volces.com/api/v3 或 http://127.0.0.1:8000/v1
	BaseURL string
	// APIKey 密钥。**不要写进配置文件**，用环境变量注入（见 config.Llm）。
	APIKey string
	// Model 默认模型名。
	Model string
	// Timeout 单次请求超时，默认 120s（Agent 场景下模型思考较久，不要用 10s）。
	Timeout time.Duration
	// HTTPClient 可选自定义客户端（测试注入 httptest.Server 时用默认即可）。
	HTTPClient *http.Client
	// ExtraHeaders 额外请求头（部分自建网关需要自定义鉴权头）。
	ExtraHeaders map[string]string
	// ChatPath 覆盖路径，默认 /chat/completions。
	ChatPath string
	// MaxResponseBytes 响应体上限，默认 1 MiB。
	MaxResponseBytes int
}

// OpenAICompatProvider 实现 Provider。
type OpenAICompatProvider struct {
	cfg     OpenAICompatConfig
	client  *http.Client
	chatURL string
}

// NewOpenAICompatProvider 构造 provider。
func NewOpenAICompatProvider(cfg OpenAICompatConfig) (*OpenAICompatProvider, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("%w: base_url 不能为空", ErrNotConfigured)
	}
	if cfg.Name == "" {
		cfg.Name = "openai-compatible"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = defaultMaxResponseBytes
	}
	if cfg.ChatPath == "" {
		cfg.ChatPath = "/chat/completions"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &OpenAICompatProvider{
		cfg:     cfg,
		client:  client,
		chatURL: strings.TrimRight(cfg.BaseURL, "/") + "/" + strings.TrimLeft(cfg.ChatPath, "/"),
	}, nil
}

// Name 实现 Provider。
func (p *OpenAICompatProvider) Name() string { return p.cfg.Name }

// Model 返回默认模型名。
func (p *OpenAICompatProvider) Model() string { return p.cfg.Model }

// ---- 线上协议结构（只声明用得到的字段，避免端点新增字段导致解析失败） ----

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireToolSpec struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Parameters  *jsonschema.Schema `json:"parameters,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireToolSpec `json:"function"`
}

type wireResponseFormat struct {
	Type string `json:"type"`
}

type wireRequest struct {
	Model          string              `json:"model"`
	Messages       []wireMessage       `json:"messages"`
	Tools          []wireTool          `json:"tools,omitempty"`
	Temperature    *float64            `json:"temperature,omitempty"`
	MaxTokens      int                 `json:"max_tokens,omitempty"`
	ResponseFormat *wireResponseFormat `json:"response_format,omitempty"`
}

// rawToolCall 兼容 "arguments 是字符串"（标准）与 "arguments 是对象"（部分自建端点）两种形态。
type rawToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string        `json:"role"`
			Content   string        `json:"content"`
			ToolCalls []rawToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string      `json:"message"`
		Type    string      `json:"type"`
		Code    interface{} `json:"code"`
	} `json:"error"`
	// 部分端点把错误放在顶层 message 字段。
	Message string `json:"message"`
}

// Complete 实现 Provider。
func (p *OpenAICompatProvider) Complete(ctx context.Context, req Request) (Completion, error) {
	started := time.Now()
	comp := Completion{Provider: p.cfg.Name, Model: req.Model}
	if comp.Model == "" {
		comp.Model = p.cfg.Model
	}

	body, err := p.buildBody(req, comp.Model)
	if err != nil {
		return comp, retry.Fatal(err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.chatURL, bytes.NewReader(body))
	if err != nil {
		return comp, retry.Fatal(fmt.Errorf("%w: 构造请求失败: %v", ErrInvalidResponse, err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	for k, v := range p.cfg.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		comp.Latency = time.Since(started)
		return comp, classifyTransportError(ctx, err)
	}
	defer resp.Body.Close()

	// 多读 1 字节用于判断"是否被截断"
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(p.cfg.MaxResponseBytes)+1))
	if readErr != nil {
		comp.Latency = time.Since(started)
		return comp, retry.Retryable(fmt.Errorf("%w: 读取响应失败: %v", ErrInvalidResponse, readErr))
	}
	if len(raw) > p.cfg.MaxResponseBytes {
		comp.Latency = time.Since(started)
		return comp, retry.Fatal(fmt.Errorf("%w: 响应体超过 %d 字节上限", ErrInvalidResponse, p.cfg.MaxResponseBytes))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		comp.Latency = time.Since(started)
		return comp, classifyHTTPError(resp.StatusCode, raw)
	}

	var parsed wireResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		comp.Latency = time.Since(started)
		// 200 但解析失败：可能是被截断/网关插了 HTML，按可重试处理（有别于 4xx 的确定性失败）
		return comp, retry.Retryable(fmt.Errorf("%w: 响应不是合法 JSON: %v", ErrInvalidResponse, err))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		comp.Latency = time.Since(started)
		return comp, retry.Fatal(fmt.Errorf("%w: 端点返回错误: %s", ErrInvalidResponse, Redact(parsed.Error.Message)))
	}
	if len(parsed.Choices) == 0 {
		msg := parsed.Message
		if msg == "" {
			msg = "choices 为空"
		}
		comp.Latency = time.Since(started)
		return comp, retry.Fatal(fmt.Errorf("%w: %s", ErrInvalidResponse, Redact(msg)))
	}

	choice := parsed.Choices[0]
	comp.Content = choice.Message.Content
	comp.FinishReason = choice.FinishReason
	comp.Usage = Usage{
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		TotalTokens:      parsed.Usage.TotalTokens,
	}.Normalize()
	if parsed.Model != "" {
		comp.Model = parsed.Model
	}
	for _, tc := range choice.Message.ToolCalls {
		args := normalizeArguments(tc.Function.Arguments)
		name := tc.Function.Name
		id := tc.ID
		if id == "" {
			// 有些端点不回 id；必须补一个，否则工具结果无法回灌（tool_call_id 是必填）
			id = fmt.Sprintf("call_%d_%s", len(comp.ToolCalls), name)
		}
		comp.ToolCalls = append(comp.ToolCalls, ToolCall{ID: id, Name: name, Arguments: args})
	}
	comp.Latency = time.Since(started)
	return comp, nil
}

// buildBody 组装请求体。
func (p *OpenAICompatProvider) buildBody(req Request, model string) ([]byte, error) {
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("%w: model 不能为空（provider %s）", ErrNotConfigured, p.cfg.Name)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: messages 不能为空", ErrInvalidResponse)
	}
	wire := wireRequest{
		Model:       model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	for _, m := range req.Messages {
		wm := wireMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: wireFunction{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		wire.Messages = append(wire.Messages, wm)
	}
	for _, t := range req.Tools {
		wire.Tools = append(wire.Tools, wireTool{
			Type: "function",
			Function: wireToolSpec{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	if req.ResponseSchema != nil {
		// 注意：OpenAI 要求"使用 json_object 时提示词里必须出现 JSON 字样"，
		// Agent 的提示词已满足（见 common/pkg/agent/prompt.go）。
		wire.ResponseFormat = &wireResponseFormat{Type: "json_object"}
	}
	return json.Marshal(wire)
}

// normalizeArguments 把 arguments 规整为 JSON 文本。
func normalizeArguments(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "{}"
	}
	// 标准形态：JSON 字符串，内容是 JSON 文本
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if strings.TrimSpace(s) == "" {
				return "{}"
			}
			return s
		}
	}
	return trimmed
}

// classifyHTTPError 把 HTTP 状态码映射到"可重试/不可重试"。
//
// 判定依据（与阶段 1 执行器对 HTTP 任务的处理保持一致）：
//   - 401/403：密钥错/过期 → 重试只会浪费时间，且可能触发风控封禁；
//   - 400/404/422：请求或模型名不对 → 确定性失败；
//   - 408/425/429：超时/过早/限流 → 可重试（429 尤其常见，必须退避）；
//   - 5xx：服务端故障 → 可重试。
func classifyHTTPError(status int, body []byte) error {
	snippet := Redact(Truncate(strings.TrimSpace(string(body)), 400))
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return retry.Fatal(fmt.Errorf("%w: HTTP %d（鉴权失败，检查 API Key/权限）: %s", ErrNotConfigured, status, snippet))
	case status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity:
		return retry.Fatal(fmt.Errorf("%w: HTTP %d: %s", ErrInvalidResponse, status, snippet))
	case status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests:
		return retry.Retryable(fmt.Errorf("%w: HTTP %d: %s", ErrRateLimited, status, snippet))
	case status >= 500:
		return retry.Retryable(fmt.Errorf("%w: HTTP %d: %s", ErrInvalidResponse, status, snippet))
	default:
		return retry.Fatal(fmt.Errorf("%w: HTTP %d: %s", ErrInvalidResponse, status, snippet))
	}
}

// classifyTransportError 处理连接层错误（超时/取消/DNS）。
func classifyTransportError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		// 主动取消（例如用户点了"停止"）→ 绝不重试
		return fmt.Errorf("%w: %v", retry.ErrCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return retry.Retryable(fmt.Errorf("%w: %v", ErrTimeout, err))
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %v", retry.ErrCanceled, ctx.Err())
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return retry.Retryable(fmt.Errorf("%w: %v", ErrTimeout, Redact(err.Error())))
	}
	// 连接被拒/DNS 失败等：可能对端正在重启，保守地按可重试处理（受重试策略与预算双重约束）
	return retry.Retryable(fmt.Errorf("%w: %v", ErrInvalidResponse, Redact(err.Error())))
}
