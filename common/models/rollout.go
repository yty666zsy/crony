package models

// CronyRolloutStateTableName 放量档位表名（阶段 5 补全）。
//
// 为什么放量档位要**落库**而不是放在内存/配置里：
//   - 它是"自动降级"的落点：坏信号出现时必须**立刻**生效并且重启后依然生效
//     （重启就把 L0 忘掉，等于自动降级只活到下一次部署）；
//   - 它是**证据的锚点**：每一档都记着"凭哪次跑批、哪次 SLO 快照放上来的"，
//     事故复盘时能回答"当时凭什么敢放"。
const CronyRolloutStateTableName = "rollout_state"

// RolloutState 一个环境当前的放量档位。
//
// Env 唯一：放量是**按环境**的（"测试环境敢放"不等于"生产敢放"），
// 与自治信用的 scope 环境维度同一个理由。
type RolloutState struct {
	ID int `json:"id" gorm:"column:id;primary_key;auto_increment"`
	// Env 环境段（唯一），与 tools.autonomy-env 同一个取值空间。
	Env string `json:"env" gorm:"size:32;column:env;not null;uniqueIndex:uk_rollout_env"`
	// Stage 当前档位（L0~L3）。
	Stage string `json:"stage" gorm:"size:8;column:stage;not null;default:'L0'"`
	// Frozen 是否处于"自动降级冻结"状态：冻结期间加宽必须重新走证据（人工确认）。
	Frozen bool `json:"frozen" gorm:"column:frozen;not null;default:0"`
	// Reason 最近一次变档的原因（加宽是"证据摘要"，收紧是"坏信号"）。必填：没有解释的变档无法复盘。
	Reason string `json:"reason" gorm:"size:512;column:reason;not null;default:''"`
	// EvidenceJSON 加宽时冻结的证据快照（可复查；收紧时保留上一次加宽的依据）。
	EvidenceJSON string `json:"evidence_json" gorm:"type:text;column:evidence_json"`
	// WidenedAt / TightenedAt 最近一次加宽 / 收紧的时间（Unix 秒）。
	WidenedAt   int64 `json:"widened_at" gorm:"column:widened_at;not null;default:0"`
	TightenedAt int64 `json:"tightened_at" gorm:"column:tightened_at;not null;default:0"`

	Created int64 `json:"created" gorm:"column:created;not null"`
	Updated int64 `json:"updated" gorm:"column:updated;default:0"`
}

// TableName 实现 gorm.Tabler。
func (r *RolloutState) TableName() string { return CronyRolloutStateTableName }
