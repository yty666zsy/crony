package tools

import "context"

// 审计决策取值。
//
// 与 models.AuditDecision* 一一对应（admin 侧落库时逐个映射，并有单测锁住字符串一致）：
// 审计表是按这些取值做复盘查询的，"两边字符串不一致"会导致查出来的永远为空，
// 而空结果看起来很像"没有发生过"。
const (
	// DecisionAllowed 真的执行了。
	DecisionAllowed = "allowed"
	// DecisionDenied 被某道闸拦下（Reason 必须写明是哪一道）。
	DecisionDenied = "denied"
	// DecisionApprovalRequired **没有副作用**：只是生成了一张待人工批准的审批单。
	DecisionApprovalRequired = "approval_required"
	// DecisionApprovalExecuted 人工批准后真的执行了。
	DecisionApprovalExecuted = "approval_executed"
)

// 拒绝原因分类（写进 CallRecord.Reason 的固定前缀，便于统计与告警）。
const (
	ReasonUnknownTool       = "未知工具（不在能力面内）"
	ReasonPolicyDenied      = "被策略 Deny/Allow 拒绝"
	ReasonRiskNotAllowed    = "风险级别未被策略允许"
	ReasonAutonomyL0        = "自治级别 L0（只建议）不允许写动作"
	ReasonInvalidArgs       = "参数不满足工具 Schema"
	ReasonSessionBudget     = "会话工具调用预算耗尽"
	ReasonRateLimited       = "每分钟调用速率超限（进程级）"
	ReasonConcurrencyLimit  = "并发调用超限（进程级）"
	ReasonBlastRadius       = "爆炸半径预算超限（单会话可影响的写动作/主机数）"
	ReasonApprovalNoChannel = "需要人工审批，但审批通道未接入（按安全默认：拒绝而不是放行）"
	ReasonToolPanic         = "工具实现 panic（已兜住）"
	ReasonTimeout           = "工具执行超时"
	ReasonExecuteFailed     = "工具执行失败"
)

// runMetaKey 上下文键（未导出，避免与其他包的 context 值冲突）。
type runMetaKey struct{}

// RunMeta 一次调用的归属信息：哪个会话类型、关联哪条执行记录。
//
// 为什么走 context 而不是加到 Call 的签名里：
// Call 的签名已经被验证过的契约（工具网关的唯一入口），加参数会牵动所有调用方；
// 而 Registry 本身**不知道也不该知道**"这是诊断会话还是 NL 起草会话"——
// 那是上层 Agent 运行时的事。用 context 传递是"上层告知、网关记录"的最短路径，
// 也与 llm.ContextWithTracker（预算记账）的既有做法一致。
type RunMeta struct {
	// AgentKind 会话类型（diagnose/nljob/job/...）。
	AgentKind string
	// RunID 关联的执行记录（诊断类会话）。
	RunID string
}

// WithRunMeta 把调用归属写进 context。
func WithRunMeta(ctx context.Context, meta RunMeta) context.Context {
	return context.WithValue(ctx, runMetaKey{}, meta)
}

// RunMetaFrom 读取调用归属（没有则为零值，不报错）。
//
// 不报错是有意的：缺少归属信息只影响审计的"可读性"，不该让工具调用失败。
func RunMetaFrom(ctx context.Context) RunMeta {
	if ctx == nil {
		return RunMeta{}
	}
	if v, ok := ctx.Value(runMetaKey{}).(RunMeta); ok {
		return v
	}
	return RunMeta{}
}