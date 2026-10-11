// Package retry 提供任务重试策略：指数退避 + 抖动 + 总时长上限，
// 以及"可重试错误 / 不可重试错误"的分类。
//
// 旧实现的重试语义（node/internal/handler/job.go）问题：
//   - 间隔固定，要么用户填的秒数，要么"第 n 次等 n 分钟"；
//   - 没有总时长上限，retry_times=100 会把一次失败拖成数小时的僵尸执行；
//   - 所有错误一律重试：脚本本身写错（语法错误）也会被重试 N 次，放大故障。
package retry

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrorDisposition 错误分类结果。
type ErrorDisposition struct {
	Retryable bool
	Reason    string
}

// classifiedError 把"是否可重试"附着在错误上，跨包传递不丢失语义。
type classifiedError struct {
	err       error
	retryable bool
	reason    string
}

func (e *classifiedError) Error() string { return e.err.Error() }
func (e *classifiedError) Unwrap() error { return e.err }

// Retryable 标记为可重试错误（瞬时故障：网络抖动、5xx、锁冲突）。
//
// 标记语义：**后标记者覆盖先标记者**。外层包装通常掌握更多上下文
// （例如执行器已判定"命令不存在"），因此外层判定优先。
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return &classifiedError{err: err, retryable: true, reason: "显式标记为可重试"}
}

// Fatal 标记为不可重试错误（配置错误、鉴权失败、脚本语法错误）。
func Fatal(err error) error {
	if err == nil {
		return nil
	}
	return &classifiedError{err: err, retryable: false, reason: "显式标记为不可重试"}
}

// Classify 判断一个错误是否应该重试。
//
// 缺省语义：**未被显式标记的错误视为可重试**（保守地保留旧行为，
// 避免升级后把原本会重试成功的瞬时故障变成一次性失败）；
// 明确标记为 Fatal 的错误、以及明确的"不可重试"信号（如 context 主动取消）不会重试。
func Classify(err error) ErrorDisposition {
	if err == nil {
		return ErrorDisposition{Retryable: false, Reason: "无错误"}
	}
	if errors.Is(err, ErrCanceled) {
		return ErrorDisposition{Retryable: false, Reason: "执行被取消"}
	}
	var classified *classifiedError
	if errors.As(err, &classified) {
		return ErrorDisposition{Retryable: classified.retryable, Reason: classified.reason}
	}
	return ErrorDisposition{Retryable: true, Reason: "未分类错误，按可重试处理"}
}

// 供执行器直接返回/包装的分类错误。
var (
	// ErrCanceled 执行被主动取消（不重试）。
	ErrCanceled = errors.New("execution canceled")
	// ErrFatal 表示"重试也不会好"的固定错误。
	ErrFatal = errors.New("fatal error")
)

// Policy 重试策略。
type Policy struct {
	// MaxAttempts 总尝试次数（含首次执行）。1 表示不重试。
	MaxAttempts int
	// InitialInterval 首次重试等待时长。
	InitialInterval time.Duration
	// MaxInterval 单次等待时长上限。
	MaxInterval time.Duration
	// Multiplier 退避倍数（<1 时按 1 处理）。
	Multiplier float64
	// Jitter 抖动比例 [0,1]：把等待时长在 [d*(1-jitter), d] 之间随机，
	// 避免大量任务失败后在同一个时刻一起重试（惊群/雪崩）。
	Jitter float64
	// MaxElapsed 从首次执行开始的总时长上限，超过则不再重试。
	MaxElapsed time.Duration
	// RetryOn 可选的自定义判定，覆盖默认 Classify。
	RetryOn func(error) bool
}

// DefaultPolicy 默认策略：最多 3 次尝试，2s 起退避、指数 2 倍、上限 30s、抖动 20%、总时长 5 分钟。
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts:     3,
		InitialInterval: 2 * time.Second,
		MaxInterval:     30 * time.Second,
		Multiplier:      2,
		Jitter:          0.2,
		MaxElapsed:      5 * time.Minute,
	}
}

// Normalize 把策略收敛到合法范围，避免 0/负数造成紧循环重试。
func (p Policy) Normalize() Policy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.MaxAttempts > 100 {
		p.MaxAttempts = 100
	}
	if p.InitialInterval < 0 {
		p.InitialInterval = 0
	}
	if p.MaxInterval <= 0 {
		p.MaxInterval = p.InitialInterval
	}
	if p.MaxInterval < p.InitialInterval {
		p.MaxInterval = p.InitialInterval
	}
	if p.Multiplier < 1 {
		p.Multiplier = 1
	}
	if p.Jitter < 0 {
		p.Jitter = 0
	}
	if p.Jitter > 1 {
		p.Jitter = 1
	}
	if p.MaxElapsed < 0 {
		p.MaxElapsed = 0
	}
	return p
}

// FromLegacy 把旧的 (retry_times, retry_interval 秒) 语义映射到新策略。
//
// 兼容规则（老任务零改动继续跑）：
//   - retry_times = 0            → MaxAttempts = 1（不重试）
//   - retry_times = N            → MaxAttempts = N+1
//   - retry_interval = 0（旧版本由代码补成"第 n 次等 n 分钟"）→ 退避 1 分钟起、2 倍、上限 10 分钟
//   - retry_interval > 0         → 固定间隔，抖动 10%
//   - MaxElapsed 固定为 30 分钟，防止"重试 100 次"变成僵尸执行
func FromLegacy(retryTimes int, retryIntervalSeconds int64) Policy {
	policy := DefaultPolicy()
	policy.MaxElapsed = 30 * time.Minute
	policy.Jitter = 0.1
	// 旧版本在 retry_interval = 0 时由代码补成"第 n 次等 n 分钟"，
	// 这里用 1 分钟起的指数退避表达同一意图，并补上单次上限。
	policy.InitialInterval = time.Minute
	policy.Multiplier = 2
	policy.MaxInterval = 10 * time.Minute

	if retryIntervalSeconds > 0 {
		policy.InitialInterval = time.Duration(retryIntervalSeconds) * time.Second
		policy.MaxInterval = policy.InitialInterval
		policy.Multiplier = 1
	}
	if retryTimes > 0 {
		policy.MaxAttempts = retryTimes + 1
	} else {
		policy.MaxAttempts = 1
	}
	return policy
}

// Backoff 计算第 attempt 次重试（attempt 从 1 开始）的等待时长。
//
// rand01 是 [0,1) 区间的随机数，由调用方注入，便于确定性测试：
// 抖动区间为 [d*(1-jitter), d]。
func (p Policy) Backoff(attempt int, rand01 float64) time.Duration {
	p = p.Normalize()
	if attempt < 1 {
		attempt = 1
	}
	base := float64(p.InitialInterval)
	if base <= 0 {
		return 0
	}
	factor := math.Pow(p.Multiplier, float64(attempt-1))
	d := base * factor
	if max := float64(p.MaxInterval); max > 0 && d > max {
		d = max
	}
	if p.Jitter > 0 {
		if rand01 < 0 {
			rand01 = 0
		}
		if rand01 >= 1 {
			rand01 = 0.999999
		}
		// 只向下抖动：保证不会超过 MaxInterval
		d = d * (1 - p.Jitter*rand01)
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}

// Decision 一次重试决策的结果。
type Decision struct {
	Retry       bool
	Delay       time.Duration
	NextAttempt int
	Reason      string
}

// Decide 根据当前尝试次数、已耗时与错误，决定是否重试以及等待多久。
//
// attempt 表示"刚刚失败的是第几次尝试"（从 1 开始）。
// elapsed 表示从首次执行到现在的总耗时。
func (p Policy) Decide(attempt int, elapsed time.Duration, err error) Decision {
	return p.DecideWithRand(attempt, elapsed, err, 0.5)
}

// DecideWithRand 与 Decide 相同，但允许注入 [0,1) 随机数，
// 使退避抖动可被确定性测试（生产代码调用 Decide 即可）。
func (p Policy) DecideWithRand(attempt int, elapsed time.Duration, err error, rand01 float64) Decision {
	p = p.Normalize()
	if err == nil {
		return Decision{Retry: false, Reason: "执行成功"}
	}
	if attempt >= p.MaxAttempts {
		return Decision{
			Retry:       false,
			NextAttempt: attempt,
			Reason:      fmt.Sprintf("已达最大尝试次数 %d", p.MaxAttempts),
		}
	}
	if p.MaxElapsed > 0 && elapsed >= p.MaxElapsed {
		return Decision{
			Retry:       false,
			NextAttempt: attempt,
			Reason:      fmt.Sprintf("已超过总时长上限 %s", p.MaxElapsed),
		}
	}
	retryable := true
	reason := "未分类错误，按可重试处理"
	if p.RetryOn != nil {
		retryable = p.RetryOn(err)
		if !retryable {
			reason = "自定义策略判定不重试"
		}
	} else {
		disposition := Classify(err)
		retryable = disposition.Retryable
		reason = disposition.Reason
	}
	if !retryable {
		return Decision{Retry: false, NextAttempt: attempt, Reason: reason}
	}
	delay := p.Backoff(attempt, rand01)
	if p.MaxElapsed > 0 {
		if remaining := p.MaxElapsed - elapsed; remaining <= 0 {
			return Decision{Retry: false, NextAttempt: attempt, Reason: "剩余时间预算不足"}
		} else if delay > remaining {
			delay = remaining
		}
	}
	return Decision{
		Retry:       true,
		Delay:       delay,
		NextAttempt: attempt + 1,
		Reason:      reason,
	}
}

// Snapshot 输出可落库的策略快照（用于复盘）。
func (p Policy) Snapshot() string {
	p = p.Normalize()
	return fmt.Sprintf(
		`{"max_attempts":%d,"initial_interval_ms":%d,"max_interval_ms":%d,"multiplier":%g,"jitter":%g,"max_elapsed_ms":%d}`,
		p.MaxAttempts, p.InitialInterval.Milliseconds(), p.MaxInterval.Milliseconds(),
		p.Multiplier, p.Jitter, p.MaxElapsed.Milliseconds())
}
