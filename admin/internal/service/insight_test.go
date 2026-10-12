package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
)

// 阶段 5 补全：概览/健康度/配置快照的纯函数单元测试。
// 依赖数据库的部分由黑盒验收覆盖；这里钉死"聚合与格式化"这类容易出错的确定性逻辑。

func TestAggregateHealthStatus(t *testing.T) {
	cases := []struct {
		name  string
		comps []HealthComponent
		want  string
	}{
		{"全 ok", []HealthComponent{{Status: HealthOK}, {Status: HealthOK}}, HealthOK},
		{"含 degraded", []HealthComponent{{Status: HealthOK}, {Status: HealthDegraded}}, HealthDegraded},
		{"down 压过 degraded", []HealthComponent{{Status: HealthDegraded}, {Status: HealthDown}, {Status: HealthOK}}, HealthDown},
		{"planned 不算故障", []HealthComponent{{Status: HealthOK}, {Status: HealthPlanned}}, HealthOK},
		{"空列表默认 ok", nil, HealthOK},
	}
	for _, c := range cases {
		if got := aggregateHealthStatus(c.comps); got != c.want {
			t.Fatalf("%s: 期望 %s，实际 %s", c.name, c.want, got)
		}
	}
}

func TestUptimeText(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m0s"},
		{90 * time.Minute, "1h30m"},
		{26 * time.Hour, "26h0m"},
	}
	for _, c := range cases {
		if got := uptimeText(c.d); got != c.want {
			t.Fatalf("uptimeText(%s) = %q，期望 %q", c.d, got, c.want)
		}
	}
}

func TestRound1(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{96.25, 96.3}, {0, 0}, {100, 100}, {33.3333, 33.3}, {66.66, 66.7},
	}
	for _, c := range cases {
		if got := round1(c.in); got != c.want {
			t.Fatalf("round1(%v) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// 鉴权快照必须暴露前端读取的字段，且**绝不**包含密钥/口令原文。
func TestAuthSnapshotExposesExpectedKeysAndNoSecrets(t *testing.T) {
	const secret = "SUPER-SECRET-JWT-KEY-should-not-leak"
	const pw = "BOOTSTRAP-PASSWORD-should-not-leak"
	cfg := &models.Config{}
	cfg.Auth.JwtSecret = secret
	cfg.Auth.BootstrapAdminUser = "ops-admin"
	cfg.Auth.BootstrapAdminPassword = pw
	cfg.Auth.TokenExpireHours = 168

	snap := authSnapshot(cfg)
	// 前端 SettingsView 读取的字段名必须齐备（否则页面显示空白）。
	for _, key := range []string{"jwt_issuer", "jwt_ttl_hours", "jwt_secret_source",
		"password_min_length", "force_password_change", "rbac", "bootstrap_admin"} {
		if _, ok := snap[key]; !ok {
			t.Fatalf("鉴权快照缺字段 %q（会导致页面空白）: %+v", key, snap)
		}
	}
	if snap["jwt_issuer"] != models.JwtIssuer {
		t.Fatalf("jwt_issuer 应为真实签发方 %q，实际 %v", models.JwtIssuer, snap["jwt_issuer"])
	}
	if got := fmt.Sprintf("%v", snap["jwt_ttl_hours"]); got != "168" {
		t.Fatalf("jwt_ttl_hours 应为配置值 168，实际 %v", snap["jwt_ttl_hours"])
	}
	if s, _ := snap["jwt_secret_source"].(string); !strings.HasPrefix(s, "已配置") {
		t.Fatalf("已配置密钥时应说明来源，实际 %q", s)
	}
	if b, _ := snap["bootstrap_admin"].(string); !strings.Contains(b, "ops-admin") {
		t.Fatalf("已配置初始管理员时应点名用户，实际 %q", b)
	}

	// 序列化后不得出现任何密钥/口令原文。
	blob, _ := json.Marshal(snap)
	if strings.Contains(string(blob), secret) || strings.Contains(string(blob), pw) {
		t.Fatalf("鉴权快照泄露了密钥/口令原文: %s", blob)
	}
}

// 未配置密钥/初始管理员时，必须如实说明"未配置"，不能假装已配置。
func TestAuthSnapshotHonestWhenUnset(t *testing.T) {
	snap := authSnapshot(&models.Config{})
	if s, _ := snap["jwt_secret_source"].(string); !strings.HasPrefix(s, "未配置") {
		t.Fatalf("未配置密钥时应如实说明，实际 %q", s)
	}
	if b, _ := snap["bootstrap_admin"].(string); !strings.HasPrefix(b, "未配置") {
		t.Fatalf("未配置初始管理员时应如实说明，实际 %q", b)
	}
}

func TestSchedulerSnapshotKeys(t *testing.T) {
	snap := schedulerSnapshot(&models.Config{})
	for _, key := range []string{"default_concurrency_policy", "default_retry_times",
		"default_retry_interval", "max_retry_times", "default_timeout_seconds", "default_allocation"} {
		if _, ok := snap[key]; !ok {
			t.Fatalf("调度快照缺字段 %q（会导致页面空白）: %+v", key, snap)
		}
	}
	if snap["max_retry_times"] != models.MaxRetryTimes {
		t.Fatalf("max_retry_times 应为 %d，实际 %v", models.MaxRetryTimes, snap["max_retry_times"])
	}
}

// 通知渠道只依据"是否配置了地址"判断，且不泄露任何地址内容。
func TestNotifyChannelsHonest(t *testing.T) {
	empty := notifyChannels(&models.Config{})
	if len(empty) != 2 {
		t.Fatalf("应报告 webhook/email 两个渠道，实际 %d", len(empty))
	}
	for _, ch := range empty {
		if ch["enabled"] != false {
			t.Fatalf("空配置下渠道应全部为未启用: %+v", ch)
		}
	}
	cfg := &models.Config{}
	cfg.WebHook.Kind = "feishu"
	cfg.WebHook.Url = "https://example.invalid/hook"
	got := notifyChannels(cfg)
	if got[0]["enabled"] != true || got[0]["label"] != "飞书机器人" {
		t.Fatalf("配置了飞书 webhook 应启用并显示对应标签: %+v", got[0])
	}
	if s, ok := got[0]["detail"]; ok {
		t.Fatalf("渠道信息不应包含地址明细（避免泄露）: %v", s)
	}
}
