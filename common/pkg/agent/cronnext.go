package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/jakecoffman/cron"
)

// NextTriggerCount 生成"接下来 N 次触发"时默认取几次。
//
// 为什么固定回显 5 次：cron 表达式最容易出的错不是语法错，而是**语义错**
// （"每天 2 点"写成了 "2 点每分钟"、"每月 1 号"写成了"每周 1"）。
// 让模型自报接下来 5 次具体时间，人一眼就能看出对不对——这比让运维读表达式可靠得多。
const NextTriggerCount = 5

// ErrInvalidCronSpec 无法解析/计算的 cron 表达式。
var ErrInvalidCronSpec = fmt.Errorf("agent: cron 表达式无法解析")

// ParseSpec 安全解析 cron 表达式。
//
// 为什么要包一层：`cron.Parse` 在表达式非法时**直接 panic**（内部是 log.Panicf），
// 而且 `spec[0]` 在空字符串上就会 panic。如果让模型产出的表达式直接进这个函数，
// 一个幻觉表达式就能把 node 进程打挂。这里把它转成普通错误。
func ParseSpec(spec string) (cron.Schedule, error) {
	clean := strings.TrimSpace(spec)
	if clean == "" {
		return nil, fmt.Errorf("%w: 表达式为空", ErrInvalidCronSpec)
	}
	if len(clean) > 128 {
		return nil, fmt.Errorf("%w: 表达式过长（%d 字符）", ErrInvalidCronSpec, len(clean))
	}
	var (
		sched cron.Schedule
		perr  error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				perr = fmt.Errorf("%w: %v", ErrInvalidCronSpec, r)
			}
		}()
		sched = cron.Parse(clean)
	}()
	if perr != nil {
		return nil, perr
	}
	if sched == nil {
		return nil, fmt.Errorf("%w: 解析结果为空", ErrInvalidCronSpec)
	}
	return sched, nil
}

// ValidateSpec 校验表达式并返回接下来 n 次触发时间（供人工确认）。
func ValidateSpec(spec string, from time.Time, n int) ([]time.Time, error) {
	if n <= 0 {
		n = NextTriggerCount
	}
	sched, err := ParseSpec(spec)
	if err != nil {
		return nil, err
	}
	if from.IsZero() {
		from = time.Now()
	}
	out := make([]time.Time, 0, n)
	cur := from
	for i := 0; i < n; i++ {
		next := nextAfter(sched, cur)
		if next.IsZero() {
			break
		}
		out = append(out, next)
		cur = next
	}
	if len(out) < n {
		return out, fmt.Errorf("%w: 只能推算 %d 次（要求 %d 次）", ErrInvalidCronSpec, len(out), n)
	}
	return out, nil
}

// nextAfter 求下一次触发，带 panic 兜底。
//
// Next 内部同样可能 panic（Desugar/边界计算），而且某些表达式在特定时间点
// 会返回零值。两者都必须变成可处理的情况，而不是崩掉调用方。
func nextAfter(sched cron.Schedule, cur time.Time) (next time.Time) {
	defer func() {
		if r := recover(); r != nil {
			next = time.Time{}
		}
	}()
	return sched.Next(cur)
}

// FormatTriggers 生成人类可读的触发时间列表。
func FormatTriggers(times []time.Time, loc *time.Location) []string {
	out := make([]string, 0, len(times))
	for i, t := range times {
		if loc != nil {
			t = t.In(loc)
		}
		out = append(out, fmt.Sprintf("%d) %s", i+1, t.Format("2006-01-02 15:04:05 MST")))
	}
	return out
}

// TriggerEcho 生成"接下来 5 次触发"的确认文本。
//
// 这段文本会同时出现在 API 响应和前端确认弹窗里，是"人工确认"环节的核心材料。
func TriggerEcho(spec string, times []time.Time, loc *time.Location) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("表达式 %q 接下来 %d 次触发时间：\n", spec, len(times)))
	for _, line := range FormatTriggers(times, loc) {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString("确认无误后再创建任务；确认前不会写入调度器。")
	return b.String()
}
