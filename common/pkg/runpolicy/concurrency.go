// Package runpolicy 提供"同任务并发执行策略"与"幂等去重"的纯决策逻辑。
//
// 旧实现完全没有防重入：一个每分钟触发的任务，如果单次执行超过一分钟，
// 就会出现同一任务多份并发执行（互相覆盖输出、争抢资源）。
// 这里把策略枚举化，默认 Skip（与 robfig/cron 的 SkipIfStillRunning 语义一致）。
package runpolicy

import (
	"fmt"
	"time"
)

// ConcurrencyPolicy 并发策略枚举。
type ConcurrencyPolicy int

const (
	// ConcurrencySkip 默认：上一次还在跑就跳过本次触发（并记录一条 skipped 执行）。
	ConcurrencySkip = ConcurrencyPolicy(0)
	// ConcurrencyBufferOne 排队一次：若已有 1 个在跑，本次进入缓冲；再触发则丢弃。
	ConcurrencyBufferOne = ConcurrencyPolicy(1)
	// ConcurrencyBufferAll 全部排队（需配合 MaxBuffer 上限，防止无界堆积）。
	ConcurrencyBufferAll = ConcurrencyPolicy(2)
	// ConcurrencyCancelOther 允许并发，但取消上一次执行后启动本次（"最新优先"）。
	ConcurrencyCancelOther = ConcurrencyPolicy(3)
	// ConcurrencyTerminateOther 允许并发，且强制终止上一次执行（含进程树）后启动本次。
	ConcurrencyTerminateOther = ConcurrencyPolicy(4)
)

var concurrencyPolicyNames = map[ConcurrencyPolicy]string{
	ConcurrencySkip:           "skip",
	ConcurrencyBufferOne:      "buffer_one",
	ConcurrencyBufferAll:      "buffer_all",
	ConcurrencyCancelOther:    "cancel_other",
	ConcurrencyTerminateOther: "terminate_other",
}

func (p ConcurrencyPolicy) String() string {
	if name, ok := concurrencyPolicyNames[p]; ok {
		return name
	}
	return fmt.Sprintf("unknown(%d)", int(p))
}

// ParseConcurrencyPolicy 解析配置/接口传入的字符串（大小写不敏感），
// 非法值返回 false，调用方应回退到默认策略而不是静默放行。
func ParseConcurrencyPolicy(raw string) (ConcurrencyPolicy, bool) {
	for policy, name := range concurrencyPolicyNames {
		if equalFold(name, raw) {
			return policy, true
		}
	}
	return ConcurrencySkip, false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// Action 调度器在触发时刻应当采取的动作。
type Action int

const (
	// ActionRun 直接执行。
	ActionRun = Action(0)
	// ActionSkip 丢弃本次触发（记录 skipped 执行以便审计"为什么没跑"）。
	ActionSkip = Action(1)
	// ActionBuffer 排队等待（当前有执行在跑）。
	ActionBuffer = Action(2)
	// ActionCancelOthers 取消其它在跑的执行后启动本次。
	ActionCancelOthers = Action(3)
	// ActionTerminateOthers 强制终止其它执行后启动本次。
	ActionTerminateOthers = Action(4)
)

func (a Action) String() string {
	switch a {
	case ActionRun:
		return "run"
	case ActionSkip:
		return "skip"
	case ActionBuffer:
		return "buffer"
	case ActionCancelOthers:
		return "cancel_others"
	case ActionTerminateOthers:
		return "terminate_others"
	default:
		return fmt.Sprintf("unknown(%d)", int(a))
	}
}

// DefaultMaxBuffer 缓冲队列上限（buffer_all 策略），防止无界堆积。
const DefaultMaxBuffer = 16

// Decision 决策结果。
type Decision struct {
	Action Action
	Reason string
}

// Decide 根据策略、当前在跑数量与缓冲中数量给出动作。
//
// active = 该任务当前正在执行的实例数；buffered = 已在缓冲队列中等待的实例数。
// maxBuffer <= 0 时使用 DefaultMaxBuffer。
func Decide(policy ConcurrencyPolicy, active, buffered, maxBuffer int) Decision {
	if active < 0 {
		active = 0
	}
	if buffered < 0 {
		buffered = 0
	}
	if maxBuffer <= 0 {
		maxBuffer = DefaultMaxBuffer
	}
	if active == 0 {
		return Decision{Action: ActionRun, Reason: "无进行中的执行"}
	}
	switch policy {
	case ConcurrencyBufferOne:
		if buffered == 0 {
			return Decision{Action: ActionBuffer, Reason: "已有执行进行中，缓冲一次"}
		}
		return Decision{Action: ActionSkip, Reason: "缓冲已有一个执行，丢弃本次触发"}
	case ConcurrencyBufferAll:
		if buffered >= maxBuffer {
			return Decision{Action: ActionSkip, Reason: fmt.Sprintf("缓冲队列已满(%d)", maxBuffer)}
		}
		return Decision{Action: ActionBuffer, Reason: "已有执行进行中，进入缓冲队列"}
	case ConcurrencyCancelOther:
		return Decision{Action: ActionCancelOthers, Reason: "策略：取消旧执行，运行最新一次"}
	case ConcurrencyTerminateOther:
		return Decision{Action: ActionTerminateOthers, Reason: "策略：强制终止旧执行，运行最新一次"}
	default: // ConcurrencySkip
		return Decision{Action: ActionSkip, Reason: "策略：上一次执行尚未结束，跳过本次"}
	}
}

// Slot 调度槽位：把触发时间对齐到 interval 的整数倍，
// 用于生成幂等键（同一槽位只允许一次执行）。
//
// interval <= 0 直接返回原始时间戳。
func Slot(t time.Time, interval time.Duration) int64 {
	if interval <= 0 {
		return t.Unix()
	}
	return t.Unix() - t.Unix()%int64(interval.Seconds())
}

// SameSlot 判断两个槽位是否相同，并给出容差窗口。
//
// 场景：节点时钟有几百毫秒漂移、或任务在两个节点上几乎同时触发，
// 允许 window 内的抖动仍判定为同一槽位（去重）。
func SameSlot(a, b int64, window time.Duration) bool {
	if a == b {
		return true
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= int64(window.Seconds())
}
