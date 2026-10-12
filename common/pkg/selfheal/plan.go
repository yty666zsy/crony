package selfheal

import (
	"encoding/json"
	"strings"

	"github.com/tmnhs/crony/common/models"
)

// Candidate 候选动作（含"为什么候选"的理由）。
type Candidate struct {
	Action HealActionRank `json:"action"`
	Reason string         `json:"reason"`
	// Base 规则给出的基础优先级（越小越优先）。
	Base int `json:"base"`
	// Historical 该动作在本症状下的历史成功率（-1 表示没有历史）。
	Historical float64 `json:"historical"`
	// Samples 历史样本数（= 成功 + 失败 + 被拒），样本太少时不参与改序。
	Samples int `json:"samples"`
	// Chosen 最终是否被选中。
	Chosen bool `json:"chosen"`
}

// HealActionRank 复用 models.HealAction（别名，避免调用方两处 import）。
type HealActionRank = models.HealAction

// HistoryStats 经验库给出的历史统计（按动作聚合）。
//
// 为什么用"成功率"而不是"让模型自由建议"：4-A 的经验库已经沉淀了
// "这种症状下这么干管不管用"的事实（来源是真实审计流水，不是模型自述）。
// 决策层用它**只做一件事**：在规则给出的并列候选中改排序。
// 排序权在历史，决定权仍在规则——这样"模型/历史胡说"最多让顺序变差，不会让系统乱动。
type HistoryStats struct {
	// Success / Fail 该动作在该症状下的成功与失败次数。
	Success int `json:"success"`
	Fail    int `json:"fail"`
	// Blocked / Denied 从未执行（被策略挡住）与被闸门拒绝的次数。
	Blocked int `json:"blocked"`
	Denied  int `json:"denied"`
}

// Samples 历史样本数。
func (s HistoryStats) Samples() int { return s.Success + s.Fail + s.Blocked + s.Denied }

// Rate 成功率：**分母只含真正执行过的动作**（success+fail）。
//
// 与 4-A 统计口径完全一致：没执行过的动作不进分母。
// 为什么这里也要坚持：把"被策略挡住"算成失败，会让系统学会"绕开策略"的错误结论。
func (s HistoryStats) Rate() (float64, bool) {
	denom := s.Success + s.Fail
	if denom <= 0 {
		return 0, false
	}
	return float64(s.Success) / float64(denom), true
}

// PlanInput 决策输入。
type PlanInput struct {
	Classification Classification
	Run            *models.JobRun
	Job            *models.Job
	Policy         Policy
	// History 按动作聚合的历史统计（来自经验库召回；可为空）。
	History map[models.HealAction]HistoryStats
	// MemoryIDs 本次召回命中的经验条目号（落库，说明"参考了什么"）。
	MemoryIDs []string
	// MemoryHint 召回的简短说明（落库给人看）。
	MemoryHint string
}

// Plan 决策结果。
type Plan struct {
	Action     models.HealAction `json:"action"`
	Reason     string            `json:"reason"`
	Confidence string            `json:"confidence"`
	Candidates []Candidate       `json:"candidates"`
	ActionArgs ActionJSON        `json:"action_args"`
	MemoryIDs  []string          `json:"memory_ids,omitempty"`
	MemoryHint string            `json:"memory_hint,omitempty"`
	// WriteRequired 该动作是否需要写能力（闸门据此判定）。
	WriteRequired bool `json:"write_required"`
}

// PlanFor 在**白名单**内为这次失败挑一个动作。
//
// 规则表（每种失败类型的候选集是固定的，不是模型生成的）：
//
//	node_unreachable      → escalate            （原节点重跑无效；换节点属阶段 4-D）
//	timeout               → retry_extend_timeout, retry_same_node
//	delivery_failed       → retry_same_node
//	command_failed        → retry_same_node
//	orchestration_invalid → escalate            （重跑不可能改变"定义非法"）
//	unknown               → escalate
//
// 历史（经验库）只能在这几个候选之间改序，不能新增候选。
func PlanFor(in PlanInput) Plan {
	p := in.Policy.Normalize()
	class := in.Classification.Class
	cands := rawCandidates(class, in)
	if len(cands) == 0 {
		// 兜底：任何未列出候选的类型一律转人工（fail-closed）。
		cands = []Candidate{{Action: models.HealActionEscalate, Reason: "没有配置安全动作的失败类型", Base: 100}}
	}

	// 用历史成功率给"同一失败类型下的候选"改序。
	// 门槛说明：样本 < 3 时不改序——两条样本的成功率是噪声，
	// 让噪声决定动作顺序，比按固定优先级更危险。
	for i := range cands {
		if st, ok := in.History[cands[i].Action]; ok {
			if rate, known := st.Rate(); known {
				cands[i].Historical = rate
			} else {
				cands[i].Historical = -1
			}
			cands[i].Samples = st.Samples()
		} else {
			cands[i].Historical = -1
		}
	}
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if better(cands[j], cands[i]) {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}
	chosen := cands[0]
	plan := Plan{
		Action:     chosen.Action,
		Reason:     chosen.Reason,
		Confidence: confidence(in, chosen, class),
		Candidates: cands,
		MemoryIDs:  in.MemoryIDs,
		MemoryHint: in.MemoryHint,
	}
	plan.Candidates[0].Chosen = true
	plan.WriteRequired = plan.Action.IsWrite()

	// 动作参数（超时自愈需要具体的目标值；算不出来就退化为重跑）。
	if plan.Action == models.HealActionRetryExtendTimeout {
		cur := int64(0)
		if in.Job != nil {
			cur = in.Job.Timeout
		}
		if next, ok := p.NextTimeout(cur); ok {
			plan.ActionArgs.FromTimeout = cur
			plan.ActionArgs.ToTimeout = next
		} else {
			// 没有可调的超时：这个动作不适用。**降级为规则允许的次选**，而不是硬改配置。
			plan.Action = models.HealActionRetrySameNode
			plan.WriteRequired = plan.Action.IsWrite()
			plan.Reason = "原超时上限缺失或无法放大，退化为原节点重跑"
			plan.Candidates = append(plan.Candidates, Candidate{
				Action: models.HealActionRetrySameNode, Reason: "超时上限缺失，退化动作", Chosen: true,
			})
		}
	}
	if in.Job != nil {
		plan.ActionArgs.JobID = in.Job.ID
		plan.ActionArgs.NodeUUID = strings.TrimSpace(in.Job.RunOn)
	}
	if plan.Action == models.HealActionRetrySameNode || plan.Action == models.HealActionRetryExtendTimeout {
		plan.ActionArgs.MaxAttempts = 1
	}
	if plan.MemoryHint != "" {
		plan.Reason = plan.Reason + "；参考历史：" + plan.MemoryHint
	}
	return plan
}

// rawCandidates 按失败类型给出候选（顺序即基础优先级）。
func rawCandidates(class FailureClass, in PlanInput) []Candidate {
	switch class {
	case ClassTimeout:
		out := []Candidate{{
			Action: models.HealActionRetryExtendTimeout, Base: 0,
			Reason: "超时类失败：先按策略倍率调大超时上限（有绝对上限）再重跑",
		}}
		out = append(out, Candidate{
			Action: models.HealActionRetrySameNode, Base: 10,
			Reason: "仅重跑（不改配置）：适合「当时慢」而非「一直不够」的情况",
		})
		return out
	case ClassDeliveryFailed:
		return []Candidate{{
			Action: models.HealActionRetrySameNode, Base: 0,
			Reason: "投递失败：任务没被节点接到，重新投递一次即可",
		}}
	case ClassCommandFailed:
		return []Candidate{{
			Action: models.HealActionRetrySameNode, Base: 0,
			Reason: "命令失败：原地重跑一次（失败原因可能是瞬时的，重跑成本低且可逆）",
		}}
	case ClassNodeUnreachable:
		return []Candidate{{
			Action: models.HealActionEscalate, Base: 0,
			Reason: "节点不可达：原节点重跑无效（自动换节点属于多目标调度，尚未实现），转人工",
		}}
	case ClassOrchestrationInvalid:
		return []Candidate{{
			Action: models.HealActionEscalate, Base: 0,
			Reason: "编排定义非法：重跑不可能改变结论，必须人工修定义，转人工",
		}}
	default:
		return []Candidate{{
			Action: models.HealActionEscalate, Base: 0,
			Reason: "失败未归类：不做自动动作，转人工",
		}}
	}
}

// better 判断 a 是否应排在 b 前面：先看历史（样本足够时），再看基础优先级。
func better(a, b Candidate) bool {
	const minSamples = 3
	const minDelta = 0.2
	// 转人工不参与"被历史顶掉"：escalate 是安全兜底，永远不能被历史数据挤到后面，
	// 否则"历史上重跑成功过"会让某些必须人工介入的失败被自动重跑。
	if a.Action == models.HealActionEscalate || b.Action == models.HealActionEscalate {
		return false
	}
	if a.Samples >= minSamples && b.Samples >= minSamples {
		if a.Historical-b.Historical >= minDelta {
			return true
		}
		if b.Historical-a.Historical >= minDelta {
			return false
		}
	}
	return a.Base < b.Base
}

// confidence 给出置信度说明（落库，让人知道"这次决策有多确定"）。
func confidence(in PlanInput, chosen Candidate, class FailureClass) string {
	if class == ClassUnknown {
		return "low"
	}
	if chosen.Samples >= 3 {
		if chosen.Historical >= 0.6 {
			return "high"
		}
		if chosen.Historical >= 0.34 {
			return "medium"
		}
		return "low"
	}
	if chosen.Base == 0 {
		return "medium"
	}
	return "low"
}

// StatsJSON 把历史统计序列化（落库）。
func StatsJSON(stats map[models.HealAction]HistoryStats) string {
	if len(stats) == 0 {
		return "{}"
	}
	b, err := json.Marshal(stats)
	if err != nil {
		return "{}"
	}
	return string(b)
}
