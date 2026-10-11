package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// stubTool 可编程工具，用于验证注册表行为（配额/拒绝/panic/截断/超时）。
type stubTool struct {
	name     string
	schema   *jsonschema.Schema
	readOnly bool
	timeout  time.Duration
	fn       func(ctx context.Context, args map[string]interface{}) (Result, error)
	calls    int
}

func (s *stubTool) Name() string               { return s.name }
func (s *stubTool) Description() string        { return "测试工具" }
func (s *stubTool) Schema() *jsonschema.Schema { return s.schema }
func (s *stubTool) ReadOnly() bool             { return s.readOnly }
func (s *stubTool) Timeout() time.Duration     { return s.timeout }
func (s *stubTool) Execute(ctx context.Context, args map[string]interface{}) (Result, error) {
	s.calls++
	if s.fn == nil {
		return Result{OK: true, Summary: "ok"}, nil
	}
	return s.fn(ctx, args)
}

func okSchema() *jsonschema.Schema {
	return jsonschema.Object(map[string]*jsonschema.Schema{"q": jsonschema.String("查询")}, "q")
}

func newStub(name string) *stubTool {
	return &stubTool{name: name, schema: okSchema(), readOnly: true, fn: func(context.Context, map[string]interface{}) (Result, error) {
		return Result{OK: true, Summary: "ok", Output: "输出"}, nil
	}}
}

// fakeNodeSource / fakeLogSource / fakeSQLSource 数据源替身。
type fakeNodeSource struct {
	status NodeStatus
	err    error
	calls  int
}

func (f *fakeNodeSource) NodeStatus(_ context.Context, _ string) (NodeStatus, error) {
	f.calls++
	return f.status, f.err
}

type fakeLogSource struct {
	hits  []LogHit
	lastQ LogQuery
	err   error
	calls int
}

func (f *fakeLogSource) SearchLogs(_ context.Context, q LogQuery) ([]LogHit, error) {
	f.calls++
	f.lastQ = q
	return f.hits, f.err
}

type fakeSQLSource struct {
	rows      []map[string]interface{}
	lastQuery string
	err       error
	calls     int
}

func (f *fakeSQLSource) QueryReadonly(_ context.Context, q string) ([]map[string]interface{}, error) {
	f.calls++
	f.lastQuery = q
	return f.rows, f.err
}

// ---------------------------------------------------------------------------
// 注册表行为
// ---------------------------------------------------------------------------

func TestRegisterRejectsNonReadOnlyAndBadSchema(t *testing.T) {
	r := NewRegistry(DefaultPolicy())
	writeTool := newStub("write_thing")
	writeTool.readOnly = false
	if err := r.Register(writeTool); !errors.Is(err, ErrToolDenied) {
		t.Fatalf("非只读工具必须被拒绝: %v", err)
	}
	badSchema := newStub("bad_schema")
	badSchema.schema = &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"a": jsonschema.String("")}, Required: []string{"b"}}
	if err := r.Register(badSchema); err == nil {
		t.Fatal("Schema 写错必须被拒绝（否则校验会静默放行）")
	}
	noSchema := newStub("no_schema")
	noSchema.schema = nil
	if err := r.Register(noSchema); err == nil {
		t.Fatal("缺 Schema 必须被拒绝")
	}
	dup := newStub("dup")
	if err := r.Register(dup); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}
	if err := r.Register(newStub("dup")); err == nil {
		t.Fatal("重复注册必须被拒绝")
	}
	empty := newStub("")
	if err := r.Register(empty); err == nil {
		t.Fatal("空名必须被拒绝")
	}
	if names := r.Names(); len(names) != 1 || names[0] != "dup" {
		t.Fatalf("只有 dup 应该注册成功: %v", names)
	}
}

// TestSpecsOnlyExposeAllowedTools 被禁的工具**不能**告诉模型：
// 模型看到就会调，调一次拒一次，白烧 token 还污染轨迹。
func TestSpecsOnlyExposeAllowedTools(t *testing.T) {
	policy := DefaultPolicy()
	policy.Deny = []string{"secret_tool"}
	r := NewRegistry(policy)
	r.MustRegister(newStub("alpha"))
	r.MustRegister(newStub("secret_tool"))
	r.MustRegister(newStub("beta"))

	specs := r.Specs()
	if len(specs) != 2 {
		t.Fatalf("应只暴露 2 个工具: %+v", specs)
	}
	for _, s := range specs {
		if s.Name == "secret_tool" {
			t.Fatal("被禁工具不应出现在 Specs 中")
		}
		if s.Parameters == nil {
			t.Fatalf("工具 %s 缺少参数 Schema", s.Name)
		}
	}
	if names := r.AllowedNames(); len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("AllowedNames 应有序: %v", names)
	}
	// Allow 白名单模式
	r2 := NewRegistry(Policy{Allow: []string{"alpha"}})
	r2.MustRegister(newStub("alpha"))
	r2.MustRegister(newStub("beta"))
	if names := r2.AllowedNames(); len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("Allow 白名单未生效: %v", names)
	}
}

func TestCallUnknownToolAndDeniedToolAndBadArgs(t *testing.T) {
	r := NewRegistry(DefaultPolicy())
	r.MustRegister(newStub("alpha"))

	_, err := r.Call(context.Background(), "s1", "不存在", map[string]interface{}{})
	if !errors.Is(err, ErrUnknownTool) || !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("未知工具应报错并带上可用列表: %v", err)
	}
	if retry.Classify(err).Retryable {
		t.Fatal("未知工具不可重试")
	}

	r2 := NewRegistry(Policy{Deny: []string{"alpha"}})
	r2.MustRegister(newStub("alpha"))
	_, err = r2.Call(context.Background(), "s1", "alpha", map[string]interface{}{"q": "x"})
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("被禁工具应报 ErrToolDenied: %v", err)
	}
	// 被拒也要留痕
	if stats := r2.SessionStats("s1"); stats.Denied != 1 {
		t.Fatalf("被拒调用必须计入审计: %+v", stats)
	}

	_, err = r.Call(context.Background(), "s1", "alpha", map[string]interface{}{"q": "x", "extra": 1})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("额外参数应报 ErrInvalidArgs: %v", err)
	}
	_, err = r.Call(context.Background(), "s1", "alpha", map[string]interface{}{})
	if !errors.Is(err, ErrInvalidArgs) || !strings.Contains(err.Error(), "缺少必填字段") {
		t.Fatalf("缺必填字段应报错: %v", err)
	}
}

func TestCallBudgetLimits(t *testing.T) {
	policy := DefaultPolicy()
	policy.MaxCallsPerTool = 2
	policy.MaxCallsPerSession = 3
	r := NewRegistry(policy)
	a, b := newStub("a"), newStub("b")
	r.MustRegister(a)
	r.MustRegister(b)

	for i := 0; i < 2; i++ {
		if _, err := r.Call(context.Background(), "s1", "a", map[string]interface{}{"q": "x"}); err != nil {
			t.Fatalf("第 %d 次应成功: %v", i+1, err)
		}
	}
	// 同一个工具第 3 次：打不出新证据，必须拦住
	_, err := r.Call(context.Background(), "s1", "a", map[string]interface{}{"q": "x"})
	if !errors.Is(err, ErrSessionBudgetExceeded) {
		t.Fatalf("单工具超限应报错: %v", err)
	}
	if retry.Classify(err).Retryable {
		t.Fatal("预算类错误不可重试")
	}
	if a.calls != 2 {
		t.Fatalf("超限后不应真的执行工具，实际 %d 次", a.calls)
	}
	// 换工具：会话总额度还剩 1
	if _, err := r.Call(context.Background(), "s1", "b", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("会话内换工具应还能调用: %v", err)
	}
	// 会话总额度用尽
	if _, err := r.Call(context.Background(), "s1", "b", map[string]interface{}{"q": "x"}); !errors.Is(err, ErrSessionBudgetExceeded) {
		t.Fatalf("会话总额度应拦截: %v", err)
	}
	if left := r.RemainingCalls("s1"); left != 0 {
		t.Fatalf("剩余额度应为 0: %d", left)
	}
	// 新会话不受影响
	if _, err := r.Call(context.Background(), "s2", "a", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("新会话不应受影响: %v", err)
	}
	if left := r.RemainingCalls("s3"); left != policy.MaxCallsPerSession {
		t.Fatalf("未开始的会话应返回全额: %d", left)
	}
}

// TestCallPanicIsContained 工具实现有 bug 不能把 Agent 带崩。
func TestCallPanicIsContained(t *testing.T) {
	r := NewRegistry(DefaultPolicy())
	bad := newStub("boom")
	bad.fn = func(context.Context, map[string]interface{}) (Result, error) { panic("工具实现炸了") }
	r.MustRegister(bad)

	res, err := r.Call(context.Background(), "s1", "boom", map[string]interface{}{"q": "x"})
	if err != nil {
		t.Fatalf("panic 应被兜住并以观测形式返回，而不是向上抛: %v", err)
	}
	if res.OK || !strings.Contains(res.Err, "panic") {
		t.Fatalf("应返回失败观测: %+v", res)
	}
}

// TestCallTimeoutBecomesObservation 超时要变成"模型能看到的观测"。
func TestCallTimeoutBecomesObservation(t *testing.T) {
	policy := DefaultPolicy()
	policy.DefaultTimeout = 50 * time.Millisecond
	r := NewRegistry(policy)
	slow := newStub("slow")
	slow.fn = func(ctx context.Context, _ map[string]interface{}) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	r.MustRegister(slow)

	res, err := r.Call(context.Background(), "s1", "slow", map[string]interface{}{"q": "x"})
	if err != nil {
		t.Fatalf("超时不应是调用错误: %v", err)
	}
	if res.OK || !strings.Contains(res.Err, "超时") {
		t.Fatalf("应报超时观测: %+v", res)
	}
	if res.DurationMS < 40 {
		t.Fatalf("耗时记录不符: %d", res.DurationMS)
	}
}

func TestCallTruncatesOutputAndEvidence(t *testing.T) {
	policy := DefaultPolicy()
	policy.MaxOutputBytes = 64
	r := NewRegistry(policy)
	big := newStub("big")
	big.fn = func(context.Context, map[string]interface{}) (Result, error) {
		return Result{
			OK:       true,
			Output:   strings.Repeat("x", 500),
			Evidence: []Evidence{{Source: "log", Ref: "f:1", Quote: strings.Repeat("q", 2000)}},
		}, nil
	}
	r.MustRegister(big)
	res, err := r.Call(context.Background(), "s1", "big", map[string]interface{}{"q": "x"})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !res.Truncated || len(res.Output) != 64 {
		t.Fatalf("输出应截断到 64 字节并标记: len=%d truncated=%v", len(res.Output), res.Truncated)
	}
	if len(res.Evidence[0].Quote) > 600 {
		t.Fatalf("证据原文也要收口: %d", len(res.Evidence[0].Quote))
	}
}

func TestCallObserverAndSessionStats(t *testing.T) {
	var mu sync.Mutex
	var records []CallRecord
	r := NewRegistry(DefaultPolicy(), WithObserver(func(rec CallRecord) {
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
	}))
	tool := newStub("alpha")
	tool.fn = func(context.Context, map[string]interface{}) (Result, error) {
		return Result{OK: true, Summary: "ok", Evidence: []Evidence{{Source: "log", Ref: "x", Quote: "y"}}}, nil
	}
	r.MustRegister(tool)

	if _, err := r.Call(context.Background(), "s1", "alpha", map[string]interface{}{"q": "hello"}); err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	mu.Lock()
	if len(records) != 1 || records[0].Tool != "alpha" || !records[0].OK || records[0].Evidence != 1 {
		t.Fatalf("审计记录不符: %+v", records)
	}
	mu.Unlock()
	stats := r.SessionStats("s1")
	if stats.Calls != 1 || stats.PerTool["alpha"] != 1 || stats.Evidence != 1 {
		t.Fatalf("会话统计不符: %+v", stats)
	}
	// 审计回调 panic 不能影响主流程
	r2 := NewRegistry(DefaultPolicy(), WithObserver(func(CallRecord) { panic("审计炸了") }))
	r2.MustRegister(newStub("alpha"))
	if _, err := r2.Call(context.Background(), "s1", "alpha", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("审计回调 panic 不应影响调用: %v", err)
	}
	// ResetSession 后额度恢复
	r.ResetSession("s1")
	if stats := r.SessionStats("s1"); stats.Calls != 0 || stats.PerTool["alpha"] != 0 {
		t.Fatalf("重置后统计应清零: %+v", stats)
	}
}

// ---------------------------------------------------------------------------
// shell_readonly
// ---------------------------------------------------------------------------

func TestShellToolUsesInjectedRunnerAndSummarizes(t *testing.T) {
	var gotCmd string
	var gotArgs []string
	sh := NewShellToolWithRunner(func(_ context.Context, name string, args []string) (string, string, int, error) {
		gotCmd, gotArgs = name, args
		return "Filesystem      Size  Used Avail Use% Mounted on\n/dev/sda1        50G   47G  3.0G  95% /\n/dev/sdb1       100G   10G   90G  10% /data\n", "", 0, nil
	}, time.Second)

	if sh.Name() != "shell_readonly" || !sh.ReadOnly() {
		t.Fatalf("工具元信息不符: %s %v", sh.Name(), sh.ReadOnly())
	}
	res, err := sh.Execute(context.Background(), map[string]interface{}{"command": "df", "args": []interface{}{"-h"}})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if gotCmd != "df" || len(gotArgs) != 1 || gotArgs[0] != "-h" {
		t.Fatalf("执行参数不符: %s %v", gotCmd, gotArgs)
	}
	if !strings.Contains(res.Summary, "95%") {
		t.Fatalf("df 摘要应抓出最高使用率: %q", res.Summary)
	}
	if len(res.Evidence) != 1 || res.Evidence[0].Source != "shell" {
		t.Fatalf("证据不符: %+v", res.Evidence)
	}
	if res.Meta["exit_code"] != 0 {
		t.Fatalf("退出码未记录: %+v", res.Meta)
	}
}

func TestShellToolRejectsBeforeRunning(t *testing.T) {
	ran := false
	sh := NewShellToolWithRunner(func(context.Context, string, []string) (string, string, int, error) {
		ran = true
		return "", "", 0, nil
	}, time.Second)

	// 白名单外命令
	if _, err := sh.Execute(context.Background(), map[string]interface{}{"command": "rm", "args": []interface{}{"-rf", "/"}}); err == nil {
		t.Fatal("rm 应被拒绝")
	}
	// 非法命令名（Schema 之外的第二道检查）
	if _, err := sh.Execute(context.Background(), map[string]interface{}{"command": "df; id"}); err == nil {
		t.Fatal("含元字符的命令名应被拒绝")
	}
	if ran {
		t.Fatal("守卫拒绝时绝不能真正执行命令")
	}
}

func TestShellToolNonZeroExitIsObservation(t *testing.T) {
	sh := NewShellToolWithRunner(func(context.Context, string, []string) (string, string, int, error) {
		return "", "tail: cannot open '/var/log/crony/x.log'", 1, nil
	}, time.Second)
	res, err := sh.Execute(context.Background(), map[string]interface{}{"command": "tail", "args": []interface{}{"-n", "10", "/var/log/crony/x.log"}})
	if err != nil {
		t.Fatalf("非零退出应是观测而不是调用错误: %v", err)
	}
	if res.OK || !strings.Contains(res.Output, "stderr") {
		t.Fatalf("应标记失败并带上 stderr: %+v", res)
	}
	if !strings.Contains(res.Text(), "失败") {
		t.Fatalf("回灌文本应说明失败: %s", res.Text())
	}
}

// TestShellToolHonorsItsOwnPolicy 工具自带策略里的目录白名单必须生效
// （否则注册表策略与工具策略不一致就等于留了后门）。
func TestShellToolHonorsItsOwnPolicy(t *testing.T) {
	policy := DefaultPolicy()
	policy.ShellReadPaths = []string{"/data/logs/"}
	sh := NewShellToolWithPolicy(policy)
	sh.runner = func(context.Context, string, []string) (string, string, int, error) {
		return "line\n", "", 0, nil
	}
	if _, err := sh.Execute(context.Background(), map[string]interface{}{"command": "tail", "args": []interface{}{"-n", "5", "/data/logs/a.log"}}); err != nil {
		t.Fatalf("自定义白名单应放行: %v", err)
	}
	if _, err := sh.Execute(context.Background(), map[string]interface{}{"command": "tail", "args": []interface{}{"-n", "5", "/var/log/syslog"}}); err == nil {
		t.Fatal("默认目录在自定义策略下应被拒绝")
	}
}

// ---------------------------------------------------------------------------
// http_get
// ---------------------------------------------------------------------------

func TestHTTPGetTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/boom":
			w.WriteHeader(500)
			_, _ = w.Write([]byte("内部错误"))
		case "/binary":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte{0x89, 0x50, 0x4E, 0x47})
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(strings.Repeat("a", 5000)))
		case "/ssrf":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	tool := NewHTTPGetTool([]string{"127.0.0.1"}, 3*time.Second, 8192)

	res, err := tool.Execute(context.Background(), map[string]interface{}{"url": server.URL + "/health"})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !res.OK || !strings.Contains(res.Output, `"status":"ok"`) {
		t.Fatalf("健康检查结果不符: %+v", res)
	}
	if res.Meta["status_code"] != 200 {
		t.Fatalf("状态码未记录: %+v", res.Meta)
	}
	if len(res.Evidence) != 1 || res.Evidence[0].Source != "http" {
		t.Fatalf("证据不符: %+v", res.Evidence)
	}

	// 5xx → 失败观测
	res, err = tool.Execute(context.Background(), map[string]interface{}{"url": server.URL + "/boom"})
	if err != nil || res.OK {
		t.Fatalf("500 应标记失败: %v %+v", err, res)
	}

	// 非文本响应不回填正文（否则白烧上下文）
	res, err = tool.Execute(context.Background(), map[string]interface{}{"url": server.URL + "/binary"})
	if err != nil || !res.OK {
		t.Fatalf("200 的二进制响应应视为成功: %v %+v", err, res)
	}
	if strings.Contains(res.Output, "\x89PNG") || !strings.Contains(res.Output, "已省略正文") {
		t.Fatalf("二进制正文不应回填: %q", res.Output)
	}

	// max_bytes 截断
	res, err = tool.Execute(context.Background(), map[string]interface{}{"url": server.URL + "/big", "max_bytes": float64(1000)})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !res.Truncated || len(res.Output) != 1000 {
		t.Fatalf("截断不符: len=%d truncated=%v", len(res.Output), res.Truncated)
	}

	// SSRF：重定向到元数据端点必须被拒（且不是"跟着跳过去再报错"）
	_, err = tool.Execute(context.Background(), map[string]interface{}{"url": server.URL + "/ssrf"})
	if err == nil {
		t.Fatal("重定向到元数据端点必须返回硬错误（策略拒绝）")
	}
	if !strings.Contains(err.Error(), "永久禁止") && !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("错误应指明重定向/元数据原因: %v", err)
	}
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("应可用 errors.Is(ErrToolDenied) 判定: %v", err)
	}

	// 未配置 AllowHosts 时的说明必须诚实
	noNet := NewHTTPGetTool(nil, time.Second, 1024)
	if !strings.Contains(noNet.Description(), "所有请求都会被拒绝") {
		t.Fatalf("未配置 AllowHosts 时描述应说明会被拒绝: %s", noNet.Description())
	}
}

// ---------------------------------------------------------------------------
// node_status / log_search / mysql_query_readonly
// ---------------------------------------------------------------------------

func TestNodeStatusTool(t *testing.T) {
	src := &fakeNodeSource{status: NodeStatus{
		NodeUUID: "n1", Hostname: "node-1", Online: true, LastHeartbeat: time.Now().Add(-30 * time.Second),
		RunningJobs: 2, CPUPercent: 12.5, MemPercent: 63.2, DiskPercent: 95.4, LoadAvg: "1.20", Version: "1.0.0",
		// CollectedAt 非零 = 水位已上报；零值时必须输出"未上报"（见 TestNodeStatusToolWithoutMetrics）
		CollectedAt: time.Now().Add(-10 * time.Second),
	}}
	tool := NewNodeStatusTool(src)
	res, err := tool.Execute(context.Background(), map[string]interface{}{})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !strings.Contains(res.Output, "online=在线") || !strings.Contains(res.Output, "disk=95.4%") {
		t.Fatalf("输出不符: %s", res.Output)
	}
	if !strings.Contains(res.Summary, "95.4%") || !strings.Contains(res.Summary, "运行中任务 2") {
		t.Fatalf("摘要不符: %s", res.Summary)
	}
	if len(res.Evidence) != 1 || res.Evidence[0].Ref != "n1" {
		t.Fatalf("证据不符: %+v", res.Evidence)
	}
	if _, err := NewNodeStatusTool(nil).Execute(context.Background(), map[string]interface{}{}); err == nil {
		t.Fatal("未接入数据源应报错")
	}
}

// TestNodeStatusToolWithoutMetrics 水位未上报时**不得**渲染成 0%。
//
// 这是本项目的核心纪律：0% 会被模型读成"节点资源空闲"，是凭空的证据捏造；
// 必须显式说"未上报"，模型才知道这条事实不存在。
func TestNodeStatusToolWithoutMetrics(t *testing.T) {
	src := &fakeNodeSource{status: NodeStatus{
		NodeUUID: "n2", Hostname: "node-2", Online: true, RunningJobs: 0, Version: "1.0.0",
	}}
	res, err := NewNodeStatusTool(src).Execute(context.Background(), map[string]interface{}{})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !strings.Contains(res.Output, "未上报") {
		t.Fatalf("水位未上报时必须显式说明，实际输出: %s", res.Output)
	}
	for _, bad := range []string{"cpu=0.0%", "mem=0.0%", "disk=0.0%"} {
		if strings.Contains(res.Output, bad) {
			t.Fatalf("未上报不得渲染成 0（发现 %q）: %s", bad, res.Output)
		}
	}
	if !strings.Contains(res.Summary, "未上报") {
		t.Fatalf("摘要也必须说明未上报: %s", res.Summary)
	}
	if reported, ok := res.Meta["metrics_reported"].(bool); !ok || reported {
		t.Fatalf("meta.metrics_reported 应为 false，实际 %+v", res.Meta)
	}
}

func TestLogSearchTool(t *testing.T) {
	now := time.Now()
	src := &fakeLogSource{hits: []LogHit{
		{Source: "node.log", Ref: "node.log:12", Line: "ERROR dial tcp timeout", Timestamp: now},
		{Source: "node.log", Ref: "node.log:13", Line: "retry 1/3", Timestamp: now},
	}}
	tool := NewLogSearchTool(src)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"keyword": "timeout", "job_id": float64(12), "limit": float64(10)})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !strings.Contains(res.Output, "ERROR dial tcp timeout") || !strings.Contains(res.Summary, "命中 2 条") {
		t.Fatalf("输出/摘要不符: %s | %s", res.Output, res.Summary)
	}
	if src.lastQ.Keyword != "timeout" || src.lastQ.JobID != 12 || src.lastQ.Limit != 10 || src.lastQ.SinceMinutes != 60 {
		t.Fatalf("查询条件传递不符: %+v", src.lastQ)
	}
	if len(res.Evidence) != 2 {
		t.Fatalf("证据数量不符: %+v", res.Evidence)
	}

	// 无过滤条件 → 拒绝（禁止全量拉日志）
	if _, err := tool.Execute(context.Background(), map[string]interface{}{}); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("无过滤条件应被拒绝: %v", err)
	}
	if src.calls != 1 {
		t.Fatalf("被拒绝的调用不应打到数据源: %d", src.calls)
	}

	// 零命中也是证据（要在摘要里说清楚）
	empty := &fakeLogSource{}
	res, err = NewLogSearchTool(empty).Execute(context.Background(), map[string]interface{}{"run_id": "j1-t1"})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !strings.Contains(res.Summary, "没有命中") || !strings.Contains(res.Summary, "重要证据") {
		t.Fatalf("零命中摘要应说明其证据意义: %s", res.Summary)
	}

	// 证据条数上限 5
	many := make([]LogHit, 12)
	for i := range many {
		many[i] = LogHit{Ref: fmt.Sprintf("f:%d", i), Line: "x"}
	}
	res, err = NewLogSearchTool(&fakeLogSource{hits: many}).Execute(context.Background(), map[string]interface{}{"keyword": "x"})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if len(res.Evidence) != 5 {
		t.Fatalf("证据最多 5 条: %d", len(res.Evidence))
	}
}

func TestSQLToolGuardsBeforeQuerying(t *testing.T) {
	src := &fakeSQLSource{rows: []map[string]interface{}{{"id": 1, "status": "failed"}}}
	tool := NewSQLTool(src, 100)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"query": "select id, status from job_run where status='failed' limit 10"})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !strings.Contains(res.Output, `"status":"failed"`) || !strings.Contains(res.Summary, "1 行") {
		t.Fatalf("输出/摘要不符: %s | %s", res.Output, res.Summary)
	}
	if len(res.Evidence) != 1 || res.Evidence[0].Source != "sql" {
		t.Fatalf("证据不符: %+v", res.Evidence)
	}

	// 写操作：守卫必须在下发查询**之前**拦住
	for _, q := range []string{"update job set spec='* * * * *' limit 1", "select 1; drop table job", "select * from job"} {
		if _, err := tool.Execute(context.Background(), map[string]interface{}{"query": q}); err == nil {
			t.Fatalf("危险 SQL 应被拒绝: %s", q)
		}
	}
	if src.calls != 1 {
		t.Fatalf("被拒绝的 SQL 绝不能下发（实际下发 %d 次）", src.calls)
	}

	// 查询失败 → 观测（不 panic、不抛错）
	bad := &fakeSQLSource{err: errors.New("Error 1142: SELECT command denied")}
	res, err = NewSQLTool(bad, 100).Execute(context.Background(), map[string]interface{}{"query": "select 1 limit 1"})
	if err != nil {
		t.Fatalf("数据源报错应是观测: %v", err)
	}
	if res.OK || !strings.Contains(res.Err, "denied") {
		t.Fatalf("失败观测不符: %+v", res)
	}
}

// TestNewReadonlyRegistryOnlyRegistersAvailableSources
// 没有数据源的工具不注册：模型看不到就不会浪费步数去调它。
func TestNewReadonlyRegistryOnlyRegistersAvailableSources(t *testing.T) {
	r, err := NewReadonlyRegistry(DefaultPolicy(), Sources{Logs: &fakeLogSource{}})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	names := r.Names()
	want := []string{"http_get", "log_search", "shell_readonly"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("注册工具不符: %v", names)
	}

	r2, err := NewReadonlyRegistry(DefaultPolicy(), Sources{
		Logs:       &fakeLogSource{},
		NodeStatus: &fakeNodeSource{},
		SQL:        &fakeSQLSource{},
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if len(r2.Names()) != 5 {
		t.Fatalf("全部数据源接入后应注册 5 个工具: %v", r2.Names())
	}
	// 所有工具都必须只读（阶段 2 的硬约束）
	for _, name := range r2.Names() {
		tool, _ := r2.Get(name)
		if !tool.ReadOnly() {
			t.Fatalf("工具 %s 不是只读", name)
		}
	}
}

// TestResultTextIsModelFriendly 回灌给模型的文本要包含状态/摘要/证据/截断标记。
func TestResultTextIsModelFriendly(t *testing.T) {
	res := Result{
		OK:        false,
		Summary:   "命令失败",
		Err:       "退出码 1",
		Output:    "some output",
		Truncated: true,
		Evidence:  []Evidence{{Source: "log", Ref: "node.log:1", Quote: "E1"}},
	}
	text := res.Text()
	for _, want := range []string{"状态: 失败", "摘要: 命令失败", "错误: 退出码 1", "[log] node.log:1", "输出:", "已截断"} {
		if !strings.Contains(text, want) {
			t.Fatalf("回灌文本缺少 %q:\n%s", want, text)
		}
	}
	if res.Failed() != true || (Result{OK: true}).Failed() != false {
		t.Fatal("Failed() 判定不符")
	}
	ok := Result{OK: true, Summary: "ok", Evidence: []Evidence{{Source: "node", Ref: "n1", Quote: "q"}}}
	if !strings.Contains(ok.Text(), "状态: 成功") {
		t.Fatalf("成功文本不符: %s", ok.Text())
	}
	// 回灌文本必须是纯文本（不含 JSON 花括号噪音）
	if strings.Contains(ok.Text(), "{") {
		t.Fatalf("应避免 JSON 噪音: %s", ok.Text())
	}
	if _, err := json.Marshal(res); err != nil {
		t.Fatalf("Result 应可序列化: %v", err)
	}
}
