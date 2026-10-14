// Package cronspec 是 cron 表达式「几段」这件事的唯一事实来源。
//
// # 为什么需要这个包
//
// 本系统调度库是 jakecoffman/cron（兼容 robfig v1）：它把 **5 段**表达式解释为
// 「秒 分 时 日 月」，**不是** Unix crontab 的「分 时 日 月 周」。
// 于是 "0 3 * * *"（本意"每天 03:00"）的真实含义是"每小时第 3 分钟"：
// 语法合法、创建成功、无任何告警，只在**运行频率**上错。这类错误最难发现。
//
// 真实事故（2026-10-14 在真集群上踩到）：控制台表单的默认值就是 "0 3 * * *"，
// 4 个 e2e 任务照抄默认值创建，结果每小时重跑一次：把成功率、SLO 违约、
// 自愈放量一起污染成噪声（"失败 12 条"里混着这些）。
//
// # 规则
//
//   - 写入（创建/更新任务）：要求 **6 段** 或 @daily/@hourly/@every 等描述符，
//     否则拒绝并给出**可操作**的报错（含段数、真实语义、6 段写法示例）。
//     拒绝而不是"自动补一段"：静默改写用户意图和把 RetryTimes=0 改成 1 是同一种错。
//   - 调度侧（node）：对历史遗留的 5 段表达式**不改变行为**（否则升级会静默停掉
//     在跑的任务），但用 FiveSegmentsWarning 在日志里明确点出来，让存量配置可见。
package cronspec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jakecoffman/cron"
)

// Segments 是本系统要求的段数：秒 分 时 日 月 周。
//
// 为什么固定 6 段而不是"跟随 Unix 的 5 段"：调度库的语义是 6 段，
// 而它宁可把 5 段错位解析也不报错。要求 6 段是唯一能消除歧义的做法。
const Segments = 6

// MaxLength 与 agent.ParseSpec 保持一致的表达式长度上限。
const MaxLength = 128

// ErrInvalid 无法解析或段数不符的 cron 表达式。
//
// 调用方用 errors.Is(err, cronspec.ErrInvalid) 判断"这是表达式的问题"，
// 与"数据库挂了"之类的错误区分开。
var ErrInvalid = errors.New("cron 表达式不合法")

// Example 是"每天 03:00"的 6 段写法，用于报错与提示文案（避免各写各的示例）。
const Example = "0 0 3 * * *"

// IsDescriptor 判断是否为 @daily/@hourly/@every 1h30m 这类描述符。
func IsDescriptor(spec string) bool {
	return strings.HasPrefix(strings.TrimSpace(spec), "@")
}

// Count 返回段数；描述符与空串返回 0。
func Count(spec string) int {
	clean := strings.TrimSpace(spec)
	if clean == "" || IsDescriptor(clean) {
		return 0
	}
	return len(strings.Fields(clean))
}

// Parse 安全解析表达式：把底层库的 panic（log.Panicf）、空串、超长都转成普通错误。
//
// 为什么不直接调用 cron.Parse：一个模型幻觉或运维手误产出的表达式就能把
// node 进程打挂（Parse 内部 log.Panicf，且 spec[0] 在空串上就 panic）。
func Parse(spec string) (cron.Schedule, error) {
	clean := strings.TrimSpace(spec)
	if clean == "" {
		return nil, fmt.Errorf("%w：表达式为空", ErrInvalid)
	}
	if len(clean) > MaxLength {
		return nil, fmt.Errorf("%w：表达式过长（%d 字符，上限 %d）", ErrInvalid, len(clean), MaxLength)
	}
	var (
		sched cron.Schedule
		perr  error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				perr = fmt.Errorf("%w：%v", ErrInvalid, r)
			}
		}()
		sched = cron.Parse(clean)
	}()
	if perr != nil {
		return nil, perr
	}
	if sched == nil {
		return nil, fmt.Errorf("%w：解析结果为空", ErrInvalid)
	}
	return sched, nil
}

// Validate 写入路径的严格校验：必须是描述符，或恰好 6 段「秒 分 时 日 月 周」且可解析。
//
// 报错文案要能让不看文档的人自己改对：说清"你写了几段""本系统怎么理解这几段"
// "正确的 6 段长什么样"。只回一句 "invalid cron spec" 等于把问题丢回给运维。
func Validate(spec string) error {
	clean := strings.TrimSpace(spec)
	if clean == "" {
		return fmt.Errorf("%w：表达式为空", ErrInvalid)
	}
	if !IsDescriptor(clean) {
		if n := Count(clean); n != Segments {
			return fmt.Errorf("%w：%q 只有 %d 段，本系统要求 %d 段「秒 分 时 日 月 周」%s",
				ErrInvalid, clean, n, Segments, semanticsNote(clean, n))
		}
	}
	if _, err := Parse(clean); err != nil {
		return err
	}
	return nil
}

// FiveSegmentsWarning 返回 5 段表达式的告警文案；不是 5 段则返回空串。
//
// 调度侧用它写日志（行为不变、只让存量问题可见），写入侧不用它（写入侧直接拒绝）。
func FiveSegmentsWarning(spec string) string {
	clean := strings.TrimSpace(spec)
	if clean == "" || IsDescriptor(clean) || Count(clean) != 5 {
		return ""
	}
	return fmt.Sprintf("cron 表达式 %q 只有 5 段：本系统按「秒 分 时 日 月」解释%s，"+
		"与 Unix crontab 的「分 时 日 月 周」不同；请到控制台改成 6 段（例如 %s）",
		clean, semanticsNote(clean, 5), Example)
}

// semanticsNote 说明"这几段在本系统的真实含义"，帮助对方判断自己写错了什么。
//
// 只在能明确表述时给具体含义（5 段且前 3 段都是纯数字/weekday 之外的整数），
// 否则退回字段对齐说明（说错含义比不说更糟，运维会照着错的改）。
func semanticsNote(spec string, n int) string {
	if n != 5 {
		return ""
	}
	f := strings.Fields(spec)
	if len(f) != 5 {
		return ""
	}
	// 5 段在本库 = 秒 分 时 日 月
	sec, minute, hour := f[0], f[1], f[2]
	fix := fmt.Sprintf("；例如「每天 03:00」应写 %q", Example)
	switch {
	case isInt(sec) && isInt(minute) && hour == "*":
		// 最常见的踩坑形态："0 3 * * *" → 每小时第 3 分钟（而不是每天 03:00）
		return fmt.Sprintf("（%s 在本系统的实际语义是「每小时第 %s 分钟的第 %s 秒」）%s",
			spec, minute, sec, fix)
	case isInt(sec) && isInt(minute) && isInt(hour):
		return fmt.Sprintf("（%s 在本系统的实际语义是「每天 %s 点 %s 分 %s 秒」）%s",
			spec, hour, minute, sec, fix)
	default:
		return "：第 1 段是秒、第 2 段是分、第 3 段是时——不是 Unix 的第 1 段=分" + fix
	}
}

// isInt 只认纯数字：* / */5 / 1-5 这类都走通用说明分支。
func isInt(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
