package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 阶段 4 闸门：风险注册、自治级别、审批、爆炸半径、速率、并发
// ---------------------------------------------------------------------------

// TestRegisterRiskGates 默认策略下写工具**注册不进来**——"默认关着"必须由测试锁死，
// 否则将来某次重构把默认值改宽，是没有任何测试会报警的。
func TestRegisterRiskGates(t *testing.T) {
	// 1) 默认策略：可逆写工具被拒
	r := NewRegistry(DefaultPolicy())
	if err := r.Register(newWriteStub("job_retry", "node-1")); !errors.Is(err, ErrToolDenied) {
		t.Fatalf("默认策略下写工具必须被拒绝: %v", err)
	}
	// 2) 默认策略：不可逆写工具同样被拒
	destructive := newWriteStub("restart_service", "node-1")
	destructive.risk = RiskDestructive
	if err := r.Register(destructive); !errors.Is(err, ErrToolDenied) {
		t.Fatalf("默认策略下不可逆工具必须被拒绝: %v", err)
	}
	// 3) 开了 allow_write 但工具没实现 TargetProvider：仍然拒绝
	//    （否则"限制影响 1 台主机"会被没声明目标的写工具静默绕开）
	rWrite := NewRegistry(writePolicy())
	bare := newStub("bare_write")
	bare.readOnly = false
	bare.risk = RiskWrite
	if err := rWrite.Register(bare); !errors.Is(err, ErrRiskNotAllowed) {
		t.Fatalf("缺 TargetProvider 的写工具必须被拒绝: %v", err)
	}
	// 4) risk 与 ReadOnly 自相矛盾：拒绝而不是猜
	contradict := newStub("contradict")
	contradict.risk = RiskWrite // readOnly 仍为 true
	if err := rWrite.Register(contradict); !errors.Is(err, ErrRiskNotAllowed) {
		t.Fatalf("风险级别与只读声明矛盾必须被拒绝: %v", err)
	}
	// 5) 未声明的风险级别（合法取值之外）：拒绝
	weird := newStub("weird")
	weird.risk = RiskLevel("high")
	if err := rWrite.Register(weird); !errors.Is(err, ErrRiskNotAllowed) {
		t.Fatalf("非法风险级别必须被拒绝: %v", err)
	}
	// 6) 开了 allow_write 的合法写工具：注册成功
	if err := rWrite.Register(newWriteStub("job_retry", "node-1")); err != nil {
		t.Fatalf("开启后合法写工具应注册成功: %v", err)
	}
	// 7) 只开 allow_destructive 不开 allow_write：destructive 也不该被放行（由 Normalize 补齐 write）
	p := DefaultPolicy()
	p.AllowDestructive = true
	if !p.Normalize().AllowWrite {
		t.Fatal("allow_destructive 隐含要求 allow_write：Normalize 应补齐")
	}
	rDestructive := NewRegistry(p)
	d2 := newWriteStub("restart_service", "node-1")
	d2.risk = RiskDestructive
	if err := rDestructive.Register(d2); err != nil {
		t.Fatalf("开启 allow_destructive 后不可逆工具应可注册: %v", err)
	}
}

// TestWriteToolDeniedWithoutApprovalChannel 没有审批通道时**必须拒绝执行**。
// 这是整个阶段 4 最重要的一条安全默认：拿不到审批单就动手，
// 等于"只要审批组件挂了，Agent 就能随便写"。
func TestWriteToolDeniedWithoutApprovalChannel(t *testing.T) {
	var records []CallRecord
	r := NewRegistry(writePolicy(), WithObserver(func(rec CallRecord) { records = append(records, rec) }))
	wt := newWriteStub("job_retry", "node-1")
	r.MustRegister(wt)

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); !errors.Is(err, ErrApprovalUnavailable) {
		t.Fatalf("无审批通道时必须拒绝: %v", err)
	}
	if wt.calls != 0 {
		t.Fatalf("被拒的写动作绝不能执行: calls=%d", wt.calls)
	}
	if len(records) != 1 || records[0].Decision != DecisionDenied || records[0].Reason != ReasonApprovalNoChannel {
		t.Fatalf("被拒的写动作必须留痕并写明哪道闸: %+v", records)
	}
	if stats := r.SessionStats("s1"); stats.WriteActions != 0 {
		t.Fatalf("被拒的写动作不应占用爆炸半径预算: %+v", stats)
	}
}

// TestApprovalRequiredIsObservationNotExecution 转审批是**成功观测**且绝不执行。
func TestApprovalRequiredIsObservationNotExecution(t *testing.T) {
	var records []CallRecord
	ap := &stubApprover{}
	r := NewRegistry(writePolicy(), WithApprover(ap), WithObserver(func(rec CallRecord) { records = append(records, rec) }))
	wt := newWriteStub("job_retry", "node-1")
	r.MustRegister(wt)

	res, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"})
	if err != nil {
		t.Fatalf("转审批不应是调用错误（模型需要看到「等待人工」这个观测）: %v", err)
	}
	if !res.OK {
		t.Fatalf("转审批应是成功观测: %+v", res)
	}
	if wt.calls != 0 {
		t.Fatalf("转审批阶段绝不能执行写动作: calls=%d", wt.calls)
	}
	if !strings.Contains(res.Output, "尚未执行") {
		t.Fatalf("观测必须写明「尚未执行」（否则模型会把等待审批写成已执行）: %s", res.Output)
	}
	if len(res.Evidence) != 1 || res.Evidence[0].Source != "approval" {
		t.Fatalf("必须挂可查证的审批证据: %+v", res.Evidence)
	}
	if got := res.Meta["executed"]; got != false {
		t.Fatalf("meta.executed 必须为 false: %v", got)
	}
	if len(ap.got) != 1 {
		t.Fatalf("应生成 1 张审批单: %+v", ap.got)
	}
	in := ap.got[0]
	if in.Tool != "job_retry" || in.Risk != RiskWrite || in.ArgsHash == "" || len(in.Targets) != 1 {
		t.Fatalf("审批单内容不完整: %+v", in)
	}
	if in.Impact == "" {
		t.Fatal("审批单必须带人类可读影响面（审批人批的是影响面）")
	}
	if len(records) != 1 || records[0].Decision != DecisionApprovalRequired || records[0].ApprovalID == "" {
		t.Fatalf("转审批必须留痕并带审批单号: %+v", records)
	}
	stats := r.SessionStats("s1")
	if stats.ApprovalRequired != 1 || stats.WriteActions != 1 {
		t.Fatalf("写动作意图必须计入统计: %+v", stats)
	}
}

// TestAutonomyL0DeniesWriteTools 自治级别 L0 = 只建议：连审批单都不生成。
func TestAutonomyL0DeniesWriteTools(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL0
	ap := &stubApprover{}
	r := NewRegistry(p, WithApprover(ap))
	wt := newWriteStub("job_retry", "node-1")
	r.MustRegister(wt)

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); !errors.Is(err, ErrRiskNotAllowed) {
		t.Fatalf("L0 下写动作必须被拒绝: %v", err)
	}
	if wt.calls != 0 || len(ap.got) != 0 {
		t.Fatalf("L0 下既不能执行也不能生成审批单: calls=%d approvals=%d", wt.calls, len(ap.got))
	}
}

// TestAutonomyL2OnlyWhitelistedAutoApproves 白名单自动放行只对**精确匹配**生效。
func TestAutonomyL2OnlyWhitelistedAutoApproves(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL2
	p.AutoApprove = []string{"job_retry@node-1"}
	p.MaxAffectedHostsPerSession = 2 // 本例要观察的是"白名单"，不是半径（半径另有专测）
	ap := &stubApprover{}
	r := NewRegistry(p, WithApprover(ap))
	allowed := newWriteStub("job_retry", "node-1")
	other := newWriteStub("job_retry", "node-2")
	other.name = "job_pause"
	r.MustRegister(allowed)
	r.MustRegister(other)

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("命中白名单应直接执行: %v", err)
	}
	if allowed.calls != 1 {
		t.Fatalf("命中白名单的写动作应真的执行: calls=%d", allowed.calls)
	}
	// 未命中白名单 → 转审批
	if _, err := r.Call(context.Background(), "s1", "job_pause", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("未命中白名单应转审批而不是报错: %v", err)
	}
	if other.calls != 0 || len(ap.got) != 1 {
		t.Fatalf("未命中白名单的写动作不能执行: calls=%d approvals=%d", other.calls, len(ap.got))
	}
	// destructive 即使命中白名单也**仍然要审批**
	p2 := writePolicy()
	p2.Autonomy = AutonomyL2
	p2.AllowDestructive = true
	p2.AutoApprove = []string{"restart_service"}
	ap2 := &stubApprover{}
	r2 := NewRegistry(p2, WithApprover(ap2))
	dest := newWriteStub("restart_service", "node-1")
	dest.risk = RiskDestructive
	r2.MustRegister(dest)
	if _, err := r2.Call(context.Background(), "s1", "restart_service", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("不可逆动作应转审批: %v", err)
	}
	if dest.calls != 0 || len(ap2.got) != 1 {
		t.Fatalf("不可逆动作绝不允许自动放行: calls=%d approvals=%d", dest.calls, len(ap2.got))
	}
}

// TestBlastRadiusGate 爆炸半径：单会话写动作数 + 去重主机数。
func TestBlastRadiusGate(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL3 // 直接执行，单独验证半径闸
	p.MaxWriteActionsPerSession = 1
	p.MaxAffectedHostsPerSession = 2
	r := NewRegistry(p)
	one := newWriteStub("job_retry", "node-1")
	r.MustRegister(one)
	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("第一次写动作应放行: %v", err)
	}
	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); !errors.Is(err, ErrBlastRadiusExceeded) {
		t.Fatalf("超过单会话写动作上限必须被拒: %v", err)
	}
	if one.calls != 1 {
		t.Fatalf("被半径闸拒掉的调用不能执行: calls=%d", one.calls)
	}

	// 主机数：一次影响 3 台 > 上限 2 → 拒绝
	p2 := writePolicy()
	p2.Autonomy = AutonomyL3
	p2.MaxAffectedHostsPerSession = 2
	r2 := NewRegistry(p2)
	many := newWriteStub("job_retry", "node-1", "node-2", "node-3")
	r2.MustRegister(many)
	if _, err := r2.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); !errors.Is(err, ErrBlastRadiusExceeded) {
		t.Fatalf("一次影响 3 台主机必须被拒: %v", err)
	}
	if many.calls != 0 {
		t.Fatalf("被半径闸拒掉的调用不能执行: calls=%d", many.calls)
	}
	// 去重：同一台主机的第二次调用不新增主机数
	p3 := writePolicy()
	p3.Autonomy = AutonomyL3
	p3.MaxAffectedHostsPerSession = 1
	p3.MaxWriteActionsPerSession = 3
	r3 := NewRegistry(p3)
	same := newWriteStub("job_retry", "node-1")
	r3.MustRegister(same)
	for i := 0; i < 2; i++ {
		if _, err := r3.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
			t.Fatalf("同一主机的第 %d 次写动作应放行: %v", i+1, err)
		}
	}
	if hosts := r3.SessionStats("s1").Hosts; len(hosts) != 1 || hosts[0] != "node-1" {
		t.Fatalf("主机去重统计不符: %v", hosts)
	}
}

// TestRateLimitGateIsProcessWide 速率闸是**进程级**的（跨会话）。
func TestRateLimitGateIsProcessWide(t *testing.T) {
	p := DefaultPolicy()
	p.MaxCallsPerMinute = 2
	var records []CallRecord
	r := NewRegistry(p, WithObserver(func(rec CallRecord) { records = append(records, rec) }))
	r.MustRegister(newStub("alpha"))

	if _, err := r.Call(context.Background(), "s1", "alpha", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("第 1 次应放行: %v", err)
	}
	if _, err := r.Call(context.Background(), "s2", "alpha", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("第 2 次（另一个会话）应放行: %v", err)
	}
	if _, err := r.Call(context.Background(), "s3", "alpha", map[string]interface{}{"q": "x"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("第 3 次必须被速率闸拒绝（跨会话累计）: %v", err)
	}
	last := records[len(records)-1]
	if last.Decision != DecisionDenied || last.Reason != ReasonRateLimited {
		t.Fatalf("速率拒绝必须能定位到具体闸门: %+v", last)
	}
	// 被速率闸拒掉的调用不消耗会话预算
	if left := r.RemainingCalls("s3"); left != p.MaxCallsPerSession {
		t.Fatalf("被限速的调用不应消耗会话预算: 剩余 %d", left)
	}
}

// TestConcurrencyGateRejectsInsteadOfQueueing 并发满时**直接拒绝**（不排队）。
func TestConcurrencyGateRejectsInsteadOfQueueing(t *testing.T) {
	p := DefaultPolicy()
	p.MaxConcurrentCalls = 1
	var records []CallRecord
	var mu sync.Mutex
	r := NewRegistry(p, WithObserver(func(rec CallRecord) {
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
	}))
	started := make(chan struct{})
	release := make(chan struct{})
	slow := newStub("slow")
	slow.timeout = 5 * time.Second
	slow.fn = func(context.Context, map[string]interface{}) (Result, error) {
		close(started)
		<-release
		return Result{OK: true, Summary: "ok"}, nil
	}
	r.MustRegister(slow)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Call(context.Background(), "s1", "slow", map[string]interface{}{"q": "x"})
	}()
	<-started
	if _, err := r.Call(context.Background(), "s2", "slow", map[string]interface{}{"q": "x"}); !errors.Is(err, ErrConcurrencyLimited) {
		t.Fatalf("并发满时必须拒绝: %v", err)
	}
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	var found bool
	for _, rec := range records {
		if rec.Decision == DecisionDenied && rec.Reason == ReasonConcurrencyLimit {
			found = true
		}
	}
	if !found {
		t.Fatalf("并发拒绝必须留痕: %+v", records)
	}
}

// TestDeniedCallDoesNotConsumeBudgetAndIsAudited 被拒调用要留痕、但不占额度。
func TestDeniedCallDoesNotConsumeBudgetAndIsAudited(t *testing.T) {
	p := DefaultPolicy()
	p.Allow = []string{"alpha"}
	var records []CallRecord
	r := NewRegistry(p, WithObserver(func(rec CallRecord) { records = append(records, rec) }))
	r.MustRegister(newStub("alpha"))
	r.MustRegister(newStub("beta"))

	// 参数不合法（缺必填 q）
	if _, err := r.Call(context.Background(), "s1", "alpha", map[string]interface{}{"other": 1}); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("参数不合法必须拒绝: %v", err)
	}
	// 策略未允许
	if _, err := r.Call(context.Background(), "s1", "beta", map[string]interface{}{"q": "x"}); !errors.Is(err, ErrToolDenied) {
		t.Fatalf("策略未允许必须拒绝: %v", err)
	}
	// 未知工具
	if _, err := r.Call(context.Background(), "s1", "ghost", map[string]interface{}{"q": "x"}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("未知工具必须拒绝: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("三种拒绝都必须留痕: %+v", records)
	}
	reasons := map[string]bool{}
	for _, rec := range records {
		if rec.Decision != DecisionDenied || !rec.Denied || rec.Reason == "" {
			t.Fatalf("拒绝记录必须写明原因: %+v", rec)
		}
		reasons[rec.Reason] = true
	}
	for _, want := range []string{ReasonInvalidArgs, ReasonPolicyDenied, ReasonUnknownTool} {
		if !reasons[want] {
			t.Fatalf("缺少拒绝原因 %q: %v", want, reasons)
		}
	}
	// 额度没被消耗：合法调用仍然可用（12 次上限 − 0）
	if left := r.RemainingCalls("s1"); left != p.MaxCallsPerSession {
		t.Fatalf("被拒调用不应消耗预算: 剩余 %d", left)
	}
	res, err := r.Call(context.Background(), "s1", "alpha", map[string]interface{}{"q": "x"})
	if err != nil || !res.OK {
		t.Fatalf("合法调用应成功: %v %+v", err, res)
	}
	if stats := r.SessionStats("s1"); stats.Denied != 3 || len(stats.DeniedBy) != 3 {
		t.Fatalf("被拒统计应可细分: %+v", stats)
	}
}

// TestRunMetaIsCarriedIntoAudit 归属信息（哪种会话/哪条执行记录）必须进审计。
func TestRunMetaIsCarriedIntoAudit(t *testing.T) {
	var records []CallRecord
	r := NewRegistry(DefaultPolicy(), WithObserver(func(rec CallRecord) { records = append(records, rec) }))
	r.MustRegister(newStub("alpha"))
	ctx := WithRunMeta(context.Background(), RunMeta{AgentKind: "diagnose", RunID: "r-1"})
	if _, err := r.Call(ctx, "s1", "alpha", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(records) != 1 || records[0].AgentKind != "diagnose" || records[0].RunID != "r-1" {
		t.Fatalf("归属信息缺失: %+v", records)
	}
	// 没有归属信息时不应报错（只影响可读性）
	if _, err := r.Call(context.Background(), "s2", "alpha", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("缺归属信息不应导致失败: %v", err)
	}
}

// TestTimeoutAndPanicAreAudited 超时与 panic 也必须有审计行（否则"没执行"与"没记录"无法区分）。
func TestTimeoutAndPanicAreAudited(t *testing.T) {
	p := DefaultPolicy()
	p.DefaultTimeout = 30 * time.Millisecond
	var records []CallRecord
	var mu sync.Mutex
	r := NewRegistry(p, WithObserver(func(rec CallRecord) {
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
	}))
	slow := newStub("slow")
	slow.fn = func(ctx context.Context, _ map[string]interface{}) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	boom := newStub("boom")
	boom.fn = func(context.Context, map[string]interface{}) (Result, error) { panic("炸了") }
	r.MustRegister(slow)
	r.MustRegister(boom)

	if _, err := r.Call(context.Background(), "s1", "slow", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("超时应是观测: %v", err)
	}
	if _, err := r.Call(context.Background(), "s1", "boom", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("panic 应被兜住: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 2 {
		t.Fatalf("超时与 panic 都必须留痕: %+v", records)
	}
	for _, rec := range records {
		if rec.Decision != DecisionAllowed || rec.OK {
			t.Fatalf("失败观测的决策应记为 allowed（真的执行了）且 ok=false: %+v", rec)
		}
	}
}

// TestTruncationNeverSplitsUTF8 截断不得切断多字节字符。
//
// 阶段 3 的真实故障：按字节截断把验收报告切成坏 UTF-8，整份文档"打不开"。
// 这里用中文（每字 3 字节）锁死这个性质。
func TestTruncationNeverSplitsUTF8(t *testing.T) {
	p := DefaultPolicy()
	p.MaxOutputBytes = 10
	r := NewRegistry(p)
	big := newStub("big")
	big.fn = func(context.Context, map[string]interface{}) (Result, error) {
		return Result{OK: true, Output: strings.Repeat("故障诊断", 20)}, nil
	}
	r.MustRegister(big)
	res, err := r.Call(context.Background(), "s1", "big", map[string]interface{}{"q": "x"})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !res.Truncated {
		t.Fatal("超限输出必须标记截断")
	}
	if len(res.Output) > 10 {
		t.Fatalf("截断后长度不符: %d", len(res.Output))
	}
	if !utf8.ValidString(res.Output) {
		t.Fatalf("截断产生了非法 UTF-8: %q", res.Output)
	}
	if res.Output != "故障诊" {
		t.Fatalf("应保留完整字符（3 字节×3=9 字节）: %q", res.Output)
	}
}

// TestPolicyReturnsDeepCopy 读策略不得改到注册表正在用的策略。
func TestPolicyReturnsDeepCopy(t *testing.T) {
	p := DefaultPolicy()
	p.AllowHosts = []string{"a.example.com"}
	p.ShellReadPaths = []string{"/var/log/"}
	p.AutoApprove = []string{"job_retry"}
	r := NewRegistry(p)
	got := r.Policy()
	got.AllowHosts[0] = "evil.example.com"
	got.ShellReadPaths[0] = "/"
	got.AutoApprove[0] = "restart_service"
	got.Allow = append(got.Allow, "x")

	again := r.Policy()
	if again.AllowHosts[0] != "a.example.com" || again.ShellReadPaths[0] != "/var/log/" || again.AutoApprove[0] != "job_retry" {
		t.Fatalf("策略被外部改写（浅拷贝）：%+v", again)
	}
	if len(again.Allow) != 0 {
		t.Fatalf("Allow 不应被外部 append 影响: %v", again.Allow)
	}
}

// TestArgsHashIsStableAndTamperSensitive 审批的 TOCTOU 校验依据。
func TestArgsHashIsStableAndTamperSensitive(t *testing.T) {
	a := map[string]interface{}{"job_id": float64(21), "reason": "重跑", "nested": map[string]interface{}{"b": float64(2), "a": float64(1)}}
	b := map[string]interface{}{"reason": "重跑", "nested": map[string]interface{}{"a": float64(1), "b": float64(2)}, "job_id": float64(21)}
	if ArgsHash(a) != ArgsHash(b) {
		t.Fatal("键顺序不同但内容相同，哈希必须一致（否则合法审批会被误判为篡改）")
	}
	c := map[string]interface{}{"job_id": float64(22), "reason": "重跑", "nested": map[string]interface{}{"a": float64(1), "b": float64(2)}}
	if ArgsHash(a) == ArgsHash(c) {
		t.Fatal("参数被改（job_id 21→22）必须导致哈希变化")
	}
	if h := ArgsHash(nil); len(h) != 64 {
		t.Fatalf("空参数也应得到 64 字符哈希: %q", h)
	}
}

// TestReadonlyRegistryDeclaresReadRisk 既有 6 个内置工具的风险声明必须与只读一致。
func TestReadonlyRegistryDeclaresReadRisk(t *testing.T) {
	r, err := NewReadonlyRegistry(DefaultPolicy(), Sources{
		Logs:       &fakeLogSource{},
		NodeStatus: &fakeNodeSource{},
		SQL:        &fakeSQLSource{},
		Probe:      &fakeProbeSource{},
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	names := r.Names()
	if len(names) != 6 {
		t.Fatalf("只读工具集应为 6 个: %v", names)
	}
	for _, name := range names {
		tool, _ := r.Get(name)
		if tool.Risk() != RiskRead || !tool.ReadOnly() {
			t.Fatalf("工具 %s 的风险声明与只读不一致: risk=%s readonly=%v", name, tool.Risk(), tool.ReadOnly())
		}
		// 只读工具不应声明影响目标（有目标就该走写工具的直径预算）
		if _, ok := tool.(TargetProvider); ok {
			t.Fatalf("只读工具 %s 不应实现 TargetProvider（否则语义混乱）", name)
		}
	}
}

// stubApprover 审批单替身。
type stubApprover struct {
	got []ApprovalInput
	err error
	id  string
}

func (s *stubApprover) RequestApproval(_ context.Context, in ApprovalInput) (ApprovalOutcome, error) {
	s.got = append(s.got, in)
	if s.err != nil {
		return ApprovalOutcome{}, s.err
	}
	id := s.id
	if id == "" {
		id = fmt.Sprintf("ap-test-%d", len(s.got))
	}
	return ApprovalOutcome{
		ApprovalID: id,
		Status:     "pending",
		ExpiresAt:  time.Now().Add(15 * time.Minute).Unix(),
		Summary:    "写动作已提交人工审批，尚未执行",
	}, nil
}