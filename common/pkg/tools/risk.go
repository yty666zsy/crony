package tools

import (
	"fmt"
	"strings"
)

// RiskLevel 工具的风险级别。
//
// 为什么要有它（而不是继续用 ReadOnly() bool）：
//   - `ReadOnly` 只能回答"有没有副作用"，回答不了"副作用可不可逆"。
//     重跑一个失败任务与重启一台生产主机，在 bool 里长得一模一样；
//   - 阶段 4 要按风险给不同待遇：只读免审批、可逆写需审批、不可逆写永远审批。
//     这些待遇必须挂在**三级**取值上，二级不够用；
//   - 三级是刻意选择的：每多一级，配置与现实之间的错配面就多一层，
//     而安全分级一旦错配，后果是"以为在审批、其实没批"。
type RiskLevel string

const (
	// RiskRead 只读：无副作用（查库、读日志、看水位、只读探测）。
	RiskRead RiskLevel = "read"
	// RiskWrite 可逆写：有副作用，但后果可撤销、爆炸半径可控（如重跑失败任务）。
	RiskWrite RiskLevel = "write"
	// RiskDestructive 不可逆/高爆写：重启服务、删数据、改网络等。
	// 本期**不实现**任何 destructive 工具，但分级与闸门先立起来——
	// 否则将来加第一个 destructive 工具时，护栏是"顺手补的"。
	RiskDestructive RiskLevel = "destructive"
)

// Valid 取值是否合法。
func (r RiskLevel) Valid() bool {
	switch r {
	case RiskRead, RiskWrite, RiskDestructive:
		return true
	}
	return false
}

// IsWrite 是否是有副作用的级别（write 或 destructive）。
func (r RiskLevel) IsWrite() bool { return r == RiskWrite || r == RiskDestructive }

// String 便于日志与契约输出。
func (r RiskLevel) String() string { return string(r) }

// ParseRiskLevel 解析配置里的风险级别（大小写不敏感、允许空白）。
func ParseRiskLevel(s string) (RiskLevel, error) {
	switch RiskLevel(strings.ToLower(strings.TrimSpace(s))) {
	case RiskRead:
		return RiskRead, nil
	case RiskWrite:
		return RiskWrite, nil
	case RiskDestructive:
		return RiskDestructive, nil
	}
	return "", fmt.Errorf("tools: 未知风险级别 %q（合法取值：read/write/destructive）", s)
}

// TargetProvider 由"写工具"实现：声明本次调用会影响哪些目标（节点 UUID 等）。
//
// 为什么写工具**必须**实现它，而只读工具可以不实现：
//
//	爆炸半径预算（单会话可影响主机数）需要知道"这次调用碰的是哪台机器"。
//	如果写工具可以不声明目标，那么"限制影响 1 台主机"就变成一句空话——
//	一个没声明的写工具会静默地绕开这条预算。因此 Register 对写工具强制要求实现本接口。
type TargetProvider interface {
	// Targets 返回本次调用影响的目标标识（去重由调用方做）；返回空表示"没有具体目标"。
	Targets(args map[string]interface{}) []string
}

// TargetsOf 安全地取影响目标（未实现 TargetProvider 的只读工具返回 nil）。
func TargetsOf(t Tool, args map[string]interface{}) []string {
	p, ok := t.(TargetProvider)
	if !ok {
		return nil
	}
	defer func() { _ = recover() }() // 目标提取不得把调用带崩
	return p.Targets(args)
}

// AutonomyLevel 自治级别（模型能自己动手到什么程度）。
//
// 默认 **L1**：写动作一律生成审批单、等人工批准后才执行。
// 这不是"保守"，而是"默认值必须安全"——把 L2/L3 设为默认，
// 等于让每个没读文档的部署者都在没有人在环的情况下跑写动作。
type AutonomyLevel string

const (
	// AutonomyL0 只建议：写工具对模型完全不可用（连审批单都不生成）。
	AutonomyL0 AutonomyLevel = "L0"
	// AutonomyL1 一键审批：写动作生成审批单，人工批准后执行（**默认**）。
	AutonomyL1 AutonomyLevel = "L1"
	// AutonomyL2 白名单内自动：命中 AutoApprove 白名单的写动作直接执行，
	// 其余仍转审批；destructive 即使在白名单里也**仍然要审批**。
	AutonomyL2 AutonomyLevel = "L2"
	// AutonomyL3 全自动：写动作直接执行（仍受爆炸半径与三道闸约束）；
	// destructive 仍然要审批。**默认禁用**，需显式配置。
	AutonomyL3 AutonomyLevel = "L3"
)

// Valid 取值是否合法。
func (a AutonomyLevel) Valid() bool {
	switch a {
	case AutonomyL0, AutonomyL1, AutonomyL2, AutonomyL3:
		return true
	}
	return false
}

// AllowsWriteTools 该级别下写工具是否"存在"（L0 直接不可用）。
func (a AutonomyLevel) AllowsWriteTools() bool { return a != AutonomyL0 }

// String 便于日志与契约输出。
func (a AutonomyLevel) String() string { return string(a) }

// ParseAutonomyLevel 解析配置（大小写不敏感；"0"/"1"/"2"/"3" 也接受）。
func ParseAutonomyLevel(s string) (AutonomyLevel, error) {
	v := strings.ToUpper(strings.TrimSpace(s))
	if v != "" && !strings.HasPrefix(v, "L") {
		v = "L" + v
	}
	switch AutonomyLevel(v) {
	case AutonomyL0:
		return AutonomyL0, nil
	case AutonomyL1:
		return AutonomyL1, nil
	case AutonomyL2:
		return AutonomyL2, nil
	case AutonomyL3:
		return AutonomyL3, nil
	}
	return "", fmt.Errorf("tools: 未知自治级别 %q（合法取值：L0/L1/L2/L3）", s)
}

// rank 自治级别的严格序：L0 < L1 < L2 < L3。非法值按最严格（L0）处理。
func (a AutonomyLevel) rank() int {
	switch a {
	case AutonomyL1:
		return 1
	case AutonomyL2:
		return 2
	case AutonomyL3:
		return 3
	default:
		return 0
	}
}

// Tighten 取两者中**更严格**（级别更低）的一档。
//
// 这是阶段 5「自治信用」接入放行判定的核心不变式：**配置是上限，信用只能往下压**。
// 无论信用解析器返回什么（哪怕是 L3），实际生效级别都不会超过运维配置的那一档。
// 把它做进网关而不是只做在服务层，是为了让"越权升级"在**最里层**就不可能发生。
func Tighten(ceiling, want AutonomyLevel) AutonomyLevel {
	if want.rank() < ceiling.rank() {
		return want
	}
	return ceiling
}

// AutonomyResolver 阶段 5：把「自治信用」解析成某工具**当下**实际生效的自治级别。
//
// ok=false 表示"无从判断"（例如该动作还没有任何历史）→ 调用方沿用配置级别。
// 它是注入点：网关本身不碰数据库（保持纯函数、可穷举测试），
// "信用从哪来、怎么算"由调用方（admin 服务层）负责。
type AutonomyResolver func(tool string) (AutonomyLevel, bool)

// EffectiveAutonomy 计算某工具实际生效的自治级别：配置为上限，信用只可收紧。
//
// 三种情形：
//   - 未装配解析器（resolve==nil）：返回配置级别（**行为不变**，零回归）；
//   - 解析器无判断（ok=false）：返回配置级别（没有历史 = 沿用运维配置）；
//   - 解析器有判断：返回 min(配置, 信用)（信用更严格才生效，否则被配置夹住）。
func (p Policy) EffectiveAutonomy(resolve AutonomyResolver, tool string) AutonomyLevel {
	if resolve == nil {
		return p.Autonomy
	}
	want, ok := resolve(tool)
	if !ok {
		return p.Autonomy
	}
	return Tighten(p.Autonomy, want)
}

// autoApproveMatch 判断某工具/目标是否命中"免审批"条件（用配置级别，兼容旧调用）。
func (p Policy) autoApproveMatch(name string, targets []string, risk RiskLevel) bool {
	return p.autoApproveMatchAt(p.Autonomy, name, targets, risk)
}

// autoApproveMatchAt 用**给定的生效级别**判断是否免审批。
//
// 三档语义（顺序即优先级）：
//   - destructive **永不**免审批（无论自治级别多高、白名单怎么写）；
//   - L3（全自动）：所有写动作免审批（仍受爆炸半径与三道闸约束）；
//   - L2（白名单）：只有精确命中白名单的条目才免审批；
//   - L1/L0：一律不免审批。
//
// 白名单条目形如 `job_retry`（任意目标）或 `job_retry@node-it-01`（指定目标）。
// 只支持"精确匹配"是刻意的：通配符白名单（`job_*`）会让人以为自己放行了一个动作，
// 实际放行了一整类动作——这是最典型的"配置事故"来源。
func (p Policy) autoApproveMatchAt(level AutonomyLevel, name string, targets []string, risk RiskLevel) bool {
	if risk == RiskDestructive {
		return false
	}
	switch level {
	case AutonomyL3:
		return true
	case AutonomyL2:
	default:
		return false
	}
	for _, entry := range p.AutoApprove {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		rawTool, rawTarget, hasTarget := strings.Cut(entry, "@")
		if strings.TrimSpace(rawTool) != name {
			continue
		}
		if !hasTarget {
			return true
		}
		want := strings.TrimSpace(rawTarget)
		for _, got := range targets {
			if got == want {
				return true
			}
		}
	}
	return false
}
