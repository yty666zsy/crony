package config

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tmnhs/crony/common/models"
)

// TestApplyEnvOverrides 覆盖"环境变量覆盖配置文件"这条链路，
// 这是"密钥/口令不落配置文件"这一安全设计的基础能力。
//
// 同时验证类型转换：字符串 -> int、字符串 -> []string（逗号分隔）。
func TestApplyEnvOverrides(t *testing.T) {
	const raw = `{
	  "mysql": {"path": "127.0.0.1", "port": "3306", "password": "root"},
	  "etcd": {"endpoints": ["http://127.0.0.1:2379"]},
	  "system": {"addr": 8089, "env": "production"},
	  "auth": {"jwt-secret": "", "allow-public-register": false}
	}`
	v := viper.New()
	v.SetConfigType("json")
	if err := v.ReadConfig(strings.NewReader(raw)); err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}

	t.Setenv(EnvPrefix+"_MYSQL_PATH", "mysql")
	t.Setenv(EnvPrefix+"_MYSQL_PASSWORD", "s3cr3t-from-env")
	t.Setenv(EnvPrefix+"_ETCD_ENDPOINTS", "http://etcd:2379")
	t.Setenv(EnvPrefix+"_SYSTEM_ADDR", "9090")
	t.Setenv(EnvPrefix+"_AUTH_JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv(EnvPrefix+"_AUTH_ALLOW_PUBLIC_REGISTER", "true")

	applyEnvOverrides(v)

	var c models.Config
	if err := v.Unmarshal(&c); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if c.Mysql.Path != "mysql" {
		t.Errorf("mysql.path = %q, want %q", c.Mysql.Path, "mysql")
	}
	if c.Mysql.Password != "s3cr3t-from-env" {
		t.Errorf("mysql.password 未被环境变量覆盖")
	}
	if c.Mysql.Port != "3306" {
		t.Errorf("mysql.port = %q, want 配置文件值 3306", c.Mysql.Port)
	}
	if len(c.Etcd.Endpoints) != 1 || c.Etcd.Endpoints[0] != "http://etcd:2379" {
		t.Errorf("etcd.endpoints = %v, want [http://etcd:2379]（字符串转切片）", c.Etcd.Endpoints)
	}
	if c.System.Addr != 9090 {
		t.Errorf("system.addr = %d, want 9090（字符串转 int）", c.System.Addr)
	}
	if c.Auth.JwtSecret != "0123456789abcdef0123456789abcdef" {
		t.Errorf("auth.jwt-secret 未被环境变量注入")
	}
	if !c.Auth.AllowPublicRegister {
		t.Errorf("auth.allow-public-register = false, want true（字符串转 bool）")
	}
}

// TestApplyEnvOverridesWithoutConfigKeys 验证"配置文件里没有 auth 块"
// （老版本配置文件）时，auth.* 仍然可以通过环境变量注入：
// 依赖 registerDefaults 把键注册进 viper。
func TestApplyEnvOverridesWithoutConfigKeys(t *testing.T) {
	const legacyRaw = `{"mysql": {"path": "127.0.0.1"}}`
	v := viper.New()
	v.SetConfigType("json")
	if err := v.ReadConfig(strings.NewReader(legacyRaw)); err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	t.Setenv(EnvPrefix+"_AUTH_JWT_SECRET", "abcdef0123456789abcdef0123456789")
	t.Setenv(EnvPrefix+"_AUTH_BOOTSTRAP_ADMIN_USER", "ops-admin")

	applyEnvOverrides(v)

	var c models.Config
	if err := v.Unmarshal(&c); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if c.Auth.JwtSecret != "abcdef0123456789abcdef0123456789" {
		t.Errorf("旧配置文件下 auth.jwt-secret 仍应可注入")
	}
	if c.Auth.BootstrapAdminUser != "ops-admin" {
		t.Errorf("auth.bootstrap-admin-user = %q, want ops-admin", c.Auth.BootstrapAdminUser)
	}
	if c.Auth.TokenExpireHours != 168 {
		t.Errorf("auth.token-expire-hours = %d, want 默认值 168", c.Auth.TokenExpireHours)
	}
}

// TestPrintConfigSummaryDoesNotLeakSecrets 保证配置摘要不打印明文口令。
func TestPrintConfigSummaryDoesNotLeakSecrets(t *testing.T) {
	c := &models.Config{}
	c.Mysql.Password = "super-secret-password"
	c.Etcd.Password = "etcd-secret"
	c.Email.Secret = "smtp-secret"
	c.Auth.JwtSecret = "jwt-secret-value"

	out := captureStdout(t, func() { printConfigSummary(c) })
	for _, secret := range []string{"super-secret-password", "etcd-secret", "smtp-secret", "jwt-secret-value"} {
		if strings.Contains(out, secret) {
			t.Fatalf("配置摘要泄漏了明文密钥：%q", secret)
		}
	}
	if !strings.Contains(out, "<set>") {
		t.Fatalf("配置摘要应显示密钥已设置，实际输出：%q", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = original }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe error = %v", err)
	}
	buf := make([]byte, 4096)
	n, err := r.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read pipe error = %v", err)
	}
	return string(buf[:n])
}
