package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 阶段 K4：k8s 边界配置必须能**只靠环境变量**注入。
//
// 为什么值得单独一个测试：容器化部署（Compose / Kubernetes）里配置一律走
// ConfigMap/环境变量，而 allow-images 是 []string。viper 的 Set(key, "a,b")
// 对 mapstructure 解成 []string 的行为不是显然的（弱类型转换规则容易变），
// 一旦这里静默变成 nil，最危险的后果是**静默放开**（空 = 不允许任何镜像，
// 看似"更安全"）或**静默拒绝一切**（任务永远起不来，但错误信息指向别处）。
func TestEnvOverridesK8sListAndScalars(t *testing.T) {
	dir := t.TempDir()
	confDir := filepath.Join(dir, "admin", "conf", "testing")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("建配置目录失败: %v", err)
	}
	// 极简配置文件：只证明"文件里没写的键也能靠环境变量注入"。
	if err := os.WriteFile(filepath.Join(confDir, "main.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("写配置文件失败: %v", err)
	}

	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir 失败: %v", err)
	}
	defer os.Chdir(old)

	t.Setenv("CRONY_K8S_ENABLED", "true")
	t.Setenv("CRONY_K8S_NAMESPACE", "crony-jobs")
	t.Setenv("CRONY_K8S_ALLOW_IMAGES", "busybox*,alpine:3.20")
	t.Setenv("CRONY_K8S_ALLOW_NAMESPACES", "crony-jobs,staging")
	t.Setenv("CRONY_K8S_TTL_SECONDS_AFTER_FINISHED", "900")

	c, err := LoadConfig("testing", "admin", "main")
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if !c.K8s.Enabled {
		t.Fatalf("k8s.enabled 没从环境变量注入")
	}
	if c.K8s.Namespace != "crony-jobs" {
		t.Fatalf("k8s.namespace 注入错误: %q", c.K8s.Namespace)
	}
	if len(c.K8s.AllowImages) != 2 || c.K8s.AllowImages[0] != "busybox*" || c.K8s.AllowImages[1] != "alpine:3.20" {
		t.Fatalf("k8s.allow-images 应按逗号切分成 2 项，实际 %#v", c.K8s.AllowImages)
	}
	if len(c.K8s.AllowNamespaces) != 2 || c.K8s.AllowNamespaces[1] != "staging" {
		t.Fatalf("k8s.allow-namespaces 应按逗号切分，实际 %#v", c.K8s.AllowNamespaces)
	}
	if c.K8s.TTLSecondsAfterFinished != 900 {
		t.Fatalf("k8s.ttl-seconds-after-finished 注入错误: %d", c.K8s.TTLSecondsAfterFinished)
	}
}
