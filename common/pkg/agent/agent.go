// Package agent 实现 crony 的 Agent 内核：**有界的 ReAct 循环**。
//
// 设计原则（阶段 2 的硬约束）：
//
//  1. 有界。模型调用次数、工具调用次数、token、墙钟时间四道闸门，任何一道触发
//     都**停下来并说明原因**，而不是无限自转。运维系统里"AI 卡住不返回"比"AI 给错答案"更糟。
//  2. 只读。Agent 手上的工具来自 tools.Registry，而该注册表**拒绝注册非只读工具**；
//     所以"模型想删库"这件事在类型层面就做不到，不是靠提示词约束。
//  3. 可回放。每一步（提示词、原始输出、工具参数、观测、证据、用量、耗时）都进 Trace，
//     落库后能离线回答"它当时为什么这么说"。
//  4. 失败要"降级成结论"而不是"抛异常白屏"：拿不到合规结构时，返回已有证据 + 明确的
//     终止原因，让运维至少能看到"查到哪儿了"。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/retry"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 终止哨兵错误。调用方可以用 errors.Is 判断"为什么没拿到结果"。
var (
	// ErrMaxSteps 达到最大模型调用步数。
	ErrMaxSteps = errors.New("agent: 达到最大步数上限")
	// ErrToolBudget 达到工具调用次数上限。
	ErrToolBudget = errors.New("agent: 达到工具调用次数上限")
	// ErrSchemaInvalid 模型输出反复不符合 Schema。
	ErrSchemaInvalid = errors.New("agent: 输出不符合要求的结构")
	// ErrNoProvider 没有可用 provider。
	ErrNoProvider = errors.New("agent: 没有可用的模型 provider")
)

// Config Agent 运行参数。零值可用（Normalize 会补默认值）。
type Config struct {
	// MaxSteps 最大模型调用次数（含修复轮）。默认 6。
	MaxSteps int
	// MaxToolCalls 一次运行内工具调用总次数上限。默认 8。
	MaxToolCalls int
	// MaxRepair 输出不合规时的修复轮上限。默认 1。
	MaxRepair int
	// MaxObserveBytes 单条观测回灌给模型的最大字节数。默认 8KiB。
	MaxObserveBytes int
	// MaxPromptBytes 轨迹里保留的提示词预览上限。默认 8KiB。
	MaxPromptBytes int
	// MaxContentBytes 轨迹里保留的模型输出上限。默认 4KiB。
	MaxContentBytes int
	// Model 覆盖 gateway 默认模型（空则用默认）。
	Model string
	// Temperature 透传给 provider（nil 用 provider 默认）。
	Temperature *float64
}

// DefaultConfig 默认参数。
func DefaultConfig() Config {
	return Config{
		MaxSteps:        6,
		MaxToolCalls:    8,
		MaxRepair:       1,
		MaxObserveBytes: 8 * 1024,
		MaxPromptBytes:  8 * 1024,
		MaxContentBytes: 4 * 1024,
	}
}

// Normalize 补齐默认值并做下限保护。
func (c Config) Normalize() Config {
	d := DefaultConfig()
	if c.MaxSteps <= 0 {
		c.MaxSteps = d.MaxSteps
	}
	if c.MaxToolCalls < 0 {
		c.MaxToolCalls = 0
	}
	if c.MaxToolCalls == 0 {
		c.MaxToolCalls = d.MaxToolCalls
	}
	if c.MaxRepair < 0 {
		c.MaxRepair = 0
	}
	if c.MaxObserveBytes <= 0 {
		c.MaxObserveBytes = d.MaxObserveBytes
	}
	if c.MaxPromptBytes <= 0 {
		c.MaxPromptBytes = d.MaxPromptBytes
	}
	if c.MaxContentBytes <= 0 {
		c.MaxContentBytes = d.MaxContentBytes
	}
	return c
}

// Input 一次 Agent 运行的输入。
type Input struct {
	// SessionID 会话标识（工具配额与审计按它分组）。
	SessionID string
	// Goal 目标的一句话描述（进轨迹，便于人看懂这次在查什么）。
	Goal string
	// System 系统提示词（由具体场景注入，见 PromptDiagnoseV1 / PromptNLJobV1）。
	System string
	// Context 背景材料片段（任务定义、最近运行结果……按顺序拼进用户消息）。
	Context []string
	// PromptVersion 提示词版本号（进轨迹，用于事后归因）。
	PromptVersion string
}

// Outcome 运行结果。
//
// 约定：**只要 Run 返回了 Outcome，Trace 一定非 nil**（即使同时返回了 error）。
// 这样上层可以在失败时照样落库轨迹——"失败的过程"同样是需要留证的信息。
type Outcome struct {
	// Structured 通过 Schema 校验的结构化结果（失败时为 nil）。
	Structured map[string]interface{}
	// Raw 模型最后一次输出的原文（截断）。
	Raw string
	// Trace 完整轨迹。
	Trace *Trace
	// StopReason 终止原因。
	StopReason StopReason
	// Warnings 过程中累积的告警。
	Warnings []string
	// Err 未得到合规结果时的原因（nil 表示 StopCompleted）。
	Err error
	// Repairs 实际使用的修复轮次数。
	Repairs int
}

// Failed 是否未得到合规结论。
func (o Outcome) Failed() bool { return o.Err != nil }

// Agent 有界的 ReAct 执行器。
type Agent struct {
	gateway  *llm.Gateway
	registry *tools.Registry
	cfg      Config
	observe  StepObserver
}

// Option 构造选项。
type Option func(*Agent)

// WithStepObserver 注册步骤回调（用于 SSE 实时展示）。
func WithStepObserver(fn StepObserver) Option {
	return func(a *Agent) { a.observe = fn }
}

// WithRegistry 替换/设置只读工具注册表。
func WithRegistry(r *tools.Registry) Option {
	return func(a *Agent) { a.registry = r }
}

// New 构造 Agent。registry 可以为 nil（无工具的纯推理 Agent）。
func New(gateway *llm.Gateway, registry *tools.Registry, cfg Config, opts ...Option) *Agent {
	a := &Agent{gateway: gateway, registry: registry, cfg: cfg.Normalize()}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a
}

// Config 返回归一化后的配置。
func (a *Agent) Config() Config { return a.cfg }

// Registry 返回工具注册表（可能为 nil）。
func (a *Agent) Registry() *tools.Registry { return a.registry }

// Run 执行一次有界 ReAct 循环。
//
// 返回的 error 与 Outcome.StopReason 一一对应：
//   - nil            → StopCompleted
//   - ErrMaxSteps    → StopMaxSteps
//   - ErrToolBudget  → StopToolBudget
//   - ErrSchemaInvalid → StopSchemaInvalid
//   - ErrNoProvider  → StopNoProvider
//   - 预算/取消/其他 → 对应 llm 包与 retry 包的哨兵错误
func (a *Agent) Run(ctx context.Context, in Input, schema *jsonschema.Schema) (Outcome, error) {
	cfg := a.cfg
	trace := newTrace(in.SessionID, in.Goal, in.PromptVersion)
	out := Outcome{Trace: trace}

	if a.gateway == nil || !a.gateway.HasProvider() {
		out.StopReason = StopNoProvider
		out.Err = fmt.Errorf("%w: 请在配置的 llm 段落里启用 provider", ErrNoProvider)
		return a.finish(out, trace)
	}
	if schema == nil {
		out.StopReason = StopSchemaInvalid
		out.Err = fmt.Errorf("%w: 场景未提供输出 Schema", ErrSchemaInvalid)
		return a.finish(out, trace)
	}
	if err := schema.Valid(); err != nil {
		out.StopReason = StopSchemaInvalid
		out.Err = fmt.Errorf("%w: 场景提供的 Schema 本身不合法: %v", ErrSchemaInvalid, err)
		return a.finish(out, trace)
	}

	var specs []llm.ToolSpec
	if a.registry != nil {
		specs = a.registry.Specs()
	}
	messages := make([]llm.Message, 0, 8)
	if strings.TrimSpace(in.System) != "" {
		messages = append(messages, llm.System(in.System))
	}
	if user := renderUserMessage(in, specs); user != "" {
		messages = append(messages, llm.User(user))
	}
	if len(specs) == 0 {
		trace.addWarning("本次运行没有可用工具：模型只能基于已提供的材料推理，结论可信度会下降")
		messages = append(messages, llm.User("注意：当前没有任何可用工具。如果已有材料不足以支撑结论，请把 evidence_sufficient 置为 false，并在 next_data_to_collect 里写清还需要什么数据。"))
	}

	toolCallsUsed := 0
	for step := 0; step < cfg.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			out.StopReason = StopCanceled
			out.Err = fmt.Errorf("%w: 运行被取消", retry.ErrCanceled)
			return a.finish(out, trace)
		}

		req := llm.Request{
			Model:         cfg.Model,
			Messages:      append([]llm.Message(nil), messages...),
			Tools:         specs,
			Temperature:   cfg.Temperature,
			PromptVersion: in.PromptVersion,
			Metadata:      map[string]string{"agent_step": fmt.Sprintf("%d", step), "session_id": in.SessionID},
		}
		comp, err := a.gateway.Complete(ctx, req)
		trace.addStep(modelStep(time.Now(), req, comp, err, cfg))
		a.emit(Step{Kind: StepModel, Provider: comp.Provider, Model: comp.Model, LatencyMS: comp.Latency.Milliseconds(), Err: errString(err)})
		if err != nil {
			out.StopReason, out.Err = classifyRunError(err)
			return a.finish(out, trace)
		}
		trace.mu.Lock()
		trace.ModelCalls++
		trace.Usage = trace.Usage.Add(comp.Usage)
		trace.mu.Unlock()

		messages = append(messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   comp.Content,
			ToolCalls: comp.ToolCalls,
		})

		// 没有工具调用 → 这是一次"想给结论"的回答，进入结构校验。
		if !comp.HasToolCalls() {
			obj, verr := llm.ValidateStructured(schema, comp.Content)
			if verr == nil {
				out.Structured = obj
				out.Raw = llm.Truncate(comp.Content, cfg.MaxContentBytes)
				out.StopReason = StopCompleted
				return a.finish(out, trace)
			}
			if out.Repairs < cfg.MaxRepair {
				out.Repairs++
				notice := fmt.Sprintf("第 %d 次输出不符合要求的结构，进入修复轮：%s", out.Repairs, verr.Error())
				trace.addStep(Step{Kind: StepNotice, Notice: notice, Content: llm.Truncate(comp.Content, cfg.MaxContentBytes)})
				trace.addWarning(notice)
				a.emit(Step{Kind: StepNotice, Notice: notice})
				messages = append(messages, llm.User(repairMessage(verr)))
				continue
			}
			out.Raw = llm.Truncate(comp.Content, cfg.MaxContentBytes)
			out.StopReason = StopSchemaInvalid
			out.Err = fmt.Errorf("%w: %v", ErrSchemaInvalid, verr)
			return a.finish(out, trace)
		}

		// 有工具调用 → 执行（只读），把观测回灌。
		for _, tc := range comp.ToolCalls {
			if toolCallsUsed >= cfg.MaxToolCalls {
				out.StopReason = StopToolBudget
				out.Err = fmt.Errorf("%w: 已用 %d 次", ErrToolBudget, toolCallsUsed)
				return a.finish(out, trace)
			}
			toolCallsUsed++
			observation, res, callErr := a.callTool(ctx, in.SessionID, tc, cfg)
			trace.addStep(Step{
				Kind:        StepTool,
				ToolName:    tc.Name,
				ToolArgs:    decodeArgsOrNil(tc.Arguments),
				Observation: llm.Truncate(observation, cfg.MaxObserveBytes),
				Evidence:    res.Evidence,
				DurationMS:  res.DurationMS,
				Err:         errString(callErr),
			})
			trace.addEvidence(res.Evidence...)
			trace.mu.Lock()
			trace.ToolCalls++
			trace.mu.Unlock()
			a.emit(Step{Kind: StepTool, ToolName: tc.Name, Observation: llm.Truncate(observation, cfg.MaxObserveBytes), DurationMS: res.DurationMS, Evidence: res.Evidence, Err: errString(callErr)})
			messages = append(messages, llm.ToolResult(tc.ID, tc.Name, llm.Truncate(observation, cfg.MaxObserveBytes)))
		}
		if ctx.Err() != nil {
			out.StopReason = StopCanceled
			out.Err = fmt.Errorf("%w: 运行被取消", retry.ErrCanceled)
			return a.finish(out, trace)
		}
	}

	out.StopReason = StopMaxSteps
	out.Err = fmt.Errorf("%w: 已用 %d 步", ErrMaxSteps, cfg.MaxSteps)
	return a.finish(out, trace)
}

// StopStopInvalidSchema 返回"场景参数不合法"的终止原因。
// （单独抽出来是为了让 Run 里的分支读起来更直白。）
func StopStopInvalidSchema() StopReason { return StopSchemaInvalid }

// finish 收尾：填充轨迹尾部信息、快照预算与工具统计、执行回调。
func (a *Agent) finish(out Outcome, trace *Trace) (Outcome, error) {
	trace.mu.Lock()
	trace.FinishedAt = time.Now()
	trace.StopReason = out.StopReason
	trace.StopExplain = out.StopReason.Explain()
	if a.gateway != nil {
		if tracker := a.gateway.Tracker(); tracker != nil {
			trace.Budget = tracker.Snapshot()
		}
	}
	if a.registry != nil {
		trace.ToolStats = a.registry.SessionStats(trace.SessionID)
	}
	out.Warnings = append([]string(nil), trace.Warnings...)
	trace.mu.Unlock()
	if out.Trace == nil {
		out.Trace = trace
	}
	return out, out.Err
}

// callTool 执行一次只读工具调用。
//
// 关键语义：**工具失败不算运行失败**。工具报错本身是一条有价值的观测
// （"日志文件不存在"往往正是根因），所以这里把它变成文本回灌给模型继续推理。
func (a *Agent) callTool(ctx context.Context, sessionID string, tc llm.ToolCall, cfg Config) (string, tools.Result, error) {
	if a.registry == nil {
		msg := "工具调用被拒绝: 本次运行没有配置任何工具"
		return msg, tools.Result{OK: false, Summary: "无工具", Err: msg}, tools.ErrUnknownTool
	}
	args, err := tc.DecodeArgs()
	if err != nil {
		msg := fmt.Sprintf("工具 %s 的参数不是合法 JSON 对象，请重新发起调用：%v", tc.Name, err)
		return msg, tools.Result{OK: false, Summary: "参数非法", Err: msg}, fmt.Errorf("%w: %v", tools.ErrInvalidArgs, err)
	}
	res, err := a.registry.Call(ctx, sessionID, tc.Name, args)
	if err != nil {
		msg := fmt.Sprintf("工具 %s 调用被拒绝: %v", tc.Name, err)
		return msg, res, err
	}
	text := res.Text()
	if text == "" {
		text = "（工具没有返回内容）"
	}
	return text, res, nil
}

// emit 安全触发步骤回调（回调 panic 不能影响 Agent 主流程）。
func (a *Agent) emit(step Step) {
	if a.observe == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			_ = r // 观测回调的 bug 不应该拖垮诊断
		}
	}()
	a.observe(step)
}

// modelStep 构造模型步骤记录。
func modelStep(at time.Time, req llm.Request, comp llm.Completion, err error, cfg Config) Step {
	step := Step{
		Kind:          StepModel,
		At:            at,
		Provider:      comp.Provider,
		Model:         comp.Model,
		PromptVersion: req.PromptVersion,
		PromptBytes:   approxPromptBytes(req.Messages),
		PromptPreview: llm.Truncate(renderPrompt(req.Messages), cfg.MaxPromptBytes),
		Content:       llm.Truncate(comp.Content, cfg.MaxContentBytes),
		ToolCalls:     comp.ToolCalls,
		Usage:         comp.Usage,
		LatencyMS:     comp.Latency.Milliseconds(),
		FinishReason:  comp.FinishReason,
		Truncated:     len(comp.Content) > cfg.MaxContentBytes,
		Err:           errString(err),
	}
	if err != nil {
		step.Provider = req.Provider
		step.Model = req.Model
	}
	return step
}

// renderPrompt 把消息渲染成可读文本（轨迹预览用）。
func renderPrompt(messages []llm.Message) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString("[" + string(m.Role) + "] ")
		if m.Content != "" {
			b.WriteString(m.Content)
		}
		for _, tc := range m.ToolCalls {
			b.WriteString(fmt.Sprintf(" <工具调用 %s(%s)>", tc.Name, tc.Arguments))
		}
		if m.ToolCallID != "" {
			b.WriteString(" <结果 " + m.ToolCallID + ">")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// approxPromptBytes 估算提示词体积（用于观察上下文膨胀）。
func approxPromptBytes(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		n += len(m.Content) + len(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments)
		}
	}
	return n
}

// renderUserMessage 把背景材料拼成一条用户消息。
func renderUserMessage(in Input, specs []llm.ToolSpec) string {
	var b strings.Builder
	if strings.TrimSpace(in.Goal) != "" {
		b.WriteString("## 目标\n" + in.Goal + "\n")
	}
	for i, part := range in.Context {
		if strings.TrimSpace(part) == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("\n## 材料 %d\n%s\n", i+1, part))
	}
	if len(specs) > 0 {
		names := make([]string, 0, len(specs))
		for _, s := range specs {
			names = append(names, s.Name)
		}
		b.WriteString("\n## 可用工具\n" + strings.Join(names, "、") + "\n")
		b.WriteString("需要更多信息就调用工具；材料已足够就直接给出结论。\n")
	}
	return b.String()
}

// repairMessage 生成修复轮的用户消息。
func repairMessage(err error) string {
	return "上一次输出不能通过结构校验，请**只输出一个 JSON 对象**（不要解释、不要代码块围栏），严格符合要求：" +
		"\n\n校验错误：" + err.Error() +
		"\n\n请修正后重新输出完整 JSON。"
}

// decodeArgsOrNil 解析工具参数（失败返回 nil，仅用于展示）。
func decodeArgsOrNil(raw string) map[string]interface{} {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// classifyRunError 把 llm 包错误映射成终止原因。
func classifyRunError(err error) (StopReason, error) {
	switch {
	case err == nil:
		return StopCompleted, nil
	case errors.Is(err, llm.ErrBudgetExceeded):
		return StopLLMBudget, err
	case errors.Is(err, retry.ErrCanceled), errors.Is(err, context.Canceled):
		return StopCanceled, fmt.Errorf("%w: %v", retry.ErrCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return StopError, err
	case errors.Is(err, llm.ErrNoProvider), errors.Is(err, llm.ErrNotConfigured):
		return StopNoProvider, err
	default:
		return StopError, err
	}
}

// errString 把错误转成字符串（nil → 空串）。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return llm.Redact(err.Error())
}
