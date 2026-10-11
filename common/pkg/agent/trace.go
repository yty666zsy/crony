package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// TraceVersion 轨迹结构版本。
//
// 为什么要版本号：轨迹会落库（agent_step 表），几周后回放旧轨迹时如果结构变了，
// 没有版本号就无法判断"字段缺失"是数据问题还是代码问题。
const TraceVersion = 1

// StopReason 循环终止原因。**每一种都要能解释"为什么停"**，
// 而不是统一报一个 "agent failed"——运维场景里"为什么没得出结论"和"结论是什么"一样重要。
type StopReason string

const (
	// StopCompleted 拿到符合 Schema 的结论。
	StopCompleted StopReason = "completed"
	// StopMaxSteps 达到最大步数（预算保护）。
	StopMaxSteps StopReason = "max_steps"
	// StopToolBudget 达到工具调用次数上限。
	StopToolBudget StopReason = "tool_budget"
	// StopLLMBudget LLM 预算（次数/token/时长）耗尽。
	StopLLMBudget StopReason = "llm_budget"
	// StopCanceled 被主动取消。
	StopCanceled StopReason = "canceled"
	// StopSchemaInvalid 多次尝试后输出仍不符合 Schema。
	StopSchemaInvalid StopReason = "schema_invalid"
	// StopNoProvider 没有可用的模型 provider。
	StopNoProvider StopReason = "no_provider"
	// StopError 其他错误（网络/鉴权/端点故障）。
	StopError StopReason = "error"
)

// Explain 给出人话解释（用于控制台与诊断报告）。
func (r StopReason) Explain() string {
	switch r {
	case StopCompleted:
		return "已得到结构化结论"
	case StopMaxSteps:
		return "达到最大步数上限（保护性终止，结论可能不完整）"
	case StopToolBudget:
		return "达到工具调用次数上限（保护性终止）"
	case StopLLMBudget:
		return "模型预算耗尽（保护性终止）"
	case StopCanceled:
		return "被主动取消"
	case StopSchemaInvalid:
		return "多次尝试后输出仍不符合要求的结构"
	case StopNoProvider:
		return "没有可用的模型 provider"
	default:
		return "执行过程中出现错误"
	}
}

// StepKind 轨迹步骤类型。
type StepKind string

const (
	// StepModel 一次模型调用（思考）。
	StepModel StepKind = "model"
	// StepTool 一次工具调用（动作 + 观测）。
	StepTool StepKind = "tool"
	// StepNotice 提示信息（修复轮、预算告警等）。
	StepNotice StepKind = "notice"
)

// Step 轨迹中的一步。
type Step struct {
	Index int       `json:"index"`
	Kind  StepKind  `json:"kind"`
	At    time.Time `json:"at"`

	// ---- 模型步骤 ----
	Provider      string         `json:"provider,omitempty"`
	Model         string         `json:"model,omitempty"`
	PromptVersion string         `json:"prompt_version,omitempty"`
	PromptPreview string         `json:"prompt_preview,omitempty"`
	PromptBytes   int            `json:"prompt_bytes,omitempty"`
	Content       string         `json:"content,omitempty"`
	ToolCalls     []llm.ToolCall `json:"tool_calls,omitempty"`
	Usage         llm.Usage      `json:"usage,omitempty"`
	LatencyMS     int64          `json:"latency_ms,omitempty"`
	FinishReason  string         `json:"finish_reason,omitempty"`

	// ---- 工具步骤 ----
	ToolName    string                 `json:"tool_name,omitempty"`
	ToolArgs    map[string]interface{} `json:"tool_args,omitempty"`
	Observation string                 `json:"observation,omitempty"`
	Evidence    []tools.Evidence       `json:"evidence,omitempty"`
	DurationMS  int64                  `json:"duration_ms,omitempty"`

	// ---- 公共 ----
	Err       string `json:"err,omitempty"`
	Notice    string `json:"notice,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Trace 一次 Agent 运行的完整轨迹。
//
// 验收标准里"可回放"指的是：**只靠这份轨迹**就能说清
// "看了什么问题 → 调了什么工具 → 看到了什么 → 得出了什么结论"，
// 不需要再查模型厂商的日志。因此这里同时记录用量、耗时、证据与终止原因。
type Trace struct {
	mu sync.Mutex

	Version       int        `json:"version"`
	SessionID     string     `json:"session_id"`
	Goal          string     `json:"goal"`
	PromptVersion string     `json:"prompt_version"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    time.Time  `json:"finished_at"`
	Steps         []Step     `json:"steps"`
	StopReason    StopReason `json:"stop_reason"`
	StopExplain   string     `json:"stop_explain"`

	ModelCalls int                `json:"model_calls"`
	ToolCalls  int                `json:"tool_calls"`
	Usage      llm.Usage          `json:"usage"`
	Evidence   []tools.Evidence   `json:"evidence"`
	Warnings   []string           `json:"warnings,omitempty"`
	Budget     llm.BudgetSnapshot `json:"budget"`
	ToolStats  tools.SessionStats `json:"tool_stats"`
}

// newTrace 创建轨迹。
func newTrace(sessionID, goal, promptVersion string) *Trace {
	return &Trace{
		Version:       TraceVersion,
		SessionID:     sessionID,
		Goal:          goal,
		PromptVersion: promptVersion,
		StartedAt:     time.Now(),
	}
}

// addStep 追加一步并返回其下标。
func (t *Trace) addStep(step Step) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	step.Index = len(t.Steps)
	if step.At.IsZero() {
		step.At = time.Now()
	}
	t.Steps = append(t.Steps, step)
}

// addWarning 记录一条告警。
func (t *Trace) addWarning(msg string) {
	if t == nil || msg == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Warnings = append(t.Warnings, msg)
}

// addEvidence 累计证据（去重按 Source+Ref+Quote）。
func (t *Trace) addEvidence(items ...tools.Evidence) {
	if t == nil || len(items) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, item := range items {
		dup := false
		for _, existing := range t.Evidence {
			if existing.Source == item.Source && existing.Ref == item.Ref && existing.Quote == item.Quote {
				dup = true
				break
			}
		}
		if !dup {
			t.Evidence = append(t.Evidence, item)
		}
	}
}

// Steps 返回步骤副本。
func (t *Trace) StepList() []Step {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Step, len(t.Steps))
	copy(out, t.Steps)
	return out
}

// EvidenceList 返回证据副本。
func (t *Trace) EvidenceList() []tools.Evidence {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]tools.Evidence, len(t.Evidence))
	copy(out, t.Evidence)
	return out
}

// WarningsList 返回告警副本。
func (t *Trace) WarningsList() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.Warnings))
	copy(out, t.Warnings)
	return out
}

// EvidenceRefs 返回本次运行**真实观测到**的证据定位集合。
//
// 用途：校验模型给出的"证据链"是否捏造（见 diagnose.go 的 grounding 检查）。
func (t *Trace) EvidenceRefs() map[string]bool {
	out := map[string]bool{}
	for _, e := range t.EvidenceList() {
		for _, key := range evidenceKeys(e) {
			out[key] = true
		}
	}
	return out
}

// evidenceKeys 生成一个证据可被引用的多种规范化写法。
func evidenceKeys(e tools.Evidence) []string {
	keys := []string{}
	add := func(s string) {
		s = strings.TrimSpace(strings.ToLower(s))
		if s != "" {
			keys = append(keys, s)
		}
	}
	add(e.Ref)
	add(e.Source + ":" + e.Ref)
	if idx := strings.LastIndex(e.Ref, "/"); idx >= 0 && idx+1 < len(e.Ref) {
		add(e.Ref[idx+1:])
	}
	return keys
}

// JSON 序列化（落库与回放）。
func (t *Trace) JSON() ([]byte, error) {
	if t == nil {
		return []byte("{}"), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return json.Marshal(t)
}

// Rendering 生成人类可读的时间线（控制台"轨迹"页直接用这段文本）。
func (t *Trace) Rendering() string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	for _, s := range t.StepList() {
		switch s.Kind {
		case StepModel:
			b.WriteString(fmt.Sprintf("#%d [模型] %s/%s 用时 %dms tokens=%d\n", s.Index, s.Provider, s.Model, s.LatencyMS, s.Usage.TotalTokens))
			if s.Content != "" {
				b.WriteString("    思考: " + oneLine(s.Content) + "\n")
			}
			for _, tc := range s.ToolCalls {
				b.WriteString(fmt.Sprintf("    → 调用工具 %s 参数 %s\n", tc.Name, llm.Truncate(tc.Arguments, 200)))
			}
			if s.Err != "" {
				b.WriteString("    错误: " + s.Err + "\n")
			}
		case StepTool:
			status := "成功"
			if s.Err != "" {
				status = "失败: " + s.Err
			}
			b.WriteString(fmt.Sprintf("#%d [工具] %s 用时 %dms %s 证据 %d 条\n", s.Index, s.ToolName, s.DurationMS, status, len(s.Evidence)))
			if s.Observation != "" {
				b.WriteString("    观测: " + oneLine(s.Observation) + "\n")
			}
		case StepNotice:
			b.WriteString(fmt.Sprintf("#%d [提示] %s\n", s.Index, s.Notice))
		}
	}
	return b.String()
}

// oneLine 把多行文本压成一行（控制台展示用）。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	return llm.Truncate(s, 400)
}

// StepObserver 步骤回调：SSE 推送/实时展示用。
// 约定：不允许阻塞（调用方若需要落库，请自己起异步）。
type StepObserver func(step Step)
