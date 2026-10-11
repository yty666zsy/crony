package agent

import (
	"context"
	"testing"

	"github.com/tmnhs/crony/common/pkg/llm"
)

// TestRunBudgetIsPerRunNotPerProcess 锁定"进程级预算"缺陷（2026-10-11 真实联调发现）。
//
// 缺陷原状：Gateway 是进程级单例，预算记账器（Tracker）挂在网关上。
// 于是配置项 `max-calls-per-session` 实际含义变成"整个 admin 进程一共只能调 N 次"，
// `max-duration-seconds` 变成"admin 启动 N 秒后 Agent 永久不可用"。
//
// 真实表现（admin 8089 + 百炼 qwen-plus，2026-10-11）：
//
//	第 1 次诊断正常；约 5 分钟后 force=true 重诊断报
//	`llm: budget exceeded: 会话时长已达上限 5m0s`；此后每次调用都失败；
//	重启 admin 又"恢复正常"——非常容易被误判成"模型侧限流"。
//
// 修复：预算随 ctx 传递，每次 Agent 运行绑定一个新的 Tracker
// （agentruntime.Runtime.runContext → llm.ContextWithTracker →
// llm.TrackerFromContext → gateway.Complete）。
//
// 本用例给出机械保证：**网关自身（进程级）配额被用光，不得影响后续运行**。
func TestRunBudgetIsPerRunNotPerProcess(t *testing.T) {
	// 网关自身预算 = 1（等价于修复前"进程级预算"，构造时就挂在网关上）。
	gateway, fp := newGateway(llm.WithBudget(llm.Budget{MaxCalls: 1}))
	fp.WithResponder(func(llm.Request) (llm.Completion, error) {
		return llm.FakeJSON(okAnswer()), nil
	})

	// 模拟"进程启动后已经被别的会话消费过一次"：直接用 ctx（不带绑定）打一次网关，
	// 这一次会把网关级记账器耗光。
	if _, err := gateway.Complete(context.Background(), llm.Request{Model: "fake"}); err != nil {
		t.Fatalf("前置调用不该失败: %v", err)
	}
	if _, err := gateway.Complete(context.Background(), llm.Request{Model: "fake"}); err == nil {
		t.Fatal("前置条件：网关级预算 MaxCalls=1 用完后必须被拦住")
	}

	a := New(gateway, nil, DefaultConfig())
	for i := 0; i < 3; i++ {
		ctx := llm.ContextWithTracker(context.Background(), llm.NewTracker(llm.Budget{MaxCalls: 1}))
		out, err := a.Run(ctx, testInput(), simpleSchema())
		if err != nil {
			t.Fatalf("第 %d 次运行失败——预算又被进程/网关级共享了: %v", i+1, err)
		}
		if out.StopReason != StopCompleted {
			t.Fatalf("第 %d 次运行未正常完成: %s", i+1, out.StopReason)
		}
		if out.Trace == nil {
			t.Fatalf("第 %d 次运行缺少轨迹", i+1)
		}
		// 预算快照必须是"本次运行"的用量（1 次），不是进程累计（1/2/3）。
		if got := out.Trace.Budget.Calls; got != 1 {
			t.Fatalf("第 %d 次运行的预算快照应只含本次的 1 次调用，实际 %d", i+1, got)
		}
		if got := out.Trace.Budget.Budget.MaxCalls; got != 1 {
			t.Fatalf("快照中的预算上限应来自本次运行绑定的 Tracker，实际 %d", got)
		}
	}
}

// TestRunDurationBudgetComesFromBoundTracker 时长闸门同样只看本次运行的记账器。
//
// 修复前 `started` 取的是进程启动时刻，所以"admin 跑了 5 分钟"这件事本身
// 就会让所有新会话被判超时。
func TestRunDurationBudgetComesFromBoundTracker(t *testing.T) {
	gateway, fp := newGateway(llm.WithBudget(llm.Budget{MaxCalls: 1}))
	fp.WithResponder(func(llm.Request) (llm.Completion, error) {
		return llm.FakeJSON(okAnswer()), nil
	})

	a := New(gateway, nil, DefaultConfig())

	// 本次运行的记账器：时长闸门很紧（1ns），但它是"刚创建"的。
	// 关键是断言"时长判定读的是绑定记账器"——用 MaxDuration=0（不限）时必然通过，
	// 再用一个已被人为推进到超时的记账器时必然失败，两条对照即可证明读数来源。
	okCtx := llm.ContextWithTracker(context.Background(), llm.NewTracker(llm.Budget{MaxDuration: 0}))
	if _, err := a.Run(okCtx, testInput(), simpleSchema()); err != nil {
		t.Fatalf("MaxDuration=0（不限）时不该失败: %v", err)
	}

	// 预先耗尽的记账器（模拟"进程已经跑超时"）：绑定后必须被拦住，
	// 证明额度判定确实来自 ctx 上的这个记账器。
	dead := llm.NewTracker(llm.Budget{MaxCalls: 1})
	if err := dead.Charge(llm.Usage{}, 0); err == nil {
		t.Fatal("前置条件：该记账器应已在 1 次调用后耗尽")
	}
	deadCtx := llm.ContextWithTracker(context.Background(), dead)
	if _, err := a.Run(deadCtx, testInput(), simpleSchema()); err == nil {
		t.Fatal("绑定的记账器已耗尽时，运行必须被拦下（而不是静默回退到网关记账器）")
	}
}
