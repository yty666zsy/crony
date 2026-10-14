package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/executor"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/orchestration"
	"github.com/tmnhs/crony/common/pkg/retry"
	"github.com/tmnhs/crony/common/pkg/runpolicy"
	"unicode/utf8"
)

// 阶段 1（任务模型 2.0）执行协调器。
//
// 职责：
//  1. 并发闸门：按任务的并发策略决定"跑 / 跳过 / 排队 / 取消旧的"（默认 skip）；
//  2. 幂等：调度触发使用 (job_id, 调度槽位) 生成确定性 run_id，同槽位至多执行一次；
//  3. 执行：把任务映射为 executor.Plan，交给 executor.Runner（步骤级重试 + 条件编排）；
//  4. 落库：job_run（一次执行）+ job_run_step（每个步骤，含输出/退出码/重试次数/结构化产物）；
//  5. 取消传播：run_id → context.CancelFunc，取消时执行器会终止整棵进程树；
//  6. 兼容：调用方仍可继续写 job_log（旧接口/旧前端依赖），新老记录并行存在。
//
// 设计约束：本文件**不直接依赖 MySQL/gorm**，存储通过 RunStore 接口注入，
// 因此并发、幂等、编排逻辑可以用内存实现做无外部依赖的单元测试。

// RunStore 执行记录存储接口。
type RunStore interface {
	InsertRun(run *models.JobRun) error
	UpdateRun(run *models.JobRun, fields map[string]interface{}) error
	FindRun(runID string) (*models.JobRun, error)
	InsertStep(step *models.JobRunStep) error
}

// MySQLRunStore 生产实现（走 common/models 的 gorm 方法）。
type MySQLRunStore struct{}

func (MySQLRunStore) InsertRun(run *models.JobRun) error {
	_, err := run.Insert()
	return err
}

func (MySQLRunStore) UpdateRun(run *models.JobRun, fields map[string]interface{}) error {
	return run.UpdateFields(fields)
}

func (MySQLRunStore) FindRun(runID string) (*models.JobRun, error) {
	run := &models.JobRun{RunID: runID}
	if err := run.FindByRunID(); err != nil {
		return nil, err
	}
	return run, nil
}

func (MySQLRunStore) InsertStep(step *models.JobRunStep) error {
	_, err := step.Insert()
	return err
}

// pendingRun 被并发策略判定为"排队"的执行请求。
type pendingRun struct {
	job       *Job
	source    models.TriggerSource
	scheduled time.Time
	runID     string
}

// cancelEntry 记录某个 run_id 的取消函数与其所属任务。
type cancelEntry struct {
	jobID  int
	cancel context.CancelFunc
}

// RunOutcome 一次执行的结果摘要，供调用方写 job_log、发通知、组装接口响应。
type RunOutcome struct {
	// Run 执行记录；并发策略跳过/幂等去重/落库失败时可能为 nil
	Run *models.JobRun
	// Output 合并后的输出（已按执行器上限截断）
	Output string
	// Err 执行错误（nil 表示成功）
	Err error
	// RetryCount 步骤重试次数（与旧 job_log 的 retry_times 语义对齐：重试次数，不是尝试次数）
	RetryCount int
	// Skipped 是否没有真正执行（并发跳过 / 幂等去重 / 排队）
	Skipped bool
	// Buffered 是否进入排队队列
	Buffered bool
	// SkipReason 跳过原因（审计"为什么没跑"）
	SkipReason string
}

// ErrorText 返回可落库的错误文本。
func (o *RunOutcome) ErrorText() string {
	if o == nil || o.Err == nil {
		return ""
	}
	return o.Err.Error()
}

// Succeeded 是否执行成功。
func (o *RunOutcome) Succeeded() bool {
	return o != nil && !o.Skipped && o.Err == nil
}

// RunCoordinator 执行协调器。
type RunCoordinator struct {
	registry *executor.Registry
	store    RunStore
	proc     ProcPublisher

	mu       sync.Mutex
	active   map[int]int32
	buffered map[int][]pendingRun
	cancels  map[string]cancelEntry

	// MaxBuffer buffer_all 策略的排队上限（<=0 用 runpolicy.DefaultMaxBuffer）
	MaxBuffer int
	// TerminateWaitTimeout terminate_other 策略下等待旧执行退出上限
	TerminateWaitTimeout time.Duration
	// IdempotencyWindow 幂等去重窗口（秒）；<=0 时默认 0（只按 run_id 去重）
	IdempotencyWindow int64
}

// NewRunCoordinator 创建协调器（registry / store 可为 nil，使用默认实现）。
func NewRunCoordinator(registry *executor.Registry, store RunStore) *RunCoordinator {
	if registry == nil {
		registry = executor.DefaultRegistry()
	}
	if store == nil {
		store = MySQLRunStore{}
	}
	return &RunCoordinator{
		registry:             registry,
		store:                store,
		proc:                 etcdProcPublisher{},
		active:               make(map[int]int32),
		buffered:             make(map[int][]pendingRun),
		cancels:              make(map[string]cancelEntry),
		MaxBuffer:            runpolicy.DefaultMaxBuffer,
		TerminateWaitTimeout: 30 * time.Second,
	}
}

// SetProcPublisher 注入执行标记发布器（传 nil 表示不发布，便于单元测试）。
func (c *RunCoordinator) SetProcPublisher(p ProcPublisher) { c.proc = p }

// defaultRunCoordinator 节点进程内的全局协调器（由 job.go 的调度入口使用）。
var defaultRunCoordinator = NewRunCoordinator(nil, nil)

// DefaultRunCoordinator 返回全局协调器（供 service 层做取消操作）。
func DefaultRunCoordinator() *RunCoordinator { return defaultRunCoordinator }

// SetAgentRunner 注入 Agent 运行时（由 service 层在启动时调用）。
//
// 为什么是"注入"而不是让 executor 直接依赖 agent/llm：
// 执行器抽象不应知道 ReAct/提示词；同时未配置模型时 Agent 步骤必须**明确失败**，
// 而不是因为"类型没注册"给出含糊报错。
func SetAgentRunner(r executor.AgentRunner) {
	defaultRunCoordinator.registry.Register(executor.NewAgentExecutor(r))
}

// SetK8sRunner 注入 K8s 集群执行器（由 service 层在启动时调用；未配置集群时传 nil）。
//
// 与 SetAgentRunner 同一个理由：执行器抽象不该依赖 client-go，
// 同时"节点没配集群"必须表现为**明确失败**（"本节点未接入 Kubernetes"），
// 而不是让 k8s 步骤悄悄跑成别的类型。
func SetK8sRunner(r executor.K8sRunner) {
	defaultRunCoordinator.registry.Register(executor.NewK8sExecutor(r))
}

// policyFor 解析任务的并发策略；未知取值回退到默认 skip（不允许静默放行）。
func (c *RunCoordinator) policyFor(job *Job) runpolicy.ConcurrencyPolicy {
	if job == nil {
		return runpolicy.ConcurrencySkip
	}
	policy := runpolicy.ConcurrencyPolicy(job.ConcurrencyPolicy)
	switch policy {
	case runpolicy.ConcurrencySkip, runpolicy.ConcurrencyBufferOne, runpolicy.ConcurrencyBufferAll,
		runpolicy.ConcurrencyCancelOther, runpolicy.ConcurrencyTerminateOther:
		return policy
	default:
		logger.GetLogger().Warn(fmt.Sprintf("job[%d] 并发策略取值非法(%d)，回退为 skip", job.ID, job.ConcurrencyPolicy))
		return runpolicy.ConcurrencySkip
	}
}

// runIDFor 生成 run_id：
//   - 调度触发：确定性幂等键 (job_id, 槽位)；
//   - 其它触发（手动/Agent/自愈/重试）：唯一 ID。
func (c *RunCoordinator) runIDFor(job *Job, source models.TriggerSource, scheduled time.Time) string {
	if source == models.TriggerScheduler {
		slot := scheduled.Truncate(time.Minute).Unix()
		return models.RunIDForSlot(job.ID, slot)
	}
	return models.NewManualRunID(job.ID, time.Now())
}

// PrepareRunID 预先生成 run_id（调用方需要在执行前公布 etcd 执行标记时使用）。
func (c *RunCoordinator) PrepareRunID(job *Job, source models.TriggerSource, scheduled time.Time) string {
	if job == nil {
		return ""
	}
	return c.runIDFor(job, source, scheduled)
}

// RunJob 执行一次任务（含并发闸门与幂等去重）。
//
// 调用方约定：cron 回调、手动触发、Agent 下发都只调用本方法；
// 被跳过时返回 Skipped=true 的 outcome（并落一条 skipped 执行记录用于审计）。
func (c *RunCoordinator) RunJob(job *Job, source models.TriggerSource, scheduled time.Time) *RunOutcome {
	return c.RunJobWithID(job, source, scheduled, "")
}

// RunJobWithID 与 RunJob 相同，但允许由调用方指定 run_id（为空时自动生成）。
//
// 为什么需要：调度触发的 run_id 是"确定性幂等键"，调用方可能需要在执行前
// 用它公布 etcd 执行标记（/crony/proc），因此两处必须使用同一个 ID。
func (c *RunCoordinator) RunJobWithID(job *Job, source models.TriggerSource, scheduled time.Time, runID string) *RunOutcome {
	if job == nil {
		return &RunOutcome{Err: errors.New("job is nil")}
	}
	if scheduled.IsZero() {
		scheduled = time.Now()
	}
	if runID == "" {
		runID = c.runIDFor(job, source, scheduled)
	}
	policy := c.policyFor(job)

	proceed, skip := c.acquire(job, policy, source, scheduled, runID)
	if !proceed {
		return skip
	}
	defer c.release(job.ID)

	return c.execute(job, runID, source, scheduled, policy)
}

// acquire 并发闸门：返回"是否可以执行"；不可执行时返回带审计原因的 outcome。
func (c *RunCoordinator) acquire(job *Job, policy runpolicy.ConcurrencyPolicy, source models.TriggerSource, scheduled time.Time, runID string) (bool, *RunOutcome) {
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		c.mu.Lock()
		active := int(c.active[job.ID])
		buffered := len(c.buffered[job.ID])
		decision := runpolicy.Decide(policy, active, buffered, c.MaxBuffer)
		switch decision.Action {
		case runpolicy.ActionRun:
			c.active[job.ID]++
			c.mu.Unlock()
			return true, nil
		case runpolicy.ActionBuffer:
			c.buffered[job.ID] = append(c.buffered[job.ID], pendingRun{job: job, source: source, scheduled: scheduled, runID: runID})
			c.mu.Unlock()
			logger.GetLogger().Info(fmt.Sprintf("job[%d] 已进入排队队列（策略=%s，队列长度=%d）", job.ID, policy, buffered+1))
			return false, &RunOutcome{Skipped: true, Buffered: true, SkipReason: decision.Reason}
		case runpolicy.ActionSkip:
			c.mu.Unlock()
			return false, c.recordSkipped(job, source, scheduled, policy, decision.Reason)
		case runpolicy.ActionCancelOthers:
			// cancel_other：发出取消后**立即**执行本次（允许旧执行在收尾期间短暂重叠）。
			c.mu.Unlock()
			canceled := c.CancelJob(job.ID)
			logger.GetLogger().Warn(fmt.Sprintf("job[%d] 策略=%s：已取消 %d 个进行中的执行，启动最新一次", job.ID, policy, canceled))
			c.mu.Lock()
			c.active[job.ID]++
			c.mu.Unlock()
			return true, nil
		default: // terminate_other
			// terminate_other：取消并**等待旧执行真正退出**后再执行本次，避免瞬时并发。
			c.mu.Unlock()
			canceled := c.CancelJob(job.ID)
			timeout := c.TerminateWaitTimeout
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			logger.GetLogger().Warn(fmt.Sprintf("job[%d] 策略=%s：已取消 %d 个进行中的执行，等待其退出（上限 %s）", job.ID, policy, canceled, timeout))
			if !c.waitIdle(job.ID, timeout) {
				// 旧执行在超时内没有退出：宁可跳过本次，也不制造无限并发
				return false, c.recordSkipped(job, source, scheduled, policy,
					fmt.Sprintf("旧执行在 %s 内未能终止，跳过本次", timeout))
			}
			// 已空闲，重新决策后正常进入 ActionRun 分支
		}
	}
	return false, c.recordSkipped(job, source, scheduled, policy, "取消失败重试次数超限，跳过本次")
}

// release 结束一次执行占位；若有排队请求，取队首异步执行。
func (c *RunCoordinator) release(jobID int) {
	c.mu.Lock()
	if c.active[jobID] > 0 {
		c.active[jobID]--
	}
	var next *pendingRun
	if queue := c.buffered[jobID]; len(queue) > 0 {
		item := queue[0]
		next = &item
		c.buffered[jobID] = queue[1:]
	}
	c.mu.Unlock()

	if next != nil {
		logger.GetLogger().Info(fmt.Sprintf("job[%d] 从排队队列出队执行（原调度时间 %s）", jobID, next.scheduled.Format(time.RFC3339)))
		go c.RunJobWithID(next.job, next.source, next.scheduled, next.runID)
	}
}

// waitIdle 等待某任务的所有执行结束。
func (c *RunCoordinator) waitIdle(jobID int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.ActiveCount(jobID) == 0 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return c.ActiveCount(jobID) == 0
}

// ActiveCount 任务当前进行中的执行数（并发闸门与接口展示共用）。
func (c *RunCoordinator) ActiveCount(jobID int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int(c.active[jobID])
}

// BufferedCount 任务当前排队数量。
func (c *RunCoordinator) BufferedCount(jobID int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buffered[jobID])
}

// registerCancel / unregisterCancel 维护 run_id → cancel 映射。
func (c *RunCoordinator) registerCancel(runID string, jobID int, cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancels[runID] = cancelEntry{jobID: jobID, cancel: cancel}
}

func (c *RunCoordinator) unregisterCancel(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cancels, runID)
}

// CancelRun 取消指定执行（返回是否找到目标）。
func (c *RunCoordinator) CancelRun(runID string) bool {
	c.mu.Lock()
	entry, ok := c.cancels[runID]
	c.mu.Unlock()
	if !ok {
		return false
	}
	entry.cancel()
	return true
}

// CancelJob 取消某任务全部进行中的执行，返回取消数量。
//
// 这是 /job/kill 的新实现路径：不再依赖"给 pid 发信号"，
// 而是取消 context，由执行器终止整棵进程树（含 shell 脚本派生的子进程）。
func (c *RunCoordinator) CancelJob(jobID int) int {
	c.mu.Lock()
	targets := make([]context.CancelFunc, 0, len(c.cancels))
	for _, entry := range c.cancels {
		if entry.jobID == jobID {
			targets = append(targets, entry.cancel)
		}
	}
	c.mu.Unlock()
	for _, cancel := range targets {
		cancel()
	}
	return len(targets)
}

// recordSkipped 记录一次"因并发策略被跳过"的执行（审计价值：回答"为什么没跑"）。
func (c *RunCoordinator) recordSkipped(job *Job, source models.TriggerSource, scheduled time.Time, policy runpolicy.ConcurrencyPolicy, reason string) *RunOutcome {
	logger.GetLogger().Warn(fmt.Sprintf("job[%d] 跳过本次执行（策略=%s）：%s", job.ID, policy, reason))
	run := &models.JobRun{
		RunID:          models.NewManualRunID(job.ID, time.Now()) + "-skip",
		JobID:          job.ID,
		JobName:        job.Name,
		Spec:           job.Spec,
		Command:        job.Command,
		JobType:        int(job.Type),
		NodeUUID:       job.RunOn,
		Hostname:       job.Hostname,
		IP:             job.Ip,
		TriggerSource:  int(source),
		Status:         int(models.RunStatusSkipped),
		Attempt:        1,
		ScheduledTime:  scheduled.Unix(),
		StepCount:      0,
		ErrorMsg:       truncateText(reason, 1000),
		PolicySnapshot: policySnapshot(job, policy),
		Created:        time.Now().Unix(),
	}
	if err := c.store.InsertRun(run); err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("job[%d] 跳过记录落库失败: %s", job.ID, err.Error()))
		return &RunOutcome{Skipped: true, SkipReason: reason}
	}
	return &RunOutcome{Run: run, Skipped: true, SkipReason: reason}
}

// recordInvalidPlan 记录一次"因编排定义非法而拒绝执行"。
//
// 与 recordSkipped 的区别：跳过是"策略决定不跑"（不是错误），这里是"定义是坏的"（是错误），
// 因此状态是 failed 而不是 skipped，并且错误信息要能直接指导修复（含具体步骤/环）。
func (c *RunCoordinator) recordInvalidPlan(job *Job, runID string, source models.TriggerSource, scheduled time.Time, policy runpolicy.ConcurrencyPolicy, cause error) *RunOutcome {
	reason := fmt.Sprintf("任务编排定义非法，已拒绝执行：%v", cause)
	logger.GetLogger().Error(fmt.Sprintf("job[%d] %s", job.ID, reason))
	run := &models.JobRun{
		RunID:          runID,
		JobID:          job.ID,
		JobName:        job.Name,
		Spec:           job.Spec,
		Command:        job.Command,
		JobType:        int(job.Type),
		NodeUUID:       job.RunOn,
		Hostname:       job.Hostname,
		IP:             job.Ip,
		TriggerSource:  int(source),
		Status:         int(models.RunStatusFailed),
		Attempt:        1,
		ScheduledTime:  scheduled.Unix(),
		StepCount:      0,
		ErrorMsg:       truncateText(reason, 1000),
		PolicySnapshot: policySnapshot(job, policy),
		Created:        time.Now().Unix(),
	}
	if err := c.store.InsertRun(run); err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("job[%d] 非法编排记录落库失败: %s", job.ID, err.Error()))
		return &RunOutcome{Err: fmt.Errorf("%s（且记录落库失败: %v）", reason, err)}
	}
	return &RunOutcome{Run: run, Err: fmt.Errorf("%s", reason)}
}

// execute 落库 + 执行 + 收尾。
func (c *RunCoordinator) execute(job *Job, runID string, source models.TriggerSource, scheduled time.Time, policy runpolicy.ConcurrencyPolicy) *RunOutcome {
	// 幂等闸门 1：run_id 已存在 → 直接复用，不重复执行。
	// 查询失败（非"记录不存在"）时放弃执行：数据库抖动时宁可少跑一次，也不能重复跑。
	if existing, err := c.store.FindRun(runID); err == nil && existing != nil {
		logger.GetLogger().Warn(fmt.Sprintf("job[%d] run_id[%s] 已存在，幂等去重，跳过本次执行", job.ID, runID))
		return &RunOutcome{Run: existing, Skipped: true, SkipReason: "同一调度槽位已执行（幂等去重）"}
	} else if err != nil && !models.IsRecordNotFound(err) {
		return &RunOutcome{Err: fmt.Errorf("幂等检查失败，已放弃本次执行: %w", err)}
	}

	plan, planErr := BuildPlan(job, runID)
	if planErr != nil {
		// 坏编排必须**拒绝执行并留下可查的记录**，而不是"跳过坏步骤、跑剩下的"。
		// 保存任务时 admin 已经拒绝过一次（见 orchestration.ValidateJobPlan），
		// 这里是对"库里已经存着一份坏定义"（手工改库、旧版本写入）的兜底。
		return c.recordInvalidPlan(job, runID, source, scheduled, policy, planErr)
	}
	started := time.Now()
	run := &models.JobRun{
		RunID:          runID,
		JobID:          job.ID,
		JobName:        job.Name,
		Spec:           job.Spec,
		Command:        job.Command,
		JobType:        int(job.Type),
		NodeUUID:       job.RunOn,
		Hostname:       job.Hostname,
		IP:             job.Ip,
		TriggerSource:  int(source),
		Status:         int(models.RunStatusPending),
		Attempt:        1,
		ScheduledTime:  scheduled.Unix(),
		StepCount:      len(plan.Steps),
		PolicySnapshot: policySnapshot(job, policy),
		Created:        started.Unix(),
	}

	// 幂等闸门 2：唯一索引兜底（多节点同时抢同一槽位时只会成功一次）。
	if err := c.store.InsertRun(run); err != nil {
		if models.IsDuplicateKey(err) {
			logger.GetLogger().Warn(fmt.Sprintf("job[%d] run_id[%s] 唯一索引冲突，幂等去重", job.ID, runID))
			return &RunOutcome{Skipped: true, SkipReason: "同一调度槽位已被其它节点执行（唯一索引去重）"}
		}
		return &RunOutcome{Run: run, Err: fmt.Errorf("创建执行记录失败: %w", err)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.registerCancel(runID, job.ID, cancel)
	defer func() {
		cancel()
		c.unregisterCancel(runID)
	}()

	// 公布 etcd 执行标记：让 /job/kill 与"当前正在执行"视图能找到这次执行。
	if c.proc != nil {
		if retract := c.proc.Publish(job.ID, job.RunOn, runID); retract != nil {
			defer retract()
		}
	}

	_ = c.store.UpdateRun(run, map[string]interface{}{
		"status":     int(models.RunStatusRunning),
		"started_at": started.Unix(),
	})

	runner := &executor.Runner{
		Registry: c.registry,
		OnStepFinish: func(res executor.Result) {
			c.persistStep(runID, res)
		},
	}
	results := runner.Execute(ctx, plan)
	status, exitCode, successSteps, failedSteps := executor.Summarize(results)

	var (
		output     string
		errText    string
		retryCount int
	)
	for _, res := range results {
		if res.Output != "" {
			output = res.Output
		}
		if res.Status != models.RunStatusSuccess && res.Status != models.RunStatusSkipped && errText == "" {
			errText = res.Error()
		}
		retryCount += res.RetryCount
	}
	finished := time.Now()
	durationMs := finished.Sub(started).Milliseconds()

	_ = c.store.UpdateRun(run, map[string]interface{}{
		"status":        int(status),
		"finished_at":   finished.Unix(),
		"duration_ms":   durationMs,
		"exit_code":     exitCode,
		"success_steps": successSteps,
		"failed_steps":  failedSteps,
		"retry_count":   retryCount,
		"error_msg":     truncateText(errText, 1000),
		"output":        truncateText(output, 2000),
	})

	run.Status = int(status)
	run.ExitCode = exitCode
	run.SuccessSteps = successSteps
	run.FailedSteps = failedSteps
	run.RetryCount = retryCount
	run.DurationMs = durationMs
	run.FinishedAt = finished.Unix()
	run.ErrorMsg = truncateText(errText, 1000)
	run.Output = truncateText(output, 2000)

	outcome := &RunOutcome{Run: run, Output: output, RetryCount: retryCount}
	if status != models.RunStatusSuccess {
		outcome.Err = errors.New(errText)
		if errText == "" {
			outcome.Err = fmt.Errorf("执行未成功（状态=%s）", status)
		}
	}
	return outcome
}

// persistStep 落库单个步骤结果。
//
// 阶段 4-B 起额外记录三件事，都是为了"跳过也说得清"：
//   - condition/join：这一步的编排条件与汇聚策略（artifact 里也留一份，便于直接看 JSON）；
//   - skipped 步骤的 error_msg：把 skip_reason 写进 error_msg，而不是留空——
//     留空的 skipped 行在控制台上就是一句"跳过"，运维无法判断是策略还是故障。
func (c *RunCoordinator) persistStep(runID string, res executor.Result) {
	artifacts := ""
	if len(res.Artifacts) > 0 {
		if b, err := json.Marshal(res.Artifacts); err == nil {
			artifacts = string(b)
		}
	}
	condition := string(res.Condition)
	if condition == "" {
		condition = string(executor.ConditionOnSuccess)
	}
	errText := res.Error()
	if errText == "" && res.Status == models.RunStatusSkipped {
		errText = res.Artifacts["skip_reason"]
	}
	step := &models.JobRunStep{
		RunID:      runID,
		StepIndex:  res.StepIndex,
		Name:       res.Name,
		Kind:       string(res.Kind),
		Condition:  condition,
		Status:     int(res.Status),
		ExitCode:   res.ExitCode,
		RetryCount: res.RetryCount,
		StartedAt:  res.StartedAt.Unix(),
		FinishedAt: res.FinishedAt.Unix(),
		DurationMs: res.Duration.Milliseconds(),
		Output:     truncateText(res.Output, 4000),
		ErrorMsg:   truncateText(errText, 1000),
		Artifacts:  truncateText(artifacts, 2000),
		Created:    time.Now().Unix(),
	}
	if step.Name == "" {
		step.Name = fmt.Sprintf("step-%d", step.StepIndex)
	}
	if err := c.store.InsertStep(step); err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("run[%s] 步骤 %d 落库失败: %s", runID, step.StepIndex, err.Error()))
	}
}

// BuildPlan 把任务定义映射为执行计划。
//
// 两条路径（阶段 4-B 起）：
//   - `job.steps_json` 非空 → 多步骤编排：交给 orchestration.BuildPlan 编译 + 图校验；
//     返回错误时调用方**必须拒绝执行**（见 RunCoordinator.execute 的 recordInvalidPlan）。
//   - `job.steps_json` 为空 → 下面这段单步逻辑，与阶段 1 完全一致（老任务行为逐字不变）。
//
// 单步兼容性（老任务零改动）：
//   - type=1（cmd）：沿用旧的 argv 语义（RawArgv=true），不做 shell 解析，
//     避免升级后引号/空格行为变化导致命令跑错；
//   - type=2（http）：沿用旧语义（GET 直接请求；POST 以 '?' 拆分 URL 与 body），
//     timeout<=0 或超过 300s 时按 300s 处理（与旧 HTTPHandler 一致）；
//   - 重试：RetryTimes/RetryInterval → retry.FromLegacy（指数退避 + 总时长上限）。
//
// 唯一的**有意行为变更**：type=1 在 timeout<=0 时，旧实现是"永不超时"，
// 新实现兜底 30 分钟（executor.DefaultTimeout），避免卡死任务永久占用调度槽。
func BuildPlan(job *Job, runID string) (executor.Plan, error) {
	if strings.TrimSpace(job.StepsJSON) != "" {
		plan, used, err := orchestration.BuildPlan(job.Job, runID)
		if err != nil {
			return executor.Plan{}, err
		}
		if !used {
			// steps_json 非空却解析出"没有编排"：这是矛盾状态，宁可拒绝执行，
			// 也不能退化成单步任务把 job.command 跑出去（那不是用户写的意图）。
			return executor.Plan{}, fmt.Errorf("steps_json 非空但没有解析出任何步骤，已拒绝执行以免误跑单步任务")
		}
		return plan, nil
	}
	step := executor.Step{
		Index:     0,
		Name:      "main",
		Condition: executor.ConditionOnSuccess,
		Policy:    legacyStepPolicy(job),
	}
	switch job.Type {
	case models.JobTypeHttp:
		method := "GET"
		url := job.Command
		body := ""
		if job.HttpMethod == models.HTTPMethodPost {
			method = "POST"
			if fields := strings.SplitN(job.Command, "?", 2); len(fields) == 2 {
				url, body = fields[0], fields[1]
			}
		}
		timeout := job.Timeout
		if timeout <= 0 || timeout > HttpExecTimeout {
			timeout = HttpExecTimeout
		}
		step.Kind = executor.KindHTTP
		step.Request = executor.Request{
			Kind:    executor.KindHTTP,
			Method:  method,
			URL:     url,
			Body:    body,
			Timeout: time.Duration(timeout) * time.Second,
		}
	case models.JobTypeAgent:
		// type=3（Agent）：任务体是自然语言目标，执行过程由只读 ReAct 循环完成。
		//
		// 与 cmd 的两个关键差别：
		//   - 不做 SplitCmd：目标里的空格是内容而不是参数分隔（"看看昨晚的备份任务为什么慢"），
		//     按空格切分会破坏语义，这也是 Agent 类型必须独立存在的理由之一；
		//   - 步骤级重试策略照旧生效，但 AgentExecutor 只在"模型限流/超时"时才真的重试
		//     （见 executor.AgentExecutor.Execute 的说明），避免把确定性的失败重跑成账单。
		step.Kind = executor.KindAgent
		step.Request = executor.Request{
			Kind:      executor.KindAgent,
			AgentKind: models.AgentKindJob,
			AgentGoal: job.Command,
			Timeout:   time.Duration(job.Timeout) * time.Second,
		}
		// 巡检材料在运行时装配（见 service 层的 nodeAgentRunner）：只有节点侧才知道
		// 本机日志目录与节点身份，编排层不该替它猜。
	case models.JobTypeK8s:
		// type=4（k8s）：任务体是"镜像 + 容器内命令"，在集群里跑一个 Job。
		//
		// 切分规则与 type=1 逐字一致（models.SplitCommand）：第一个空格前是镜像，
		// 其余是容器内 argv。需要命名空间/环境变量时用 steps_json（kind=k8s）。
		cmd := job.Cmd
		if len(cmd) == 0 {
			job.SplitCmd()
			cmd = job.Cmd
		}
		if len(cmd) == 0 {
			return executor.Plan{}, fmt.Errorf("k8s 任务 %d 没有解析出任何内容（需要至少一个容器镜像）", job.ID)
		}
		step.Kind = executor.KindK8s
		step.Request = executor.Request{
			Kind:    executor.KindK8s,
			Command: cmd[0],
			Args:    cmd[1:],
			Timeout: time.Duration(job.Timeout) * time.Second,
		}
	default:
		cmd := job.Cmd
		if len(cmd) == 0 {
			job.SplitCmd()
			cmd = job.Cmd
		}
		var args []string
		if len(cmd) > 1 {
			args = cmd[1:]
		}
		step.Kind = executor.KindShell
		step.Request = executor.Request{
			Kind:    executor.KindShell,
			Command: cmd[0],
			Args:    args,
			RawArgv: true,
			Timeout: time.Duration(job.Timeout) * time.Second,
		}
	}
	return executor.Plan{RunID: runID, JobID: job.ID, Steps: []executor.Step{step}}, nil
}

// legacyStepPolicy 把旧的重试参数映射为步骤级重试策略。
func legacyStepPolicy(job *Job) *retry.Policy {
	if job.RetryTimes <= 0 {
		return nil // 不重试
	}
	policy := retry.FromLegacy(job.RetryTimes, job.RetryInterval)
	return &policy
}

// policySnapshot 生成执行策略快照（落库，用于复盘"当时用的什么策略"）。
func policySnapshot(job *Job, policy runpolicy.ConcurrencyPolicy) string {
	retrySnapshot := "null"
	if p := legacyStepPolicy(job); p != nil {
		retrySnapshot = p.Snapshot()
	}
	return fmt.Sprintf(`{"concurrency":"%s","retry":%s}`, policy, retrySnapshot)
}

// truncateText 截断文本到指定字节上限（按 rune 边界，避免把 UTF-8 切成乱码）。
//
// 修复：原实现先按 len(s)（字节数）判断是否超限，再用 runes[:limit] 截断（limit 被当成
// rune 数）。中文文本 len(s)>limit 但 len(runes)<=limit 时，runes[:limit] 必然越界，
// 触发 "slice bounds out of range" panic 并拖垮整个节点进程。
// 现在按 rune 边界累加字节，保证结果不超过 limit 字节且不切坏 UTF-8。
func truncateText(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	var b strings.Builder
	b.Grow(limit)
	for _, r := range s {
		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
