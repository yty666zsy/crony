package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/executor"
	"github.com/tmnhs/crony/common/pkg/retry"
	"github.com/tmnhs/crony/common/pkg/runpolicy"
	"gorm.io/gorm"
)

// memStore 内存版执行记录存储：让并发/幂等/编排逻辑可以脱离 MySQL 测试。
type memStore struct {
	mu        sync.Mutex
	runs      map[string]*models.JobRun
	steps     []*models.JobRunStep
	insertErr error
}

func newMemStore() *memStore {
	return &memStore{runs: make(map[string]*models.JobRun)}
}

func (m *memStore) InsertRun(run *models.JobRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insertErr != nil {
		return m.insertErr
	}
	if _, exists := m.runs[run.RunID]; exists {
		return errors.New("Error 1062: Duplicate entry '" + run.RunID + "' for key 'uk_job_run_run_id'")
	}
	copied := *run
	m.runs[run.RunID] = &copied
	return nil
}

func (m *memStore) UpdateRun(run *models.JobRun, fields map[string]interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.runs[run.RunID]
	if !ok {
		return gorm.ErrRecordNotFound
	}
	for k, v := range fields {
		switch k {
		case "status":
			stored.Status = v.(int)
		case "started_at":
			stored.StartedAt = v.(int64)
		case "finished_at":
			stored.FinishedAt = v.(int64)
		case "duration_ms":
			stored.DurationMs = v.(int64)
		case "exit_code":
			stored.ExitCode = v.(int)
		case "success_steps":
			stored.SuccessSteps = v.(int)
		case "failed_steps":
			stored.FailedSteps = v.(int)
		case "retry_count":
			stored.RetryCount = v.(int)
		case "error_msg":
			stored.ErrorMsg = v.(string)
		case "output":
			stored.Output = v.(string)
		}
	}
	return nil
}

func (m *memStore) FindRun(runID string) (*models.JobRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	copied := *run
	return &copied, nil
}

func (m *memStore) InsertStep(step *models.JobRunStep) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *step
	m.steps = append(m.steps, &copied)
	return nil
}

func (m *memStore) runCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runs)
}

func (m *memStore) stepCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.steps)
}

func (m *memStore) snapshot(runID string) *models.JobRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[runID]
}

// fakeExec 可脚本化的执行器。
//
// blockCalls 表示"前 N 次调用阻塞不返回"，用于并发策略测试：
// 例如 cancel_other 需要第 1 次卡住、第 2 次正常跑完。
type fakeExec struct {
	mu         sync.Mutex
	calls      int
	entered    chan struct{}
	release    chan struct{}
	blockCalls int
	status     models.RunStatus
	output     string
	err        error
	delay      time.Duration
}

func (f *fakeExec) Kind() executor.Kind { return executor.KindShell }

func (f *fakeExec) Validate(req executor.Request) error { return nil }

func (f *fakeExec) Execute(ctx context.Context, req executor.Request) executor.Result {
	f.mu.Lock()
	f.calls++
	callIndex := f.calls
	blockCalls := f.blockCalls
	f.mu.Unlock()
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if blockCalls >= callIndex {
		select {
		case <-f.release:
		case <-ctx.Done():
			return f.result(req, models.RunStatusCanceled, "执行被取消")
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return f.result(req, models.RunStatusCanceled, "执行被取消")
		}
	}
	status := f.status
	if status == 0 && f.err == nil {
		status = models.RunStatusSuccess
	}
	return f.result(req, status, f.output)
}

func (f *fakeExec) result(req executor.Request, status models.RunStatus, output string) executor.Result {
	now := time.Now()
	exitCode := 0
	if status != models.RunStatusSuccess {
		exitCode = 1
	}
	return executor.Result{
		StepIndex:  req.StepIndex,
		Name:       req.Name,
		Kind:       req.Kind,
		Status:     status,
		ExitCode:   exitCode,
		Output:     output,
		StartedAt:  now,
		FinishedAt: now,
		Artifacts:  map[string]string{"fake": "true"},
		Err:        f.err,
	}
}

func (f *fakeExec) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testJob(id int) *Job {
	return &Job{Job: &models.Job{
		ID:      id,
		Name:    "清理 90 天前日志",
		Command: "bash /opt/scripts/clean.sh --days 90",
		Type:    models.JobTypeCmd,
		Spec:    "0 0 3 * * *",
		RunOn:   "node-prod-01",
		Cmd:     []string{"bash", "/opt/scripts/clean.sh", "--days", "90"},
	}}
}

func newTestCoordinator(fake *fakeExec) (*RunCoordinator, *memStore) {
	store := newMemStore()
	c := NewRunCoordinator(executor.NewRegistry(fake), store)
	c.TerminateWaitTimeout = 2 * time.Second
	// 测试不连 etcd：关闭执行标记发布
	c.SetProcPublisher(nil)
	return c, store
}

// TestRunJobPersistsRunAndSteps 新模型的落库契约：
// 一次执行一行 job_run（含幂等键/策略快照），每个步骤一行 job_run_step（含产物）。
func TestRunJobPersistsRunAndSteps(t *testing.T) {
	fake := &fakeExec{output: "cleaned 42 files"}
	c, store := newTestCoordinator(fake)
	job := testJob(7)
	slot := time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC)

	outcome := c.RunJob(job, models.TriggerScheduler, slot)
	if outcome.Skipped || outcome.Err != nil {
		t.Fatalf("应执行成功: skipped=%v err=%v", outcome.Skipped, outcome.Err)
	}
	if outcome.Output != "cleaned 42 files" {
		t.Fatalf("输出未回传: %q", outcome.Output)
	}

	runID := models.RunIDForSlot(job.ID, slot.Unix())
	run := store.snapshot(runID)
	if run == nil {
		t.Fatalf("未找到 run_id=%s 的执行记录", runID)
	}
	if run.Status != int(models.RunStatusSuccess) {
		t.Fatalf("状态 = %d, want success(%d)", run.Status, models.RunStatusSuccess)
	}
	if run.TriggerSource != int(models.TriggerScheduler) {
		t.Fatalf("触发来源 = %d, want scheduler", run.TriggerSource)
	}
	if run.ScheduledTime != slot.Unix() {
		t.Fatalf("调度槽位 = %d, want %d", run.ScheduledTime, slot.Unix())
	}
	if run.StepCount != 1 || run.SuccessSteps != 1 || run.FailedSteps != 0 {
		t.Fatalf("步骤统计不符: step=%d success=%d failed=%d", run.StepCount, run.SuccessSteps, run.FailedSteps)
	}
	if run.DurationMs < 0 || run.StartedAt == 0 || run.FinishedAt == 0 {
		t.Fatalf("耗时/时间戳未落库: %+v", run)
	}
	if !strings.Contains(run.PolicySnapshot, `"concurrency":"skip"`) {
		t.Fatalf("策略快照缺少并发策略: %s", run.PolicySnapshot)
	}
	if store.stepCount() != 1 {
		t.Fatalf("步骤记录数 = %d, want 1", store.stepCount())
	}
	if store.steps[0].Kind != string(executor.KindShell) || store.steps[0].RunID != runID {
		t.Fatalf("步骤记录不完整: %+v", store.steps[0])
	}
	if !strings.Contains(store.steps[0].Artifacts, "fake") {
		t.Fatalf("结构化产物未落库: %s", store.steps[0].Artifacts)
	}
}

// TestRunJobSchedulerIdempotent 幂等键：同一任务同一调度槽位重复触发只执行一次。
func TestRunJobSchedulerIdempotent(t *testing.T) {
	fake := &fakeExec{output: "ok"}
	c, store := newTestCoordinator(fake)
	job := testJob(11)
	slot := time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC)

	first := c.RunJob(job, models.TriggerScheduler, slot)
	if first.Skipped || first.Err != nil {
		t.Fatalf("首次触发应执行: %+v", first)
	}
	second := c.RunJob(job, models.TriggerScheduler, slot)
	if !second.Skipped {
		t.Fatal("同槽位第二次触发必须被幂等去重")
	}
	if !strings.Contains(second.SkipReason, "幂等") {
		t.Fatalf("去重原因应可审计，实际 %q", second.SkipReason)
	}
	if fake.callCount() != 1 {
		t.Fatalf("执行器调用次数 = %d, want 1", fake.callCount())
	}
	if store.runCount() != 1 {
		t.Fatalf("执行记录数 = %d, want 1", store.runCount())
	}
}

// TestRunJobManualTriggerAlwaysRuns 手动触发没有调度槽位，每次都应执行。
func TestRunJobManualTriggerAlwaysRuns(t *testing.T) {
	fake := &fakeExec{output: "ok"}
	c, store := newTestCoordinator(fake)
	job := testJob(12)
	for i := 0; i < 3; i++ {
		outcome := c.RunJob(job, models.TriggerManual, time.Now())
		if outcome.Skipped || outcome.Err != nil {
			t.Fatalf("第 %d 次手动触发应执行: %+v", i+1, outcome)
		}
	}
	if fake.callCount() != 3 {
		t.Fatalf("执行器调用次数 = %d, want 3", fake.callCount())
	}
	if store.runCount() != 3 {
		t.Fatalf("执行记录数 = %d, want 3（手动执行 run_id 必须唯一）", store.runCount())
	}
}

// TestConcurrencyDefaultSkip 默认策略（skip）：上一次没跑完就跳过，并留下可审计的 skipped 记录。
func TestConcurrencyDefaultSkip(t *testing.T) {
	fake := &fakeExec{blockCalls: 1, entered: make(chan struct{}, 4), release: make(chan struct{})}
	c, store := newTestCoordinator(fake)
	job := testJob(21)

	done := make(chan *RunOutcome, 1)
	go func() { done <- c.RunJob(job, models.TriggerManual, time.Now()) }()
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("第一次执行未开始")
	}

	second := c.RunJob(job, models.TriggerManual, time.Now())
	if !second.Skipped || second.Buffered {
		t.Fatalf("默认策略下应直接跳过，实际 %+v", second)
	}
	if !strings.Contains(second.SkipReason, "跳过") {
		t.Fatalf("跳过原因应可审计，实际 %q", second.SkipReason)
	}
	if second.Run == nil || second.Run.Status != int(models.RunStatusSkipped) {
		t.Fatalf("跳过事件也应落库（回答'为什么没跑'），实际 %+v", second.Run)
	}
	if fake.callCount() != 1 {
		t.Fatalf("被跳过的触发不得调用执行器，实际 %d 次", fake.callCount())
	}

	close(fake.release)
	first := <-done
	if first.Skipped || first.Err != nil {
		t.Fatalf("第一次执行应成功: %+v", first)
	}
	if store.runCount() != 2 {
		t.Fatalf("应有两行执行记录（正常 + 跳过），实际 %d", store.runCount())
	}
}

// TestConcurrencyBufferOne buffer_one：第二次进入排队，前一次结束后自动执行。
func TestConcurrencyBufferOne(t *testing.T) {
	fake := &fakeExec{blockCalls: 1, entered: make(chan struct{}, 4), release: make(chan struct{})}
	c, store := newTestCoordinator(fake)
	job := testJob(22)
	job.ConcurrencyPolicy = int(runpolicy.ConcurrencyBufferOne)

	done := make(chan *RunOutcome, 1)
	go func() { done <- c.RunJob(job, models.TriggerManual, time.Now()) }()
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("第一次执行未开始")
	}

	buffered := c.RunJob(job, models.TriggerManual, time.Now())
	if !buffered.Buffered {
		t.Fatalf("buffer_one 应进入排队，实际 %+v", buffered)
	}
	if c.BufferedCount(job.ID) != 1 {
		t.Fatalf("排队数量 = %d, want 1", c.BufferedCount(job.ID))
	}
	// 队列已有一个，再来一个应被丢弃
	dropped := c.RunJob(job, models.TriggerManual, time.Now())
	if !dropped.Skipped || dropped.Buffered {
		t.Fatalf("缓冲已满应丢弃，实际 %+v", dropped)
	}

	// 放开第一次执行：队首应在执行结束后自动出队执行
	close(fake.release)

	if first := <-done; first.Err != nil {
		t.Fatalf("第一次执行应成功: %+v", first)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.BufferedCount(job.ID) == 0 && fake.callCount() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fake.callCount() < 2 {
		t.Fatalf("排队项应在首次执行结束后执行，执行器调用次数 = %d", fake.callCount())
	}
	if c.BufferedCount(job.ID) != 0 {
		t.Fatalf("排队队列未清空: %d", c.BufferedCount(job.ID))
	}
	if store.runCount() < 2 {
		t.Fatalf("执行记录数 = %d, want >= 2", store.runCount())
	}
}

// TestConcurrencyCancelOther cancel_other：取消旧执行后执行本次（"最新优先"）。
func TestConcurrencyCancelOther(t *testing.T) {
	fake := &fakeExec{blockCalls: 1, entered: make(chan struct{}, 4), release: make(chan struct{})}
	c, _ := newTestCoordinator(fake)
	job := testJob(23)
	job.ConcurrencyPolicy = int(runpolicy.ConcurrencyCancelOther)

	firstDone := make(chan *RunOutcome, 1)
	go func() { firstDone <- c.RunJob(job, models.TriggerManual, time.Now()) }()
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("第一次执行未开始")
	}

	second := c.RunJob(job, models.TriggerManual, time.Now())
	if second.Skipped {
		t.Fatalf("cancel_other 应执行最新一次，实际 %+v", second)
	}
	first := <-firstDone
	if first.Run == nil || first.Run.Status != int(models.RunStatusCanceled) {
		t.Fatalf("旧执行应被取消并落库为 canceled，实际 %+v", first.Run)
	}
	if first.Err == nil {
		t.Fatal("被取消的执行应带错误说明")
	}
}

// TestRunJobCancelledByKill 取消传播：CancelJob 让进行中的执行以 canceled 收尾。
func TestRunJobCancelledByKill(t *testing.T) {
	fake := &fakeExec{blockCalls: 1, entered: make(chan struct{}, 4), release: make(chan struct{})}
	c, store := newTestCoordinator(fake)
	job := testJob(24)

	done := make(chan *RunOutcome, 1)
	go func() { done <- c.RunJob(job, models.TriggerManual, time.Now()) }()
	select {
	case <-fake.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("执行未开始")
	}
	if c.ActiveCount(job.ID) != 1 {
		t.Fatalf("进行中执行数 = %d, want 1", c.ActiveCount(job.ID))
	}
	if canceled := c.CancelJob(job.ID); canceled != 1 {
		t.Fatalf("CancelJob 返回 %d, want 1", canceled)
	}

	select {
	case outcome := <-done:
		if outcome.Run == nil {
			t.Fatal("取消后仍应有执行记录")
		}
		if outcome.Run.Status != int(models.RunStatusCanceled) {
			t.Fatalf("状态 = %d, want canceled(%d)", outcome.Run.Status, models.RunStatusCanceled)
		}
		// 落库记录也必须是 canceled（供 /job/run 查询与审计）
		stored := store.snapshot(outcome.Run.RunID)
		if stored == nil || stored.Status != int(models.RunStatusCanceled) {
			t.Fatalf("落库状态不符: %+v", stored)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后执行未及时结束")
	}
	if c.ActiveCount(job.ID) != 0 {
		t.Fatalf("取消后占位未释放: %d", c.ActiveCount(job.ID))
	}
}

// TestRunJobDuplicateKeyRace 唯一索引兜底：即使幂等预检被绕过，落库冲突也不会重复执行。
func TestRunJobDuplicateKeyRace(t *testing.T) {
	fake := &fakeExec{output: "ok"}
	store := newMemStore()
	job := testJob(31)
	slot := time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)
	// 预先塞入同槽位记录，模拟"另一个节点抢先执行"
	pre := &models.JobRun{RunID: models.RunIDForSlot(job.ID, slot.Unix()), JobID: job.ID, Status: int(models.RunStatusRunning)}
	if err := store.InsertRun(pre); err != nil {
		t.Fatal(err)
	}
	c := NewRunCoordinator(executor.NewRegistry(fake), store)
	c.SetProcPublisher(nil)

	outcome := c.RunJob(job, models.TriggerScheduler, slot)
	if !outcome.Skipped || !strings.Contains(outcome.SkipReason, "已执行") {
		t.Fatalf("应被幂等去重，实际 %+v", outcome)
	}
	if fake.callCount() != 0 {
		t.Fatalf("去重的触发不得调用执行器，实际 %d 次", fake.callCount())
	}
}

// TestRunJobAbortsWhenIdempotencyCheckFails 幂等检查失败（数据库抖动）时放弃执行，
// 而不是"查不到就当没跑过"——后者会在故障期间造成重复执行。
func TestRunJobAbortsWhenIdempotencyCheckFails(t *testing.T) {
	fake := &fakeExec{output: "ok"}
	store := &failingFindStore{memStore: newMemStore()}
	c := NewRunCoordinator(executor.NewRegistry(fake), store)
	c.SetProcPublisher(nil)

	outcome := c.RunJob(testJob(32), models.TriggerManual, time.Now())
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "幂等检查失败") {
		t.Fatalf("应因幂等检查失败而放弃执行，实际 %+v", outcome)
	}
	if fake.callCount() != 0 {
		t.Fatalf("不得执行，实际调用 %d 次", fake.callCount())
	}
}

type failingFindStore struct{ *memStore }

func (f *failingFindStore) FindRun(string) (*models.JobRun, error) {
	return nil, errors.New("dial tcp: connection refused")
}

// mustBuildPlan 断言编译成功（老任务路径不允许报错）。
func mustBuildPlan(t *testing.T, job *Job, runID string) executor.Plan {
	t.Helper()
	plan, err := BuildPlan(job, runID)
	if err != nil {
		t.Fatalf("BuildPlan 意外失败: %v", err)
	}
	return plan
}

// TestBuildPlanLegacyCompatibility 老任务零改动兼容：type=1/2 的映射语义必须逐字保留。
func TestBuildPlanLegacyCompatibility(t *testing.T) {
	t.Run("cmd_argv语义", func(t *testing.T) {
		job := testJob(41)
		job.Timeout = 60
		plan := mustBuildPlan(t, job, "j41-t1")
		step := plan.Steps[0]
		if step.Kind != executor.KindShell {
			t.Fatalf("Kind = %s, want shell", step.Kind)
		}
		if !step.Request.RawArgv {
			t.Fatal("旧任务必须走 argv 模式（不做 shell 解析），否则引号/空格行为会变化")
		}
		if step.Request.Command != "bash" || len(step.Request.Args) != 3 {
			t.Fatalf("argv 拆分不符: %q %v", step.Request.Command, step.Request.Args)
		}
		if step.Request.Timeout != 60*time.Second {
			t.Fatalf("超时 = %v, want 60s", step.Request.Timeout)
		}
		if plan.RunID != "j41-t1" || plan.JobID != 41 {
			t.Fatalf("计划上下文不符: %+v", plan)
		}
	})

	t.Run("cmd_未填超时使用兜底", func(t *testing.T) {
		job := testJob(42)
		plan := mustBuildPlan(t, job, "j42-t1")
		// timeout=0 交给执行器兜底（30min），不再"永不超时"
		if plan.Steps[0].Request.Timeout != 0 {
			t.Fatalf("未填超时应保持 0 由执行器兜底，实际 %v", plan.Steps[0].Request.Timeout)
		}
	})

	t.Run("http_get", func(t *testing.T) {
		job := &Job{Job: &models.Job{ID: 43, Name: "健康检查", Command: "https://api.example.com/health", Type: models.JobTypeHttp, HttpMethod: models.HTTPMethodGet}}
		step := mustBuildPlan(t, job, "j43-t1").Steps[0]
		if step.Kind != executor.KindHTTP || step.Request.Method != "GET" {
			t.Fatalf("GET 映射不符: %s %s", step.Kind, step.Request.Method)
		}
		if step.Request.Timeout != time.Duration(HttpExecTimeout)*time.Second {
			t.Fatalf("旧 HTTP 超时应固定为 %ds，实际 %v", HttpExecTimeout, step.Request.Timeout)
		}
	})

	t.Run("http_post_按问号拆body", func(t *testing.T) {
		job := &Job{Job: &models.Job{ID: 44, Name: "回调", Command: "https://api.example.com/hook?{\"a\":1}", Type: models.JobTypeHttp, HttpMethod: models.HTTPMethodPost, Timeout: 10}}
		step := mustBuildPlan(t, job, "j44-t1").Steps[0]
		if step.Request.Method != "POST" {
			t.Fatalf("Method = %s, want POST", step.Request.Method)
		}
		if step.Request.URL != "https://api.example.com/hook" {
			t.Fatalf("URL 拆分不符: %q", step.Request.URL)
		}
		if step.Request.Body != `{"a":1}` {
			t.Fatalf("Body 拆分不符: %q", step.Request.Body)
		}
		if step.Request.Timeout != 10*time.Second {
			t.Fatalf("Timeout = %v, want 10s", step.Request.Timeout)
		}
	})

	t.Run("http_超大超时被压到300s", func(t *testing.T) {
		job := &Job{Job: &models.Job{ID: 45, Command: "https://api.example.com/health", Type: models.JobTypeHttp, Timeout: 99999}}
		step := mustBuildPlan(t, job, "j45-t1").Steps[0]
		if step.Request.Timeout != time.Duration(HttpExecTimeout)*time.Second {
			t.Fatalf("Timeout = %v, want %ds", step.Request.Timeout, HttpExecTimeout)
		}
	})
}

// TestLegacyRetryPolicyMapping 旧重试参数 → 新策略（指数退避 + 总时长上限）。
func TestLegacyRetryPolicyMapping(t *testing.T) {
	job := testJob(51)
	if policy := legacyStepPolicy(job); policy != nil {
		t.Fatalf("retry_times=0 表示不重试，实际 %+v", policy)
	}

	job.RetryTimes = 3
	job.RetryInterval = 30
	policy := legacyStepPolicy(job)
	if policy == nil {
		t.Fatal("retry_times=3 应产生重试策略")
	}
	if policy.MaxAttempts != 4 || policy.InitialInterval != 30*time.Second || policy.MaxInterval != 30*time.Second {
		t.Fatalf("固定间隔映射不符: %+v", policy)
	}
	if policy.MaxElapsed != 30*time.Minute {
		t.Fatalf("必须有总时长上限，实际 %v", policy.MaxElapsed)
	}

	job.RetryInterval = 0
	policy = legacyStepPolicy(job)
	if policy.InitialInterval != time.Minute || policy.Multiplier != 2 {
		t.Fatalf("未填间隔时应使用指数退避（1 分钟起），实际 %+v", policy)
	}
}

// TestPolicySnapshotIncludesConcurrency 策略快照用于复盘"当时用的什么策略"。
func TestPolicySnapshotIncludesConcurrency(t *testing.T) {
	job := testJob(52)
	job.RetryTimes = 2
	snapshot := policySnapshot(job, runpolicy.ConcurrencyTerminateOther)
	if !strings.Contains(snapshot, "terminate_other") || !strings.Contains(snapshot, "max_attempts") {
		t.Fatalf("策略快照不完整: %s", snapshot)
	}
	if !strings.Contains(policySnapshot(testJob(53), runpolicy.ConcurrencySkip), `"retry":null`) {
		t.Fatal("不重试时应显式记录 retry=null")
	}
}

// TestInvalidConcurrencyPolicyFallsBackToSkip 非法策略取值必须回退默认值，而不是静默放行。
func TestInvalidConcurrencyPolicyFallsBackToSkip(t *testing.T) {
	c, _ := newTestCoordinator(&fakeExec{})
	job := testJob(54)
	job.ConcurrencyPolicy = 99
	if got := c.policyFor(job); got != runpolicy.ConcurrencySkip {
		t.Fatalf("非法策略应回退 skip，实际 %s", got)
	}
}

// TestTruncateTextKeepsUTF8 输出截断不能切出乱码（中文输出很常见）。
func TestTruncateTextKeepsUTF8(t *testing.T) {
	text := strings.Repeat("执行失败原因", 50)
	truncated := truncateText(text, 20)
	if len([]rune(truncated)) > 20 {
		t.Fatalf("截断后长度 %d 超过 20 个字符", len([]rune(truncated)))
	}
	if !strings.HasPrefix(text, truncated) {
		t.Fatal("截断必须按字符边界保留前缀")
	}
	if got := truncateText("short", 100); got != "short" {
		t.Fatalf("未超限的文本不应被改动: %q", got)
	}
	if got := truncateText("anything", 0); got != "" {
		t.Fatalf("上限为 0 时应返回空串，实际 %q", got)
	}
}

// TestOutcomeHelpers 供 job.go 判定成功/错误文本。
func TestOutcomeHelpers(t *testing.T) {
	if (&RunOutcome{}).Succeeded() != true {
		t.Fatal("无错误且未跳过应视为成功")
	}
	if (&RunOutcome{Skipped: true}).Succeeded() {
		t.Fatal("被跳过不得算成功")
	}
	if (&RunOutcome{Err: errors.New("boom")}).Succeeded() {
		t.Fatal("有错误不得算成功")
	}
	if got := (&RunOutcome{Err: errors.New("boom")}).ErrorText(); got != "boom" {
		t.Fatalf("ErrorText = %q", got)
	}
	var nilOutcome *RunOutcome
	if nilOutcome.ErrorText() != "" {
		t.Fatal("nil outcome 的 ErrorText 应为空串（避免调用方判空）")
	}
}

var _ = retry.Policy{}

// recordingProc 记录执行标记的公布/撤销，验证 /job/kill 能找到目标这一契约。
type recordingProc struct {
	mu         sync.Mutex
	published  []string
	retracted  int
	publishNil bool
}

func (p *recordingProc) Publish(jobID int, nodeUUID, runID string) func() {
	p.mu.Lock()
	p.published = append(p.published, fmt.Sprintf("%d/%s/%s", jobID, nodeUUID, runID))
	p.mu.Unlock()
	if p.publishNil {
		return nil
	}
	return func() {
		p.mu.Lock()
		p.retracted++
		p.mu.Unlock()
	}
}

// TestProcMarkerLifecycle 执行期间必须存在可通过 /job/kill 定位的标记，且执行结束后撤销。
func TestProcMarkerLifecycle(t *testing.T) {
	fake := &fakeExec{output: "ok"}
	store := newMemStore()
	proc := &recordingProc{}
	c := NewRunCoordinator(executor.NewRegistry(fake), store)
	c.SetProcPublisher(proc)

	job := testJob(61)
	slot := time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC)
	outcome := c.RunJob(job, models.TriggerScheduler, slot)
	if outcome.Err != nil {
		t.Fatalf("执行应成功: %+v", outcome)
	}
	want := fmt.Sprintf("61/node-prod-01/%s", models.RunIDForSlot(61, slot.Unix()))
	if len(proc.published) != 1 || proc.published[0] != want {
		t.Fatalf("执行标记不符: %v, want [%s]", proc.published, want)
	}
	if proc.retracted != 1 {
		t.Fatalf("执行结束后必须撤销标记，实际撤销 %d 次", proc.retracted)
	}

	// 被并发策略跳过 / 幂等去重时不应公布标记（没有执行，就没有可取消的目标）
	proc2 := &recordingProc{}
	c.SetProcPublisher(proc2)
	if dup := c.RunJob(job, models.TriggerScheduler, slot); !dup.Skipped {
		t.Fatalf("同槽位应被去重: %+v", dup)
	}
	if len(proc2.published) != 0 {
		t.Fatalf("去重的执行不应公布标记: %v", proc2.published)
	}
}

// TestProcMarkerFailureDoesNotBlockRun 标记写失败（etcd 不可用）不能影响任务执行。
func TestProcMarkerFailureDoesNotBlockRun(t *testing.T) {
	fake := &fakeExec{output: "ok"}
	c, _ := newTestCoordinator(fake)
	c.SetProcPublisher(failingProc{})

	outcome := c.RunJob(testJob(62), models.TriggerManual, time.Now())
	if outcome.Err != nil || outcome.Output != "ok" {
		t.Fatalf("标记失败不应影响执行: %+v", outcome)
	}
}

type failingProc struct{}

func (failingProc) Publish(int, string, string) func() {
	// 模拟 publish 失败：不返回撤销函数
	return nil
}
