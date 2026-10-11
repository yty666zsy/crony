package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// StepCondition 步骤执行条件（阶段 1 的"线性步骤 + 条件"编排）。
//
// 语义（针对线性步骤序列）：
//   - on_success：之前步骤全部成功才执行（默认）；
//   - on_failure：之前有步骤失败才执行（典型用途：收集现场、发送告警、回滚）；
//   - always：无论如何都执行（典型用途：清理临时文件、释放锁）。
type StepCondition string

const (
	ConditionOnSuccess StepCondition = "on_success"
	ConditionOnFailure StepCondition = "on_failure"
	ConditionAlways    StepCondition = "always"
)

// ParseCondition 解析条件字符串（大小写不敏感），空串默认 on_success。
func ParseCondition(raw string) (StepCondition, bool) {
	switch StepCondition(strings.ToLower(strings.TrimSpace(raw))) {
	case "":
		return ConditionOnSuccess, true
	case ConditionOnSuccess:
		return ConditionOnSuccess, true
	case ConditionOnFailure:
		return ConditionOnFailure, true
	case ConditionAlways:
		return ConditionAlways, true
	default:
		return ConditionOnSuccess, false
	}
}

// Step 编排中的一个步骤。
type Step struct {
	Index   int
	Name    string
	Kind    Kind
	Request Request
	// Condition 见上；未设置视为 on_success。
	Condition StepCondition
	// ContinueOnFailure 为 true 时，本步骤失败后继续执行后续 on_success 步骤
	// （默认 false：成功链断掉，只保留 on_failure/always 步骤）。
	ContinueOnFailure bool
	// Policy 步骤级重试策略；nil 表示不重试（只执行一次）。
	Policy *retry.Policy
}

// Plan 一次执行的步骤编排。
type Plan struct {
	RunID string
	JobID int
	Steps []Step
}

// normalize 补齐步骤索引与默认条件，保证调用方漏填也能安全执行。
func (p *Plan) normalize() {
	for i := range p.Steps {
		if p.Steps[i].Condition == "" {
			p.Steps[i].Condition = ConditionOnSuccess
		}
		if p.Steps[i].Index == 0 {
			p.Steps[i].Index = i
		}
		if p.Steps[i].Kind == "" {
			p.Steps[i].Kind = p.Steps[i].Request.Kind
		}
		p.Steps[i].Request.Kind = p.Steps[i].Kind
		p.Steps[i].Request.RunID = p.RunID
		p.Steps[i].Request.JobID = p.JobID
		p.Steps[i].Request.StepIndex = p.Steps[i].Index
	}
}

// Runner 执行 Plan。所有外部依赖（时间、随机、休眠）都可注入，便于确定性测试。
type Runner struct {
	Registry *Registry
	// Now 当前时间（默认 time.Now）。
	Now func() time.Time
	// Sleep 休眠（默认 time.Sleep）；测试可替换为"立即返回并记录"。
	Sleep func(time.Duration)
	// Rand 返回 [0,1) 随机数（默认 0.5，等价于无抖动偏置）。
	Rand func() float64
	// OnStepStart / OnStepFinish 回调：用于落库 job_run_step 与实时推送前端。
	OnStepStart  func(step Step)
	OnStepFinish func(res Result)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	if r.Sleep != nil {
		r.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (r *Runner) rand() float64 {
	if r.Rand != nil {
		return r.Rand()
	}
	return 0.5
}

// Execute 顺序执行 Plan，返回每一步的结果（含被条件跳过的步骤，状态为 skipped）。
//
// 关键行为：
//  1. 条件步骤：on_success/on_failure/always 按"是否已出现失败"判定；
//  2. 步骤级重试：按 Policy 指数退避 + 抖动，超限或不可重试错误立即停止；
//  3. 失败传播：默认中断成功链（后续 on_success 步骤记为 skipped），
//     但 on_failure/always 步骤仍会执行——这是"失败也要收集现场/清理"的前提；
//  4. 取消传播：ctx 取消后不再开始新步骤，正在执行的步骤由执行器负责终止进程树。
func (r *Runner) Execute(ctx context.Context, plan Plan) []Result {
	plan.normalize()
	results := make([]Result, 0, len(plan.Steps))
	anyFailure := false
	aborted := false

	for _, step := range plan.Steps {
		if ctxErr := ctx.Err(); ctxErr != nil {
			results = append(results, skippedResult(step, fmt.Sprintf("执行已取消: %v", ctxErr)))
			continue
		}
		if !shouldRun(step, anyFailure, aborted) {
			results = append(results, skippedResult(step, skipReason(step, aborted)))
			continue
		}
		res := r.runStep(ctx, plan, step)
		results = append(results, res)
		if res.Status != models.RunStatusSuccess && res.Status != models.RunStatusSkipped {
			anyFailure = true
			if !step.ContinueOnFailure {
				aborted = true
			}
		}
	}
	return results
}

// shouldRun 判定步骤是否应执行。
//
// anyFailure 表示"之前出现过失败"（供 on_failure 使用）；
// aborted 表示"成功链已中断"（供 on_success 使用）。
// 两者必须分开：ContinueOnFailure=true 的步骤失败后 anyFailure 仍为真
// （on_failure 的收尾步骤该跑），但成功链并未中断。
func shouldRun(step Step, anyFailure, aborted bool) bool {
	switch step.Condition {
	case ConditionAlways:
		return true
	case ConditionOnFailure:
		return anyFailure
	default: // on_success
		return !aborted
	}
}

func skipReason(step Step, aborted bool) string {
	if step.Condition == ConditionOnFailure {
		return "无失败步骤，跳过失败处理步骤"
	}
	if aborted {
		return "前序步骤失败，成功链已中断"
	}
	return "前序步骤未满足执行条件"
}

func skippedResult(step Step, reason string) Result {
	now := time.Now()
	return Result{
		StepIndex:  step.Index,
		Name:       step.Name,
		Kind:       step.Kind,
		Status:     models.RunStatusSkipped,
		ExitCode:   -1,
		StartedAt:  now,
		FinishedAt: now,
		Artifacts:  map[string]string{"skip_reason": reason},
	}
}

// runStep 执行单个步骤（含步骤级重试）。
func (r *Runner) runStep(ctx context.Context, plan Plan, step Step) Result {
	if r.OnStepStart != nil {
		r.OnStepStart(step)
	}
	req := step.Request
	if req.Timeout <= 0 && step.Kind == KindShell {
		// 未指定超时时使用执行器兜底值（30min），避免"永不超时"的僵尸步骤
		req.Timeout = DefaultTimeout
	}

	var (
		last       Result
		attempt    = 1
		startedAt  = r.now()
		retryCount = 0
	)
	registry := r.Registry
	if registry == nil {
		registry = DefaultRegistry()
	}

	for {
		last = registry.Run(ctx, req)
		last.RetryCount = retryCount

		if last.Status == models.RunStatusSuccess || step.Policy == nil {
			break
		}
		policy := step.Policy.Normalize()
		elapsed := r.now().Sub(startedAt)
		decision := policy.DecideWithRand(attempt, elapsed, last.Err, r.rand())
		if !decision.Retry {
			last.Artifacts["retry_stop"] = decision.Reason
			break
		}
		retryCount++
		attempt = decision.NextAttempt
		last.Artifacts["retry_delay_ms"] = fmt.Sprintf("%d", decision.Delay.Milliseconds())
		r.sleep(decision.Delay)
		if ctx.Err() != nil {
			last = registry.Run(ctx, req) // ctx 已取消：让执行器返回 canceled
			last.RetryCount = retryCount
			break
		}
	}
	last.RetryCount = retryCount
	if r.OnStepFinish != nil {
		r.OnStepFinish(last)
	}
	return last
}
