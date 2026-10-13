package rollout

import (
	"fmt"
	"time"

	"github.com/tmnhs/crony/common/pkg/tools"
)

// 阶段 5 补全：放量档位（rollout gate）。
//
// 为什么在"配置自治级别"与"自治信用"之外**还需要第三层**：
//
//   - 配置是**静态上限**：它回答"这套系统允许放到多松"，回答不了"今天敢不敢放"。
//     一个 L2 白名单在配置里是合法的，但在"刚刚评测判红"的此刻放出去就是错的。
//   - 信用是**按动作学出来的**：它回答"这类动作值不值得信任"。
//     一个工具可以信用满分，而整个平台此刻正在违约——信用看不见平台级的状态。
//   - 放量档位是**证据驱动的平台级上限**：只能靠"新鲜的、干净的证据"加宽，
//     一旦出现坏信号**自动收回 L0**，不需要任何人拍板。
//
// 三层的关系仍然是一句话：**配置是上限，信用与放量都只能往下压**（tools.Tighten）。
// 本包是纯函数（不碰数据库、不看时钟），"证据从哪来、状态存哪"由调用方负责——
// 这样"什么算坏信号""凭什么加宽"可以穷举测试，而不是靠集成环境去碰。
//
// 与"限流"的分工：限流管**单位时间的调用量**（进程级，已有），
// 放量档位管**能力的开合**（能不能自动动手）。两者不可互相替代。

// Config 放量闸门的判定参数。
//
// 为什么证据必须有**有效期**：三天前的一次绿色评测不能为今天的放行背书——
// 中间可能已经改过 prompt、换过模型、动过工具。没有有效期，闸门就退化成"曾经好过"。
type Config struct {
	// EvidenceTTL 证据有效期。
	EvidenceTTL time.Duration
	// RequireEval 加宽是否必须有"评测证据"（最近一次回放跑批）。
	RequireEval bool
	// RequireSLO 加宽是否必须有"SLO 证据"（最近一次契约评估）。
	RequireSLO bool
	// MaxStage 放量允许放到的最高档。
	//
	// 默认 **L2**：L3（全自动）不通过放量闸门放出去，要改配置——
	// 放量是"逐步扩大已验证的行为"，不是"绕过配置的捷径"。
	MaxStage tools.AutonomyLevel
}

// DefaultConfig 默认判定参数。
func DefaultConfig() Config {
	return Config{
		EvidenceTTL: 24 * time.Hour,
		RequireEval: true,
		RequireSLO:  true,
		MaxStage:    tools.AutonomyL2,
	}
}

// Evidence 一次加宽的"放行依据"。
//
// 只接受**可复查的事实**（来自库里已有的行），不接受"我觉得可以"：
// 每一条都能被第三个人拿去做同样的判断，加宽才谈得上可审计。
type Evidence struct {
	// EvalRunID 最近一次回放跑批的 id；0 = 还没有跑过批。
	EvalRunID  int64 `json:"eval_run_id"`
	EvalPassed int   `json:"eval_passed"`
	EvalFailed int   `json:"eval_failed"`
	// EvalSkipped 跳过的样本数。**"跳过"不是"通过"**：全跳过的跑批不构成放行依据。
	EvalSkipped int     `json:"eval_skipped"`
	EvalScore   float64 `json:"eval_score"`
	// EvalAt 该次跑批的结束时间（Unix 秒）。
	EvalAt int64 `json:"eval_at"`

	// SloTotal 本轮检查过的契约数；SloBreaches 其中判违约的条数。
	SloTotal    int `json:"slo_total"`
	SloBreaches int `json:"slo_breaches"`
	// SloAt 本次 SLO 快照时间（Unix 秒）；0 = 从未评估过。
	SloAt int64 `json:"slo_at"`
}

// Blockers 返回"必须立刻收回 L0"的坏信号（自动降级的判据）。
//
// 刻意**不看证据新旧**：坏事实不会因为过期而变好。
// 只有一份"更新的、干净的事实"（例如坏跑批之后又跑了一次全绿）才能把它顶掉，
// 而"最近一次跑批"本身就已经是更新的事实。
func Blockers(ev Evidence) []string {
	out := make([]string, 0, 2)
	if ev.EvalRunID > 0 && ev.EvalFailed > 0 {
		out = append(out, fmt.Sprintf("最近一次评测有 %d 条样本不通过（run_id=%d）", ev.EvalFailed, ev.EvalRunID))
	}
	if ev.SloBreaches > 0 {
		out = append(out, fmt.Sprintf("SLO 违约 %d/%d 条", ev.SloBreaches, ev.SloTotal))
	}
	return out
}

// WidenReasons 说明"为什么不能加宽到 stage"；返回空切片表示可以加宽。
//
// 顺序即优先级：先把坏信号摆出来（它们最容易被忽视），再说缺什么证据。
func WidenReasons(stage tools.AutonomyLevel, ev Evidence, cfg Config, now time.Time) []string {
	if !stage.Valid() {
		return []string{fmt.Sprintf("未知放量档位 %q（合法取值：L0/L1/L2/L3）", stage)}
	}
	if stage == tools.AutonomyL0 {
		return []string{"L0 是收紧的结果、不是放量的目标：请改用 tighten"}
	}
	reasons := make([]string, 0, 4)
	// 超过放量上限：用公开的 Tighten 判定"谁更严"，避免依赖未导出的 rank。
	if tools.Tighten(stage, cfg.MaxStage) != stage {
		reasons = append(reasons, fmt.Sprintf("超过放量允许的最高档 %s", cfg.MaxStage))
	}
	reasons = append(reasons, Blockers(ev)...)

	if cfg.RequireEval {
		switch {
		case ev.EvalRunID <= 0:
			reasons = append(reasons, "缺少评测证据：还没有可回放的跑批记录（先跑一次评测台回放）")
		case isStale(ev.EvalAt, cfg.EvidenceTTL, now):
			reasons = append(reasons, fmt.Sprintf("评测证据已过期（%s），不能为今天的放行背书", ageText(ev.EvalAt, now)))
		case ev.EvalFailed == 0 && ev.EvalPassed <= 0:
			// 只有「全跳过」才算「没有有效样本」：有失败的跑批由 Blockers 单独说，
			// 两条一起报只会让运维看到同一件事被说两遍。
			reasons = append(reasons, "最近一次评测没有有效样本（全部跳过），跳过不等于通过、不构成放行依据")
		}
	}
	if cfg.RequireSLO {
		switch {
		case ev.SloAt <= 0:
			reasons = append(reasons, "缺少 SLO 证据：还没有评估过任何契约")
		case isStale(ev.SloAt, cfg.EvidenceTTL, now):
			reasons = append(reasons, fmt.Sprintf("SLO 证据已过期（%s）", ageText(ev.SloAt, now)))
		}
	}
	return reasons
}

// AutoTarget 给出"依据证据应当立即生效的档位"。
//
// ok=false 表示**没有坏信号**，不需要自动收紧（返回的档位无意义，调用方不要用它）。
// 有坏信号时固定返回 L0：写动作整体收回，只留建议——
// 这正是"宁可不做，也不能做错"在放量这一层的样子。
func AutoTarget(ev Evidence) (stage tools.AutonomyLevel, reasons []string, ok bool) {
	reasons = Blockers(ev)
	if len(reasons) == 0 {
		return "", nil, false
	}
	return tools.AutonomyL0, reasons, true
}

// isStale 证据是否过期（时间为 0 视为"没有证据"，由调用方单独描述）。
func isStale(at int64, ttl time.Duration, now time.Time) bool {
	if at <= 0 {
		return true
	}
	if ttl <= 0 {
		return false // 未配置有效期 = 不过期（由调用方决定是否要求证据）
	}
	return now.Sub(time.Unix(at, 0)) > ttl
}

// ageText 把"多久之前"说成人话，让拒绝理由自带时间感。
func ageText(at int64, now time.Time) string {
	if at <= 0 {
		return "从未"
	}
	d := now.Sub(time.Unix(at, 0))
	if d < 0 {
		return "未来时间（时钟不一致？）"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒前", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	}
}
