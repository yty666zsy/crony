package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// fakeExecutor 按预设脚本返回结果，用来确定性地验证编排与重试语义。
type fakeExecutor struct {
	script []struct {
		status models.RunStatus
		err    error
	}
	calls []Request
	idx   int
}

func (f *fakeExecutor) Kind() Kind { return KindShell }

func (f *fakeExecutor) Validate(req Request) error { return nil }

func (f *fakeExecutor) Execute(ctx context.Context, req Request) Result {
	f.calls = append(f.calls, req)
	res := newResult(req)
	if len(f.script) == 0 {
		return res.finish(models.RunStatusSuccess, nil)
	}
	item := f.script[f.idx]
	if f.idx < len(f.script)-1 {
		f.idx++
	}
	if item.status == models.RunStatusTimeout {
		res.Artifacts["timeout"] = "1s"
	}
	if item.err == nil && item.status != models.RunStatusSuccess {
		item.err = errors.New("scripted failure")
	}
	return res.finish(item.status, item.err)
}

func newTestRunner(fake *fakeExecutor) (*Runner, *[]time.Duration) {
	delays := &[]time.Duration{}
	runner := &Runner{
		Registry: NewRegistry(fake),
		Sleep:    func(d time.Duration) { *delays = append(*delays, d) },
		Rand:     func() float64 { return 0 },
	}
	return runner, delays
}

func shellStep(index int, name string, condition StepCondition) Step {
	return Step{
		Index:     index,
		Name:      name,
		Kind:      KindShell,
		Condition: condition,
		Request:   Request{Command: "echo " + name, Timeout: time.Second},
	}
}

// TestPlanConditionSemantics 是阶段 1 编排语义的核心断言：
//
//	s0 on_success 失败 → s1 on_success 跳过 → s2 on_failure 执行 → s3 always 执行
func TestPlanConditionSemantics(t *testing.T) {
	fake := &fakeExecutor{script: []struct {
		status models.RunStatus
		err    error
	}{
		{models.RunStatusFailed, errors.New("boom")},
		{models.RunStatusSuccess, nil},
		{models.RunStatusSuccess, nil},
	}}
	runner, _ := newTestRunner(fake)

	plan := Plan{
		RunID: "j9-t1000",
		JobID: 9,
		Steps: []Step{
			shellStep(0, "collect", ConditionOnSuccess),
			shellStep(1, "restart", ConditionOnSuccess),
			shellStep(2, "dump-state", ConditionOnFailure),
			shellStep(3, "cleanup", ConditionAlways),
		},
	}
	results := runner.Execute(context.Background(), plan)
	if len(results) != 4 {
		t.Fatalf("应返回 4 个步骤结果（含跳过的），实际 %d", len(results))
	}
	want := []models.RunStatus{
		models.RunStatusFailed,
		models.RunStatusSkipped,
		models.RunStatusSuccess,
		models.RunStatusSuccess,
	}
	for i, w := range want {
		if results[i].Status != w {
			t.Errorf("步骤 %d(%s) 状态 = %s, want %s（原因=%s）",
				i, plan.Steps[i].Name, results[i].Status, w, results[i].Artifacts["skip_reason"])
		}
	}
	if len(fake.calls) != 3 {
		t.Fatalf("被跳过的步骤不应调用执行器，实际调用 %d 次", len(fake.calls))
	}
	status, _, successSteps, failedSteps := Summarize(results)
	if status != models.RunStatusFailed || successSteps != 2 || failedSteps != 1 {
		t.Fatalf("整体归纳 = %s(success=%d failed=%d), want failed(success=2 failed=1)", status, successSteps, failedSteps)
	}
}

// TestPlanContinueOnFailure 允许"尽力而为"的步骤：失败后仍继续成功链。
func TestPlanContinueOnFailure(t *testing.T) {
	fake := &fakeExecutor{script: []struct {
		status models.RunStatus
		err    error
	}{
		{models.RunStatusFailed, errors.New("soft failure")},
		{models.RunStatusSuccess, nil},
	}}
	runner, _ := newTestRunner(fake)
	soft := shellStep(0, "optional", ConditionOnSuccess)
	soft.ContinueOnFailure = true

	results := runner.Execute(context.Background(), Plan{
		RunID: "j9-t1",
		JobID: 9,
		Steps: []Step{soft, shellStep(1, "must-run", ConditionOnSuccess)},
	})
	if results[0].Status != models.RunStatusFailed {
		t.Fatalf("步骤0 应为失败，实际 %s", results[0].Status)
	}
	if results[1].Status != models.RunStatusSuccess {
		t.Fatalf("ContinueOnFailure 后步骤1 应继续执行，实际 %s", results[1].Status)
	}
}

// TestPlanStepRetryUsesExponentialBackoff 步骤级重试：次数、退避序列、RetryCount 落库。
func TestPlanStepRetryUsesExponentialBackoff(t *testing.T) {
	fake := &fakeExecutor{script: []struct {
		status models.RunStatus
		err    error
	}{
		{models.RunStatusFailed, retry.Retryable(errors.New("502"))},
		{models.RunStatusFailed, retry.Retryable(errors.New("502"))},
		{models.RunStatusSuccess, nil},
	}}
	runner, delays := newTestRunner(fake)
	policy := retry.Policy{
		MaxAttempts:     3,
		InitialInterval: 100 * time.Millisecond,
		MaxInterval:     time.Second,
		Multiplier:      2,
		Jitter:          0,
		MaxElapsed:      time.Minute,
	}
	step := shellStep(0, "flaky", ConditionOnSuccess)
	step.Policy = &policy

	results := runner.Execute(context.Background(), Plan{RunID: "j9-t1", JobID: 9, Steps: []Step{step}})
	if results[0].Status != models.RunStatusSuccess {
		t.Fatalf("重试后应成功，实际 %s (err=%v)", results[0].Status, results[0].Err)
	}
	if results[0].RetryCount != 2 {
		t.Fatalf("RetryCount = %d, want 2", results[0].RetryCount)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("执行器应被调用 3 次，实际 %d", len(fake.calls))
	}
	if len(*delays) != 2 || (*delays)[0] != 100*time.Millisecond || (*delays)[1] != 200*time.Millisecond {
		t.Fatalf("退避序列应为 [100ms 200ms]，实际 %v", *delays)
	}
}

// TestPlanStepRetryStopsOnFatalError 不可重试错误必须立刻停止，避免放大故障。
func TestPlanStepRetryStopsOnFatalError(t *testing.T) {
	fake := &fakeExecutor{script: []struct {
		status models.RunStatus
		err    error
	}{
		{models.RunStatusFailed, retry.Fatal(errors.New("命令不存在"))},
	}}
	runner, delays := newTestRunner(fake)
	policy := retry.Policy{MaxAttempts: 5, InitialInterval: time.Second, MaxInterval: time.Second, Multiplier: 1}
	step := shellStep(0, "bad-cmd", ConditionOnSuccess)
	step.Policy = &policy

	results := runner.Execute(context.Background(), Plan{RunID: "j9-t1", JobID: 9, Steps: []Step{step}})
	if len(fake.calls) != 1 {
		t.Fatalf("不可重试错误只能执行 1 次，实际 %d 次", len(fake.calls))
	}
	if len(*delays) != 0 {
		t.Fatalf("不可重试错误不得产生退避等待，实际 %v", *delays)
	}
	if results[0].Artifacts["retry_stop"] == "" {
		t.Fatal("必须记录停止重试的原因，便于复盘")
	}
}

// TestPlanCancellationSkipsRemainingSteps 取消传播：
// ctx 已取消时不再启动新步骤，后续步骤记为 skipped（而不是假装成功）。
func TestPlanCancellationSkipsRemainingSteps(t *testing.T) {
	fake := &fakeExecutor{}
	runner, _ := newTestRunner(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results := runner.Execute(ctx, Plan{
		RunID: "j9-t1",
		JobID: 9,
		Steps: []Step{
			shellStep(0, "a", ConditionOnSuccess),
			shellStep(1, "b", ConditionAlways),
		},
	})
	for i, res := range results {
		if res.Status != models.RunStatusSkipped {
			t.Errorf("步骤 %d 状态 = %s, want skipped", i, res.Status)
		}
		if res.Artifacts["skip_reason"] == "" {
			t.Errorf("步骤 %d 必须说明为什么没跑", i)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("取消后不得执行任何步骤，实际调用 %d 次", len(fake.calls))
	}
	if status, _, _, _ := Summarize(results); status != models.RunStatusSkipped {
		t.Fatalf("全部跳过时整体状态应为 skipped，实际 %s", status)
	}
}

func TestSummarize(t *testing.T) {
	ok := Result{Status: models.RunStatusSuccess, ExitCode: 0}
	fail := Result{Status: models.RunStatusFailed, ExitCode: 1}
	timeout := Result{Status: models.RunStatusTimeout, ExitCode: -1}
	skipped := Result{Status: models.RunStatusSkipped, ExitCode: -1}
	canceled := Result{Status: models.RunStatusCanceled, ExitCode: 1}

	cases := []struct {
		name          string
		results       []Result
		wantStatus    models.RunStatus
		wantExitCode  int
		wantSuccesses int
		wantFailures  int
	}{
		{"空结果视为跳过", nil, models.RunStatusSkipped, -1, 0, 0},
		{"单步成功", []Result{ok}, models.RunStatusSuccess, 0, 1, 0},
		{"单步失败", []Result{fail}, models.RunStatusFailed, 1, 0, 1},
		{"超时优先于失败", []Result{fail, timeout}, models.RunStatusTimeout, -1, 0, 2},
		{"部分成功仍整体失败", []Result{ok, fail}, models.RunStatusFailed, 1, 1, 1},
		{"全部跳过", []Result{skipped, skipped}, models.RunStatusSkipped, -1, 0, 0},
		{"成功+跳过", []Result{ok, skipped}, models.RunStatusSuccess, 0, 1, 0},
		// 被取消的执行是一次真实发生并被中断的执行，不能被归为"跳过"，
		// 否则 /job/kill 之后执行记录会显示"未执行"，掩盖了"跑了一半被杀"
		{"单步取消", []Result{canceled}, models.RunStatusCanceled, 1, 0, 1},
		{"取消优先于失败与超时", []Result{fail, timeout, canceled}, models.RunStatusCanceled, 1, 0, 3},
		{"成功步骤+取消步骤", []Result{ok, canceled}, models.RunStatusCanceled, 1, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, exitCode, successes, failures := Summarize(tc.results)
			if status != tc.wantStatus || exitCode != tc.wantExitCode || successes != tc.wantSuccesses || failures != tc.wantFailures {
				t.Fatalf("Summarize() = (%s, %d, %d, %d), want (%s, %d, %d, %d)",
					status, exitCode, successes, failures, tc.wantStatus, tc.wantExitCode, tc.wantSuccesses, tc.wantFailures)
			}
		})
	}
}

func TestParseCondition(t *testing.T) {
	cases := map[string]StepCondition{
		"":           ConditionOnSuccess,
		"on_success": ConditionOnSuccess,
		"ON_FAILURE": ConditionOnFailure,
		" Always ":   ConditionAlways,
	}
	for raw, want := range cases {
		got, ok := ParseCondition(raw)
		if !ok || got != want {
			t.Errorf("ParseCondition(%q) = %v, ok=%v, want %v", raw, got, ok, want)
		}
	}
	if _, ok := ParseCondition("sometimes"); ok {
		t.Error("未知条件必须返回 ok=false，由调用方拒绝该步骤")
	}
}

// TestPlanNormalizeFillsRequestContext 步骤请求必须带上 run_id/job_id/step_index，
// 否则执行器的结构化结果无法关联回具体的执行记录。
func TestPlanNormalizeFillsRequestContext(t *testing.T) {
	fake := &fakeExecutor{}
	runner, _ := newTestRunner(fake)
	plan := Plan{RunID: "j11-t2000", JobID: 11, Steps: []Step{
		{Name: "no-context", Kind: KindShell, Request: Request{Command: "echo hi"}},
	}}
	runner.Execute(context.Background(), plan)
	if len(fake.calls) != 1 {
		t.Fatalf("应执行 1 次，实际 %d", len(fake.calls))
	}
	got := fake.calls[0]
	if got.RunID != "j11-t2000" || got.JobID != 11 || got.StepIndex != 0 || got.Kind != KindShell {
		t.Fatalf("步骤请求缺少上下文: %+v", got)
	}
	if got.Timeout <= 0 {
		t.Fatal("未指定超时的步骤必须落到执行器兜底超时，避免僵尸步骤")
	}
}

// TestRunnerCallbacksFire 落库回调：每个步骤开始/结束都要被通知（前端实时展示依赖它）。
func TestRunnerCallbacksFire(t *testing.T) {
	fake := &fakeExecutor{}
	var started, finished int
	runner := &Runner{
		Registry:     NewRegistry(fake),
		Sleep:        func(time.Duration) {},
		OnStepStart:  func(Step) { started++ },
		OnStepFinish: func(Result) { finished++ },
	}
	plan := Plan{RunID: "j1-t1", JobID: 1, Steps: []Step{
		shellStep(0, "a", ConditionAlways),
		shellStep(1, "b", ConditionAlways),
	}}
	runner.Execute(context.Background(), plan)
	if started != 2 || finished != 2 {
		t.Fatalf("回调次数 started=%d finished=%d, want 2/2", started, finished)
	}
}
