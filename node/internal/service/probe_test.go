package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// TestLoadOrCreateNodeUUIDIsStable 节点身份必须跨重启稳定。
//
// 这是 node_probe 通道与 node_metric 归集的前置条件：身份一变，
// 探测请求就发给一个永不存在的节点，水位也接不上。
func TestLoadOrCreateNodeUUIDIsStable(t *testing.T) {
	dir := t.TempDir()

	first, warn := LoadOrCreateNodeUUID(dir)
	if warn != "" {
		t.Fatalf("首次生成不应有告警: %s", warn)
	}
	if !nodeIDPattern.MatchString(first) {
		t.Fatalf("生成的不是标准 UUID: %q", first)
	}
	// 文件真的落盘了
	if _, err := os.Stat(filepath.Join(dir, nodeIDFileName)); err != nil {
		t.Fatalf("身份文件应存在: %v", err)
	}
	// 再次加载（模拟重启）必须一模一样
	second, warn2 := LoadOrCreateNodeUUID(dir)
	if warn2 != "" {
		t.Fatalf("已有身份不应有告警: %s", warn2)
	}
	if second != first {
		t.Fatalf("重启后身份变了：%q → %q", first, second)
	}
	// 末尾换行不影响
	raw, _ := os.ReadFile(filepath.Join(dir, nodeIDFileName))
	if strings.TrimSpace(string(raw)) != first {
		t.Fatalf("文件内容与返回值不一致: %q vs %q", string(raw), first)
	}
}

// TestLoadOrCreateNodeUUIDRegeneratesDirtyFile 脏身份文件必须重新生成并告警。
func TestLoadOrCreateNodeUUIDRegeneratesDirtyFile(t *testing.T) {
	dir := t.TempDir()
	cases := []string{"", "   ", "not-a-uuid", "12345678-1234-1234-1234-12345678901"} // 最后一例缺一位
	for _, dirty := range cases {
		if err := os.WriteFile(filepath.Join(dir, nodeIDFileName), []byte(dirty), 0o644); err != nil {
			t.Fatalf("准备脏文件失败: %v", err)
		}
		got, warn := LoadOrCreateNodeUUID(dir)
		if warn == "" {
			t.Fatalf("脏内容 %q 必须告警（不能静默换身份）", dirty)
		}
		if !nodeIDPattern.MatchString(got) {
			t.Fatalf("脏内容 %q 之后应生成标准 UUID，实际 %q", dirty, got)
		}
	}
}

// TestLoadOrCreateNodeUUIDUnwritableDir 无法持久化时必须**明确告警**而不是假装成功。
func TestLoadOrCreateNodeUUIDUnwritableDir(t *testing.T) {
	// 用一个"父路径是文件"的目录：MkdirAll 必然失败（跨平台稳定，
	// 不依赖 Unix 的 os.Getuid / 权限位）
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备阻塞文件失败: %v", err)
	}
	got, warn := LoadOrCreateNodeUUID(filepath.Join(blocker, "sub"))
	if warn == "" {
		t.Fatal("无法写盘时必须告警（否则重启后身份会变而无人知晓）")
	}
	if !nodeIDPattern.MatchString(got) {
		t.Fatalf("即使无法持久化也应返回可用 UUID，实际 %q", got)
	}
	if !strings.Contains(warn, "重启后会变化") {
		t.Fatalf("告警要说清后果，实际: %s", warn)
	}
}

// TestTruncateProbeOutputMarksTruncation 截断必须显式标注。
//
// 否则模型会把"被截断的输出"当成"就这么多"，从而断言"没有这个进程"——
// 又是一次捏造证据。
func TestTruncateProbeOutputMarksTruncation(t *testing.T) {
	// 不截断
	out, cut := truncateProbeOutput("hello", "", 1024)
	if cut || out != "hello" {
		t.Fatalf("短输出不应截断: %q cut=%v", out, cut)
	}
	// stderr 合并
	out, cut = truncateProbeOutput("ok", "warn", 1024)
	if cut || !strings.Contains(out, "[stderr]") || !strings.Contains(out, "warn") {
		t.Fatalf("stderr 应合并进输出: %q", out)
	}
	// 截断：头尾都要保留，且必须能看出被截断
	long := strings.Repeat("A", 300) + strings.Repeat("B", 300)
	out, cut = truncateProbeOutput(long, "", 100)
	if !cut {
		t.Fatalf("超长输出必须标记截断: len=%d", len(out))
	}
	if !strings.Contains(out, "中间省略") {
		t.Fatalf("截断标记缺失: %q", out)
	}
	if !strings.HasPrefix(out, "A") || !strings.HasSuffix(out, "B") {
		t.Fatalf("头尾都要保留（汇总行常在末尾）: %q", out)
	}
	// max <= 0 时用默认上限，不 panic
	if _, cut := truncateProbeOutput("x", "", 0); cut {
		t.Fatal("默认上限下短输出不应截断")
	}
}

// TestDecodeArgvRejectsGarbage 参数不是 argv 数组时必须失败（不能当成空参数执行）。
func TestDecodeArgvRejectsGarbage(t *testing.T) {
	if argv, err := decodeArgv(""); err != nil || len(argv) != 0 {
		t.Fatalf("空串应是空 argv: %v %v", argv, err)
	}
	if argv, err := decodeArgv(`["-ano"]`); err != nil || len(argv) != 1 || argv[0] != "-ano" {
		t.Fatalf("正常 argv 解析失败: %v %v", argv, err)
	}
	if argv, err := decodeArgv(`"netstat -ano"`); err == nil {
		// JSON 字符串（不是数组）必须报错，而不是被当成"一条参数"执行
		t.Fatalf("非数组必须报错，实际解析出 %v", argv)
	}
	if _, err := decodeArgv(`["-ano", 3]`); err == nil {
		t.Fatal("元素类型不对必须报错")
	}
	if _, err := decodeArgv(`{"a":1}`); err == nil {
		t.Fatal("对象不是合法的 argv")
	}
	if _, err := decodeArgv(`not json`); err == nil {
		t.Fatal("坏 JSON 必须报错")
	}
}

// TestProbeWorkerRequiresIdentity 没有身份就不该启动（否则会领取别的节点的请求）。
func TestProbeWorkerRequiresIdentity(t *testing.T) {
	if _, err := NewProbeWorker(nil, "node-1", "h"); err == nil {
		t.Fatal("没有数据库连接必须报错")
	}
	if _, err := NewProbeWorker(nil, "  ", "h"); err == nil {
		t.Fatal("没有 node_uuid 必须报错")
	}
}

// TestProbeExecuteRejectsNonWhitelistedCommand 节点侧必须**自己**再校验一次。
//
// 请求表在共享库里：任何能写库的人都能塞一条 argv。节点领取后不重新校验
// 就等于把"能写库"变成"能在所有节点上执行任意命令"。
func TestProbeExecuteRejectsNonWhitelistedCommand(t *testing.T) {
	cases := []struct {
		name string
		req  *models.NodeProbeRequest
	}{
		{name: "白名单外命令", req: &models.NodeProbeRequest{Command: "rm", ArgvJSON: `["-rf","/"]`}},
		{name: "写动作（sc stop）", req: &models.NodeProbeRequest{Command: "sc", ArgvJSON: `["stop","MySQL84"]`}},
		{name: "管道注入", req: &models.NodeProbeRequest{Command: "netstat", ArgvJSON: `["-ano","|","findstr","13307"]`}},
		{name: "参数不是数组", req: &models.NodeProbeRequest{Command: "netstat", ArgvJSON: `"netstat -ano"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每个子用例独立的"是否真的执行过"标记——共享变量会让前一个用例的
			// 执行痕迹污染后一个用例的断言（这个测试自身的 bug 也是被跑出来的）
			var called bool
			w := newTestProbeWorker(func(_ context.Context, _ string, _ []string) (string, string, int, error) {
				called = true
				return "", "", 0, nil
			})
			out := w.execute(context.Background(), tc.req)
			if out.Status != models.NodeProbeFailed {
				t.Fatalf("必须失败，实际 %s（output=%q）", out.Status, out.Output)
			}
			if out.ExitCode != -1 {
				t.Fatalf("未执行时退出码应为 -1，实际 %d", out.ExitCode)
			}
			if called {
				t.Fatal("被拒绝的命令**绝不能**真的执行")
			}
			if out.Error == "" {
				t.Fatal("失败必须带原因（否则报告里只有一句'失败'）")
			}
		})
	}
}

// TestProbeExecuteTimeoutIsExplicit 超时要落成 timeout 状态，而不是 done+半截输出。
func TestProbeExecuteTimeoutIsExplicit(t *testing.T) {
	w := newTestProbeWorker(func(ctx context.Context, _ string, _ []string) (string, string, int, error) {
		<-ctx.Done()
		return "partial", "", -1, ctx.Err()
	})
	w.maxTimeout = 50 * time.Millisecond
	out := w.execute(context.Background(), &models.NodeProbeRequest{
		Command: "netstat", ArgvJSON: `["-ano"]`, TimeoutSeconds: 30, // 请求 30s，被上限压到 50ms
	})
	if out.Status != models.NodeProbeTimedOut {
		t.Fatalf("应标记超时，实际 %s", out.Status)
	}
	if !strings.Contains(out.Error, "执行超时") {
		t.Fatalf("超时原因要写清: %s", out.Error)
	}
	if !strings.Contains(out.Output, "partial") {
		t.Fatalf("超时前拿到的输出要保留（可能已含关键证据）: %q", out.Output)
	}
	if !strings.Contains(out.Output, "win-live-host") {
		t.Fatalf("输出应带节点来源: %q", out.Output)
	}
}

// TestProbeExecuteNonZeroExitIsStillEvidence 命令非 0 退出仍是**有效证据**。
//
// tasklist 找不到匹配进程会返回 1；把它当成"探测失败"会让模型丢掉
// "这个进程确实不在"这条关键证据，于是又回到猜。
func TestProbeExecuteNonZeroExitIsStillEvidence(t *testing.T) {
	w := newTestProbeWorker(func(_ context.Context, _ string, _ []string) (string, string, int, error) {
		return "INFO: No tasks are running which match the specified criteria.", "", 1, nil
	})
	out := w.execute(context.Background(), &models.NodeProbeRequest{
		Command: "tasklist", ArgvJSON: `["/fi","imagename eq mysqld.exe"]`, TimeoutSeconds: 10,
	})
	if out.Status != models.NodeProbeDone {
		t.Fatalf("非 0 退出但执行成功应是 done，实际 %s（%s）", out.Status, out.Error)
	}
	if out.ExitCode != 1 {
		t.Fatalf("退出码要如实记录，实际 %d", out.ExitCode)
	}
	if !strings.Contains(out.Output, "No tasks are running") {
		t.Fatalf("输出必须保留: %q", out.Output)
	}
}

// TestProbeOutputIncludesNodeProvenance 输出里必须带节点来源信息。
//
// 证据链要能回答"这条输出是哪台机器上跑的、跑的什么命令"，
// 否则模型（和读报告的人）无法判断这条证据是否属于目标节点。
func TestProbeOutputIncludesNodeProvenance(t *testing.T) {
	w := newTestProbeWorker(func(_ context.Context, _ string, _ []string) (string, string, int, error) {
		return "TCP 0.0.0.0:13307 LISTENING", "", 0, nil
	})
	out := w.execute(context.Background(), &models.NodeProbeRequest{
		Command: "netstat", ArgvJSON: `["-ano"]`, TimeoutSeconds: 10,
	})
	if out.Status != models.NodeProbeDone || out.Error != "" || out.ExitCode != 0 {
		t.Fatalf("应成功: status=%s err=%s code=%d", out.Status, out.Error, out.ExitCode)
	}
	if !strings.Contains(out.Output, "win-live-host") || !strings.Contains(out.Output, "netstat") {
		t.Fatalf("输出缺少节点/命令来源信息: %q", out.Output)
	}
	if !strings.Contains(out.Output, "13307") {
		t.Fatalf("输出应包含命令真实输出: %q", out.Output)
	}
}

// newTestProbeWorker 构造一个不连库的探测执行器（注入假执行器）。
func newTestProbeWorker(runner tools.ShellRunner) *ProbeWorker {
	return &ProbeWorker{
		nodeUUID:   "node-it-01",
		hostname:   "win-live-host",
		policy:     tools.DefaultPolicy(),
		runner:     runner,
		maxTimeout: 20 * time.Second,
	}
}
