package models

// CronySloBreachTableName SLO 违约处置记录表名（阶段 5 补全）。
//
// 为什么 SLO 违约要**单独一张事件表**，而不是只打日志：
//   - 日志会轮转、会被清、跨进程查不到；"这个契约上个月违约过几次、每次都做了什么"
//     是事故复盘的一手材料，必须像 job_heal 一样是**不可去重的事件行**；
//   - 它同时承担**跨进程冷却**：worker 与 admin 若都跑违约处置，靠"最近一行的时间"
//     做退避，比各自内存里的 map 更可靠（重启不丢）。
const CronySloBreachTableName = "slo_breach"

// SloBreachStatus 一次违约处置的结果状态。
type SloBreachStatus string

const (
	// SloBreachObserved 只留痕：动作是 none，或本应动作但没有可动作的对象
	// （全局契约没有绑定任务 / 任务没配收件人 / 窗口内没有失败执行）。
	SloBreachObserved SloBreachStatus = "observed"
	// SloBreachActed 动作已执行（通知已投递 / 诊断已触发）。
	SloBreachActed SloBreachStatus = "acted"
	// SloBreachFailed 动作执行失败（通知/诊断通道报错）。
	SloBreachFailed SloBreachStatus = "failed"
)

// SloBreachAction 违约后的动作。**这是一个闭合白名单**：
// 系统只会在这几件事里选，不存在"让模型现编一个动作"的路径。
type SloBreachAction string

const (
	// SloBreachActionNone 只记录，不做任何外部动作。
	SloBreachActionNone SloBreachAction = "none"
	// SloBreachActionNotify 发送违约告警（沿用任务自身的通知收件人）。
	SloBreachActionNotify SloBreachAction = "notify"
	// SloBreachActionDiagnose 触发一次失败诊断（需要 Agent 运行时；
	// 不可用时降级为通知，并在 detail 里说明，绝不假装诊断过）。
	SloBreachActionDiagnose SloBreachAction = "diagnose"
)

// SloBreach 一次违约处置的完整记录。
//
// 字段取舍标准与 job_heal 一致：**只看这一行就能回答"哪个契约违约了、为什么、
// 系统做了什么、结果如何"**。因此 report_json（违约快照）与 detail（处置说明）
// 都必须落库，而不是只记一个动作名。
type SloBreach struct {
	ID int64 `json:"id" gorm:"column:id;primary_key;auto_increment"`
	// PolicyID 命中的 SLO 契约。
	PolicyID int `json:"policy_id" gorm:"column:policy_id;not null;index:idx_slo_breach_policy"`
	// JobID 契约绑定的任务；0 = 全局契约。
	JobID      int    `json:"job_id" gorm:"column:job_id;not null;default:0"`
	PolicyName string `json:"policy_name" gorm:"size:64;column:policy_name"`
	JobName    string `json:"job_name" gorm:"size:64;column:job_name"`

	Action SloBreachAction `json:"action" gorm:"size:32;column:action;default:'none'"`
	Status SloBreachStatus `json:"status" gorm:"size:16;column:status;not null;default:'observed'"`
	// Reason 违约原因（由 slo.Report.Reasons 汇总，人可读）。
	Reason string `json:"reason" gorm:"size:512;column:reason;default:''"`
	// ReportJSON 违约评估快照（成功率/准时率/样本数/原因），事后可复算。
	ReportJSON string `json:"report_json" gorm:"size:1024;column:report_json;default:''"`
	// Detail 处置说明：做了什么、为什么没做（例如"Agent 未启用，降级为通知"）。
	Detail string `json:"detail" gorm:"size:512;column:detail;default:''"`
	// ExecutedBy 触发方：auto（周期循环）/ manual（人工触发接口）。
	ExecutedBy string `json:"executed_by" gorm:"size:32;column:executed_by;default:''"`

	Created   int64 `json:"created" gorm:"column:created;not null"`
	CreatedMS int64 `json:"created_ms" gorm:"column:created_ms;default:0"`
}

// TableName 实现 gorm.Tabler。
func (b *SloBreach) TableName() string { return CronySloBreachTableName }
