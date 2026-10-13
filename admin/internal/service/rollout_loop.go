package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/logger"
)

// 阶段 5 补全：放量自动降级的常驻循环。
//
// 为什么需要它：`RolloutEvaluate` 的判定本来就是全自动的，但在此之前只能靠人/脚本
// 调 `POST /ops/rollout/evaluate` 驱动 —— "坏信号自动收回 L0" 里的"自动"名不副实。
// 本循环把它挂进与自愈 / SLO 同一套 ticker 模式，让坏信号出现后**不需要任何人拍板**
// 就会被收回。判定与落库仍完全复用服务层，这里只负责"什么时候敲一下"。
//
// 与另外两个循环对齐的两条做法：
//   - **每轮重读配置**（与 self-heal 一致）：改配置应当在不重启的情况下生效，
//     否则"先把自动降级关掉"会变成一次重启流程；
//   - **跨进程退避靠库**（与 SLO 违约处置一致）：admin 与 worker 都跑时，
//     靠 rollout_state.tightened_at 去重，而不是各自内存里的时间戳（重启即丢）。

// InitRolloutAutoEval 装配并启动自动降级循环（admin 与 worker 共用）。
//
// 循环**始终启动**（即使开关关着）：关着时每轮只做守卫判断并安静跳过，
// 这样运维能先在 `/ops/rollout/state` 的 `auto_eval_skip_reason` 里看到
// "打开之后会被什么挡住"，而不是靠承诺。
func InitRolloutAutoEval(closeRef *chan struct{}) {
	interval := RolloutAutoEvalInterval()
	cooldown := RolloutAutoEvalCooldown()
	if closeRef != nil {
		*closeRef = RunRolloutAutoEvalLoop(interval, cooldown)
	}
	logger.GetLogger().Info(fmt.Sprintf("[rollout] 自动降级循环已就绪：enabled=%v 间隔=%s 冷却=%s",
		RolloutAutoEvalEnabled(), interval, cooldown))
}

// RunRolloutAutoEvalLoop 周期评估放量档位（与 RunSelfHealLoop 同一模式：ticker + close 通道）。
func RunRolloutAutoEvalLoop(interval, cooldown time.Duration) chan struct{} {
	if interval <= 0 {
		interval = DefaultRolloutAutoEvalInterval
	}
	t := time.NewTicker(interval)
	closed := make(chan struct{})
	go func() {
		for {
			select {
			case <-t.C:
				RolloutAutoEvalOnce(RolloutEnv(""))
			case <-closed:
				t.Stop()
				return
			}
		}
	}()
	return closed
}

// RolloutAutoEvalOnce 跑一轮自动降级：守卫 → 采集证据 → 命中坏信号则收回 L0。
//
// 单独抽成可直接调用的函数（而不是只藏在 ticker 里）：定时那一层与判定这一层分开，
// 测试就能直接驱动"一轮"，不必等 ticker 到点。返回 tightened=true 表示真的收紧了。
func RolloutAutoEvalOnce(env string) (tightened bool, skipReason string) {
	enabled := RolloutAutoEvalEnabled()
	st, err := DefaultRolloutService.GetRolloutState(env)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[rollout] 自动降级读取状态失败：%v", err))
		return false, ""
	}
	if skip, reason := AutoEvalSkip(enabled, st, time.Now(), RolloutAutoEvalCooldown()); skip {
		return false, reason
	}
	row, ev, reasons, tightened, err := DefaultRolloutService.RolloutEvaluate(env)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[rollout] 自动降级评估失败：%v", err))
		return false, ""
	}
	if !tightened {
		return false, "没有坏信号"
	}
	stage := ""
	if row != nil {
		stage = row.Stage
	}
	logger.GetLogger().Warn(fmt.Sprintf(
		"[rollout] 自动降级：env=%s -> %s（%s；证据：评测 run_id=%d 失败=%d，SLO 违约=%d/%d）",
		RolloutEnv(env), stage, strings.Join(reasons, "；"),
		ev.EvalRunID, ev.EvalFailed, ev.SloBreaches, ev.SloTotal))
	return true, ""
}
