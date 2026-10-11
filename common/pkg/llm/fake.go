package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// FakeProvider 离线 provider：测试与"没有模型也能跑通全链路"的演示用。
//
// 存在的意义不只是省测试成本：Agent 的**编排逻辑**（循环、工具分发、预算、
// 轨迹）必须能在没有模型、没有网络的情况下被确定性地测到——
// 否则"Agent 有 bug"和"模型不听话"永远分不开。
type FakeProvider struct {
	mu        sync.Mutex
	name      string
	script    []Completion
	errs      []error
	idx       int
	responder func(Request) (Completion, error)
	usage     Usage
	requests  []Request
}

// NewFakeProvider 创建离线 provider，按顺序返回给定结果；用完后默认回一句固定的收尾文本。
func NewFakeProvider(name string, script ...Completion) *FakeProvider {
	if name == "" {
		name = "fake"
	}
	return &FakeProvider{name: name, script: script, usage: Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}}
}

// WithResponder 用回调决定响应（优先级高于 script）。
func (p *FakeProvider) WithResponder(fn func(Request) (Completion, error)) *FakeProvider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responder = fn
	return p
}

// WithUsage 设置每次调用的模拟用量。
func (p *FakeProvider) WithUsage(u Usage) *FakeProvider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.usage = u
	return p
}

// WithErrors 设置按调用序号返回的错误（与 script 交替消费）。
func (p *FakeProvider) WithErrors(errs ...error) *FakeProvider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = errs
	return p
}

// Name 实现 Provider。
func (p *FakeProvider) Name() string { return p.name }

// Complete 实现 Provider。
func (p *FakeProvider) Complete(_ context.Context, req Request) (Completion, error) {
	p.mu.Lock()
	responder := p.responder
	var scripted *Completion
	if p.idx < len(p.script) {
		c := p.script[p.idx]
		scripted = &c
	}
	var scriptedErr error
	if p.idx < len(p.errs) {
		scriptedErr = p.errs[p.idx]
	}
	p.idx++
	p.requests = append(p.requests, req)
	usage := p.usage
	p.mu.Unlock()

	if responder != nil {
		comp, err := responder(req)
		if comp.Usage.IsZero() {
			comp.Usage = usage
		}
		if comp.Provider == "" {
			comp.Provider = p.name
		}
		return comp, err
	}
	if scriptedErr != nil {
		return Completion{Provider: p.name, Usage: usage}, scriptedErr
	}
	if scripted != nil {
		comp := *scripted
		if comp.Usage.IsZero() {
			comp.Usage = usage
		}
		if comp.Provider == "" {
			comp.Provider = p.name
		}
		return comp, nil
	}
	// 脚本用尽：返回一个"不再调用工具"的收尾回答，避免测试里出现死循环
	return Completion{Content: "（离线 provider：已无脚本，结束）", Provider: p.name, Usage: usage, FinishReason: "stop"}, nil
}

// Requests 返回收到的请求（断言提示词内容用）。
func (p *FakeProvider) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Request, len(p.requests))
	copy(out, p.requests)
	return out
}

// CallCount 已调用次数。
func (p *FakeProvider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idx
}

// 便捷构造：让测试与示例代码读起来接近自然语言。

// FakeToolCall 构造"要求调用工具"的响应。
func FakeToolCall(toolName string, args map[string]interface{}) Completion {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = []byte("{}")
	}
	return Completion{
		ToolCalls:    []ToolCall{{ID: fmt.Sprintf("call_%s", toolName), Name: toolName, Arguments: string(raw)}},
		FinishReason: "tool_calls",
	}
}

// FakeText 构造文本响应。
func FakeText(content string) Completion {
	return Completion{Content: content, FinishReason: "stop"}
}

// FakeJSON 构造"输出结构化 JSON"的响应（内容为 JSON 文本）。
func FakeJSON(v interface{}) Completion {
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte("{}")
	}
	return Completion{Content: string(raw), FinishReason: "stop"}
}
