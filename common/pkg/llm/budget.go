package llm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tmnhs/crony/common/pkg/retry"
)

// Budget 会话级预算。零值表示"不限制"，但生产环境**不建议**不限制：
// 一个 ReAct 循环配上一个会反复调用工具的模型，可以在几分钟内烧掉一个月配额。
type Budget struct {
	// MaxCalls 最多调用次数（0 = 不限）。
	MaxCalls int
	// MaxTokens 最多 token 数（0 = 不限）。注意：部分自建端点不返回 usage，
	// 此时只能靠 MaxCalls 兜底——这也是必须同时限制调用次数的原因。
	MaxTokens int
	// MaxDuration 会话最长墙钟时长（0 = 不限）。
	MaxDuration time.Duration
}

// DefaultBudget 默认预算：**双闸门**（次数 + token），30 分钟墙钟。
func DefaultBudget() Budget {
	return Budget{MaxCalls: 20, MaxTokens: 200000, MaxDuration: 30 * time.Minute}
}

// Normalize 负值归零（0 表示不限）。
func (b Budget) Normalize() Budget {
	if b.MaxCalls < 0 {
		b.MaxCalls = 0
	}
	if b.MaxTokens < 0 {
		b.MaxTokens = 0
	}
	if b.MaxDuration < 0 {
		b.MaxDuration = 0
	}
	return b
}

// BudgetSnapshot 预算快照（落库 + 控制台展示"这次会话花了多少"）。
type BudgetSnapshot struct {
	Budget           Budget        `json:"budget"`
	Calls            int           `json:"calls"`
	Tokens           int           `json:"tokens"`
	PromptTokens     int           `json:"prompt_tokens"`
	CompletionTokens int           `json:"completion_tokens"`
	Elapsed          time.Duration `json:"elapsed"`
	RemainingCalls   int           `json:"remaining_calls"`
	RemainingTokens  int           `json:"remaining_tokens"`
	Exceeded         bool          `json:"exceeded"`
	Reason           string        `json:"reason,omitempty"`
}

// Tracker 预算记账器（并发安全）。
//
// ⚠️ 语义纠偏（2026-10-11 真实联调发现）：**预算是"一次 Agent 运行/一个会话"的，
// 不是"一个进程"的。** 早期实现把 Tracker 挂在 Gateway 上、网关又是进程级单例，
// 于是 `max-calls-per-session=8` 变成"整个 admin 进程一共只能调 8 次"，
// `max-duration-seconds=300` 变成"admin 启动 5 分钟后 Agent 永久不可用"——
// 实测表现：`force=true` 重诊断第一次报 "会话时长已达上限 5m0s"，
// 第二次起每次调用都失败，而重启 admin 又"好了"。
// 现在由入口层用 ContextWithTracker 为每次运行绑定独立记账器（见 gateway.Complete）。
type Tracker struct {
	mu         sync.Mutex
	budget     Budget
	calls      int
	tokens     int
	prompt     int
	completion int
	started    time.Time
	exceeded   bool
	reason     string
	now        func() time.Time
}

// trackerCtxKey 上下文键（私有类型，避免与其他包的键冲突）。
type trackerCtxKey struct{}

// ContextWithTracker 把记账器绑定到 ctx，供本次运行内的所有模型调用共用。
func ContextWithTracker(ctx context.Context, t *Tracker) context.Context {
	if ctx == nil || t == nil {
		return ctx
	}
	return context.WithValue(ctx, trackerCtxKey{}, t)
}

// TrackerFromContext 取出绑定的记账器；没有绑定返回 nil（调用方回退到网关默认）。
func TrackerFromContext(ctx context.Context) *Tracker {
	if ctx == nil {
		return nil
	}
	if t, ok := ctx.Value(trackerCtxKey{}).(*Tracker); ok {
		return t
	}
	return nil
}

// NewTracker 创建记账器。
func NewTracker(b Budget) *Tracker {
	return &Tracker{budget: b.Normalize(), started: time.Now(), now: time.Now}
}

// NewTrackerWithClock 注入时钟（测试用）。
func NewTrackerWithClock(b Budget, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{budget: b.Normalize(), started: now(), now: now}
}

// Check 判断当前是否还能继续调用。已耗尽返回 ErrBudgetExceeded（不可重试）。
func (t *Tracker) Check() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.checkLocked()
}

func (t *Tracker) checkLocked() error {
	if t.exceeded {
		return retry.Fatal(fmt.Errorf("%w: %s", ErrBudgetExceeded, t.reason))
	}
	return nil
}

// Charge 记账（在每次调用之后调用，无论成功失败都要记调用次数）。
//
// 语义说明：**超预算的这一次照样记账**——它确实花了钱；
// 只是下一次 Check 会拦住。反过来做（先扣后调）会在失败重试时重复扣。
func (t *Tracker) Charge(u Usage, d time.Duration) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	u = u.Normalize()
	t.calls++
	t.tokens += u.TotalTokens
	t.prompt += u.PromptTokens
	t.completion += u.CompletionTokens

	if t.budget.MaxCalls > 0 && t.calls >= t.budget.MaxCalls {
		t.exceeded = true
		t.reason = fmt.Sprintf("调用次数已达上限 %d/%d", t.calls, t.budget.MaxCalls)
	} else if t.budget.MaxTokens > 0 && t.tokens >= t.budget.MaxTokens {
		t.exceeded = true
		t.reason = fmt.Sprintf("token 用量已达上限 %d/%d", t.tokens, t.budget.MaxTokens)
	} else if t.budget.MaxDuration > 0 && t.now().Sub(t.started) >= t.budget.MaxDuration {
		t.exceeded = true
		t.reason = fmt.Sprintf("会话时长已达上限 %s", t.budget.MaxDuration)
	}
	if t.exceeded {
		return retry.Fatal(fmt.Errorf("%w: %s", ErrBudgetExceeded, t.reason))
	}
	return nil
}

// Snapshot 输出快照。
func (t *Tracker) Snapshot() BudgetSnapshot {
	if t == nil {
		return BudgetSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	snap := BudgetSnapshot{
		Budget:           t.budget,
		Calls:            t.calls,
		Tokens:           t.tokens,
		PromptTokens:     t.prompt,
		CompletionTokens: t.completion,
		Elapsed:          t.now().Sub(t.started),
		RemainingCalls:   -1,
		RemainingTokens:  -1,
		Exceeded:         t.exceeded,
		Reason:           t.reason,
	}
	if t.budget.MaxCalls > 0 {
		if r := t.budget.MaxCalls - t.calls; r > 0 {
			snap.RemainingCalls = r
		} else {
			snap.RemainingCalls = 0
		}
	}
	if t.budget.MaxTokens > 0 {
		if r := t.budget.MaxTokens - t.tokens; r > 0 {
			snap.RemainingTokens = r
		} else {
			snap.RemainingTokens = 0
		}
	}
	return snap
}

// Remaining 返回剩余次数与 token（-1 表示不限），供 Agent 循环在"还剩 1 步"时收尾。
func (t *Tracker) Remaining() (calls int, tokens int) {
	snap := t.Snapshot()
	return snap.RemainingCalls, snap.RemainingTokens
}

// 说明：这里**不做** context 携带预算的预留实现。
// 阶段 3 若要支持"人工点击的诊断给更高配额"，届时按请求显式传 Budget 更可靠
// （context 里藏配置会让"为什么这次能多花 5 倍钱"变得难以追溯）。
