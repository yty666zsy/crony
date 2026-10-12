package models

// CronySloPolicyTableName SLO 契约表名（阶段 4-D）。
const CronySloPolicyTableName = "slo_policy"

// SloPolicy 一条 SLO 契约（"任务/分组对结果负责"的声明）。
//
// 为什么用独立表而不是塞进 job：SLO 是**运维侧的期望**，与任务的执行定义生命周期不同
// （一个任务可以先上线、后补 SLO；也可以一个 SLO 管一批任务）。JobID=0 表示"该 SLO 面向全部任务"
// （分组 SLO 的第一形态，后续可扩展为标签选择器）。
type SloPolicy struct {
	ID int `json:"id" gorm:"column:id;primary_key;auto_increment"`
	// Name 契约名（人可读）。
	Name string `json:"name" gorm:"size:64;column:name;not null"`
	// JobID 目标任务；0 = 全局。
	JobID int `json:"job_id" gorm:"column:job_id;not null;default:0;index:idx_slo_policy_job"`
	// TargetSuccessRate / TargetOnTimeRate 目标（0~1）；0 表示该项不设目标。
	TargetSuccessRate float64 `json:"target_success_rate" gorm:"column:target_success_rate;not null;default:0"`
	TargetOnTimeRate  float64 `json:"target_on_time_rate" gorm:"column:target_on_time_rate;not null;default:0"`
	// WindowSeconds 统计窗口。
	WindowSeconds int64 `json:"window_seconds" gorm:"column:window_seconds;not null;default:86400"`
	// GraceSeconds 准时宽限。
	GraceSeconds int64 `json:"grace_seconds" gorm:"column:grace_seconds;not null;default:300"`
	// Enabled 是否启用。
	Enabled bool `json:"enabled" gorm:"column:enabled;not null;default:1"`
	// BreachAction 违约后的动作（预留）：diagnose=触发诊断（阶段 5 接线）。
	BreachAction string `json:"breach_action" gorm:"size:32;column:breach_action;default:'diagnose'"`

	Created int64 `json:"created" gorm:"column:created;not null"`
	Updated int64 `json:"updated" gorm:"column:updated;default:0"`
}

// TableName 实现 gorm.Tabler。
func (s *SloPolicy) TableName() string { return CronySloPolicyTableName }
