package models

// CronyAutonomyCreditTableName 自治信用表名（阶段 5）。
const CronyAutonomyCreditTableName = "autonomy_credit"

// AutonomyCredit 一个「动作 × 环境」的自治信用状态（持久化，重启不丢）。
//
// Scope 是维度键，形如 `<tool>@<env>`（例如 `job_retry@prod`）。为什么必须有环境维度：
// "重跑任务在测试环境很安全"不等于"在生产很安全"，把两者混成一个信用分会把生产拖下水。
type AutonomyCredit struct {
	ID int `json:"id" gorm:"column:id;primary_key;auto_increment"`
	// Scope 维度键（唯一）。
	Scope string `json:"scope" gorm:"size:160;column:scope;not null;uniqueIndex:uk_autonomy_scope"`
	// Level 当前等级（0~3，见 common/pkg/autonomy）。
	Level int `json:"level" gorm:"column:level;not null;default:0"`
	// Streak 当前连续成功次数。
	Streak int `json:"streak" gorm:"column:streak;not null;default:0"`
	// TotalUp / TotalDown 累计成功 / 不良后果。
	TotalUp   int `json:"total_up" gorm:"column:total_up;not null;default:0"`
	TotalDown int `json:"total_down" gorm:"column:total_down;not null;default:0"`
	// Frozen 是否冻结（冻结时实际生效等级回落 L1）。
	Frozen bool `json:"frozen" gorm:"column:frozen;not null;default:0"`
	// FrozenReason 冻结原因。
	FrozenReason string `json:"frozen_reason" gorm:"size:256;column:frozen_reason;default:''"`

	Created   int64 `json:"created" gorm:"column:created;not null"`
	Updated   int64 `json:"updated" gorm:"column:updated;default:0"`
	UpdatedMS int64 `json:"updated_ms" gorm:"column:updated_ms;default:0"`
}

// TableName 实现 gorm.Tabler。
func (a *AutonomyCredit) TableName() string { return CronyAutonomyCreditTableName }
