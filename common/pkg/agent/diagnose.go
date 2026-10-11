package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
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

	// MemoryHints 历史相似经验的展示行（阶段 4-A，**由调用方先召回再填入**）。
	//
	// 为什么要"行"而不是整段文本：**声明历史经验不可当证据的那句话由本包写死**
	// （见 memoryHintHeader），调用方只能提供条目内容。这样"注入历史经验"
	// 就永远带着这条约束，不依赖每个调用方记得自己加免责声明。
	//
	// 本包的职责边界：这里**只做提示词注入与审计记录**，不做召回——
	// 召回要读数据库，而 agent 包是无数据库依赖的纯逻辑层。
	MemoryHints []string `json:"memory_hints"`
	// MemoryHintIDs 被注入的历史经验条目号（用于结论归因：这次结论受过哪些历史提示影响）。
	MemoryHintIDs []string `json:"memory_hint_ids"`
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

	// MemoryUsed 本次结论在提示词里注入过的历史经验条目号（阶段 4-A）。
	//
	// 为什么必须记下来：如果事后发现某次结论是错的，第一个要回答的问题是
	// "它是不是被历史经验带偏了"。没有这个字段就只能在提示词全文里翻。
	MemoryUsed []string `json:"memory_used,omitempty"`

	// UnverifiedRefs 模型引用了但本次运行**并未观测到**的证据定位。
	// 非空意味着这条结论里存在"看起来有据、其实没据"的成分，必须显式告警。
	UnverifiedRefs []string `json:"unverified_refs,omitempty"`
	// UnverifiedNodeClaims 结论句里出现的"节点侧事实断言"，而本次运行没有节点侧观测证据。
	//
	// 与 UnverifiedRefs 的区别：UnverifiedRefs 抓"引用假证据"，
	// 本字段抓"结论本身越界"——ref 都是真的，但结论断言了只有目标机器才知道的事实
	// （端口未监听 / 服务未运行 / 磁盘已满……），这类结论**本应观测却靠推测**。
	// 见 claimguard.go 的说明与真实联调实证。
	UnverifiedNodeClaims []string `json:"unverified_node_claims,omitempty"`
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
	if len(d.UnverifiedNodeClaims) > 0 {
		b.WriteString("⚠ 结论含未观测的节点侧断言（本应观测却靠推测）: " + strings.Join(d.UnverifiedNodeClaims, "、") + "\n")
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
	return DiagnoseWithSession(ctx, a, rc, DiagnoseSessionID(rc.JobID, rc.RunID))
}

// DiagnoseSessionID 诊断会话的默认（确定性）id：同一次运行只有一个"第一次诊断"。
func DiagnoseSessionID(jobID int, runID string) string {
	return sessionIDFor("diagnose", jobID, runID)
}

// RetrySessionID 生成"重新诊断"的会话 id。
//
// 为什么不能复用确定性 id：agent_session 上 session_id 有唯一索引，
// 第二次诊断同一个 run 直接 Insert 会撞 1062（真实联调中 force=true 就是这么失败的，
// 结果接口返回了新结论但库里仍是旧结论）。
// 也不能原地覆盖：一次轨迹是审计对象，不同提示词版本/不同模型的结论必须都留证。
// 因此用"已存在的最近一行 id"做后缀：既保证唯一，也能让人看出这是第几次重诊断。
func RetrySessionID(base string, previousRowID int64) string {
	if previousRowID <= 0 {
		return base
	}
	return fmt.Sprintf("%s#r%d", base, previousRowID)
}

// DiagnoseWithSession 与 Diagnose 相同，但由调用方指定会话 id
// （force 重诊断时用 RetrySessionID，避免覆盖历史轨迹）。
func DiagnoseWithSession(ctx context.Context, a *Agent, rc RunContext, sessionID string) (Diagnosis, error) {
	start := time.Now()
	diag := Diagnosis{
		JobID:         rc.JobID,
		RunID:         rc.RunID,
		AdvisoryOnly:  true,
		PromptVersion: PromptVersionDiagnoseV5,
		Category:      "unknown",
	}
	if a == nil {
		return diag, fmt.Errorf("%w: agent 未初始化", ErrNoProvider)
	}
	if strings.TrimSpace(sessionID) == "" {
		sessionID = DiagnoseSessionID(rc.JobID, rc.RunID)
	}

	material := []string{renderRunContext(rc)}
	// 历史经验块：材料里**增一块**，而不是混进客观事实里——
	// 这是"线索"与"事实"在输入侧的物理隔离。声明文字由本包写死（见 memoryHintHeader）。
	memWarning := ""
	if block := renderMemoryHints(rc.MemoryHints); block != "" {
		material = append(material, block)
		diag.MemoryUsed = append([]string{}, rc.MemoryHintIDs...)
		memWarning = fmt.Sprintf(
			"本次提示词注入了 %d 条历史经验（%s）：它们只是排查线索，不计入证据；若模型引用它们会被判为未观测",
			len(rc.MemoryHints), strings.Join(dedupe(rc.MemoryHintIDs), ", "))
		diag.Warnings = append(diag.Warnings, memWarning)
	}

	in := Input{
		SessionID:     sessionID,
		Goal:          fmt.Sprintf("诊断任务 %d（%s）本次失败的原因", rc.JobID, rc.JobName),
		System:        PromptDiagnoseV2,
		Context:       material,
		Given:         givenEvidence(rc),
		PromptVersion: PromptVersionDiagnoseV5,
	}
	out, err := a.Run(ctx, in, DiagnosisSchema())
	diag.Trace = out.Trace
	diag.StopReason = out.StopReason
	diag.Warnings = append(diag.Warnings, out.Warnings...)
	// 同一句免责声明也要进**会话级**告警（trace.Warnings → agent_session.warnings_json）：
	// 只写在结论里的话，人从会话列表一眼看不出"这次结论参考过历史经验"，
	// 而这恰恰是复盘时要先知道的事（结论是模型自己编的，还是被旧经验带过去的）。
	if memWarning != "" {
		diag.Trace.addWarning(memWarning)
	}
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
	// ---- 结论句守卫：节点侧事实必须观测过才能断言（见 claimguard.go） ----
	if out.Trace != nil {
		applyNodeClaimGuard(&diag, out.Trace.EvidenceList())
	}
	// ---- 历史经验守卫（阶段 4-A）：经验条目**永远不能**充当本次证据 ----
	//
	// 常规 grounding 校验（上面的 refObserved）本来就会把 "mem-xxx" 判为未观测，
	// 但这里额外做一次**指名道姓**的判定，原因有两个：
	//  1. 告警文字要能直接说明"它引用的是历史经验"，而不是笼统的"未观测到的 ref"——
	//     前者能让人一眼看出是提示词注入的副作用，后者会让人以为是模型编造；
	//  2. 这条规则是本阶段的**核心不变量**，需要一个独立的、可单独测试的守门点，
	//     不能只依赖另一条通用校验顺带覆盖。
	applyMemoryEvidenceGuard(&diag, rc)
	// 证据不足时必须有"还需要什么"的清单，否则这条结论没法推进
	if !diag.EvidenceSufficient && len(diag.NextDataToCollect) == 0 {
		diag.Warnings = append(diag.Warnings, "结论为证据不足但未给出待收集数据清单")
	}
	return diag, nil
}

// refRun / refStep / refJob 是材料里事实块的引用标识。
// 模型必须逐字引用它们，grounding 校验才能确认"结论确实挂在观测到的证据上"。
func refJob(jobID int) string { return fmt.Sprintf("job:%d", jobID) }

func refRun(runID string) string { return "job_run:" + runID }

func refStep(runID string, stepIndex int) string {
	return fmt.Sprintf("job_run_step:%s#%d", runID, stepIndex)
}

// givenEvidence 把材料里的客观事实登记成可引用证据（见 Input.Given 的说明）。
// Quote 只放事实摘要，不放整段输出——真正的原文在材料正文里，模型引用时看得到。
func givenEvidence(rc RunContext) []tools.Evidence {
	out := make([]tools.Evidence, 0, 3+len(rc.StepSummaries)+len(rc.RecentRuns))
	add := func(source, ref, quote string) {
		quote = strings.TrimSpace(quote)
		if strings.TrimSpace(ref) == "" || quote == "" {
			return
		}
		out = append(out, tools.Evidence{Source: source, Ref: ref, Quote: llm.Truncate(llm.Redact(quote), 300)})
	}
	add("job", refJob(rc.JobID), fmt.Sprintf("job_id=%d 名称=%s 类型=%s 调度=%s 节点=%s 目标=%s",
		rc.JobID, orUnknown(rc.JobName), orUnknown(rc.JobType), orUnknown(rc.Spec), orUnknown(rc.NodeUUID), orUnknown(rc.RunOn)))
	if rc.RunID != "" {
		add("job_run", refRun(rc.RunID), fmt.Sprintf("run_id=%s 状态=%s 退出码=%d 尝试=%d stdout=%s stderr=%s",
			rc.RunID, orUnknown(rc.Status), rc.ExitCode, rc.Attempts,
			llm.Truncate(rc.StdoutTail, 120), llm.Truncate(rc.StderrTail, 120)))
	}
	for i, s := range rc.StepSummaries {
		add("job_run_step", refStep(rc.RunID, i), s)
	}
	for _, r := range rc.RecentRuns {
		add("job_run", refRun(r.RunID), fmt.Sprintf("%s 状态=%s 退出码=%d %s", r.RunID, r.Status, r.ExitCode, r.Note))
	}
	return out
}

// memoryHintHeader 注入历史经验块时**由本包写死**的声明。
//
// 这段文字是"历史提示 ≠ 本次证据"这条纪律在输入侧的唯一落点，所以不能由调用方提供：
// 一旦允许调用方自定义标题，就会出现"某条调用路径把历史经验当事实材料塞进去"的漏洞，
// 而这种漏洞在测试里很难被发现（模型照样能给出看起来合理的结论）。
const memoryHintHeader = `### 历史相似经验（线索，不是证据）
以下条目由系统从**过去**的会话自动沉淀：可能与当前情况不同、可能已经过时，也可能当时就是错的。
它们只能提示"先查什么"；**引用它们不算本次证据**——本次结论必须挂在本次工具观测的 ref 上。
如果历史经验与本次观测冲突，以本次观测为准。`

// renderMemoryHints 把历史经验行渲染成一个材料块；无内容时返回空字符串。
//
// 返回空字符串而不是"带标题的空块"：材料里出现一个空标题会让模型以为
// "有历史但没有内容"，进而写出"历史数据缺失"这类无意义的结论。
func renderMemoryHints(lines []string) string {
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, strings.TrimSpace(l))
		}
	}
	if len(kept) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(memoryHintHeader)
	b.WriteString("\n")
	for _, l := range kept {
		b.WriteString("- " + llm.Truncate(l, 800) + "\n")
	}
	return b.String()
}

// looksLikeMemoryRef 判断一个证据 ref 是不是"历史经验条目号"。
//
// 判定放在**前缀**上（mem- 是 models.NewMemoryID 的固定前缀）：
// 用"包含 memory 字样"之类模糊规则会把 tool 名（memory_search 之类）误判。
func looksLikeMemoryRef(ref string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToLower(ref)), "mem-")
}

// applyMemoryEvidenceGuard 把"拿历史经验当证据"的引用单独标出来并降级结论。
//
// 与通用 grounding 的关系：通用校验把未观测的 ref 一律记为 UnverifiedRefs（本守卫也依赖它），
// 这里额外给出**指名道姓的原因**，并保证即使 ref 恰好被写进了注入过的经验号
// （即"ref 确实存在"）也照样降级——因为"这条经验存在"与"本次观测到了"是两件事。
func applyMemoryEvidenceGuard(diag *Diagnosis, rc RunContext) {
	if diag == nil {
		return
	}
	used := map[string]bool{}
	for _, id := range rc.MemoryHintIDs {
		used[strings.TrimSpace(id)] = true
	}
	var bad []string
	for _, e := range diag.Evidence {
		if looksLikeMemoryRef(e.Ref) || used[strings.TrimSpace(e.Ref)] {
			bad = append(bad, e.Ref)
		}
	}
	if len(bad) == 0 {
		return
	}
	diag.EvidenceSufficient = false
	diag.Warnings = append(diag.Warnings, fmt.Sprintf(
		"模型把 %d 条历史经验当成本次证据（%s）：历史经验不是观测，已降级为需人工复核",
		len(bad), strings.Join(dedupe(bad), ", ")))
}

// renderRunContext 把客观事实渲染成模型可读材料。
//
// 每个事实块都显式标出 ref（[ref: ...]），这样模型可以逐字引用，
// grounding 校验才能区分"引用真实观测"与"编造证据"。
func renderRunContext(rc RunContext) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("### 任务定义 [ref: %s]\n", refJob(rc.JobID)))
	b.WriteString(fmt.Sprintf("- job_id: %d\n- 名称: %s\n- 类型: %s\n- 调度表达式: %s\n- 运行节点: %s\n- 运行账号/目标: %s\n",
		rc.JobID, orUnknown(rc.JobName), orUnknown(rc.JobType), orUnknown(rc.Spec), orUnknown(rc.NodeUUID), orUnknown(rc.RunOn)))
	if rc.Command != "" {
		b.WriteString("- 本次实际执行的命令/请求（已脱敏）: " + llm.Truncate(rc.Command, 1000) + "\n")
	}

	b.WriteString(fmt.Sprintf("\n### 本次运行 [ref: %s]\n", refRun(rc.RunID)))
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
			b.WriteString(fmt.Sprintf("%d. [ref: %s] %s\n", i+1, refStep(rc.RunID, i), s))
		}
	}
	if rc.StdoutTail != "" {
		b.WriteString(fmt.Sprintf("\n### stdout 尾部 [ref: %s]\n```\n%s\n```\n", refRun(rc.RunID), llm.Truncate(rc.StdoutTail, 4000)))
	}
	if rc.StderrTail != "" {
		b.WriteString(fmt.Sprintf("\n### stderr 尾部 [ref: %s]\n```\n%s\n```\n", refRun(rc.RunID), llm.Truncate(rc.StderrTail, 4000)))
	}
	if len(rc.RecentRuns) > 0 {
		b.WriteString("\n### 最近运行历史（由近及远）\n")
		for _, r := range rc.RecentRuns {
			b.WriteString(fmt.Sprintf("- [ref: %s] %s | %s | %s | 耗时 %dms | 退出码 %d %s\n",
				refRun(r.RunID), r.RunID, r.ScheduledAt.Format("2006-01-02 15:04:05"), r.Status, r.DurationMS, r.ExitCode, r.Note))
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
