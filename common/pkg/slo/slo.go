// Package slo 实现阶段 4-D 的「SLO 契约」评估内核。
//
// 现状：crony 只保证"按时触发"，不保证"结果"。一个任务可以连续失败 100 次，
// 系统除了发个失败通知之外没有任何"对结果负责"的机制。SLO 契约要补的就是这一层：
// 任务/分组声明**成功率**与**准时率**目标，违约时不是发告警，而是触发一次诊断
// （见方案 §8-7 SLO 契约驱动的运维）。
//
// 本包只做**评估**（纯函数）：给定目标与一组执行观测，算出指标、判定是否违约、给出原因。
// 数据来源与"违约后怎么办"由调用方负责。
package slo

import (
	"strings"
	"time"
)

// Targets 一个 SLO 契约的目标。
type Targets struct {
	// TargetSuccessRate 目标成功率（0~1）。
	TargetSuccessRate float64
	// TargetOnTimeRate 目标准时率（0~1）。
	TargetOnTimeRate float64
	// Window 统计窗口。
	Window time.Duration
	// Grace 准时的宽限：实际开始时间晚于计划时间超过 Grace 即算"不准时"。
	Grace time.Duration
}

// DefaultGrace 默认宽限：计划时刻起 5 分钟内开始即算准时。
// 为什么需要宽限而不是"零容忍"：调度抖动、节点注册、秒级 cron 的边界都会带来秒级偏差，
// 用 0 宽限会让准时率永远不达标（噪声淹没信号）。
const DefaultGrace = 5 * time.Minute

// Observation 一次执行的观测（评估的最小事实单元）。
type Observation struct {
	RunID string
	// Status 见 models.RunStatus 的取值（2=success）。
	Status int
	// ScheduledTime 计划触发时刻（unix 秒）；0 表示无计划时间（如手动触发），准时率不计它。
	ScheduledTime int64
	// StartedAt 实际开始时刻（unix 秒）。
	StartedAt int64
	// FinishedAt 结束时刻（unix 秒）；0 表示未结束（不进统计）。
	FinishedAt int64
}

// Report 一次 SLO 评估结果（可归档、可解释）。
type Report struct {
	// Total 计入统计的执行数（只含已结束的）。
	Total int `json:"total"`
	// Success 成功数（status=success）。
	Success int `json:"success"`
	// OnTime 准时数（在宽限内开始的有计划时间执行）。
	OnTime int `json:"on_time"`
	// OnTimeEligible 有资格计准时率的执行数（有计划时间且已结束）。
	OnTimeEligible int `json:"on_time_eligible"`
	// SuccessRate / OnTimeRate 实际值；分母为 0 时为 -1（表示"没样本"）。
	SuccessRate float64 `json:"success_rate"`
	OnTimeRate  float64 `json:"on_time_rate"`
	// Breached 是否违约（任一目标未达标）。
	Breached bool `json:"breached"`
	// Reasons 违约原因（人可读，逐条说明哪个目标、差多少）。
	Reasons []string `json:"reasons,omitempty"`
	// Unverifiable 无样本时无法评估（**不当作违约**，也**不当作达标**）。
	Unverifiable bool `json:"unverifiable"`
}

// Evaluate 评估：给定目标与观测，产出报告。
//
// 口径纪律：
//   - 只统计**已结束**的执行（FinishedAt>0）——还在跑的不进分母；
//   - 成功 = status==2（与 models.RunStatusSuccess 一致）；
//   - 准时率只统计"有计划时间"的执行（手动/API 触发没有计划时间，不该拉低准时率）；
//   - **无样本时 Unverifiable=true**，既不判违约也不判达标——这是本包最重要的一条纪律：
//     把"没数据"读成"违约"会造成误报风暴，读成"达标"会掩盖真实故障。
func Evaluate(t Targets, obs []Observation) Report {
	grace := t.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	r := Report{}
	for _, o := range obs {
		if o.FinishedAt <= 0 {
			continue // 未结束：不进统计
		}
		r.Total++
		if o.Status == 2 {
			r.Success++
		}
		if o.ScheduledTime > 0 && o.StartedAt > 0 {
			r.OnTimeEligible++
			if o.StartedAt-o.ScheduledTime <= int64(grace.Seconds()) {
				r.OnTime++
			}
		}
	}
	r.SuccessRate = rate(r.Success, r.Total)
	r.OnTimeRate = rate(r.OnTime, r.OnTimeEligible)

	if r.Total == 0 {
		r.Unverifiable = true
		r.Reasons = append(r.Reasons, "窗口内没有已结束的执行：无样本，SLO 无法评估（不判违约）")
		return r
	}
	if t.TargetSuccessRate > 0 && r.SuccessRate < t.TargetSuccessRate {
		r.Breached = true
		r.Reasons = append(r.Reasons, "成功率 "+pct(r.SuccessRate)+" 低于目标 "+pct(t.TargetSuccessRate))
	}
	// 准时率：只有在**有资格样本**时才判（没计划时间的任务不该因准时率被判违约）。
	if t.TargetOnTimeRate > 0 && r.OnTimeEligible > 0 && r.OnTimeRate < t.TargetOnTimeRate {
		r.Breached = true
		r.Reasons = append(r.Reasons, "准时率 "+pct(r.OnTimeRate)+" 低于目标 "+pct(t.TargetOnTimeRate))
	}
	return r
}

// rate 计算比率；分母为 0 返回 -1（区别于"真的 0%"）。
func rate(num, denom int) float64 {
	if denom <= 0 {
		return -1
	}
	return float64(num) / float64(denom)
}

func pct(r float64) string {
	if r < 0 {
		return "无样本"
	}
	n := int(r*100 + 0.5)
	return itoa(n) + "%"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return strings.TrimSpace(string(b[i:]))
}
