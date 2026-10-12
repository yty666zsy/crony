package service

import (
	"strings"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/autonomy"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/tools"
	"gorm.io/gorm"
)

// 阶段 5：把「自治信用」接进写动作的放行判定（只读侧的读取与解释）。
//
// 目标：让"某类写动作在出了不良后果之后**自动**回到人工审批"这件事真的生效，
// 而不是只停留在 `autonomy_credit` 表里。
//
// 三条不变式（前两条在 common/pkg/tools 里由网关强制，第三条在这里）：
//  1. **配置是上限**：实际生效级别 = min(配置自治级别, 信用级别)。信用只能收紧，不能越权。
//  2. **没有历史 = 沿用配置**：解析不到该动作的信用时返回 ok=false，网关回落到配置级别
//     （这保证"升级在跑的系统时行为不变"）。
//  3. **查不到/出错一律回落到配置**：解析器绝不因为数据库抖动而"猜一个更宽的级别"。

// AutonomyEnvDefault 未配置环境段时的默认值。
const AutonomyEnvDefault = "default"

// ConfiguredAutonomyEnv 进程配置的信用环境段（`tools.autonomy-env`），未配置时用默认值。
//
// 单独抽出来是为了让"信用按哪个环境计"只有一个来源：网关解析器与解释接口都用它，
// 避免出现"放行时用 acc、解释时用 prod"这种两套口径。
func ConfiguredAutonomyEnv() string {
	if cfg := config.GetConfigModels(); cfg != nil {
		if e := strings.TrimSpace(cfg.Tools.AutonomyEnv); e != "" {
			return e
		}
	}
	return AutonomyEnvDefault
}

// AutonomyScope 拼出「动作@环境」的信用维度键。
func AutonomyScope(tool, env string) string {
	tool = strings.TrimSpace(tool)
	env = strings.TrimSpace(env)
	if env == "" {
		env = AutonomyEnvDefault
	}
	return tool + "@" + env
}

// toolsLevelOf 把信用内核的等级映射成工具网关的自治级别。
// 两套枚举语义一致（L0 只建议 / L1 审批 / L2 白名单自动 / L3 全自动）。
func toolsLevelOf(l autonomy.Level) tools.AutonomyLevel {
	switch l {
	case autonomy.LevelAdvisory:
		return tools.AutonomyL0
	case autonomy.LevelApproval:
		return tools.AutonomyL1
	case autonomy.LevelWhitelist:
		return tools.AutonomyL2
	case autonomy.LevelFull:
		return tools.AutonomyL3
	}
	return tools.AutonomyL1
}

// resolveCredit 读取某 scope 的信用行，算出"信用侧建议的等级"（raw）与
// "用于放行判定的等级"（enforcement，含 L1 底线）。
// 返回 ok=false 表示"没有历史/无法判断"（含记录不存在与数据库不可用）。
//
// 为什么未命中也要与"查询出错"一样返回 ok=false：两者都意味着
// "没有证据支持收紧"，此时最安全的选择是沿用运维配置，而不是凭空造一个级别。
func resolveCredit(db *gorm.DB, scope string) (raw, enforcement autonomy.Level, ok, frozen bool, row models.AutonomyCredit) {
	if db == nil {
		return autonomy.LevelApproval, autonomy.LevelApproval, false, false, models.AutonomyCredit{}
	}
	err := db.Table(models.CronyAutonomyCreditTableName).Where("scope = ?", scope).First(&row).Error
	if err != nil {
		return autonomy.LevelApproval, autonomy.LevelApproval, false, false, row
	}
	c := autonomy.Credit{Level: autonomy.Level(row.Level), Frozen: row.Frozen}
	return autonomy.Effective(c), autonomy.EnforcementLevel(c), true, row.Frozen, row
}

// AutonomyResolverFor 构造写入工具网关的信用解析器。
//
// resolver 在**每次写动作调用时**执行（因此信用一冻结，下一次调用立刻回到审批），
// 这就是"任何一次实际发生的不良后果都必须能立刻降级"的落地方式。
//
// 返回的是 EnforcementLevel（含 L1 底线）：信用只能决定"要不要自动"，不能决定"要不要写"。
func AutonomyResolverFor(env string) tools.AutonomyResolver {
	return func(tool string) (tools.AutonomyLevel, bool) {
		_, enforced, ok, _, _ := resolveCredit(dbclient.GetMysqlDB(), AutonomyScope(tool, env))
		if !ok {
			return "", false
		}
		return toolsLevelOf(enforced), true
	}
}

// AutonomyEffective 一个工具**当下**的自治级别裁决（给控制台/接口解释用）。
type AutonomyEffective struct {
	Tool string `json:"tool"`
	Env  string `json:"env"`
	// Scope 信用维度键。
	Scope string `json:"scope"`
	// ConfiguredCeiling 运维配置的自治级别上限。
	ConfiguredCeiling string `json:"configured_ceiling"`
	// CreditLevel 信用侧的建议等级（原样，未加底线）；CreditKnown=false 时为空。
	CreditLevel string `json:"credit_level,omitempty"`
	// EnforcementLevel 信用侧用于放行判定的等级（= max(L1, Effective(credit))）。
	EnforcementLevel string `json:"enforcement_level,omitempty"`
	// Frozen 该 scope 是否已冻结。
	Frozen bool `json:"frozen"`
	// FrozenReason 冻结原因。
	FrozenReason string `json:"frozen_reason,omitempty"`
	// Effective 实际生效级别（= min(配置上限, EnforcementLevel)，与网关走同一条路径）。
	Effective string `json:"effective"`
	// Source 生效级别的来源：config（无历史/未启用）/ credit（信用生效）/ credit_capped（信用更宽，被上限夹住）。
	Source string `json:"source"`
	// CreditEnabled 进程是否装配了信用解析器（false 时信用不参与放行判定）。
	CreditEnabled bool `json:"credit_enabled"`
	// CreditKnown 是否查到了该 scope 的信用历史。
	CreditKnown bool `json:"credit_known"`
}

// EffectiveForTool 计算某工具的实际生效自治级别，并解释它为什么是这个值。
//
// effective 取自运行时的同一条计算路径（`rt.EffectiveAutonomyForTool`），
// 避免"解释"与"生效"两套口径；运行时不可用时按最保守的 L1 解释。
func (s *AutonomyService) EffectiveForTool(tool, env string) AutonomyEffective {
	tool = strings.TrimSpace(tool)
	if env = strings.TrimSpace(env); env == "" {
		// 留空 = 用进程配置的环境段（网关解析器用的是同一个值），避免两套口径。
		env = ConfiguredAutonomyEnv()
	}
	out := AutonomyEffective{Tool: tool, Env: env, Scope: AutonomyScope(tool, env)}

	ceiling := tools.AutonomyL1
	if rt := DefaultAgentRuntime(); rt != nil {
		ceiling = rt.Policy().Autonomy
		out.CreditEnabled = rt.AutonomyCreditEnabled()
		out.Effective = rt.EffectiveAutonomyForTool(tool).String()
	} else {
		out.Effective = ceiling.String()
	}
	out.ConfiguredCeiling = ceiling.String()
	out.Source = "config"

	raw, enforced, ok, frozen, row := resolveCredit(dbclient.GetMysqlDB(), out.Scope)
	if !ok {
		return out
	}
	out.CreditKnown = true
	out.CreditLevel = toolsLevelOf(raw).String()
	out.EnforcementLevel = toolsLevelOf(enforced).String()
	out.Frozen = frozen
	out.FrozenReason = row.FrozenReason
	if !out.CreditEnabled {
		// 有历史但进程未启用信用：放行仍按配置，解释要如实说明。
		return out
	}
	out.Source = "credit"
	if out.EnforcementLevel != out.Effective {
		// 信用想给的等级更宽，被配置上限夹住。
		out.Source = "credit_capped"
	}
	return out
}
