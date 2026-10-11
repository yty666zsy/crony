package agentmemory

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/llm"
)

// MaxMemoriesPerSession 一次会话最多沉淀几条经验。
//
// 上限的意义：一次诊断可能调 8 次工具，如果每次都沉淀一条，
// 高频会话会用噪声把经验库填满（召回窗口是有限的），
// 而"这次故障是怎么被定位的"其实只需要几条关键动作。
const MaxMemoriesPerSession = 5

// MaxToolMemories 一次会话最多沉淀几条工具动作经验（其余名额留给会话结论）。
const MaxToolMemories = 4

// MaxSymptomRunes 症状文本的长度上限（与库列宽 512 对齐，留出余量）。
const MaxSymptomRunes = 200

// MaxActionRunes 动作描述长度上限（库列宽 512）。
const MaxActionRunes = 300

// MaxResultRunes 结果摘要长度上限（库列宽 1024）。
const MaxResultRunes = 600

// MaxResultDetailRunes 长结果长度上限（text 列，但不要无限大）。
const MaxResultDetailRunes = 2000

// OutcomeForAuditDecision 把一条工具审计行映射成经验结果。
//
// 映射关系必须是"事实→事实"，不能借用别的语义：
//
//	allowed + ok=true            → success（真的执行成功了）
//	allowed + ok=false           → failed （真的执行了但失败）
//	denied                       → denied （被闸门拒绝，没有执行）
//	approval_required            → blocked（转人工审批，当时**没有执行**）
//	approval_executed + ok=true  → success
//	approval_executed + ok=false → failed （人工批准后执行失败）
//
// 特别注意 approval_required / denied / blocked 都**不能**算进成功率分母：
// 它们从没执行过，把它们算成失败会让"这个动作成功率很低"变成一句假话。
func OutcomeForAuditDecision(decision string, ok bool) string {
	switch decision {
	case models.AuditDecisionAllowed, models.AuditDecisionApprovalExecuted:
		if ok {
			return models.MemoryOutcomeSuccess
		}
		return models.MemoryOutcomeFailed
	case models.AuditDecisionDenied:
		return models.MemoryOutcomeDenied
	case models.AuditDecisionApprovalRequired:
		return models.MemoryOutcomeBlocked
	default:
		return models.MemoryOutcomeFailed
	}
}

// ActionTextForAudit 生成"动作"的人可读描述：工具名 + 关键参数摘要。
//
// 参数在这里**必须脱敏**：审计行本身已脱敏，但经验库是另一张表，
// 不能假设"上游一定干净"（这条链路会被人手工补数据/迁移）。
func ActionTextForAudit(tool, argsJSON string) string {
	tool = strings.TrimSpace(tool)
	args := strings.TrimSpace(llm.Redact(argsJSON))
	if args == "" || args == "{}" || args == "null" {
		return tool
	}
	return clipRunes(tool+" "+compactJSON(args), MaxActionRunes)
}

// compactJSON 去掉 JSON 里的多余空白（经验库里的动作描述要能一眼看完）。
func compactJSON(s string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		// 不是合法 JSON：原样返回（截断由调用方负责），不要假装解析成功。
		return s
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(raw)
}

// SymptomFromSession 由会话概要构造症状文本。
//
// 为什么用 goal 而不是"结论摘要"：goal 在会话开始前就确定，**跨次重复出现时是稳定的**
// （同一个任务反复失败 → 同一个 goal），这正是去重计数能生效的前提。
// 结论摘要每次都可能不同，用它做去重键会导致"经验库只增不去重"。
func SymptomFromSession(s *models.AgentSession) string {
	if s == nil {
		return ""
	}
	goal := strings.TrimSpace(s.Goal)
	if goal == "" {
		goal = fmt.Sprintf("%s 会话（无目标文本）", s.Kind)
	}
	status := map[string]string{
		models.AgentStatusCompleted: "（会话完成）",
		models.AgentStatusFailed:    "（会话未完成）",
		models.AgentStatusCanceled:  "（会话被取消）",
	}[s.Status]
	return clipRunes(goal+" "+status, MaxSymptomRunes)
}

// DiagnosisBrief 从会话的 ResultJSON 里取出的结论要点（用于会话级经验）。
type DiagnosisBrief struct {
	Summary            string   `json:"summary"`
	Cause              string   `json:"cause"`
	Category           string   `json:"category"`
	EvidenceSufficient bool     `json:"evidence_sufficient"`
	NextData           []string `json:"next_data_to_collect"`
	Actions            []string `json:"suggested_action_titles"`
	UnverifiedRefs     []string `json:"unverified_refs"`
	UnverifiedClaims   []string `json:"unverified_node_claims"`
}

// ParseDiagnosisBrief 解析结论 JSON。
//
// 解析失败不返回错误：**沉淀经验不能因为结论格式变了就整体失败**，
// 调用方拿到 nil 时会退化成"只沉淀工具动作经验"（少一条比全丢好）。
func ParseDiagnosisBrief(resultJSON string) *DiagnosisBrief {
	s := strings.TrimSpace(resultJSON)
	if s == "" {
		return nil
	}
	var brief DiagnosisBrief
	if err := json.Unmarshal([]byte(s), &brief); err != nil {
		return nil
	}
	return &brief
}

// Extract 把一次会话的轨迹与审计事实抽成"症状→动作→结果"经验。
//
// 抽取规则（全部是**规则驱动**，不调模型）：
//
//	A. 每条工具审计事实 → 一条经验（最多 MaxToolMemories 条，按"有信息量"排序取前几条）；
//	B. 会话结论 → 一条"建议类"经验（action_kind=advice）：
//	   把结论的 cause + 建议动作标题作为"动作"，结果标记为 advised（没有动手）；
//	   若结论证据不足，则在结果文本里写明，避免下次被当成"已验证的处置"。
//
// 为什么用规则而不是让模型自己总结经验：
//   - 经验会被注入**下一次**诊断的提示词，模型自己写的总结一旦带错，
//     错误会被反复放大（自我强化），而且无法审计；
//   - 规则抽取的每条经验都能指回"哪条审计行/哪次会话"，可回溯。
//
// 这一轮的诚实边界：抽取只覆盖"诊断/巡检类会话的动作"，不判断经验对错、
// 不做跨会话因果推断（那属于自愈调度与回放评测）。
func Extract(session *models.AgentSession, audits []models.AgentToolAudit) []models.MemoryObservation {
	if session == nil {
		return nil
	}
	symptom := SymptomFromSession(session)
	if symptom == "" {
		return nil
	}
	tokens := TokenizeString(symptom)
	out := make([]models.MemoryObservation, 0, MaxMemoriesPerSession)

	// A. 工具动作经验：先把"有信息量"的审计行挑出来。
	// 排序原则：有原因/有错误 > 有输出 > 其余；同档按时间升序（先发生的动作更能解释后面）。
	picked := pickInformativeAudits(audits, MaxToolMemories)
	for _, a := range picked {
		outcome := OutcomeForAuditDecision(a.Decision, a.OK)
		detail := strings.TrimSpace(a.Err)
		result := resultSummaryForAudit(a, outcome)
		out = append(out, models.MemoryObservation{
			SessionID:     session.SessionID,
			RunID:         session.RunID,
			JobID:         session.JobID,
			AgentKind:     session.Kind,
			Symptom:       symptom,
			SymptomTokens: tokens,
			ActionKind:    models.MemoryActionTool,
			ActionText:    ActionTextForAudit(a.Tool, a.ArgsJSON),
			Tool:          a.Tool,
			TargetNode:    a.TargetNode,
			Risk:          a.Risk,
			Outcome:       outcome,
			ResultText:    clipRunes(result, MaxResultRunes),
			ResultDetail:  clipRunes(llm.Redact(detail), MaxResultDetailRunes),
		})
	}

	// B. 会话结论经验。
	if brief := ParseDiagnosisBrief(session.ResultJSON); brief != nil && len(out) < MaxMemoriesPerSession {
		action := strings.TrimSpace(brief.Cause)
		if action == "" {
			action = strings.TrimSpace(brief.Summary)
		}
		if len(brief.Actions) > 0 {
			action = strings.TrimSpace(action + "｜建议：" + strings.Join(brief.Actions, "、"))
		}
		result := "只给出结论与建议，本次没有执行任何动作"
		if summary := strings.TrimSpace(brief.Summary); summary != "" {
			result = summary + "（只建议，未执行）"
		}
		if !brief.EvidenceSufficient {
			result = "证据不足：" + result
		}
		if len(brief.UnverifiedRefs) > 0 || len(brief.UnverifiedClaims) > 0 {
			// 含未经证实的引用/断言的结论**必须**被标注，否则下次召回会把它当成可信经验。
			result += fmt.Sprintf("｜注意：该结论含 %d 条未观测引用、%d 条未验证的节点侧断言",
				len(brief.UnverifiedRefs), len(brief.UnverifiedClaims))
		}
		out = append(out, models.MemoryObservation{
			SessionID:     session.SessionID,
			RunID:         session.RunID,
			JobID:         session.JobID,
			AgentKind:     session.Kind,
			Symptom:       symptom,
			SymptomTokens: tokens,
			ActionKind:    models.MemoryActionAdvice,
			ActionText:    clipRunes(action, MaxActionRunes),
			Tool:          "",
			TargetNode:    session.NodeUUID,
			Risk:          "",
			Outcome:       models.MemoryOutcomeAdvised,
			ResultText:    clipRunes(result, MaxResultRunes),
			ResultDetail:  clipRunes(strings.Join(brief.NextData, "；"), MaxResultDetailRunes),
		})
	}
	if len(out) > MaxMemoriesPerSession {
		out = out[:MaxMemoriesPerSession]
	}
	return out
}

// pickInformativeAudits 挑选最有信息量的审计行。
func pickInformativeAudits(audits []models.AgentToolAudit, limit int) []models.AgentToolAudit {
	if len(audits) <= limit {
		return audits
	}
	scored := make([]models.AgentToolAudit, len(audits))
	copy(scored, audits)
	// 稳定排序：先按"信息量"降序，再按 created_ms 升序。
	info := func(a models.AgentToolAudit) int {
		switch {
		case strings.TrimSpace(a.Reason) != "" || strings.TrimSpace(a.Err) != "":
			return 2
		case a.OutputBytes > 0 || a.EvidenceCount > 0:
			return 1
		default:
			return 0
		}
	}
	for i := 1; i < len(scored); i++ {
		cur := scored[i]
		j := i - 1
		for j >= 0 && (info(scored[j]) < info(cur) ||
			(info(scored[j]) == info(cur) && scored[j].CreatedMS > cur.CreatedMS)) {
			scored[j+1] = scored[j]
			j--
		}
		scored[j+1] = cur
	}
	return scored[:limit]
}

// resultSummaryForAudit 生成审计事实的结果摘要。
func resultSummaryForAudit(a models.AgentToolAudit, outcome string) string {
	switch outcome {
	case models.MemoryOutcomeSuccess:
		return fmt.Sprintf("决策=%s，执行成功（输出 %d 字节，证据 %d 条）", a.Decision, a.OutputBytes, a.EvidenceCount)
	case models.MemoryOutcomeFailed:
		if strings.TrimSpace(a.Err) != "" {
			return "决策=" + a.Decision + "，执行失败：" + a.Err
		}
		return "决策=" + a.Decision + "，执行失败（无错误详情）"
	case models.MemoryOutcomeBlocked:
		return "决策=" + a.Decision + "，**没有执行**（" + orText(a.Reason, "已转人工审批") + "）"
	case models.MemoryOutcomeDenied:
		return "决策=" + a.Decision + "，被拦截：" + orText(a.Reason, "未写明原因")
	default:
		return "决策=" + a.Decision
	}
}

// orText 空值兜底。
func orText(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
