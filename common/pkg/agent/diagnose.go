package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
)

// RunContext 一次失败（或可疑）运行的客观事实。
//
// 设计原则：**只放系统里真实存在的数据**，不做推测。凡是没提供的字段就是"未知"，
// 而不是"空字符串"——提示词里会明确告知模型这一点，避免模型把零值当成事实。
type RunContext struct {
	JobID    int    `json:"job_id"`
	JobName  string `json:"job_name"`
	JobType  string `json:"job_type"`
	Spec     string `json:"spec"`
	NodeUUID string `json:"node_uuid"`
	RunID    string `json:"run_id"`

	Status      string    `json:"status"`
	ExitCode    int       `json:"exit_code"`
	ScheduledAt time.Time `json:"scheduled_at"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	DurationMS  int64     `json:"duration_ms"`
	Attempts    int       `json:"attempts"`
	RunOn       string    `json:"run_on"`

	// Command 渲染后的命令或 URL（**已脱敏**：调用方负责去掉密钥）。
	Command string `json:"command"`
	// StdoutTail / StderrTail 输出尾部。
	StdoutTail string `json:"stdout_tail"`
	StderrTail string `json:"stderr_tail"`
	// StepSummaries 各步骤摘要（阶段 1 的多步骤执行）。
	StepSummaries []string `json:"step_summaries"`
	// RecentRuns 最近若干次运行（用于判断"一直失败"还是"最近开始失败"）。
	RecentRuns []RunSummary `json:"recent_runs"`
	// Extra 其他可用材料（调用方自行组装，如节点状态、告警记录）。
	Extra []string `json:"extra"`
}

// RunSummary 历史运行摘要。
type RunSummary struct {
	RunID       string    `json:"run_id"`
	Status      string    `json:"status"`
	ScheduledAt time.Time `json:"scheduled_at"`
	DurationMS  int64     `json:"duration_ms"`
	ExitCode    int       `json:"exit_code"`
	Note        string    `json:"note"`
}

// EvidenceRef 一条证据引用。
type EvidenceRef struct {
	Ref   string `json:"ref"`
	Quote string `json:"quote"`
	Why   string `json:"why"`
}

// RuledOut 被排除的假设。
type RuledOut struct {
	Hypothesis string `json:"hypothesis"`
	Reason     string `json:"reason"`
}

// SuggestedAction 处置建议（**只建议，不自动执行**）。
type SuggestedAction struct {
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Risk     string `json:"risk"`
	Rollback string `json:"rollback"`
}

// Diagnosis 诊断报告。
type Diagnosis struct {
	JobID int    `json:"job_id"`
	RunID string `json:"run_id"`

	Summary    string  `json:"summary"`
	Category   string  `json:"category"`
	Cause      string  `json:"cause"`
	Confidence float64 `json:"confidence"`

	Evidence           []EvidenceRef     `json:"evidence"`
	Assumptions        []string          `json:"assumptions"`
	RuledOut           []RuledOut        `json:"ruled_out"`
	SuggestedActions   []SuggestedAction `json:"suggested_actions"`
	EvidenceSufficient bool              `json:"evidence_sufficient"`
	NextDataToCollect  []string          `json:"next_data_to_collect"`

	// AdvisoryOnly 恒为 true：诊断结论永远不直接触发变更。
	AdvisoryOnly bool `json:"advisory_only"`

	// UnverifiedRefs 模型引用了但本次运行**并未观测到**的证据定位。
	// 非空意味着这条结论里存在"看起来有据、其实没据"的成分，必须显式告警。
	UnverifiedRefs []string `json:"unverified_refs,omitempty"`
	// Warnings 运行与校验过程中的告警。
	Warnings []string `json:"warnings,omitempty"`

	// 运行元信息（落库与归因用）。
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	PromptVersion string     `json:"prompt_version"`
	StopReason    StopReason `json:"stop_reason"`
	DurationMS    int64      `json:"duration_ms"`
	ToolCalls     int        `json:"tool_calls"`
	TotalTokens   int        `json:"total_tokens"`
	Trace         *Trace     `json:"-"`
}

// Explain 生成人类可读报告（控制台与通知用）。
func (d Diagnosis) Explain() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("【诊断】任务 %d（run %s）\n", d.JobID, d.RunID))
	b.WriteString("结论: " + d.Summary + "\n")
	b.WriteString(fmt.Sprintf("分类: %s  置信度: %.2f  证据充分: %v\n", d.Category, d.Confidence, d.EvidenceSufficient))
	if d.Cause != "" {
		b.WriteString("原因: " + d.Cause + "\n")
	}
	if len(d.Evidence) > 0 {
		b.WriteString("证据链:\n")
		for i, e := range d.Evidence {
			b.WriteString(fmt.Sprintf("  %d. [%s] %s\n     作用: %s\n", i+1, e.Ref, e.Quote, e.Why))
		}
	}
	if len(d.Assumptions) > 0 {
		b.WriteString("未验证假设:\n")
		for _, a := range d.Assumptions {
			b.WriteString("  - " + a + "\n")
		}
	}
	if len(d.RuledOut) > 0 {
		b.WriteString("已排除:\n")
		for _, r := range d.RuledOut {
			b.WriteString("  - " + r.Hypothesis + "（" + r.Reason + "）\n")
		}
	}
	if len(d.SuggestedActions) > 0 {
		b.WriteString("建议动作（需人工执行）:\n")
		for i, a := range d.SuggestedActions {
			b.WriteString(fmt.Sprintf("  %d. [%s] %s —— %s\n", i+1, a.Risk, a.Title, a.Detail))
			if a.Rollback != "" {
				b.WriteString("     回滚: " + a.Rollback + "\n")
			}
		}
	}
	if len(d.UnverifiedRefs) > 0 {
		b.WriteString("⚠ 未观测到的引用（可能是模型编造）: " + strings.Join(d.UnverifiedRefs, ", ") + "\n")
	}
	if len(d.NextDataToCollect) > 0 {
		b.WriteString("还需要的数据:\n")
		for _, n := range d.NextDataToCollect {
			b.WriteString("  - " + n + "\n")
		}
	}
	b.WriteString(fmt.Sprintf("终止原因: %s（%s）\n", d.StopReason, d.StopReason.Explain()))
	return b.String()
}

// DiagnosisSchema 诊断输出的 JSON Schema。
//
// 关键点：所有会让模型"含糊过关"的字段都不给默认值，全部必填。
// required 里包含 evidence：**逼模型把话说在证据上**；没有证据就要显式声明
// evidence_sufficient=false 并给出待收集数据。
func DiagnosisSchema() *jsonschema.Schema {
	evidence := jsonschema.Array(jsonschema.Object(map[string]*jsonschema.Schema{
		"ref":   jsonschema.String("观测里出现的证据定位").WithLength(1, 256),
		"quote": jsonschema.String("观测原文片段").WithLength(1, 400),
		"why":   jsonschema.String("这条证据如何支持结论").WithLength(1, 400),
	}, "ref", "quote", "why"), "本次观测到的证据")
	ruledOut := jsonschema.Array(jsonschema.Object(map[string]*jsonschema.Schema{
		"hypothesis": jsonschema.String("假设").WithLength(1, 200),
		"reason":     jsonschema.String("排除依据").WithLength(1, 400),
	}, "hypothesis", "reason"), "被排除的可能")
	actions := jsonschema.Array(jsonschema.Object(map[string]*jsonschema.Schema{
		"title":    jsonschema.String("动作标题").WithLength(1, 120),
		"detail":   jsonschema.String("具体做法").WithLength(1, 800),
		"risk":     jsonschema.String("风险等级").WithEnum("low", "medium", "high"),
		"rollback": jsonschema.String("回滚方式").WithLength(0, 400),
	}, "title", "detail", "risk", "rollback"), "处置建议（人工执行）")

	return jsonschema.Object(map[string]*jsonschema.Schema{
		"summary":              jsonschema.String("一句话结论").WithLength(1, 600),
		"category":             jsonschema.String("失败分类").WithEnum("config", "dependency", "environment", "network", "resource", "permission", "code", "schedule", "unknown"),
		"cause":                jsonschema.String("最可能原因").WithLength(1, 1200),
		"confidence":           jsonschema.Integer("置信度百分比（0-100）").WithRange(0, 100),
		"evidence":             evidence.WithLength(0, 20),
		"assumptions":          jsonschema.Array(jsonschema.String("假设").WithLength(1, 300), "未验证假设").WithLength(0, 10),
		"ruled_out":            ruledOut.WithLength(0, 10),
		"suggested_actions":    actions.WithLength(0, 8),
		"evidence_sufficient":  jsonschema.Bool("已有材料是否足以定位原因"),
		"next_data_to_collect": jsonschema.Array(jsonschema.String("数据项").WithLength(1, 300), "还需要收集的数据").WithLength(0, 10),
	}, "summary", "category", "cause", "confidence", "evidence", "assumptions", "ruled_out",
		"suggested_actions", "evidence_sufficient", "next_data_to_collect")
}

// Diagnose 执行一次故障诊断。
//
// 返回语义：
//   - err == nil：拿到结构化诊断（报告里可能带 Warnings/UnverifiedRefs，需一并展示）；
//   - err != nil：未拿到合规结论（预算/取消/步数/Schema），此时 **diag 仍可能带部分信息**
//     与完整 Trace，调用方应把 Trace 落库并在界面上说明"为什么没有结论"。
func Diagnose(ctx context.Context, a *Agent, rc RunContext) (Diagnosis, error) {
	start := time.Now()
	diag := Diagnosis{
		JobID:         rc.JobID,
		RunID:         rc.RunID,
		AdvisoryOnly:  true,
		PromptVersion: PromptVersionDiagnoseV1,
		Category:      "unknown",
	}
	if a == nil {
		return diag, fmt.Errorf("%w: agent 未初始化", ErrNoProvider)
	}

	in := Input{
		SessionID:     sessionIDFor("diagnose", rc.JobID, rc.RunID),
		Goal:          fmt.Sprintf("诊断任务 %d（%s）本次失败的原因", rc.JobID, rc.JobName),
		System:        PromptDiagnoseV1,
		Context:       []string{renderRunContext(rc)},
		PromptVersion: PromptVersionDiagnoseV1,
	}
	out, err := a.Run(ctx, in, DiagnosisSchema())
	diag.Trace = out.Trace
	diag.StopReason = out.StopReason
	diag.Warnings = append(diag.Warnings, out.Warnings...)
	diag.DurationMS = time.Since(start).Milliseconds()
	if out.Trace != nil {
		diag.ToolCalls = out.Trace.ToolCalls
		diag.TotalTokens = out.Trace.Usage.TotalTokens
		diag.Provider = providerOf(out.Trace)
		diag.Model = modelOf(out.Trace)
	}
	if err != nil {
		diag.Summary = fmt.Sprintf("未能给出诊断结论：%s", out.StopReason.Explain())
		if out.StopReason == StopSchemaInvalid && out.Raw != "" {
			diag.Warnings = append(diag.Warnings, "模型原文（未通过结构校验）: "+llm.Truncate(out.Raw, 500))
		}
		return diag, err
	}

	diag.Summary = llm.StringOr(out.Structured, "summary", "")
	diag.Category = llm.StringOr(out.Structured, "category", "unknown")
	diag.Cause = llm.StringOr(out.Structured, "cause", "")
	diag.Confidence = clamp01(llm.FloatOr(out.Structured, "confidence", 0) / 100.0)
	diag.EvidenceSufficient = boolOr(out.Structured["evidence_sufficient"], false)
	diag.Evidence = parseEvidence(out.Structured["evidence"])
	diag.Assumptions = stringList(out.Structured, "assumptions")
	diag.RuledOut = parseRuledOut(out.Structured["ruled_out"])
	diag.SuggestedActions = parseActions(out.Structured["suggested_actions"])
	diag.NextDataToCollect = stringList(out.Structured, "next_data_to_collect")

	// ---- 证据 grounding 校验：模型引用的 ref 必须是本次真实观测到的 ----
	if out.Trace != nil {
		observed := out.Trace.EvidenceRefs()
		for _, e := range diag.Evidence {
			if !refObserved(observed, e.Ref) {
				diag.UnverifiedRefs = append(diag.UnverifiedRefs, e.Ref)
			}
		}
		if len(diag.UnverifiedRefs) > 0 {
			diag.EvidenceSufficient = false
			diag.Warnings = append(diag.Warnings, fmt.Sprintf(
				"模型引用了 %d 条本次并未观测到的证据（%s），已标记为不可信，结论需人工复核",
				len(diag.UnverifiedRefs), strings.Join(dedupe(diag.UnverifiedRefs), ", ")))
		}
		if len(diag.Evidence) == 0 && diag.EvidenceSufficient {
			diag.EvidenceSufficient = false
			diag.Warnings = append(diag.Warnings, "模型声称证据充分但未给出任何证据条目，已降级为证据不足")
		}
	}
	// 证据不足时必须有"还需要什么"的清单，否则这条结论没法推进
	if !diag.EvidenceSufficient && len(diag.NextDataToCollect) == 0 {
		diag.Warnings = append(diag.Warnings, "结论为证据不足但未给出待收集数据清单")
	}
	return diag, nil
}

// renderRunContext 把客观事实渲染成模型可读材料。
func renderRunContext(rc RunContext) string {
	var b strings.Builder
	b.WriteString("### 任务定义\n")
	b.WriteString(fmt.Sprintf("- job_id: %d\n- 名称: %s\n- 类型: %s\n- 调度表达式: %s\n- 运行节点: %s\n- 运行账号/目标: %s\n",
		rc.JobID, orUnknown(rc.JobName), orUnknown(rc.JobType), orUnknown(rc.Spec), orUnknown(rc.NodeUUID), orUnknown(rc.RunOn)))
	if rc.Command != "" {
		b.WriteString("- 本次实际执行的命令/请求（已脱敏）: " + llm.Truncate(rc.Command, 1000) + "\n")
	}

	b.WriteString("\n### 本次运行\n")
	b.WriteString(fmt.Sprintf("- run_id: %s\n- 状态: %s\n- 退出码: %d\n- 尝试次数: %d\n",
		orUnknown(rc.RunID), orUnknown(rc.Status), rc.ExitCode, rc.Attempts))
	if !rc.ScheduledAt.IsZero() {
		b.WriteString("- 计划时间: " + rc.ScheduledAt.Format(time.RFC3339) + "\n")
	}
	if !rc.StartedAt.IsZero() {
		b.WriteString("- 开始时间: " + rc.StartedAt.Format(time.RFC3339) + "\n")
	}
	if !rc.FinishedAt.IsZero() {
		b.WriteString("- 结束时间: " + rc.FinishedAt.Format(time.RFC3339) + "\n")
	}
	if rc.DurationMS > 0 {
		b.WriteString(fmt.Sprintf("- 耗时: %d ms\n", rc.DurationMS))
	}

	if len(rc.StepSummaries) > 0 {
		b.WriteString("\n### 步骤\n")
		for i, s := range rc.StepSummaries {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, s))
		}
	}
	if rc.StdoutTail != "" {
		b.WriteString("\n### stdout 尾部\n```\n" + llm.Truncate(rc.StdoutTail, 4000) + "\n```\n")
	}
	if rc.StderrTail != "" {
		b.WriteString("\n### stderr 尾部\n```\n" + llm.Truncate(rc.StderrTail, 4000) + "\n```\n")
	}
	if len(rc.RecentRuns) > 0 {
		b.WriteString("\n### 最近运行历史（由近及远）\n")
		for _, r := range rc.RecentRuns {
			b.WriteString(fmt.Sprintf("- %s | %s | %s | 耗时 %dms | 退出码 %d %s\n",
				r.RunID, r.ScheduledAt.Format("2006-01-02 15:04:05"), r.Status, r.DurationMS, r.ExitCode, r.Note))
		}
		b.WriteString("注意：历史记录用于判断\"一直失败\"还是\"最近开始失败\"，两种情况的根因通常不同。\n")
	}
	for i, extra := range rc.Extra {
		if strings.TrimSpace(extra) == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("\n### 补充材料 %d\n%s\n", i+1, llm.Truncate(extra, 2000)))
	}
	b.WriteString("\n（未列出的信息即系统未提供，不要臆测其内容。）")
	return b.String()
}

// parseEvidence 解析证据数组。
func parseEvidence(raw interface{}) []EvidenceRef {
	out := []EvidenceRef{}
	for _, item := range asList(raw) {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, EvidenceRef{
			Ref:   llm.StringOr(m, "ref", ""),
			Quote: llm.Truncate(llm.StringOr(m, "quote", ""), 400),
			Why:   llm.StringOr(m, "why", ""),
		})
	}
	return out
}

// parseRuledOut 解析被排除假设。
func parseRuledOut(raw interface{}) []RuledOut {
	out := []RuledOut{}
	for _, item := range asList(raw) {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, RuledOut{
			Hypothesis: llm.StringOr(m, "hypothesis", ""),
			Reason:     llm.StringOr(m, "reason", ""),
		})
	}
	return out
}

// parseActions 解析处置建议。
func parseActions(raw interface{}) []SuggestedAction {
	out := []SuggestedAction{}
	for _, item := range asList(raw) {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, SuggestedAction{
			Title:    llm.StringOr(m, "title", ""),
			Detail:   llm.StringOr(m, "detail", ""),
			Risk:     llm.StringOr(m, "risk", "medium"),
			Rollback: llm.StringOr(m, "rollback", ""),
		})
	}
	return out
}

// asList 把 JSON 数组转成切片（非数组返回空）。
func asList(raw interface{}) []interface{} {
	switch v := raw.(type) {
	case []interface{}:
		return v
	default:
		return nil
	}
}

// stringList 读取字符串数组字段（过滤空串与非字符串项）。
//
// 为什么不直接用 llm.SliceOf：那个助手返回的是**对象数组**（map 切片），
// 语义不同，混用会在"数组里混了对象"时静默丢数据。
func stringList(obj map[string]interface{}, key string) []string {
	out := []string{}
	for _, item := range asList(obj[key]) {
		s, ok := item.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// argvList 读取命令参数数组：**任何非法元素都报错**，不做静默过滤。
//
// 与 stringList 的关键差别：空串在这里是错误而不是"忽略"。argv 里的空参数会改变
// 命令语义（`cmd ""` 与 `cmd` 不是一回事），静默丢参数等于悄悄改掉用户要执行的命令。
func argvList(obj map[string]interface{}, key string) ([]string, error) {
	raw := asList(obj[key])
	out := make([]string, 0, len(raw))
	for i, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%w: 第 %d 个参数不是字符串", ErrDraftInvalid, i+1)
		}
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%w: 第 %d 个参数为空", ErrDraftInvalid, i+1)
		}
		out = append(out, s)
	}
	return out, nil
}

// refObserved 判断模型引用的证据定位是否来自本次真实观测。
//
// 判定规则（故意"偏宽容但不可伪造"）：
//   - 规范化后完全相等；或
//   - 已观测到的 ref 是被引用 ref 的子串（允许模型把 "node.log:12" 引用成
//     "node.log:12-15" 这种更具体的范围写法）。
//
// 纯粹编造的定位（syslog:99）不会命中任何一条。
func refObserved(observed map[string]bool, ref string) bool {
	norm := strings.ToLower(strings.TrimSpace(ref))
	if norm == "" {
		return false
	}
	if observed[norm] {
		return true
	}
	for key := range observed {
		if key != "" && strings.Contains(norm, key) {
			return true
		}
	}
	return false
}

// dedupe 去重并保持顺序。
func dedupe(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

// sessionIDFor 生成会话标识（工具配额与审计按会话分组）。
func sessionIDFor(prefix string, jobID int, runID string) string {
	if runID != "" {
		return fmt.Sprintf("%s:%s", prefix, runID)
	}
	return fmt.Sprintf("%s:job-%d", prefix, jobID)
}

// providerOf / modelOf 从轨迹里取模型信息（取最后一次模型调用）。
func providerOf(t *Trace) string {
	steps := t.StepList()
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Kind == StepModel && steps[i].Provider != "" {
			return steps[i].Provider
		}
	}
	return ""
}

func modelOf(t *Trace) string {
	steps := t.StepList()
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Kind == StepModel && steps[i].Model != "" {
			return steps[i].Model
		}
	}
	return ""
}

// clamp01 把数值夹到 [0,1]。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// boolOr 读取布尔值（缺失按默认）。
func boolOr(raw interface{}, def bool) bool {
	if v, ok := raw.(bool); ok {
		return v
	}
	return def
}

// orUnknown 空值显示为 "未知"（而不是空字符串，避免模型把缺失当事实）。
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未知"
	}
	return s
}
