package llm

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/retry"
)

// countingProvider 每次调用成功返回一段固定 JSON，用于验证"预算记在谁头上"。
type countingProvider struct {
	name  string
	mu    sync.Mutex
	calls int
}

func (p *countingProvider) Name() string { return p.name }

func (p *countingProvider) Complete(ctx context.Context, req Request) (Completion, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return Completion{Provider: p.name, Model: "fake", Content: `{"ok":true}`, Usage: Usage{TotalTokens: 10}}, nil
}

func (p *countingProvider) callsMade() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestGatewayBudgetFromContextIsPerRun 锁定一个真实缺陷（2026-10-11 联调发现）。
//
// 缺陷原状：Tracker 挂在**进程级单例** Gateway 上，于是
//   - `max-calls-per-session` 变成"进程一共只能调 N 次"；
//   - `max-duration-seconds` 变成"admin 启动 N 秒后 Agent 永久不可用"。
//
// 实测表现：force=true 重诊断第一次报 "会话时长已达上限 5m0s"，重启 admin 就好了。
//
// 正确语义：每次 Agent 运行（会话）绑定自己的记账器，互不消耗。
func TestGatewayBudgetFromContextIsPerRun(t *testing.T) {
	p := &countingProvider{name: "fake"}
	g := NewGateway(WithProvider(p), WithBudget(Budget{MaxCalls: 2}))

	call := func(ctx context.Context) error {
		_, err := g.Complete(ctx, Request{Model: "fake"})
		return err
	}

	// 第 1 次运行：用满自己的 2 次配额。
	run1 := ContextWithTracker(context.Background(), NewTracker(Budget{MaxCalls: 2}))
	if err := call(run1); err != nil {
		t.Fatalf("运行 1 第 1 次调用不该失败: %v", err)
	}
	if err := call(run1); err != nil {
		t.Fatalf("运行 1 第 2 次调用不该失败: %v", err)
	}
	// 第 3 次必须被自己的预算拦住（会话内限额仍然有效）。
	err := call(run1)
	if err == nil {
		t.Fatal("运行 1 超出 MaxCalls=2 后必须被拒绝")
	}
	if !strings.Contains(err.Error(), "调用次数已达上限") {
		t.Fatalf("错误信息应说明次数超限，实际: %v", err)
	}
	if retry.Classify(err).Retryable {
		t.Fatalf("预算耗尽必须是不可重试错误，实际: %v", err)
	}

	// 关键断言：第 2 次运行拿到**全新配额**，不被第 1 次消耗掉。
	run2 := ContextWithTracker(context.Background(), NewTracker(Budget{MaxCalls: 2}))
	if err := call(run2); err != nil {
		t.Fatalf("运行 2 应拿到独立预算，却被上面的运行拖累: %v", err)
	}
	if err := call(run2); err != nil {
		t.Fatalf("运行 2 第 2 次调用不该失败: %v", err)
	}
	if got := p.callsMade(); got != 4 {
		t.Fatalf("provider 实际被调用 4 次（2 次运行 × 2），实际 %d", got)
	}
}

// TestGatewayWithoutContextTrackerFallsBack 没有绑定 ctx 时仍走网关自身记账器。
//
// 这条保护测试/单次调用场景：行为与修复前一致（回退而非"不限额"）。
func TestGatewayWithoutContextTrackerFallsBack(t *testing.T) {
	p := &countingProvider{name: "fake"}
	g := NewGateway(WithProvider(p), WithBudget(Budget{MaxCalls: 1}))

	if _, err := g.Complete(context.Background(), Request{Model: "fake"}); err != nil {
		t.Fatalf("第 1 次调用不该失败: %v", err)
	}
	if _, err := g.Complete(context.Background(), Request{Model: "fake"}); err == nil {
		t.Fatal("网关自身 MaxCalls=1，第 2 次必须被拒绝（回退语义）")
	}
}

// TestGatewayConcurrentRunsDoNotShareBudget 两次并发运行的记账互不污染。
//
// 用 ctx 绑定（而不是"给网关换一个 tracker"）就是为了这条：
// 换 tracker 的做法在并发下会把 A 的消耗记到 B 头上。
func TestGatewayConcurrentRunsDoNotShareBudget(t *testing.T) {
	p := &countingProvider{name: "fake"}
	g := NewGateway(WithProvider(p), WithBudget(Budget{MaxCalls: 1}))

	var wg sync.WaitGroup
	errs := make([]error, 8)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := ContextWithTracker(context.Background(), NewTracker(Budget{MaxCalls: 1}))
			<-start
			_, errs[i] = g.Complete(ctx, Request{Model: "fake"})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发运行 %d 应各自拿到完整配额，实际: %v", i, err)
		}
	}
	if got := p.callsMade(); got != len(errs) {
		t.Fatalf("每次并发运行都应真正调用 provider，期望 %d 次，实际 %d", len(errs), got)
	}
}

// TestTrackerDurationBudgetCountsFromRunStart 时长预算从**运行开始**计时。
//
// 修复前从进程启动计时（Gateway 单例构造时刻），所以"跑了 5 分钟的 admin"
// 上任何新会话都会被判超时。
func TestTrackerDurationBudgetCountsFromRunStart(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// 模拟"进程已经跑了 1 小时"：进程级记账器早已超时。
	processTracker := NewTrackerWithClock(Budget{MaxDuration: 5 * time.Minute}, func() time.Time { return now })
	now = now.Add(time.Hour)
	if err := processTracker.Check(); err != nil {
		t.Fatalf("前置条件：进程级记账器在真正 Charge 过之前不应报超时: %v", err)
	}
	_ = processTracker.Charge(Usage{}, 0) // 触发时长判定 → 进程级记账器现在已耗尽
	if err := processTracker.Check(); err == nil {
		t.Fatal("前置条件：进程级记账器应已被时长判定耗尽")
	}

	// 新运行绑定新记账器：即使"进程已跑 1 小时"，这次运行也还有完整 5 分钟。
	runCtx := ContextWithTracker(context.Background(), NewTrackerWithClock(Budget{MaxDuration: 5 * time.Minute}, func() time.Time { return now }))
	if got := TrackerFromContext(runCtx); got == nil {
		t.Fatal("ctx 里应能取回本次运行的记账器")
	} else if err := got.Check(); err != nil {
		t.Fatalf("新运行不该继承进程级记账器的超时状态: %v", err)
	}
}
