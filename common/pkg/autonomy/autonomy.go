// Package autonomy 实现阶段 5 的「自治等级随证据自动升降」（方案 §8-3 Autonomy Credit）。
//
// 绝大多数平台只有两极：人工审批 / 全自动。本包把"信任"做成一个**可计算、可回退**的状态机：
// 一个「动作类型 × 环境」维护一个信用分，连续成功若干次自动升一级；出现一次误动作或被驳回
// 立即降级并冻结（冻结 = 回落到人工审批，直到显式解冻）。
//
// 三条纪律：
//  1. **只升不降的自动机制是危险的**——任何一次"实际发生的不良后果"都必须能立刻降级；
//  2. **冻结优先于升级**：冻结状态下永远不升级，哪怕连续成功了很多次；
//  3. **纯函数**：同样的输入永远得到同样的信用状态，才能"事后复盘为什么当时是 L2"。
package autonomy

import "strings"

// Level 自治等级。
type Level int

const (
	// LevelAdvisory L0：只建议，不执行任何动作。
	LevelAdvisory Level = 0
	// LevelApproval L1：一切写动作都需要人工审批（默认档）。
	LevelApproval Level = 1
	// LevelWhitelist L2：白名单内的动作可自动执行。
	LevelWhitelist Level = 2
	// LevelFull L3：全自动（默认禁用；高风险动作永远不该到这里）。
	LevelFull Level = 3
)

// Name 返回 L0~L3 的短名（落库与展示用）。
func (l Level) Name() string {
	switch l {
	case LevelAdvisory:
		return "L0"
	case LevelApproval:
		return "L1"
	case LevelWhitelist:
		return "L2"
	case LevelFull:
		return "L3"
	}
	return "L?"
}

// ParseLevel 解析等级字符串（大小写不敏感）；非法返回 ok=false。
func ParseLevel(s string) (Level, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "L0", "0":
		return LevelAdvisory, true
	case "L1", "1":
		return LevelApproval, true
	case "L2", "2":
		return LevelWhitelist, true
	case "L3", "3":
		return LevelFull, true
	}
	return LevelAdvisory, false
}

// Config 升降规则。
type Config struct {
	// PromoteAfter 连续成功多少次升一级（默认 5）。
	PromoteAfter int
	// DemoteOnMistake 一次不良后果降几级（默认 1）。
	DemoteOnMistake int
	// FreezeOnMistake 出现不良后果是否冻结（默认 true）。
	FreezeOnMistake bool
	// MaxLevel 允许自动升到的最高等级（默认 L2；L3 必须人工开启）。
	MaxLevel Level
}

// DefaultConfig 保守默认：连成 5 次升一级，一次不良后果降级并冻结，最高只自动升到 L2。
func DefaultConfig() Config {
	return Config{PromoteAfter: 5, DemoteOnMistake: 1, FreezeOnMistake: true, MaxLevel: LevelWhitelist}
}

// Credit 一个（动作 × 环境）的信用状态。
type Credit struct {
	Level       Level  `json:"level"`
	Streak      int    `json:"streak"`       // 当前连续成功次数
	TotalUp     int    `json:"total_up"`     // 累计成功
	TotalDown   int    `json:"total_down"`   // 累计不良后果
	Frozen      bool   `json:"frozen"`       // 冻结：回落到人工
	FrozenReason string `json:"frozen_reason,omitempty"`
}

// Observe 吃进一次结果，返回新的信用状态。
//
// result 语义：
//   - success=true  → 连续成功 +1；达到门槛且未冻结则升一级；
//   - success=false → 视为"不良后果"：降级、冻结、清零连续成功。
//
// 为什么把任何失败都当"不良后果"（而不是区分瞬时/永久）：自治等级的升级对象是**写动作**，
// 写动作失败的代价不对称。宁可保守（失败即冻结），也不让一个反复失败的动作用"连续成功"洗白。
// 需要区分时，调用方可以只在"确认是系统侧不良后果"时才调 Observe(false)。
func Observe(c Credit, success bool, cfg Config) Credit {
	cfg = normalizeConfig(cfg)
	if success {
		c.TotalUp++
		c.Streak++
		if !c.Frozen && c.Streak >= cfg.PromoteAfter && c.Level < cfg.MaxLevel {
			c.Level++
			c.Streak = 0
		}
		return c
	}
	// 不良后果。
	c.TotalDown++
	c.Streak = 0
	c.Level -= Level(cfg.DemoteOnMistake)
	if c.Level < LevelAdvisory {
		c.Level = LevelAdvisory
	}
	if cfg.FreezeOnMistake {
		c.Frozen = true
		c.FrozenReason = "最近一次动作产生了不良后果：已降级并冻结，需人工解冻"
	}
	return c
}

// normalizeConfig 补齐非法/缺省字段。零值 Config 用默认（保守）。
func normalizeConfig(cfg Config) Config {
	if cfg == (Config{}) {
		return DefaultConfig()
	}
	d := DefaultConfig()
	if cfg.PromoteAfter <= 0 {
		cfg.PromoteAfter = d.PromoteAfter
	}
	if cfg.DemoteOnMistake <= 0 {
		cfg.DemoteOnMistake = d.DemoteOnMistake
	}
	if cfg.MaxLevel <= 0 {
		cfg.MaxLevel = d.MaxLevel
	}
	return cfg
}

// Unfreeze 人工解冻（显式动作；解冻不改变等级，也不清零历史）。
func Unfreeze(c Credit) Credit {
	c.Frozen = false
	c.FrozenReason = ""
	c.Streak = 0
	return c
}

// Effective 返回"当前实际生效的等级"：冻结时回落 L1（人工审批）。
//
// 为什么要回落到 L1 而不是 L0：冻结的对象是"自动执行"，运维仍希望系统继续给建议、
// 走审批；回落 L0 会把"暂时不许自动"误解成"连诊断都别做了"。
func Effective(c Credit) Level {
	if c.Frozen {
		return LevelApproval
	}
	return c.Level
}

// EnforcementLevel 返回"用于放行判定的等级"：在 Effective 之上再加一条底线 L1。
//
// 为什么需要这条底线（而不是直接把 Effective 接到放行判定上）：
// 信用只负责回答"**要不要自动**"，不负责回答"**要不要写**"。写动作的门槛由
// 运维配置（L0 = 只建议 / L1 = 审批）决定，不该被"某动作刚被观测过一次"这种事
// 悄悄改掉。否则会出现一个荒谬的序列：某动作首次自动执行成功 → 记一笔信用
// （等级 0）→ 下一次却被判成 L0、**连审批单都不生成**。刚成功过的动作反而被
// 全盘拒绝，既反直觉，也让"升级一套在跑的系统"变得不安全。
//
// 因此：等级 0 在放行判定里等价于 L1（照旧走审批），要靠连续成功挣到 L2 才免审批；
// 冻结仍然回落 L1；L0 只建议只可能来自运维配置，不来自信用。
func EnforcementLevel(c Credit) Level {
	l := Effective(c)
	if l < LevelApproval {
		return LevelApproval
	}
	return l
}
