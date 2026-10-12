package models

import (
	"fmt"
	"sync/atomic"
	"time"
)

// CronyJobHealTableName 自愈记录的存储表名（阶段 4-C）。
//
// 为什么自愈需要**自己的**一张表，而不是只写 agent_memory：
//   - agent_memory 是"经验"（可复用、会去重累加、面向未来决策）；
//   - job_heal 是"事件"（这一次失败被怎么处置、依据是什么、结果如何），**不可去重**——
//     把两次自愈合并成一条，等于把事故处置过程抹掉，事故复盘就没有第一手材料了。
//
// 两者的关系是：每次自愈尝试**同时**落一行 job_heal（事件）与一条 agent_memory（经验）。
const CronyJobHealTableName = "job_heal"

// HealStatus 自愈尝试的状态。
//
// 状态机的全貌（服务层实现）：
//
//	planned ──> denied      策略拒绝（写动作未开 / 超次数 / 冷却中 / 并发满）——终态
//	        └─> executed ──> succeeded  自愈后观测到后续执行成功——终态
//	                    ├─> failed     动作投递失败，或自愈后仍然失败——终态（可再次规划）
//	                    └─> escalated  超出能力/次数，转人工——终态（不再自动尝试）
//	        └─> escalated     没有安全动作可选（类未知 / 编排非法 / 节点不可达）——终态
type HealStatus string

const (
	// HealStatusPlanned 已决策出动作，尚未执行（执行瞬间的中间态）。
	HealStatusPlanned HealStatus = "planned"
	// HealStatusDenied 被策略拒绝：**没有动手**。拒绝本身是结果，必须留痕。
	HealStatusDenied HealStatus = "denied"
	// HealStatusExecuted 动作已投递/已生效，等待观测结果。
	HealStatusExecuted HealStatus = "executed"
	// HealStatusSucceeded 自愈后观测到后续执行成功。
	HealStatusSucceeded HealStatus = "succeeded"
	// HealStatusFailed 自愈动作执行失败，或自愈后仍失败。
	HealStatusFailed HealStatus = "failed"
	// HealStatusEscalated 转人工：不再由系统自动尝试。
	HealStatusEscalated HealStatus = "escalated"
)

// healStatusNames 中文解释：控制台与报告都要给人看懂，不让调用方自己猜。
var healStatusNames = map[HealStatus]string{
	HealStatusPlanned:   "已规划",
	HealStatusDenied:    "被策略拒绝（未动手）",
	HealStatusExecuted:  "已执行待观测",
	HealStatusSucceeded: "自愈成功",
	HealStatusFailed:    "自愈失败",
	HealStatusEscalated: "已转人工",
}

// String 实现 fmt.Stringer。
func (s HealStatus) String() string {
	if n, ok := healStatusNames[s]; ok {
		return n
	}
	return string(s)
}

// IsFinal 是否为终态（终态才允许写 finished_at 并停止观测）。
func (s HealStatus) IsFinal() bool {
	switch s {
	case HealStatusDenied, HealStatusSucceeded, HealStatusFailed, HealStatusEscalated:
		return true
	}
	return false
}

// HealAction 自愈动作。**这是一个白名单**：系统只会做这几件事。
//
// 为什么必须是闭合枚举而不是"让模型给个命令"：自愈是**写动作**。写动作的自由度
// 就是系统的爆炸半径。白名单之外的失败一律转人工（escalate），
// 这不是能力不足，而是把"没有把握时不要动手"变成代码里的事实。
type HealAction string

const (
	// HealActionRetrySameNode 在原节点重跑一次（复用 job_retry 通道）。
	HealActionRetrySameNode HealAction = "retry_same_node"
	// HealActionRetryExtendTimeout 先把该任务的超时上限调大（有上限约束）再重跑。
	HealActionRetryExtendTimeout HealAction = "retry_extend_timeout"
	// HealActionEscalate 转人工：不做任何自动动作。
	HealActionEscalate HealAction = "escalate"
)

// HealActionNames 动作的中文解释。
var HealActionNames = map[HealAction]string{
	HealActionRetrySameNode:      "原节点重跑",
	HealActionRetryExtendTimeout: "调大超时后重跑",
	HealActionEscalate:           "转人工处理",
}

// String 实现 fmt.Stringer。
func (a HealAction) String() string {
	if n, ok := HealActionNames[a]; ok {
		return n
	}
	return string(a)
}

// IsWrite 该动作是否会**改变系统状态**（写动作）。
//
// 转人工不是写动作；重跑与改超时都是。这个判据决定了动作要不要过"写能力已开启"这道闸门。
func (a HealAction) IsWrite() bool {
	return a == HealActionRetrySameNode || a == HealActionRetryExtendTimeout
}

// JobHeal 一次自愈尝试的完整记录。
//
// 字段取舍的唯一标准：**事后能不能只看这一行回答"为什么动手/为什么没动手、做了什么、结果如何"**。
// 因此 reason（为什么这么决策）、policy_json（命中了哪条策略规则）、
// evidence_json（依据的失败事实）三者都必须落库，而不是只记一个动作名。
type JobHeal struct {
	ID int64 `json:"id" gorm:"column:id;primary_key;auto_increment"`
	// HealID 外部引用号（heal-<纳秒>-<序号>），不暴露自增 id。
	HealID string `json:"heal_id" gorm:"size:64;column:heal_id;not null;uniqueIndex:uk_job_heal_id"`
	// SourceRunID 触发本次自愈的那次失败执行的 run_id（幂等与追溯的锚点）。
	SourceRunID string `json:"source_run_id" gorm:"size:64;column:source_run_id;not null;index:idx_job_heal_source"`
	JobID       int    `json:"job_id" gorm:"column:job_id;not null;index:idx_job_heal_job"`
	JobName     string `json:"job_name" gorm:"size:64;column:job_name"`
	// Attempt 这是针对同一次失败的**第几次**尝试（从 1 开始）。
	// 为什么按 source_run 计数而不是按 job 计数：同一次故障重试三次与三次不同故障各重试一次，
	// 是完全不同的两件事。
	Attempt int `json:"attempt" gorm:"column:attempt;not null;default:1"`

	Status HealStatus `json:"status" gorm:"size:16;column:status;not null;default:'planned';index:idx_job_heal_status"`
	// FailureClass 失败分类（确定性规则给出）：自愈的**唯一入口判据**。
	FailureClass string `json:"failure_class" gorm:"size:32;column:failure_class;default:'';index:idx_job_heal_class"`
	// Symptom 人可读的症状描述（同时作为经验库去重键的输入）。
	Symptom string `json:"symptom" gorm:"size:512;column:symptom;not null"`

	Action HealAction `json:"action" gorm:"size:32;column:action;default:'';index:idx_job_heal_action"`
	// ActionJSON 动作参数快照（例如把超时从 60 调到 120）。
	ActionJSON string `json:"action_json" gorm:"size:512;column:action_json;default:''"`
	// Reason 决策理由（给人看的一句话，含"为什么不是别的动作"）。
	Reason string `json:"reason" gorm:"size:512;column:reason;default:''"`
	// PolicyJSON 策略判定快照：允许/拒绝 + 命中哪条规则（事后复盘安全边界用）。
	PolicyJSON string `json:"policy_json" gorm:"size:512;column:policy_json;default:''"`
	// EvidenceJSON 依据的失败事实（run 状态/退出码/错误摘要/节点状态/失败步骤），
	// **只放本系统自己观测到的东西**，不放模型自述。
	EvidenceJSON string `json:"evidence_json" gorm:"size:1024;column:evidence_json;default:''"`
	// MemoryJSON 决策时用到的历史经验（命中条目与成功率），说明"这次决策参考了什么"。
	MemoryJSON string `json:"memory_json" gorm:"size:1024;column:memory_json;default:''"`

	NodeUUID string `json:"node_uuid" gorm:"size:128;column:node_uuid;default:'';index:idx_job_heal_node"`
	// NewRunID 自愈后观测到的那次新执行（可为空：投递失败或还没跑）。
	NewRunID string `json:"new_run_id" gorm:"size:64;column:new_run_id;default:''"`
	// OutcomeNote 结果说明（成功/失败的判据文本）。
	OutcomeNote string `json:"outcome_note" gorm:"size:512;column:outcome_note;default:''"`
	// Deadline 观测截止时刻（unix 秒）：超过它还没有新执行，就按"没观测到"处理。
	Deadline int64 `json:"deadline" gorm:"column:deadline;default:0"`
	// MemoryIDs 本次沉淀的经验条目号（JSON 数组字符串），把事件与经验连起来。
	MemoryIDs string `json:"memory_ids" gorm:"size:512;column:memory_ids;default:''"`
	// ExecutedBy 触发方：self_heal_loop（自动）/ manual（人工点）/ 用户名。
	ExecutedBy string `json:"executed_by" gorm:"size:64;column:executed_by;default:''"`

	Created   int64 `json:"created" gorm:"column:created;not null"`
	CreatedMS int64 `json:"created_ms" gorm:"column:created_ms;default:0"`
	Updated   int64 `json:"updated" gorm:"column:updated;default:0"`
	UpdatedMS int64 `json:"updated_ms" gorm:"column:updated_ms;default:0"`
	// FinishedAt 进入终态的时刻（未终结为 0）。
	FinishedAt int64 `json:"finished_at" gorm:"column:finished_at;default:0"`
}

// TableName 实现 gorm.Tabler。
func (h *JobHeal) TableName() string { return CronyJobHealTableName }

// healSeq 进程内自增序号，兜住同纳秒并发。
var healSeq uint64

// NewHealID 生成自愈单号：`heal-<纳秒>-<序号>`。
func NewHealID(now time.Time) string {
	seq := atomic.AddUint64(&healSeq, 1)
	return fmt.Sprintf("heal-%d-%d", now.UnixNano(), seq)
}
