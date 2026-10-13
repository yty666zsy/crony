package service

import "github.com/tmnhs/crony/common/models"

// ---------- 指标看板的口径（纯函数） ----------
//
// 为什么把口径单独拎成纯函数：指标看板最容易错的不是"算不出来"，而是**把没有样本算成 0**。
// 看板上 `0%` 与"无样本"是两个完全不同的结论（一个说"全坏"、一个说"还没数据"），
// 一旦混同，看板就会撒谎——而且是没人会去复核的那种撒谎。
// 所以口径在这里穷举测试（metrics_test.go），DB 聚合只是"喂给它的输入"。

// NoSample 表示"没有样本"：分母为 0 时所有比率都取这个值。
//
// 为什么不用 0：0 的含义是"全是坏的"。为什么不用 null：调用方要判空/兼容 null，
// 而 -1 只要 `< 0` 一个判断（见 console/src/views/OpsView.vue 的 pct()）。
const NoSample = -1.0

// RatioOrNoSample 计算 num/den；den<=0 返回 NoSample（无样本）。
func RatioOrNoSample(num, den int64) float64 {
	if den <= 0 {
		return NoSample
	}
	return float64(num) / float64(den)
}

// HealRates 自愈类三率，以及它们各自的分子分母。
//
// 口径——分母是"**已出结论**的处置"，尚未有结论的状态（planned / executed）和
// "根本没动手"的 denied **都不进分母**：
//
//   - 自动修复率 = succeeded / decided —— 需要处置的失败里，系统自己修好的占比
//   - 人工接管率 = escalated / decided —— 系统判定"没把握"转人的占比
//   - 误动作率   = failed    / decided —— 动了手但没修好的占比
//
// 把 denied 放进分母是很容易犯的错：策略拒绝一次（系统正确地**不动手**）会把
// "自动修复率"往下拉，等于惩罚正确行为。
//
// 关于"误动作率"：方案原意接近"动作造成了额外损害"，那需要因果评估，本系统**观测不到**。
// 这里给的是**可观测的代理口径**（动手后仍失败），字段名与注释都把这件事说清楚，不冒充原意。
type HealRates struct {
	Decided    int64   `json:"decided"`
	Succeeded  int64   `json:"succeeded"`
	Escalated  int64   `json:"escalated"`
	Failed     int64   `json:"failed"`
	AutoRepair float64 `json:"auto_repair_rate"`
	Takeover   float64 `json:"human_takeover_rate"`
	Misaction  float64 `json:"misaction_rate"`
}

// FoldHealRates 从 job_heal 的状态分布折出自愈三率。
func FoldHealRates(byStatus map[string]int64) HealRates {
	r := HealRates{
		Succeeded: byStatus[string(models.HealStatusSucceeded)],
		Escalated: byStatus[string(models.HealStatusEscalated)],
		Failed:    byStatus[string(models.HealStatusFailed)],
	}
	r.Decided = r.Succeeded + r.Escalated + r.Failed
	r.AutoRepair = RatioOrNoSample(r.Succeeded, r.Decided)
	r.Takeover = RatioOrNoSample(r.Escalated, r.Decided)
	r.Misaction = RatioOrNoSample(r.Failed, r.Decided)
	return r
}

// ApprovalApprovedStatuses 视为"人工通过过"的状态集合。
//
// 为什么不只数 `approved`：批准之后单子会继续流转（executed / execution_failed / blocked），
// 状态字段被覆盖。只数 `approved` 会让通过率随着"被执行"而**假性下降**——
// 明明批了 10 单，执行完 8 单之后通过率反而从 100% 掉到 20%。
// 这四个状态都记着"当时人批过"这个事实。
var ApprovalApprovedStatuses = []string{
	models.ApprovalStatusApproved,
	models.ApprovalStatusExecuted,
	models.ApprovalStatusExecutionFailed,
	models.ApprovalStatusBlocked,
}

// ApprovalRates 审批通过率及其分子分母。
//
// 口径：只看**有"通过/不通过"结论**的单子。pending（还没人看）、expired（超时无人处理）
// 与 canceled（发起方撤回）**都不进分母**——把"没人管"或"撤回了"算成"不通过"，
// 会把注意力引向错误的结论。
type ApprovalRates struct {
	Approved int64   `json:"approved"`
	Rejected int64   `json:"rejected"`
	Decided  int64   `json:"decided"`
	PassRate float64 `json:"pass_rate"`
}

// FoldApprovalRates 从 approval_request 的状态分布折出审批通过率。
func FoldApprovalRates(byStatus map[string]int64) ApprovalRates {
	var r ApprovalRates
	for _, st := range ApprovalApprovedStatuses {
		r.Approved += byStatus[st]
	}
	r.Rejected = byStatus[models.ApprovalStatusRejected]
	r.Decided = r.Approved + r.Rejected
	r.PassRate = RatioOrNoSample(r.Approved, r.Decided)
	return r
}

// SessionTokenCost 会话 token 成本的聚合结果。
type SessionTokenCost struct {
	Sessions  int64   `json:"sessions"`
	Total     int64   `json:"total_tokens"`
	AvgTokens float64 `json:"avg_tokens"`
}

// FoldSessionTokenCost 折出"单次会话 token 成本"。
//
// 只统计**有 token 记账**的会话（total_tokens > 0）：token 记账上线之前的会话是 0，
// 把它们算进分母只会把平均成本稀释成一个好看但没意义的数字
// （"平均每次会话 300 token"听着很省，实际是拿几十条没记账的记录摊出来的）。
func FoldSessionTokenCost(tokens []int64) SessionTokenCost {
	var c SessionTokenCost
	for _, n := range tokens {
		if n <= 0 {
			continue
		}
		c.Sessions++
		c.Total += n
	}
	c.AvgTokens = RatioOrNoSample(c.Total, c.Sessions)
	return c
}
