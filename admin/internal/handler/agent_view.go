package handler

import (
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agent"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/runpolicy"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 本文件是"契约视图层"：把内部结构翻译成 PHASE2-接口契约.md 里的字段。
//
// 为什么单独一层而不是直接 json 序列化内部结构：
//  1. 内部结构带 Trace（可能几百 KB）与实现细节，直接吐给前端既慢又泄露信息；
//  2. 契约里有 *_text 字段（stop_reason_text / risk_text / category_text），
//     翻译逻辑必须**只有一份**，且能被单元测试覆盖；
//  3. 前端只渲染不推断——凡是 UI 需要的解释性文本，都由后端给出。

// ---------- 会话（契约 §1） ----------

type agentSessionView struct {
	SessionID      string `json:"session_id"`
	Kind           string `json:"kind"`
	JobID          int    `json:"job_id"`
	RunID          string `json:"run_id"`
	NodeUUID       string `json:"node_uuid"`
	Goal           string `json:"goal"`
	Status         string `json:"status"`
	StatusText     string `json:"status_text"`
	StopReason     string `json:"stop_reason"`
	StopReasonText string `json:"stop_reason_text"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	PromptVersion  string `json:"prompt_version"`
	ModelCalls     int    `json:"model_calls"`
	ToolCalls      int    `json:"tool_calls"`
	TotalTokens    int    `json:"total_tokens"`
	DurationMS     int64  `json:"duration_ms"`
	WarnCount      int    `json:"warn_count"`
	EvidenceCount  int    `json:"evidence_count"`
	Created        string `json:"created"`
	CreatedMS      int64  `json:"created_ms"`
}

func sessionView(s *models.AgentSession) agentSessionView {
	if s == nil {
		return agentSessionView{}
	}
	return agentSessionView{
		SessionID:      s.SessionID,
		Kind:           s.Kind,
		JobID:          s.JobID,
		RunID:          s.RunID,
		NodeUUID:       s.NodeUUID,
		Goal:           s.Goal,
		Status:         s.Status,
		StatusText:     sessionStatusText(s.Status),
		StopReason:     s.StopReason,
		StopReasonText: stopReasonText(s.StopReason),
		Provider:       s.Provider,
		Model:          s.Model,
		PromptVersion:  s.PromptVersion,
		ModelCalls:     s.ModelCalls,
		ToolCalls:      s.ToolCalls,
		TotalTokens:    s.TotalTokens,
		DurationMS:     s.DurationMs,
		WarnCount:      s.WarnCount,
		EvidenceCount:  s.EvidenceCount,
		Created:        rfc3339OrZero(s.Created),
		CreatedMS:      milliseconds(s.Created),
	}
}

// ---------- 会话详情（契约 §2） ----------

type agentSessionDetailView struct {
	agentSessionView
	System    string             `json:"system"`
	Context   []string           `json:"context"`
	Budget    agentBudgetView    `json:"budget"`
	Warnings  []string           `json:"warnings"`
	Evidence  []evidenceView     `json:"evidence"`
	ToolStats agentToolStatsView `json:"tool_stats"`
	Steps     []agentStepView    `json:"steps"`
	Result    interface{}        `json:"result"`
}

type agentBudgetView struct {
	Calls         int   `json:"calls"`
	Tokens        int   `json:"tokens"`
	DurationMS    int64 `json:"duration_ms"`
	MaxCalls      int   `json:"max_calls"`
	MaxTokens     int   `json:"max_tokens"`
	MaxDurationMS int64 `json:"max_duration_ms"`
}

type evidenceView struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
	Quote  string `json:"quote"`
}

type agentToolStatsView struct {
	Calls     int `json:"calls"`
	Denied    int `json:"denied"`
	Truncated int `json:"truncated"`
	Remaining int `json:"remaining"`
	// PerTool 不加 omitempty：控制台的回放面板要按工具名渲染分布，
	// "没有工具调用"应当是 `per_tool: {}`，而不是**字段消失**——
	// 字段消失会让前端无法区分"这次没调工具"和"服务端版本不支持"，
	// 也会让契约类验收用例时红时绿（取决于本轮模型用没用工具）。
	PerTool map[string]int `json:"per_tool"`
}

type agentStepView struct {
	Seq           int                    `json:"seq"`
	Kind          string                 `json:"kind"`
	KindText      string                 `json:"kind_text"`
	Provider      string                 `json:"provider,omitempty"`
	Model         string                 `json:"model,omitempty"`
	PromptPreview string                 `json:"prompt_preview,omitempty"`
	PromptBytes   int                    `json:"prompt_bytes,omitempty"`
	Content       string                 `json:"content,omitempty"`
	ToolCalls     []agentToolCallView    `json:"tool_calls,omitempty"`
	ToolName      string                 `json:"tool_name,omitempty"`
	ToolArgs      map[string]interface{} `json:"tool_args,omitempty"`
	Observation   string                 `json:"observation,omitempty"`
	Evidence      []evidenceView         `json:"evidence,omitempty"`
	Notice        string                 `json:"notice,omitempty"`
	DurationMS    int64                  `json:"duration_ms,omitempty"`
	LatencyMS     int64                  `json:"latency_ms,omitempty"`
	TotalTokens   int                    `json:"total_tokens,omitempty"`
	FinishReason  string                 `json:"finish_reason,omitempty"`
	Error         string                 `json:"error,omitempty"`
	Truncated     bool                   `json:"truncated"`
	Created       string                 `json:"created"`
}

type agentToolCallView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// stepView 把一个持久化步骤翻译成契约视图。
func stepView(s *models.AgentStep, maxOutput int) agentStepView {
	if s == nil {
		return agentStepView{}
	}
	out := agentStepView{
		Seq:           s.Seq,
		Kind:          s.Kind,
		KindText:      stepKindText(s.Kind),
		Provider:      s.Provider,
		Model:         s.Model,
		PromptPreview: s.PromptPreview,
		PromptBytes:   s.PromptBytes,
		Content:       s.Content,
		ToolName:      s.ToolName,
		Observation:   truncateForView(s.Observation, maxOutput),
		Notice:        s.Notice,
		DurationMS:    s.DurationMs,
		LatencyMS:     s.LatencyMs,
		TotalTokens:   s.TotalTokens,
		FinishReason:  s.FinishReason,
		Error:         s.ErrorMsg,
		Truncated:     s.Truncated,
		Created:       rfc3339OrZero(s.Created),
	}
	if len(s.ToolCallsJSON) > 0 {
		var calls []llm.ToolCall
		if err := json.Unmarshal([]byte(s.ToolCallsJSON), &calls); err == nil {
			for _, c := range calls {
				out.ToolCalls = append(out.ToolCalls, agentToolCallView{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
			}
		}
	}
	if len(s.ToolArgsJSON) > 0 {
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(s.ToolArgsJSON), &args); err == nil {
			out.ToolArgs = args
		}
	}
	out.Evidence = evidenceViews(decodeEvidence(s.EvidenceJSON))
	return out
}

// ---------- 诊断（契约 §3） ----------

type agentDiagnosisView struct {
	Summary            string                  `json:"summary"`
	Category           string                  `json:"category"`
	CategoryText       string                  `json:"category_text"`
	Cause              string                  `json:"cause"`
	Confidence         int                     `json:"confidence"`
	ConfidenceNote     string                  `json:"confidence_note"`
	Evidence           []diagnosisEvidenceView `json:"evidence"`
	Assumptions        []string                `json:"assumptions"`
	RuledOut           []ruledOutView          `json:"ruled_out"`
	SuggestedActions   []suggestedActionView   `json:"suggested_actions"`
	EvidenceSufficient bool                    `json:"evidence_sufficient"`
	NextDataToCollect  []string                `json:"next_data_to_collect"`
	UnverifiedRefs     []string                `json:"unverified_refs"`
	// UnverifiedNodeClaims 结论句里"本应观测却靠推测"的节点侧断言类型。
	// 与 unverified_refs 并列但不相同：后者是引用了假 ref，前者是 ref 全真但结论越界
	// （例如断言"端口未监听"，而本次运行没有任何节点侧观测）。前端应同样高亮展示。
	UnverifiedNodeClaims []string `json:"unverified_node_claims"`
	Warnings             []string `json:"warnings"`
	AdvisoryOnly         bool     `json:"advisory_only"`
	Provider             string   `json:"provider"`
	Model                string   `json:"model"`
	PromptVersion        string   `json:"prompt_version"`
	StopReason           string   `json:"stop_reason"`
	StopReasonText       string   `json:"stop_reason_text"`
	DurationMS           int64    `json:"duration_ms"`
	ToolCalls            int      `json:"tool_calls"`
	TotalTokens          int      `json:"total_tokens"`
	SessionID            string   `json:"session_id"`
	RunID                string   `json:"run_id"`
	Created              string   `json:"created"`
	Explain              string   `json:"explain"`
}

type diagnosisEvidenceView struct {
	Ref   string `json:"ref"`
	Quote string `json:"quote"`
	Why   string `json:"why"`
}

type ruledOutView struct {
	Hypothesis string `json:"hypothesis"`
	Reason     string `json:"reason"`
}

type suggestedActionView struct {
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Risk     string `json:"risk"`
	RiskText string `json:"risk_text"`
	Rollback string `json:"rollback"`
}

// diagnosisView 把内部诊断结构翻译成契约视图。
//
// ConfidenceNote 是**固定文案**：confidence 是模型自评，不是统计准确率。
// 没有评测集的情况下任何"准确率"表述都是虚假宣传，这里必须显式标注。
func diagnosisView(d agent.Diagnosis, session *models.AgentSession) agentDiagnosisView {
	out := agentDiagnosisView{
		Summary:      d.Summary,
		Category:     d.Category,
		CategoryText: categoryText(d.Category),
		Cause:        d.Cause,
		// 内部置信度是 0-1 的比例，契约按 0-100 展示（模型自评，不代表准确率）。
		Confidence:           int(math.Round(d.Confidence * 100)),
		ConfidenceNote:       "模型自评置信度（未经验证，不代表准确率）",
		Assumptions:          orEmpty(d.Assumptions),
		EvidenceSufficient:   d.EvidenceSufficient,
		NextDataToCollect:    orEmpty(d.NextDataToCollect),
		UnverifiedRefs:       orEmpty(d.UnverifiedRefs),
		UnverifiedNodeClaims: orEmpty(d.UnverifiedNodeClaims),
		Warnings:             orEmpty(d.Warnings),
		AdvisoryOnly:         true, // 恒定：诊断不执行任何动作（契约 §7.2）
		Provider:             d.Provider,
		Model:                d.Model,
		PromptVersion:        d.PromptVersion,
		StopReason:           string(d.StopReason),
		StopReasonText:       stopReasonText(string(d.StopReason)),
		DurationMS:           d.DurationMS,
		ToolCalls:            d.ToolCalls,
		TotalTokens:          d.TotalTokens,
		RunID:                d.RunID,
		Explain:              d.Explain(),
	}
	for _, e := range d.Evidence {
		out.Evidence = append(out.Evidence, diagnosisEvidenceView{Ref: e.Ref, Quote: e.Quote, Why: e.Why})
	}
	for _, r := range d.RuledOut {
		out.RuledOut = append(out.RuledOut, ruledOutView{Hypothesis: r.Hypothesis, Reason: r.Reason})
	}
	for _, a := range d.SuggestedActions {
		out.SuggestedActions = append(out.SuggestedActions, suggestedActionView{
			Title: a.Title, Detail: a.Detail, Risk: a.Risk, RiskText: riskText(a.Risk), Rollback: a.Rollback,
		})
	}
	if session != nil {
		out.SessionID = session.SessionID
		out.Created = rfc3339OrZero(session.Created)
		if len(out.Warnings) == 0 && len(session.WarningsJSON) > 0 {
			var ws []string
			if err := json.Unmarshal([]byte(session.WarningsJSON), &ws); err == nil {
				out.Warnings = orEmpty(ws)
			}
		}
	}
	return out
}

// ---------- 草案（契约 §5.1） ----------

type agentDraftView struct {
	SessionID             string           `json:"session_id"`
	Name                  string           `json:"name"`
	Description           string           `json:"description"`
	JobType               string           `json:"job_type"`
	Spec                  string           `json:"spec"`
	Command               []string         `json:"command"`
	RawArgv               bool             `json:"raw_argv"`
	HTTP                  *agent.HTTPDraft `json:"http"`
	TimeoutSeconds        int64            `json:"timeout_seconds"`
	RetryTimes            int              `json:"retry_times"`
	RetryInterval         int              `json:"retry_interval"`
	ConcurrencyPolicy     string           `json:"concurrency_policy"`
	ConcurrencyPolicyText string           `json:"concurrency_policy_text"`
	ConcurrencyValue      int              `json:"concurrency_value"`
	Rationale             string           `json:"rationale"`
	Warnings              []string         `json:"warnings"`
	NextTriggers          []string         `json:"next_triggers"`
	NextTriggerText       []string         `json:"next_trigger_text"`
	TriggerEcho           string           `json:"trigger_echo"`
	RequiresConfirmation  bool             `json:"requires_confirmation"`
	Confirmed             bool             `json:"confirmed"`
	Timezone              string           `json:"timezone"`
	Provider              string           `json:"provider"`
	Model                 string           `json:"model"`
	PromptVersion         string           `json:"prompt_version"`
	Repairs               int              `json:"repairs"`
	StopReason            string           `json:"stop_reason"`
	StopReasonText        string           `json:"stop_reason_text"`
	DurationMS            int64            `json:"duration_ms"`
	TotalTokens           int              `json:"total_tokens"`
}

func draftView(d agent.JobDraft, sessionID string) agentDraftView {
	out := agentDraftView{
		SessionID:             sessionID,
		Name:                  d.Name,
		Description:           d.Description,
		JobType:               d.JobType,
		Spec:                  d.Spec,
		Command:               d.Command,
		RawArgv:               d.RawArgv,
		HTTP:                  d.HTTP,
		TimeoutSeconds:        d.TimeoutSeconds,
		RetryTimes:            d.RetryTimes,
		RetryInterval:         d.RetryInterval,
		ConcurrencyPolicy:     d.ConcurrencyPolicy,
		ConcurrencyPolicyText: concurrencyPolicyText(d.ConcurrencyPolicy),
		ConcurrencyValue:      d.ConcurrencyValue,
		Rationale:             d.Rationale,
		Warnings:              orEmpty(d.Warnings),
		NextTriggerText:       orEmpty(d.NextTriggerText),
		TriggerEcho:           d.TriggerEcho,
		RequiresConfirmation:  true,
		Confirmed:             d.Confirmed,
		Timezone:              d.Timezone,
		Provider:              d.Provider,
		Model:                 d.Model,
		PromptVersion:         d.PromptVersion,
		Repairs:               d.Repairs,
		StopReason:            string(d.StopReason),
		StopReasonText:        stopReasonText(string(d.StopReason)),
		DurationMS:            d.DurationMS,
		TotalTokens:           d.TotalTokens,
	}
	for _, t := range d.NextTriggers {
		out.NextTriggers = append(out.NextTriggers, t.Format(time.RFC3339))
	}
	return out
}

// ---------- 工具清单（契约 §4） ----------

type agentToolView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Readonly    bool   `json:"readonly"`
	// Risk 风险级别（阶段 4）：read/write/destructive。
	// 与 readonly 不是冗余：`readonly` 只说"有没有副作用"，
	// `risk` 还说"副作用可不可逆"——重启主机与重跑任务在这两个字段上才分得开。
	Risk      string      `json:"risk"`
	TimeoutMS int64       `json:"timeout_ms"`
	Schema    interface{} `json:"schema"`
}

type agentToolPolicyView struct {
	MaxCallsPerSession int      `json:"max_calls_per_session"`
	MaxCallsPerTool    int      `json:"max_calls_per_tool"`
	MaxOutputBytes     int      `json:"max_output_bytes"`
	SQLMaxRows         int      `json:"sql_max_rows"`
	AllowHosts         []string `json:"allow_hosts"`
	ShellReadPaths     []string `json:"shell_read_paths"`
	// ---- 阶段 4：风险与自治（控制台据此展示"这台部署到底放开了什么"）----
	AllowWrite                 bool     `json:"allow_write"`
	AllowDestructive           bool     `json:"allow_destructive"`
	Autonomy                   string   `json:"autonomy"`
	AutoApprove                []string `json:"auto_approve"`
	ApprovalTTLSeconds         int      `json:"approval_ttl_seconds"`
	MaxCallsPerMinute          int      `json:"max_calls_per_minute"`
	MaxConcurrentCalls         int      `json:"max_concurrent_calls"`
	MaxWriteActionsPerSession  int      `json:"max_write_actions_per_session"`
	MaxAffectedHostsPerSession int      `json:"max_affected_hosts_per_session"`
	DefaultTimeoutMS           int64    `json:"default_timeout_ms"`
	Deny                       []string `json:"deny"`
}

// toolViews 从**注册表**构造工具清单。
//
// 为什么不再从 []llm.ToolSpec 构造：风险级别与超时都在 Tool 实现上，
// 旧写法只能硬编码 `Readonly: true` 与 `policy.DefaultTimeout`，
// 结果是"接口说都是只读、超时都是 15s"，而实际注册的工具可能并非如此——
// 能力边界的对外声明与实际生效值必须是同一个来源。
func toolViews(reg *tools.Registry, policy tools.Policy) []agentToolView {
	policy = policy.Normalize()
	specs := reg.Specs()
	out := make([]agentToolView, 0, len(specs))
	for _, s := range specs {
		risk := tools.RiskRead
		timeout := policy.DefaultTimeout
		if tool, ok := reg.Get(s.Name); ok {
			risk = tool.Risk()
			if t := tool.Timeout(); t > 0 {
				timeout = t
			}
		}
		out = append(out, agentToolView{
			Name:        s.Name,
			Description: s.Description,
			Readonly:    risk == tools.RiskRead,
			Risk:        risk.String(),
			TimeoutMS:   timeout.Milliseconds(),
			Schema:      s.Parameters,
		})
	}
	return out
}

func toolPolicyView(policy tools.Policy) agentToolPolicyView {
	p := policy.Normalize()
	return agentToolPolicyView{
		MaxCallsPerSession:         p.MaxCallsPerSession,
		MaxCallsPerTool:            p.MaxCallsPerTool,
		MaxOutputBytes:             p.MaxOutputBytes,
		SQLMaxRows:                 p.SQLMaxRows,
		AllowHosts:                 orEmptyStrings(p.AllowHosts),
		ShellReadPaths:             orEmptyStrings(p.ShellReadPaths),
		AllowWrite:                 p.AllowWrite,
		AllowDestructive:           p.AllowDestructive,
		Autonomy:                   p.Autonomy.String(),
		AutoApprove:                orEmptyStrings(p.AutoApprove),
		ApprovalTTLSeconds:         int(p.ApprovalTTL.Seconds()),
		MaxCallsPerMinute:          p.MaxCallsPerMinute,
		MaxConcurrentCalls:         p.MaxConcurrentCalls,
		MaxWriteActionsPerSession:  p.MaxWriteActionsPerSession,
		MaxAffectedHostsPerSession: p.MaxAffectedHostsPerSession,
		DefaultTimeoutMS:           p.DefaultTimeout.Milliseconds(),
		Deny:                       orEmptyStrings(p.Deny),
	}
}

// ---------- 文本翻译（唯一一份） ----------

func sessionStatusText(status string) string {
	switch status {
	case models.AgentStatusCompleted:
		return "正常完成"
	case models.AgentStatusFailed:
		return "失败"
	case models.AgentStatusCanceled:
		return "已取消"
	default:
		return orUnknown(status)
	}
}

func stopReasonText(reason string) string {
	switch reason {
	case string(agent.StopCompleted):
		return "正常完成"
	case string(agent.StopMaxSteps):
		return "达到步数上限（未得出最终结论）"
	case string(agent.StopToolBudget):
		return "工具调用达到预算上限"
	case string(agent.StopLLMBudget):
		return "模型调用达到预算上限"
	case string(agent.StopCanceled):
		return "已取消"
	case string(agent.StopSchemaInvalid):
		return "输出未通过结构校验"
	case string(agent.StopNoProvider):
		return "未配置模型"
	case string(agent.StopError):
		return "运行出错"
	default:
		return orUnknown(reason)
	}
}

func stepKindText(kind string) string {
	switch kind {
	case string(agent.StepModel):
		return "模型调用"
	case string(agent.StepTool):
		return "工具调用"
	case string(agent.StepNotice):
		return "系统提示"
	default:
		return orUnknown(kind)
	}
}

// categoryText 诊断分类 → 中文。分类取值来自提示词约定，未知分类如实显示原值。
func categoryText(category string) string {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "permission":
		return "权限/凭据"
	case "network":
		return "网络/连通性"
	case "resource":
		return "资源不足（CPU/内存/磁盘）"
	case "config":
		return "配置错误"
	case "dependency":
		return "依赖不可用"
	case "code":
		return "命令/脚本自身错误"
	case "scheduler":
		return "调度/触发问题"
	case "unknown", "":
		return "未能分类"
	default:
		return category
	}
}

func riskText(risk string) string {
	switch strings.ToLower(strings.TrimSpace(risk)) {
	case "low":
		return "低风险"
	case "medium":
		return "中等风险"
	case "high":
		return "高风险"
	default:
		return orUnknown(risk)
	}
}

func concurrencyPolicyText(policy string) string {
	p, ok := runpolicy.ParseConcurrencyPolicy(policy)
	if !ok {
		return orUnknown(policy)
	}
	switch p {
	case runpolicy.ConcurrencySkip:
		return "跳过（上一次未跑完则跳过本次）"
	case runpolicy.ConcurrencyBufferOne:
		return "缓冲一次（等上一次结束后补跑一次）"
	case runpolicy.ConcurrencyBufferAll:
		return "全部缓冲（按上限排队补跑）"
	case runpolicy.ConcurrencyCancelOther:
		return "取消上一次（软取消，不杀进程）"
	case runpolicy.ConcurrencyTerminateOther:
		return "终止上一次（强杀进程，慎用）"
	default:
		return p.String()
	}
}

// ---------- 小工具 ----------

// truncateForView 按**字节**截断（limit 是字节数：契约里的 8KiB 上限是传输体积）。
// llm.Truncate 按 rune 计数，中文会超出字节预算，因此这里自己做并回退到 rune 边界，
// 避免把多字节字符切成乱码。
func truncateForView(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…（观测已截断，完整内容见会话轨迹）"
}

func evidenceViews(list []tools.Evidence) []evidenceView {
	out := make([]evidenceView, 0, len(list))
	for _, e := range list {
		out = append(out, evidenceView{Source: e.Source, Ref: e.Ref, Quote: e.Quote})
	}
	return out
}

func decodeEvidence(raw string) []tools.Evidence {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []tools.Evidence
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// rfc3339OrZero 统一用 UTC 输出，保证同一时刻在不同时区的部署上序列化结果一致
// （前端自己按浏览器时区渲染）。
func rfc3339OrZero(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

func milliseconds(unix int64) int64 {
	if unix <= 0 {
		return 0
	}
	return unix * 1000
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未知"
	}
	return s
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func orEmptyStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// orEmptyIntMap 把 nil map 归一成空 map：JSON 里 `null` 与 `{}` 对前端不是一回事。
func orEmptyIntMap(in map[string]int) map[string]int {
	if in == nil {
		return map[string]int{}
	}
	return in
}
