// Package agentruntime 把"配置 + 数据源 + 工具策略"装配成可执行的 Agent 运行时。
//
// 它是 admin 与 node 共用的唯一装配点，原因是两边的差异只有**数据源**：
//   - admin（控制平面）：能读 MySQL（job_run/job_run_step/job_log），做自然语言起草与诊断；
//   - node（执行平面）：在 MySQL 之外还能读**本机日志文件**，做 Agent 型定时任务。
//
// 装配逻辑只有一份，避免出现"admin 说配置好了、node 说没配置"这类最难查的不一致。
//
// 关于证据边界（必须诚实说明）：
// 阶段 2 的 Agent 只读工具覆盖"已落库的执行记录 + etcd 进程记录 + （节点侧）本机日志文件"；
// 容器沙箱、Prometheus、K8s、Vault 等数据源尚未接入，因此 Agent **不能**声称看过这些系统，
// 相关结论只能以"无法确认 + 需要采集什么"的形式输出。
package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agent"
	"github.com/tmnhs/crony/common/pkg/agentconfig"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// Options 装配参数。
type Options struct {
	// Config 配置文件的 llm 段。未配置时 New 返回 llm.ErrNotConfigured。
	Config models.LLM
	// Policy 工具策略。零值会被 Normalize 补齐（默认保守策略）。
	Policy tools.Policy
	// Sources 只读数据源。为 nil 的对应工具不会被注册。
	Sources tools.Sources
	// Observer 工具调用审计回调（可选）。
	Observer tools.Observer
	// NodeUUID 本进程所属节点（用于把会话关联到节点）；admin 侧可留空。
	NodeUUID string
}

// Runtime 装配好的 Agent 运行时。
type Runtime struct {
	rt       *agentconfig.Runtime
	policy   tools.Policy
	registry *tools.Registry
	agent    *agent.Agent
	nodeUUID string
}

// New 装配运行时。配置缺失返回 llm.ErrNotConfigured（调用方转 1007）。
func New(opts Options) (*Runtime, error) {
	rt, err := agentconfig.FromConfig(opts.Config)
	if err != nil {
		return nil, err
	}
	policy := opts.Policy.Normalize()
	registry, err := tools.NewReadonlyRegistry(policy, opts.Sources, tools.WithObserver(opts.Observer))
	if err != nil {
		return nil, fmt.Errorf("装配只读工具集失败: %w", err)
	}
	a := agent.New(rt.Gateway, registry, rt.Config)
	return &Runtime{
		rt:       rt,
		policy:   policy,
		registry: registry,
		agent:    a,
		nodeUUID: opts.NodeUUID,
	}, nil
}

// ProviderName 实际生效的 provider 名（UI 必须如实展示）。
func (r *Runtime) ProviderName() string { return r.rt.ProviderName }

// Offline 是否为离线确定性模式。
func (r *Runtime) Offline() bool { return r.rt.Offline }

// Model 默认模型名。
func (r *Runtime) Model() string { return r.rt.Config.Model }

// ToolNames 已注册的只读工具名（接口 /agent/tool/list 用）。
func (r *Runtime) ToolNames() []string { return r.registry.AllowedNames() }

// Policy 生效的工具策略（接口展示用）。
func (r *Runtime) Policy() tools.Policy { return r.policy }

// Agent 底层 Agent（测试与高级调用用）。
func (r *Runtime) Agent() *agent.Agent { return r.agent }

// DiagnosisRequest 诊断请求。
type DiagnosisRequest struct {
	RunID string
	// Force 为 true 时忽略已有诊断，重新调用模型。
	Force bool
	// Extra 追加材料（例如节点状态摘要）。
	Extra []string
}

// DiagnosisResult 诊断结果（含会话与回放信息）。
type DiagnosisResult struct {
	Diagnosis agent.Diagnosis `json:"diagnosis"`
	SessionID string          `json:"session_id"`
	// Replayed 为 true 表示直接返回了库里已有的诊断，**没有**再花一次 token。
	Replayed bool   `json:"replayed"`
	Explain  string `json:"explain"`
	// PersistError 轨迹落库失败原因（为空表示落库成功）。
	// 落库失败不影响结论返回，但必须在响应里露出，避免"以为可回放其实没存"。
	PersistError string `json:"persist_error,omitempty"`
}

// RunDiagnosis 诊断一次失败执行。
//
// 语义要点：
//   - 未加 force 且库里已有诊断 → 直接回放（不调用模型），Replayed=true；
//   - 诊断永远只是建议（Diagnosis.AdvisoryOnly 恒为 true），不做任何执行。
func (r *Runtime) RunDiagnosis(ctx context.Context, req DiagnosisRequest) (*DiagnosisResult, error) {
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		return nil, fmt.Errorf("run_id 不能为空")
	}

	if !req.Force {
		if existing, err := loadDiagnosisSession(runID); err != nil {
			return nil, err
		} else if existing != nil {
			d, derr := decodeDiagnosis(existing)
			if derr == nil {
				return &DiagnosisResult{
					Diagnosis: d,
					SessionID: existing.SessionID,
					Replayed:  true,
					Explain:   d.Explain(),
				}, nil
			}
			// 落库内容解不出来（老版本/被截断）：不静默回放，改为重新诊断。
		}
	}

	rc, err := LoadRunContext(runID)
	if err != nil {
		return nil, err
	}
	if len(req.Extra) > 0 {
		rc.Extra = append(rc.Extra, req.Extra...)
	}

	started := time.Now()
	out, derr := agent.Diagnose(ctx, r.agent, rc)
	sessionID := ""
	if out.Trace != nil {
		sessionID = out.Trace.SessionID
	}

	res := &DiagnosisResult{Diagnosis: out, SessionID: sessionID, Explain: out.Explain()}
	goalText := fmt.Sprintf("诊断任务[%s]（id=%d）第 %s 次运行失败的原因", orDash(rc.JobName), rc.JobID, orDash(rc.RunID))
	if err := r.persist(models.AgentKindDiagnose, out.Trace, out, rc.JobID, rc.RunID, goalText,
		agent.PromptDiagnoseV1, started); err != nil {
		res.PersistError = err.Error()
	}
	// 结论与错误同时可能：拿到结构化结论但 Agent 循环以 error 结束（例如修复失败），
	// 这里把两者都交给调用方，由接口决定展示方式（当前实现：结论优先，错误进 warnings）。
	if derr != nil && out.Summary == "" {
		return res, derr
	}
	return res, derr
}

// InspectRequest 巡检（Agent 型定时任务）请求。
type InspectRequest struct {
	SessionID string
	JobID     int
	JobName   string
	Goal      string
	Context   []string
}

// InspectionResult 巡检结果（含会话）。
type InspectionResult struct {
	Inspection *agent.Inspection `json:"inspection"`
	SessionID  string            `json:"session_id"`
	Explain    string            `json:"explain"`
	// PersistError 同 DiagnosisResult。
	PersistError string `json:"persist_error,omitempty"`
}

// RunInspect 执行一次巡检。
func (r *Runtime) RunInspect(ctx context.Context, req InspectRequest) (*InspectionResult, error) {
	started := time.Now()
	out, err := agent.Inspect(ctx, r.agent, agent.InspectRequest{
		SessionID: req.SessionID,
		Goal:      req.Goal,
		Context:   req.Context,
		Now:       started,
	})
	if out == nil {
		return nil, err
	}
	sessionID := req.SessionID
	if out.Trace != nil && out.Trace.SessionID != "" {
		sessionID = out.Trace.SessionID
	}
	res := &InspectionResult{Inspection: out, SessionID: sessionID, Explain: out.Explain()}
	if perr := r.persist(models.AgentKindJob, out.Trace, out, req.JobID, "", req.Goal,
		agent.PromptInspectV1, started); perr != nil {
		res.PersistError = perr.Error()
	}
	return res, err
}

// DraftRequest 自然语言 → 任务草案请求。
type DraftRequest struct {
	Text     string
	NodeUUID string
	Timezone string
	Context  []string
	// Now 注入时钟（便于测试与"固定触发回显"）。
	Now time.Time
}

// DraftResult 草案结果（含会话 ID，供确认接口使用）。
type DraftResult struct {
	Draft        agent.JobDraft `json:"draft"`
	SessionID    string         `json:"session_id"`
	Explain      string         `json:"explain"`
	PersistError string         `json:"persist_error,omitempty"`
}

// RunDraftJob 起草任务定义。
//
// **只产出草案，不落 job、不进调度器**：确认动作由接口层在人工确认后执行。
// 会话会落库（kind=nljob），这样"确认"能通过 session_id 找回草案原文，
// 而不是信任浏览器回传的草案内容。
func (r *Runtime) RunDraftJob(ctx context.Context, req DraftRequest) (*DraftResult, error) {
	started := req.Now
	if started.IsZero() {
		started = time.Now()
	}
	draft, err := agent.DraftJob(ctx, r.agent, agent.NLJobRequest{
		Text:     req.Text,
		NodeUUID: req.NodeUUID,
		Timezone: req.Timezone,
		Now:      started,
		Context:  req.Context,
	})
	res := &DraftResult{Draft: draft, Explain: draft.Explain()}
	if draft.Trace != nil {
		res.SessionID = draft.Trace.SessionID
	}
	if perr := r.persist(models.AgentKindNLJob, draft.Trace, draft, 0, "", "把自然语言描述翻译成定时任务定义",
		agent.PromptVersionNLJobV1, started); perr != nil {
		res.PersistError = perr.Error()
	}
	return res, err
}

// LoadDraft 从会话里读回草案（确认接口用：不信任浏览器回传的草案）。
func (r *Runtime) LoadDraft(sessionID string) (*models.AgentSession, agent.JobDraft, error) {
	session := &models.AgentSession{SessionID: strings.TrimSpace(sessionID)}
	if err := session.FindBySessionID(); err != nil {
		if models.IsRecordNotFound(err) {
			return nil, agent.JobDraft{}, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
		}
		return nil, agent.JobDraft{}, err
	}
	if session.Kind != models.AgentKindNLJob {
		return session, agent.JobDraft{}, fmt.Errorf("%w: 会话 %s 的类型是 %s，不是任务草案",
			ErrSessionNotFound, session.SessionID, session.Kind)
	}
	var draft agent.JobDraft
	if strings.TrimSpace(session.ResultJSON) == "" {
		return session, draft, fmt.Errorf("%w: 会话 %s 没有草案内容", ErrSessionNotFound, session.SessionID)
	}
	if err := json.Unmarshal([]byte(session.ResultJSON), &draft); err != nil {
		return session, draft, fmt.Errorf("解析会话 %s 的草案失败: %w", session.SessionID, err)
	}
	return session, draft, nil
}

// ErrSessionNotFound 会话不存在（供上层映射为业务码 1008）。
var ErrSessionNotFound = fmt.Errorf("会话不存在")

// AgentContext 组装 Agent 型任务的材料（任务定义 + 最近运行摘要）。
func (r *Runtime) AgentContext(jobID int, jobName, spec, command, extra string) []string {
	lines := []string{
		fmt.Sprintf("任务：%s（id=%d）", jobName, jobID),
		fmt.Sprintf("调度表达式：%s", orDash(spec)),
		fmt.Sprintf("任务体（自然语言目标）：%s", orDash(command)),
	}
	if r.nodeUUID != "" {
		lines = append(lines, "所在节点："+r.nodeUUID)
	}
	if extra != "" {
		lines = append(lines, extra)
	}
	if recent, err := loadRecentRunSummaries(jobID, 5, ""); err == nil && len(recent) > 0 {
		var b strings.Builder
		b.WriteString("最近运行：")
		for _, s := range recent {
			fmt.Fprintf(&b, "\n  - %s %s 用时%dms 退出码%d",
				s.ScheduledAt.Format("01-02 15:04:05"), s.Status, s.DurationMS, s.ExitCode)
			if strings.TrimSpace(s.Note) != "" {
				fmt.Fprintf(&b, " 备注=%s", s.Note)
			}
		}
		lines = append(lines, b.String())
	}
	if r.registry != nil {
		lines = append(lines, "可用只读工具："+strings.Join(r.registry.AllowedNames(), "、"))
	}
	lines = append(lines, "注意：你没有执行权，只能读与建议；每条发现都必须带证据定位。")
	return lines
}

// persist 把一次会话的轨迹与结论落库。
//
// 失败不静默：调用方把错误放进响应的 persist_error 字段，前端能显示"这次结论没能回放"。
func (r *Runtime) persist(kind string, trace *agent.Trace, result interface{}, jobID int, runID, goal, promptVersion string, started time.Time) error {
	if trace == nil {
		return nil
	}
	if err := ensureDB(); err != nil {
		return err
	}
	steps := trace.StepList()
	warnings := trace.WarningsList()

	resultJSON := ""
	if result != nil {
		if raw, err := json.Marshal(result); err == nil {
			resultJSON = string(raw)
		}
	}
	traceJSON := ""
	if raw, err := trace.JSON(); err == nil {
		traceJSON = string(raw)
	}
	warningsJSON := ""
	if raw, err := json.Marshal(warnings); err == nil {
		warningsJSON = string(raw)
	}
	contextJSON := ""
	if raw, err := json.Marshal(map[string]interface{}{
		"goal":        goal,
		"job_id":      jobID,
		"run_id":      runID,
		"prompt_ver":  promptVersion,
		"tool_stats":  trace.ToolStats,
		"budget":      trace.Budget,
		"stop_reason": string(trace.StopReason),
	}); err == nil {
		contextJSON = string(raw)
	}

	session := &models.AgentSession{
		SessionID:     trace.SessionID,
		Kind:          kind,
		JobID:         jobID,
		RunID:         runID,
		NodeUUID:      r.nodeUUID,
		Goal:          goal,
		Status:        sessionStatus(trace),
		StopReason:    string(trace.StopReason),
		Provider:      stepProvider(steps),
		Model:         stepModel(steps),
		PromptVersion: promptVersion,
		ModelCalls:    trace.ModelCalls,
		ToolCalls:     trace.ToolCalls,
		TotalTokens:   trace.Usage.TotalTokens,
		DurationMs:    trace.FinishedAt.Sub(trace.StartedAt).Milliseconds(),
		WarnCount:     len(warnings),
		EvidenceCount: len(trace.EvidenceList()),
		WarningsJSON:  warningsJSON,
		ContextJSON:   contextJSON,
		TraceJSON:     traceJSON,
		ResultJSON:    resultJSON,
		Created:       started.Unix(),
		Updated:       time.Now().Unix(),
	}
	if session.SessionID == "" {
		session.SessionID = models.NewAgentSessionID(kind, started)
	}
	if _, err := session.Insert(); err != nil {
		return fmt.Errorf("会话落库失败: %w", err)
	}
	for _, st := range steps {
		row := stepToModel(session.SessionID, st)
		if _, err := row.Insert(); err != nil {
			return fmt.Errorf("轨迹步骤[%d]落库失败: %w", st.Index, err)
		}
	}
	return nil
}

// sessionStatus 由轨迹推导会话状态。
func sessionStatus(trace *agent.Trace) string {
	if trace == nil {
		return models.AgentStatusFailed
	}
	switch trace.StopReason {
	case agent.StopCompleted:
		return models.AgentStatusCompleted
	case agent.StopCanceled:
		return models.AgentStatusCanceled
	default:
		return models.AgentStatusFailed
	}
}

// stepToModel 轨迹步骤 → 落库行。
func stepToModel(sessionID string, st agent.Step) *models.AgentStep {
	row := &models.AgentStep{
		SessionID:     sessionID,
		Seq:           st.Index,
		Kind:          string(st.Kind),
		Provider:      st.Provider,
		Model:         st.Model,
		PromptPreview: llm.Truncate(llm.Redact(st.PromptPreview), 4000),
		PromptBytes:   st.PromptBytes,
		Content:       llm.Truncate(llm.Redact(st.Content), 16000),
		ToolName:      st.ToolName,
		Observation:   llm.Truncate(llm.Redact(st.Observation), 16000),
		DurationMs:    st.DurationMS,
		LatencyMs:     st.LatencyMS,
		TotalTokens:   st.Usage.TotalTokens,
		FinishReason:  st.FinishReason,
		ErrorMsg:      llm.Truncate(llm.Redact(st.Err), 4000),
		Notice:        llm.Truncate(st.Notice, 4000),
		Truncated:     st.Truncated,
		Created:       time.Now().Unix(),
	}
	if len(st.ToolArgs) > 0 {
		if raw, err := json.Marshal(st.ToolArgs); err == nil {
			row.ToolArgsJSON = llm.Truncate(string(raw), 4000)
		}
	}
	if len(st.ToolCalls) > 0 {
		if raw, err := json.Marshal(st.ToolCalls); err == nil {
			row.ToolCallsJSON = llm.Truncate(string(raw), 4000)
		}
	}
	if len(st.Evidence) > 0 {
		if raw, err := json.Marshal(st.Evidence); err == nil {
			row.EvidenceJSON = llm.Truncate(string(raw), 8000)
		}
	}
	return row
}

// stepProvider / stepModel 从轨迹里取实际使用的 provider/model。
//
// 取"第一条模型步骤"而不是配置值：配置可能在中途被改，轨迹必须反映当时实际发生了什么。
func stepProvider(steps []agent.Step) string {
	for _, st := range steps {
		if st.Kind == agent.StepModel && st.Provider != "" {
			return st.Provider
		}
	}
	return ""
}

func stepModel(steps []agent.Step) string {
	for _, st := range steps {
		if st.Kind == agent.StepModel && st.Model != "" {
			return st.Model
		}
	}
	return ""
}

// loadDiagnosisSession 读取已有诊断会话（不存在返回 nil, nil）。
func loadDiagnosisSession(runID string) (*models.AgentSession, error) {
	return models.FindLatestAgentSession(runID, models.AgentKindDiagnose)
}

// decodeDiagnosis 从落库 JSON 还原诊断。
func decodeDiagnosis(s *models.AgentSession) (agent.Diagnosis, error) {
	d := agent.Diagnosis{}
	if s == nil || strings.TrimSpace(s.ResultJSON) == "" {
		return d, fmt.Errorf("会话 %s 没有结构化结论", sessionIDOf(s))
	}
	if err := json.Unmarshal([]byte(s.ResultJSON), &d); err != nil {
		return d, err
	}
	d.AdvisoryOnly = true
	return d, nil
}

func sessionIDOf(s *models.AgentSession) string {
	if s == nil {
		return ""
	}
	return s.SessionID
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
