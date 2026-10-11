package agentconfig

import (
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// ToolsPolicyFromConfig 把配置里的 `tools` 段转成工具网关策略。
//
// 为什么要有这个函数（而不是各装配点自己拼）：
// 阶段 2/3 的默认策略被 7 个调用点各自 `tools.DefaultPolicy()` 复制，
// 结果是同一个语义出现两个值（只读 SQL 行数上限网关侧 100、执行路径 200）。
// 现在**只有这一个转换点**，"配置 → 生效策略"是一条可测试的单向路径。
//
// 无论配置怎么写，都无法放宽到策略自身默认值之外的安全底线：
//   - destructive 永远需要审批（由 tools 包的 autoApproveMatch 强制，不在这里"配置"）；
//   - 自治级别无法用非法值绕过（非法值返回错误，而不是退回默认值——静默退回
//     会让"我配了 L3 为什么没生效/我配了 L0 为什么还在动"变成一个查不出来的谜）。
func ToolsPolicyFromConfig(cfg models.Tools) (tools.Policy, error) {
	p := tools.DefaultPolicy()

	autonomy := tools.AutonomyL1
	if s := strings.TrimSpace(cfg.Autonomy); s != "" {
		parsed, err := tools.ParseAutonomyLevel(s)
		if err != nil {
			return p, err
		}
		autonomy = parsed
	}

	// 自相矛盾的配置必须报错，不能"默默按更宽的那一档理解"：
	// 配了 allow-write=false + autonomy=L3，说明配置者以为自己在放行 write，
	// 实际写工具注册不进来——这种"配了却没生效"的落差必须在启动时就喊出来。
	//
	// 判定用的是**生效后的**写开关：Policy.Normalize 里 destructive 隐含 write
	// （只开 destructive 不开 write 时按"都得显式打开"理解），
	// 所以这里不能只看 cfg.AllowWrite，否则
	// `allow-destructive=true + autonomy=L3` 这个合法组合会被误判成矛盾。
	allowWrite := cfg.AllowWrite || cfg.AllowDestructive
	if !allowWrite && (autonomy == tools.AutonomyL2 || autonomy == tools.AutonomyL3) {
		return p, fmt.Errorf("tools: allow-write=false 与 autonomy=%s 自相矛盾（L2/L3 会自动放行写动作，但写工具根本没注册进来）", autonomy)
	}
	if !allowWrite && len(cfg.AutoApprove) > 0 {
		return p, fmt.Errorf("tools: allow-write=false 时 auto-approve 白名单没有意义（写工具未开启）")
	}

	p.AllowWrite = cfg.AllowWrite
	p.AllowDestructive = cfg.AllowDestructive
	p.Autonomy = autonomy
	p.AutoApprove = append([]string(nil), cfg.AutoApprove...)
	p.Allow = append([]string(nil), cfg.Allow...)
	p.Deny = append([]string(nil), cfg.Deny...)
	p.AllowHosts = append([]string(nil), cfg.AllowHosts...)
	p.ShellReadPaths = append([]string(nil), cfg.ShellReadPaths...)

	if cfg.ApprovalTTLSeconds > 0 {
		p.ApprovalTTL = time.Duration(cfg.ApprovalTTLSeconds) * time.Second
	}
	if cfg.MaxCallsPerSession > 0 {
		p.MaxCallsPerSession = cfg.MaxCallsPerSession
	}
	if cfg.MaxCallsPerTool > 0 {
		p.MaxCallsPerTool = cfg.MaxCallsPerTool
	}
	if cfg.MaxCallsPerMinute > 0 {
		p.MaxCallsPerMinute = cfg.MaxCallsPerMinute
	}
	if cfg.MaxConcurrentCalls > 0 {
		p.MaxConcurrentCalls = cfg.MaxConcurrentCalls
	}
	if cfg.MaxWriteActionsPerSession > 0 {
		p.MaxWriteActionsPerSession = cfg.MaxWriteActionsPerSession
	}
	if cfg.MaxAffectedHostsPerSession > 0 {
		p.MaxAffectedHostsPerSession = cfg.MaxAffectedHostsPerSession
	}
	if cfg.DefaultTimeoutSeconds > 0 {
		p.DefaultTimeout = time.Duration(cfg.DefaultTimeoutSeconds) * time.Second
	}
	if cfg.MaxOutputBytes > 0 {
		p.MaxOutputBytes = cfg.MaxOutputBytes
	}
	if cfg.SQLMaxRows > 0 {
		p.SQLMaxRows = cfg.SQLMaxRows
	}
	return p.Normalize(), nil
}