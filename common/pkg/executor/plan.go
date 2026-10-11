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

	// ---- 阶段 4-B：DAG 编排字段 ----

	// DependsOn 上游步骤的 Index 列表。为空表示"没有上游"。
	//
	// 注意：**只有 Plan 里至少有一个步骤声明了 DependsOn 时，整个计划才走 DAG 引擎**
	// （见 Plan.IsDAG）；否则按阶段 1 的线性语义执行，本字段不参与判定。
	// 也就是说，给已有计划里的孤步骤填 DependsOn 是"开启 DAG"的动作，不是"微调"。
	DependsOn []int
	// Join 汇聚策略，见 JoinPolicy 注释；空视为 all_success。
	Join JoinPolicy
}

// Plan 一次执行的步骤编排。
type Plan struct {
	RunID string
	JobID int
	Steps []Step

	// MaxParallel 同时执行的步骤数上限（DAG 计划）。
	// 0 或 1 表示串行（默认）：DAG 的默认值刻意选串行，因为"默认并行"会让
	// 一次编排的副作用顺序变得不可预期，而排障时最怕的就是"顺序说不清"。
	MaxParallel int
	// FailFast 为 true 时，任一步骤失败后不再启动其它待启动步骤
	// （汇总/清理类步骤仍会执行：Join=all_done 或 Condition=always）。
	//
	// DAG 计划默认 false：分支之间互不影响才是 DAG 的意义所在；
	// 线性计划不受此字段影响（其"成功链中断"语义由 ContinueOnFailure 决定，见 Execute）。
	FailFast bool
}

// normalize 补齐步骤索引与默认条件，保证调用方漏填也能安全执行。
func (p *Plan) normalize() {
	for i := range p.Steps {
		if p.Steps[i].Condition == "" {
			p.Steps[i].Condition = ConditionOnSuccess
		}
		// 汇聚策略也要显式化：引擎对空值按 all_success 处理，但落库/推前端的
		// 结果里如果留着空串，运维看到的就是"没有汇聚策略"——而实际上它按
		// all_success 在挡分支。默认值必须写出来，否则"默认"和"没配"无法区分。
		//
		// **只在 condition=on_success 时补默认值**：condition=on_failure/always 的步骤
		// 靠"没有 Join"来表达自己的汇聚语义（任一上游失败即跑 / 上游终结即跑）。
		// 如果无脑补成 all_success，on_failure 的收尾步骤会在上游**成功**时被执行——
		// 语义正好反过来，而且没人会注意到（这正是本行注释存在的原因）。
		if p.Steps[i].Join == "" && p.Steps[i].Condition == ConditionOnSuccess {
			p.Steps[i].Join = JoinAllSuccess
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
		if p.Steps[i].Request.Name == "" {
			// 步骤名要跟着请求走进 Result（执行器用 Request.Name 填 Result.Name）。
			// 不补这一步的后果是"计划里明明有名字，落库/推前端的结果里名字是空的"，
			// 而这种缺失在单步任务上一直没被发现（单步任务的展示名走了另一条路）。
			p.Steps[i].Request.Name = p.Steps[i].Name
		}
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

// Execute 执行 Plan，返回每一步的结果（含被条件跳过的步骤，状态为 skipped）。
//
// **引擎选择**：Plan 里有步骤显式声明了 DependsOn → DAG 引擎（dag.go）；
// 否则走下面的线性引擎（阶段 1 行为，逐字节保持不变）。
//
// 线性引擎的关键行为：
//  1. 条件步骤：on_success/on_failure/always 按"是否已出现失败"判定；
//  2. 步骤级重试：按 Policy 指数退避 + 抖动，超限或不可重试错误立即停止；
//  3. 失败传播：默认中断成功链（后续 on_success 步骤记为 skipped），
//     但 on_failure/always 步骤仍会执行——这是"失败也要收集现场/清理"的前提；
//  4. 取消传播：ctx 取消后不再开始新步骤，正在执行的步骤由执行器负责终止进程树。
func (r *Runner) Execute(ctx context.Context, plan Plan) []Result {
	if plan.IsDAG() {
		plan.normalize()
		return r.executeDAG(ctx, plan)
	}
	plan.normalize()
	results := make([]Result, 0, len(plan.Steps))
	anyFailure := false
	aborted := false

	for _, step := range plan.Steps {
		if ctxErr := ctx.Err(); ctxErr != nil {
			results = append(results, r.recordSkipped(step, fmt.Sprintf("执行已取消: %v", ctxErr)))
			continue
		}
		if !shouldRun(step, anyFailure, aborted) {
			results = append(results, r.recordSkipped(step, skipReason(step, aborted)))
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

// recordSkipped 生成"跳过"结果并**触发 OnStepFinish**。
//
// 为什么跳过的步骤也要回调：OnStepFinish 是节点侧落库 job_run_step 的唯一入口。
// 如果只有真正执行过的步骤才回调，"没跑的步骤"在库里根本不存在，
// 于是控制台只能显示 3 行而任务其实有 5 步——运维无法区分
// "这步不存在"与"这步被跳过了"，也就无法回答"为什么没跑"。
func (r *Runner) recordSkipped(step Step, reason string) Result {
	res := skippedResult(step, reason)
	if r.OnStepFinish != nil {
		r.OnStepFinish(res)
	}
	return res
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
	arts := map[string]string{"skip_reason": reason}
	arts["condition"] = string(step.Condition)
	if step.Join != "" {
		arts["join"] = string(step.Join)
	}
	if len(step.DependsOn) > 0 {
		deps := make([]string, len(step.DependsOn))
		for i, d := range step.DependsOn {
			deps[i] = fmt.Sprintf("%d", d)
		}
		arts["depends_on"] = strings.Join(deps, ",")
	}
	return Result{
		StepIndex:  step.Index,
		Name:       step.Name,
		Kind:       step.Kind,
		Status:     models.RunStatusSkipped,
		ExitCode:   -1,
		Condition:  step.Condition,
		Join:       step.Join,
		DependsOn:  append([]int(nil), step.DependsOn...),
		StartedAt:  now,
		FinishedAt: now,
		Artifacts:  arts,
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
	// 把步骤的编排属性回填到结果上：落库后每一个步骤行都能回答
	// "它为什么会被执行/跳过"（条件与汇聚策略），而不只是"成功或失败"。
	last.Condition = step.Condition
	last.Join = step.Join
	last.DependsOn = append([]int(nil), step.DependsOn...)
	if r.OnStepFinish != nil {
		r.OnStepFinish(last)
	}
	return last
}
