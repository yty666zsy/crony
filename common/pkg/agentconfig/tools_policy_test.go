package agentconfig

import (
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// TestToolsPolicyDefaultsAreConservative：空配置必须得到"关着"的一档。
//
// 这条是阶段 4 的安全底线测试：配置段缺失（老配置文件）时，
// 行为必须是只读 + L1，而不能因为"字段没填"退化成放开写。
func TestToolsPolicyDefaultsAreConservative(t *testing.T) {
	p, err := ToolsPolicyFromConfig(models.Tools{})
	if err != nil {
		t.Fatalf("空配置不应报错: %v", err)
	}
	if p.AllowWrite || p.AllowDestructive {
		t.Fatalf("空配置不得放开写动作: allow_write=%v allow_destructive=%v", p.AllowWrite, p.AllowDestructive)
	}
	if p.Autonomy != tools.AutonomyL1 {
		t.Fatalf("空配置自治级别应为 L1，实际 %s", p.Autonomy)
	}
	if len(p.AutoApprove) != 0 {
		t.Fatalf("空配置不得有免审批白名单: %v", p.AutoApprove)
	}
	if p.MaxCallsPerSession != 12 || p.MaxCallsPerTool != 4 || p.SQLMaxRows != 100 ||
		p.MaxOutputBytes != tools.DefaultMaxOutputBytes {
		t.Fatalf("数值默认值与 DefaultPolicy 不一致: %#v", p)
	}
	if p.MaxCallsPerMinute != 60 || p.MaxConcurrentCalls != 4 ||
		p.MaxWriteActionsPerSession != 2 || p.MaxAffectedHostsPerSession != 1 {
		t.Fatalf("阶段 4 新增默认值与策略不一致: %#v", p)
	}
	if p.ApprovalTTL != 15*time.Minute {
		t.Fatalf("审批有效期默认应为 15m，实际 %s", p.ApprovalTTL)
	}
	if p.DefaultTimeout != 15*time.Second {
		t.Fatalf("默认超时应为 15s，实际 %s", p.DefaultTimeout)
	}
	if len(p.ShellReadPaths) == 0 {
		t.Fatal("空配置应补齐可读路径白名单（空白名单会被理解成「什么都不能读」或「什么都能读」，两种都不是本意）")
	}
}

// TestToolsPolicyOverrides：显式配置逐项生效。
func TestToolsPolicyOverrides(t *testing.T) {
	cfg := models.Tools{
		AllowWrite:                 true,
		AllowDestructive:           false,
		Autonomy:                   "L2",
		AutoApprove:                []string{"job_retry@node-it-01"},
		ApprovalTTLSeconds:         60,
		MaxCallsPerSession:         5,
		MaxCallsPerTool:            2,
		MaxCallsPerMinute:          10,
		MaxConcurrentCalls:         1,
		MaxWriteActionsPerSession:  1,
		MaxAffectedHostsPerSession: 3,
		DefaultTimeoutSeconds:      5,
		MaxOutputBytes:             1024,
		AllowHosts:                 []string{"example.com"},
		ShellReadPaths:             []string{"/tmp/"},
		SQLMaxRows:                 7,
		Deny:                       []string{"http_get"},
		Allow:                      []string{"job_retry", "shell_readonly"},
	}
	p, err := ToolsPolicyFromConfig(cfg)
	if err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if !p.AllowWrite || p.AllowDestructive {
		t.Fatalf("写开关不符: %#v", p)
	}
	if p.Autonomy != tools.AutonomyL2 {
		t.Fatalf("自治级别不符: %s", p.Autonomy)
	}
	if len(p.AutoApprove) != 1 || p.AutoApprove[0] != "job_retry@node-it-01" {
		t.Fatalf("白名单不符: %v", p.AutoApprove)
	}
	if p.ApprovalTTL != time.Minute || p.DefaultTimeout != 5*time.Second {
		t.Fatalf("时长换算不符: ttl=%s timeout=%s", p.ApprovalTTL, p.DefaultTimeout)
	}
	if p.MaxCallsPerSession != 5 || p.MaxCallsPerTool != 2 || p.MaxCallsPerMinute != 10 ||
		p.MaxConcurrentCalls != 1 || p.MaxWriteActionsPerSession != 1 ||
		p.MaxAffectedHostsPerSession != 3 || p.MaxOutputBytes != 1024 || p.SQLMaxRows != 7 {
		t.Fatalf("数值覆盖不符: %#v", p)
	}
	if len(p.AllowHosts) != 1 || p.AllowHosts[0] != "example.com" {
		t.Fatalf("出网白名单不符: %v", p.AllowHosts)
	}
	if len(p.ShellReadPaths) != 1 || p.ShellReadPaths[0] != "/tmp/" {
		t.Fatalf("可读路径不符: %v", p.ShellReadPaths)
	}
	// allow/deny 的**可观测效果**：暴露给模型的工具清单被过滤。
	// 只断言字段是不够的——allow/deny 的价值全在"模型到底看得见哪些工具"。
	reg, err := tools.NewReadonlyRegistry(p, tools.Sources{})
	if err != nil {
		t.Fatalf("按策略装配只读工具集失败: %v", err)
	}
	allowed := reg.AllowedNames()
	if len(allowed) != 1 || allowed[0] != "shell_readonly" {
		t.Fatalf("allow/deny 过滤不符：允许=%v（期望只有 shell_readonly）", allowed)
	}
	if specs := reg.Specs(); len(specs) != 1 || specs[0].Name != "shell_readonly" {
		t.Fatalf("暴露给模型的工具声明应只剩 1 个，实际 %d 个", len(specs))
	}

	// 拷贝语义：改配置切片不能反过来改策略（否则运行期策略会被调用方意外改写）。
	cfg.AllowHosts[0] = "evil.com"
	cfg.AutoApprove[0] = "x"
	if p.AllowHosts[0] != "example.com" || p.AutoApprove[0] != "job_retry@node-it-01" {
		t.Fatal("策略必须持有配置切片的副本，不能被外部改动污染")
	}
}

// TestToolsPolicyRejectsContradictions：自相矛盾的配置必须在启动时就报错。
//
// 为什么不"按更严格的一档静默理解"：配置者以为自己在放开写动作，
// 实际写工具根本没注册进来——这种"配了却没生效"的落差会变成线上查不出的谜。
func TestToolsPolicyRejectsContradictions(t *testing.T) {
	cases := []struct {
		name string
		cfg  models.Tools
	}{
		{"非法自治级别", models.Tools{Autonomy: "L9", AllowWrite: true}},
		{"空写开关配 L2", models.Tools{Autonomy: "L2"}},
		{"空写开关配 L3", models.Tools{Autonomy: "L3"}},
		{"空写开关配白名单", models.Tools{AutoApprove: []string{"job_retry"}}},
	}
	for _, c := range cases {
		if _, err := ToolsPolicyFromConfig(c.cfg); err == nil {
			t.Fatalf("%s：应当报错而不是静默降级", c.name)
		}
	}
}

// TestToolsPolicyDestructiveImpliesWrite：只开 destructive 时按"都得打开"理解。
func TestToolsPolicyDestructiveImpliesWrite(t *testing.T) {
	p, err := ToolsPolicyFromConfig(models.Tools{AllowDestructive: true, Autonomy: "L3"})
	if err != nil {
		t.Fatalf("AllowDestructive=true + L3 应合法: %v", err)
	}
	if !p.AllowWrite || !p.AllowDestructive {
		t.Fatalf("destructive 应隐含 write: %#v", p)
	}
}