package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/retry"
)

func shellRequest(command string) Request {
	return Request{
		RunID:     "j1-t100",
		JobID:     1,
		StepIndex: 0,
		Name:      "step-0",
		Kind:      KindShell,
		Command:   command,
		Timeout:   10 * time.Second,
	}
}

// sleepCommand 返回一个"能睡一小会儿"的平台命令，用于超时/取消测试。
func sleepCommand() string {
	if runtime.GOOS == "windows" {
		return "ping -n 20 127.0.0.1 > NUL"
	}
	return "sleep 20"
}

func TestShellExecutorSuccess(t *testing.T) {
	e := NewShellExecutor()
	res := e.Execute(context.Background(), shellRequest("echo crony-agent"))
	if res.Status != models.RunStatusSuccess {
		t.Fatalf("Status = %s, err = %v, output = %q", res.Status, res.Err, res.Output)
	}
	if !strings.Contains(res.Output, "crony-agent") {
		t.Fatalf("输出未包含预期内容: %q", res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Duration <= 0 {
		t.Fatal("必须记录耗时（阶段 1 的结构化产物）")
	}
	if res.Artifacts["pid"] == "" {
		t.Fatal("必须记录子进程 pid，否则无法做取消传播/进程审计")
	}
}

// TestShellExecutorArgvMode 兼容旧任务：Args 非空时按 argv 直接执行，不再经 shell 解析。
func TestShellExecutorArgvMode(t *testing.T) {
	e := NewShellExecutor()
	req := shellRequest("")
	if runtime.GOOS == "windows" {
		req.Command = "cmd.exe"
		req.Args = []string{"/C", "echo argv-mode"}
	} else {
		req.Command = "sh"
		req.Args = []string{"-c", "echo argv-mode"}
	}
	res := e.Execute(context.Background(), req)
	if res.Status != models.RunStatusSuccess || !strings.Contains(res.Output, "argv-mode") {
		t.Fatalf("argv 模式执行失败: status=%s output=%q err=%v", res.Status, res.Output, res.Err)
	}
}

// TestShellExecutorClassifiesExitCodes 固化"可重试/不可重试"的退出码分类。
func TestShellExecutorClassifiesExitCodes(t *testing.T) {
	cases := []struct {
		exitCode      int
		wantRetryable bool
	}{
		{1, true},
		{2, false},    // 用法错误
		{126, false},  // 不可执行
		{127, false},  // 命令不存在（Unix）
		{9009, false}, // 命令不存在（Windows cmd）
		{137, true},   // 被 OOM/信号杀死，重试可能成功
		{143, true},
	}
	for _, tc := range cases {
		err := classifyExitCode(tc.exitCode, "boom")
		got := retry.Classify(err).Retryable
		if got != tc.wantRetryable {
			t.Errorf("退出码 %d: retryable = %v, want %v", tc.exitCode, got, tc.wantRetryable)
		}
	}
}

func TestShellExecutorNonZeroExitIsFailure(t *testing.T) {
	e := NewShellExecutor()
	res := e.Execute(context.Background(), shellRequest("exit 3"))
	if res.Status != models.RunStatusFailed {
		t.Fatalf("Status = %s, want failed（err=%v）", res.Status, res.Err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestShellExecutorTimeout(t *testing.T) {
	e := NewShellExecutor()
	req := shellRequest(sleepCommand())
	req.Timeout = 500 * time.Millisecond
	start := time.Now()
	res := e.Execute(context.Background(), req)
	elapsed := time.Since(start)
	if res.Status != models.RunStatusTimeout {
		t.Fatalf("Status = %s, want timeout（err=%v）", res.Status, res.Err)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("超时后 %v 才返回：进程树终止/兜底等待失效", elapsed)
	}
	if retry.Classify(res.Err).Retryable != true {
		t.Error("超时应归类为可重试（瞬时变慢），由上层总时长预算兜底")
	}
}

// TestShellExecutorCancelKillsProcessTree 取消传播：
// ctx 取消后必须尽快返回 canceled，且子进程被真正终止（不能留孤儿进程）。
func TestShellExecutorCancelKillsProcessTree(t *testing.T) {
	e := NewShellExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res := e.Execute(ctx, shellRequest(sleepCommand()))
	elapsed := time.Since(start)
	if res.Status != models.RunStatusCanceled {
		t.Fatalf("Status = %s, want canceled（err=%v）", res.Status, res.Err)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("取消后 %v 才返回，说明进程树未被终止", elapsed)
	}
	if retry.Classify(res.Err).Retryable {
		t.Error("被主动取消的执行不得重试")
	}
}

// TestShellExecutorTruncatesOutput 输出上限：防止一条死循环命令打满内存与数据库。
func TestShellExecutorTruncatesOutput(t *testing.T) {
	e := NewShellExecutor()
	req := shellRequest("echo " + strings.Repeat("A", 2000))
	req.MaxOutputBytes = 128
	res := e.Execute(context.Background(), req)
	if res.Status != models.RunStatusSuccess {
		t.Fatalf("Status = %s, err = %v", res.Status, res.Err)
	}
	if !res.Truncated {
		t.Fatal("超出上限的输出必须标记 Truncated")
	}
	if len(res.Output) > 128+64 {
		t.Fatalf("输出长度 %d 未受上限约束", len(res.Output))
	}
}

func TestShellExecutorRejectsInvalidRequest(t *testing.T) {
	e := NewShellExecutor()
	res := e.Execute(context.Background(), Request{RunID: "j1-t1", Kind: KindShell})
	if res.Status != models.RunStatusFailed {
		t.Fatalf("Status = %s, want failed", res.Status)
	}
	if retry.Classify(res.Err).Retryable {
		t.Error("参数不合法属于配置错误，不应重试")
	}
}

// TestRegistryRunValidatesAndDispatches 注册表的职责边界：
// 未注册的类型/非法请求都返回结构化失败，而不是 panic 或静默成功。
func TestRegistryRunValidatesAndDispatches(t *testing.T) {
	registry := DefaultRegistry()
	if kinds := registry.Kinds(); len(kinds) != 2 || kinds[0] != KindHTTP || kinds[1] != KindShell {
		t.Fatalf("默认注册表应包含 http/shell（有序），实际 %v", kinds)
	}
	if _, ok := KindOf("SHELL"); !ok {
		t.Error("KindOf 应大小写不敏感")
	}
	if _, ok := KindOf("container"); ok {
		t.Error("尚未落地的执行器类型不应被解析成功")
	}

	res := registry.Run(context.Background(), Request{RunID: "j1-t1", Kind: Kind("docker")})
	if res.Status != models.RunStatusFailed {
		t.Fatalf("未注册类型应失败，实际 %s", res.Status)
	}
	res = registry.Run(context.Background(), Request{Kind: KindShell, Command: "echo hi"})
	if res.Status != models.RunStatusFailed || !strings.Contains(res.Error(), "run_id") {
		t.Fatalf("缺少 run_id 应被拒绝，实际 status=%s err=%s", res.Status, res.Error())
	}
	res = registry.Run(context.Background(), Request{RunID: "j1-t1", Kind: KindHTTP})
	if res.Status != models.RunStatusFailed || !strings.Contains(res.Error(), "url") {
		t.Fatalf("缺少 url 应被拒绝，实际 status=%s err=%s", res.Status, res.Error())
	}
}

func TestHTTPExecutorStatusClassification(t *testing.T) {
	cases := []struct {
		status        int
		wantStatus    models.RunStatus
		wantRetryable bool
	}{
		{200, models.RunStatusSuccess, false},
		{204, models.RunStatusSuccess, false},
		{400, models.RunStatusFailed, false},
		{401, models.RunStatusFailed, false},
		{404, models.RunStatusFailed, false},
		{408, models.RunStatusFailed, true},
		{429, models.RunStatusFailed, true},
		{500, models.RunStatusFailed, true},
		{503, models.RunStatusFailed, true},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"code":0}`))
		}))
		e := NewHTTPExecutor()
		res := e.Execute(context.Background(), Request{
			RunID: "j1-t1", JobID: 1, Kind: KindHTTP, URL: srv.URL, Timeout: 5 * time.Second,
		})
		srv.Close()
		if res.Status != tc.wantStatus {
			t.Errorf("状态码 %d: Status = %s, want %s", tc.status, res.Status, tc.wantStatus)
		}
		if res.ExitCode != tc.status {
			t.Errorf("状态码 %d: ExitCode = %d", tc.status, res.ExitCode)
		}
		if res.Status == models.RunStatusFailed {
			if got := retry.Classify(res.Err).Retryable; got != tc.wantRetryable {
				t.Errorf("状态码 %d: retryable = %v, want %v", tc.status, got, tc.wantRetryable)
			}
		}
	}
}

func TestHTTPExecutorSendsMethodHeadersAndBody(t *testing.T) {
	var got struct {
		method, header, body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 128)
		n, _ := r.Body.Read(buf)
		got.method = r.Method
		got.header = r.Header.Get("X-Trace")
		got.body = string(buf[:n])
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	e := NewHTTPExecutor()
	res := e.Execute(context.Background(), Request{
		RunID: "j1-t1", Kind: KindHTTP, URL: srv.URL, Method: "POST",
		Headers: map[string]string{"X-Trace": "abc123"}, Body: `{"hello":"world"}`,
		ExpectStatus: []int{201}, Timeout: 5 * time.Second,
	})
	if res.Status != models.RunStatusSuccess {
		t.Fatalf("Status = %s, err = %v", res.Status, res.Err)
	}
	if got.method != "POST" || got.header != "abc123" || !strings.Contains(got.body, "hello") {
		t.Fatalf("请求未按预期发送: %+v", got)
	}
	if res.Artifacts["status_code"] != "201" {
		t.Errorf("结构化产物应包含状态码，实际 %v", res.Artifacts)
	}
}

func TestHTTPExecutorTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	e := NewHTTPExecutor()
	res := e.Execute(context.Background(), Request{
		RunID: "j1-t1", Kind: KindHTTP, URL: srv.URL, Timeout: 300 * time.Millisecond,
	})
	if res.Status != models.RunStatusTimeout {
		t.Fatalf("Status = %s, want timeout（err=%v）", res.Status, res.Err)
	}
}

func TestHTTPExecutorTruncatesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("B", 5000)))
	}))
	defer srv.Close()

	e := NewHTTPExecutor()
	req := Request{RunID: "j1-t1", Kind: KindHTTP, URL: srv.URL, Timeout: 5 * time.Second, MaxOutputBytes: 256}
	res := e.Execute(context.Background(), req)
	if !res.Truncated {
		t.Fatal("超大响应体必须被截断")
	}
	if len(res.Output) > 256+64 {
		t.Fatalf("响应体长度 %d 未受上限约束", len(res.Output))
	}
	if res.Status != models.RunStatusSuccess {
		t.Fatalf("截断不应导致失败：%s %v", res.Status, res.Err)
	}
}

func TestHTTPExecutorRejectsInvalidURL(t *testing.T) {
	e := NewHTTPExecutor()
	res := e.Execute(context.Background(), Request{RunID: "j1-t1", Kind: KindHTTP, URL: "ftp://example.com"})
	if res.Status != models.RunStatusFailed || retry.Classify(res.Err).Retryable {
		t.Fatalf("非法协议应快速失败且不重试，实际 status=%s err=%v", res.Status, res.Err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	buf := &limitedBuffer{limit: 8}
	if _, err := buf.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Write([]byte("67890")); err != nil {
		t.Fatal(err)
	}
	if !buf.Truncated() {
		t.Fatal("超过上限应标记截断")
	}
	if !strings.HasPrefix(buf.String(), "12345678") {
		t.Fatalf("应保留前 8 字节，实际 %q", buf.String())
	}
	if !strings.Contains(buf.String(), "已截断") {
		t.Fatal("截断应显式提示，避免使用者误判输出完整性")
	}
	if n, err := buf.Write([]byte("x")); err != nil || n != 1 {
		t.Fatalf("截断后仍需消费写入（避免子进程因管道写满阻塞）: n=%d err=%v", n, err)
	}
}
