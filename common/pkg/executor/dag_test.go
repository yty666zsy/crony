package executor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
)

// dagFake 按命令名决定成败，并记录并发峰值与启动顺序。
type dagFake struct {
	mu         sync.Mutex
	failByCmd  map[string]bool
	duration   time.Duration
	running    int
	maxRunning int
	started    []string
	blocked    chan struct{} // 非 nil 时：每个步骤先等这个通道关闭
}

func (f *dagFake) Kind() Kind { return KindShell }

func (f *dagFake) Validate(req Request) error { return nil }

func (f *dagFake) Execute(ctx context.Context, req Request) Result {
	f.mu.Lock()
	f.running++
	if f.running > f.maxRunning {
		f.maxRunning = f.running
	}
	f.started = append(f.started, req.Command)
	f.mu.Unlock()

	if f.blocked != nil {
		select {
		case <-f.blocked:
		case <-ctx.Done():
		}
	}
	if f.duration > 0 {
		time.Sleep(f.duration)
	}

	f.mu.Lock()
	f.running--
	fail := f.failByCmd[req.Command]
	f.mu.Unlock()

	res := newResult(req)
	if fail {
		return res.finish(models.RunStatusFailed, errScripted)
	}
	return res.finish(models.RunStatusSuccess, nil)
}

var errScripted = &scriptError{}

type scriptError struct{}

func (*scriptError) Error() string { return "scripted failure" }

func (f *dagFake) peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxRunning
}

func (f *dagFake) startedOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

func newDAGRunner(fake *dagFake) *Runner {
	return &Runner{Registry: NewRegistry(fake), Sleep: func(time.Duration) {}, Rand: func() float64 { return 0 }}
}

// dagStep 构造一个 DAG 步骤（命令名即步骤名，便于在 fake 里按名字控制成败）。
func dagStep(index int, name string, deps ...int) Step {
	return Step{
		Index:     index,
		Name:      name,
		Kind:      KindShell,
		DependsOn: deps,
		Request:   Request{Command: name, Timeout: time.Second},
	}
}

func statuses(res []Result) []models.RunStatus {
	out := make([]models.RunStatus, len(res))
	for i, r := range res {
		out[i] = r.Status
	}
	return out
}

func statusString(res []Result) string {
	parts := make([]string, len(res))
	for i, r := range res {
		parts[i] = r.Status.String()
	}
	return strings.Join(parts, ",")
}

// ---------- 1) 执行前校验：坏图必须在开工前被拒绝 ----------

func TestPlanValidateRejectsBadGraphs(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
		want string
	}{
		{
			name: "空编排",
			plan: Plan{},
			want: "编排为空",
		},
		{
			name: "索引重复",
			plan: Plan{Steps: []Step{dagStep(1, "a"), dagStep(1, "b", 1)}},
			want: "步骤索引重复：1",
		},
		{
			name: "依赖不存在的步骤",
			plan: Plan{Steps: []Step{dagStep(0, "a"), dagStep(1, "b", 9)}},
			want: "依赖了不存在的步骤 9",
		},
		{
			name: "依赖自身",
			plan: Plan{Steps: []Step{dagStep(0, "a", 0)}},
			want: "依赖了自身",
		},
		{
			name: "成环（报错必须给出环的路径）",
			plan: Plan{Steps: []Step{dagStep(0, "load", 2), dagStep(1, "verify", 0), dagStep(2, "notify", 1)}},
			want: "编排存在环",
		},
		{
			name: "并发上限为负",
			plan: Plan{MaxParallel: -1, Steps: []Step{dagStep(0, "a"), dagStep(1, "b", 0)}},
			want: "并发上限不能为负",
		},
		{
			name: "汇聚策略非法",
			plan: Plan{Steps: []Step{dagStep(0, "a"), dagStep(1, "b", 0)}}, // 下面改 Join

			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "汇聚策略非法" {
				p := Plan{Steps: []Step{dagStep(0, "a"), dagStep(1, "b", 0)}}
				p.Steps[1].Join = "whatever"
				assertErrContains(t, p.Validate(), "汇聚策略非法")
				return
			}
			err := tc.plan.Validate()
			if err == nil {
				t.Fatalf("期望校验失败，实际通过")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

func assertErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望报错包含 %q，实际没有报错", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("错误信息 %q 未包含 %q", err.Error(), want)
	}
}

func TestPlanValidateCycleMessageNamesTheLoop(t *testing.T) {
	// 特意让第一次遍历落在 0 上：报错必须给出可读的环路径，而不是"有环"三个字。
	p := Plan{Steps: []Step{dagStep(0, "load", 2), dagStep(1, "verify", 0), dagStep(2, "notify", 1)}}
	err := p.Validate()
	assertErrContains(t, err, "0 load → 2 notify → 1 verify → 0 load")
}

func TestPlanValidateAcceptsDiamond(t *testing.T) {
	p := Plan{
		RunID: "r-1",
		Steps: []Step{
			dagStep(0, "extract"),
			dagStep(1, "transform", 0),
			dagStep(2, "enrich", 0),
			dagStep(3, "load", 1, 2),
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("正常的菱形编排不该报错：%v", err)
	}
	if !p.IsDAG() {
		t.Fatalf("声明了依赖的计划必须被识别为 DAG")
	}
	if p.MaxParallel != 0 {
		t.Fatalf("默认应为串行（MaxParallel=0 表示串行），实际 %d", p.MaxParallel)
	}
}

// ---------- 2) 向后兼容：没有依赖的计划必须走原来的线性语义 ----------

func TestPlanWithoutDepsKeepsLegacyLinearSemantics(t *testing.T) {
	p := Plan{
		RunID: "r-legacy",
		Steps: []Step{
			shellStep(0, "first", ConditionOnSuccess),
			shellStep(1, "second", ConditionOnSuccess),
			shellStep(2, "collect", ConditionOnFailure),
			shellStep(3, "cleanup", ConditionAlways),
		},
	}
	if p.IsDAG() {
		t.Fatalf("没有 DependsOn 的计划不得走 DAG 引擎")
	}
	fake := &dagFake{failByCmd: map[string]bool{"echo second": true}}
	runner := newDAGRunner(fake)
	res := runner.Execute(context.Background(), p)

	want := "success,failed,success,success"
	if got := statusString(res); got != want {
		t.Fatalf("线性语义被改变：期望 %s，实际 %s", want, got)
	}
	if res[2].Artifacts["skip_reason"] != "" {
		t.Fatalf("on_failure 步骤不该带跳过原因")
	}
}

// ---------- 3) 菱形编排：分支都跑，join 后收口 ----------

func TestDAGDiamondRunsBranchesThenJoin(t *testing.T) {
	p := Plan{
		RunID: "r-diamond",
		Steps: []Step{
			dagStep(0, "extract"),
			dagStep(1, "transform", 0),
			dagStep(2, "enrich", 0),
			dagStep(3, "load", 1, 2),
		},
	}
	fake := &dagFake{}
	runner := newDAGRunner(fake)
	res := runner.Execute(context.Background(), p)

	if got := statusString(res); got != "success,success,success,success" {
		t.Fatalf("期望四步全成功，实际 %s", got)
	}
	// 结果顺序必须与 plan.Steps 一致（不是完成顺序）
	for i, r := range res {
		if r.Name != p.Steps[i].Name {
			t.Fatalf("结果 %d 与步骤错位：%s vs %s", i, r.Name, p.Steps[i].Name)
		}
	}
	order := fake.startedOrder()
	if order[0] != "extract" {
		t.Fatalf("根步骤必须先启动，实际首个是 %s", order[0])
	}
	if order[len(order)-1] != "load" {
		t.Fatalf("收口步骤必须最后启动，实际最后是 %s", order[len(order)-1])
	}
}

func TestDAGAllSuccessJoinSkipsDownstreamWhenUpstreamFails(t *testing.T) {
	p := Plan{
		RunID: "r-dag",
		Steps: []Step{
			dagStep(0, "extract"),
			dagStep(1, "transform", 0),
			dagStep(2, "load", 1),
		},
	}
	fake := &dagFake{failByCmd: map[string]bool{"transform": true}}
	res := newDAGRunner(fake).Execute(context.Background(), p)

	if got := statusString(res); got != "success,failed,skipped" {
		t.Fatalf("期望 success,failed,skipped，实际 %s", got)
	}
	reason := res[2].Artifacts["skip_reason"]
	if !strings.Contains(reason, "上游未全部成功") || !strings.Contains(reason, "失败 1") {
		t.Fatalf("跳过原因必须说清是上游没成功：%q", reason)
	}
}

func TestDAGAnySuccessJoinRunsWithOneGoodBranch(t *testing.T) {
	// s0 分两支：s1 失败、s2 成功 → s3（any_success）仍应执行。
	p := Plan{
		RunID: "r-dag",
		Steps: []Step{
			dagStep(0, "extract"),
			dagStep(1, "source_a", 0),
			dagStep(2, "source_b", 0),
			dagStep(3, "merge", 1, 2),
		},
	}
	p.Steps[3].Join = JoinAnySuccess
	fake := &dagFake{failByCmd: map[string]bool{"source_a": true}}
	res := newDAGRunner(fake).Execute(context.Background(), p)
	if got := statusString(res); got != "success,failed,success,success" {
		t.Fatalf("any_success 应在一条分支成功时执行，实际 %s", got)
	}

	// 两条分支都失败 → 必须跳过，并且原因写明 any_success 不满足。
	fake2 := &dagFake{failByCmd: map[string]bool{"source_a": true, "source_b": true}}
	res2 := newDAGRunner(fake2).Execute(context.Background(), p)
	if got := statusString(res2); got != "success,failed,failed,skipped" {
		t.Fatalf("全部上游失败时应收口跳过，实际 %s", got)
	}
	if r := res2[3].Artifacts["skip_reason"]; !strings.Contains(r, "any_success") {
		t.Fatalf("跳过原因必须写明汇聚策略：%q", r)
	}
}

func TestDAGAllDoneCollectorRunsAfterFailure(t *testing.T) {
	p := Plan{
		RunID: "r-dag",
		Steps: []Step{
			dagStep(0, "extract"),
			dagStep(1, "load", 0),
			dagStep(2, "cleanup", 0, 1),
		},
	}
	p.Steps[2].Join = JoinAllDone
	fake := &dagFake{failByCmd: map[string]bool{"load": true}}
	res := newDAGRunner(fake).Execute(context.Background(), p)
	if got := statusString(res); got != "success,failed,success" {
		t.Fatalf("all_done 收尾步骤必须在失败后照跑，实际 %s", got)
	}
}

func TestDAGConditionOnFailureActsAsJoinAnyFailure(t *testing.T) {
	build := func() Plan {
		return Plan{
			RunID: "r-dag",
			Steps: []Step{
				dagStep(0, "load"),
				func() Step {
					s := dagStep(1, "collect", 0)
					s.Condition = ConditionOnFailure
					return s
				}(),
			},
		}
	}

	// 上游成功 → on_failure 步骤跳过，原因自述"无上游失败"
	res := newDAGRunner(&dagFake{}).Execute(context.Background(), build())
	if got := statusString(res); got != "success,skipped" {
		t.Fatalf("上游成功时 on_failure 步骤应跳过，实际 %s", got)
	}
	if r := res[1].Artifacts["skip_reason"]; !strings.Contains(r, "无上游失败") {
		t.Fatalf("跳过原因不自述：%q", r)
	}

	// 上游失败 → on_failure 步骤执行
	res2 := newDAGRunner(&dagFake{failByCmd: map[string]bool{"load": true}}).Execute(context.Background(), build())
	if got := statusString(res2); got != "failed,success" {
		t.Fatalf("上游失败时 on_failure 步骤应执行，实际 %s", got)
	}
}

// ---------- 4) 并发上限与确定性 ----------

func TestDAGMaxParallelBoundsConcurrency(t *testing.T) {
	plan := func(parallel int) Plan {
		return Plan{
			RunID:       "r-dag",
			MaxParallel: parallel,
			Steps: []Step{
				dagStep(0, "a"),
				dagStep(1, "b"),
				dagStep(2, "c"),
				dagStep(3, "join", 0, 1, 2),
			},
		}
	}

	// 串行（默认）：峰值并发必须是 1——这是"默认不并行"的可验证含义。
	fake1 := &dagFake{duration: 5 * time.Millisecond}
	newDAGRunner(fake1).Execute(context.Background(), plan(1))
	if peak := fake1.peak(); peak != 1 {
		t.Fatalf("串行时峰值并发应为 1，实际 %d", peak)
	}

	// 显式并行 3：三条根分支应真的并发起来（否则"并行"只是文档里的说法）。
	fake3 := &dagFake{duration: 30 * time.Millisecond}
	newDAGRunner(fake3).Execute(context.Background(), plan(3))
	if peak := fake3.peak(); peak != 3 {
		t.Fatalf("并行上限 3 时峰值并发应为 3，实际 %d", peak)
	}

	// 上限不得被突破：给 4 个根节点但上限 2。
	fake2 := &dagFake{duration: 20 * time.Millisecond}
	p4 := Plan{
		RunID:       "r-dag",
		MaxParallel: 2,
		Steps: []Step{
			dagStep(0, "a"), dagStep(1, "b"), dagStep(2, "c"), dagStep(3, "d"),
			dagStep(4, "join", 0, 1, 2, 3),
		},
	}
	newDAGRunner(fake2).Execute(context.Background(), p4)
	if peak := fake2.peak(); peak > 2 {
		t.Fatalf("并发上限被突破：峰值 %d > 2", peak)
	}
	if peak := fake2.peak(); peak != 2 {
		t.Fatalf("上限 2 且有 4 个就绪步骤时应达到 2，实际 %d", peak)
	}
}

func TestDAGResultsAreDeterministicAcrossRuns(t *testing.T) {
	build := func() Plan {
		return Plan{
			MaxParallel: 3,
			Steps: []Step{
				dagStep(0, "a"), dagStep(1, "b"), dagStep(2, "c"),
				dagStep(3, "join", 0, 1, 2),
			},
		}
	}
	var first string
	for i := 0; i < 5; i++ {
		fake := &dagFake{failByCmd: map[string]bool{"b": true}}
		res := newDAGRunner(fake).Execute(context.Background(), build())
		got := statusString(res) + "|" + res[3].Artifacts["skip_reason"]
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("同一张图多次执行结果不一致：%q vs %q", first, got)
		}
	}
}

// ---------- 5) fail_fast、取消 ----------

func TestDAGFailFastStopsPendingWorkButRunsCollector(t *testing.T) {
	build := func(failFast bool) Plan {
		p := Plan{
			RunID:    "r-dag",
			FailFast: failFast,
			Steps: []Step{
				dagStep(0, "extract"),
				dagStep(1, "load", 0),       // 失败
				dagStep(2, "verify", 0),     // 成功，为 3 制造"失败之后才就绪"
				dagStep(3, "publish", 2),    // 急停时应被拦下
				dagStep(4, "cleanup", 0, 1), // all_done：无论如何都要跑
			},
		}
		p.Steps[4].Join = JoinAllDone
		return p
	}

	fake := &dagFake{failByCmd: map[string]bool{"load": true}}
	res := newDAGRunner(fake).Execute(context.Background(), build(true))
	if r := res[3].Artifacts["skip_reason"]; !strings.Contains(r, "fail_fast") {
		t.Fatalf("fail_fast 应拦下尚未启动的步骤，实际结果 %s（原因 %q）", res[3].Status, r)
	}
	if got := res[4].Status; got != models.RunStatusSuccess {
		t.Fatalf("all_done 收尾步骤必须放行，实际 %s", got)
	}

	// 不开启 fail_fast：另一条分支照常跑完（DAG 的默认行为）
	fake2 := &dagFake{failByCmd: map[string]bool{"load": true}}
	res2 := newDAGRunner(fake2).Execute(context.Background(), build(false))
	if got := res2[3].Status; got != models.RunStatusSuccess {
		t.Fatalf("默认（不 fail_fast）时其它分支应继续，实际 %s", got)
	}
}

func TestDAGCancellationSkipsPendingSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &dagFake{blocked: make(chan struct{})}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
		close(fake.blocked)
	}()

	p := Plan{
		RunID: "r-dag",
		Steps: []Step{
			dagStep(0, "root"),
			dagStep(1, "child", 0),
			dagStep(2, "grandchild", 1),
		},
	}
	res := newDAGRunner(fake).Execute(ctx, p)
	tail := res[2].Artifacts["skip_reason"]
	if !strings.Contains(tail, "执行已取消") {
		t.Fatalf("取消后未启动的步骤必须记为 skipped 且说明原因，实际 %q（状态 %s）", tail, res[2].Status)
	}
	if got := res[0].Status; got != models.RunStatusSuccess {
		t.Fatalf("已经启动的步骤应正常结束，实际 %s", got)
	}
}

func TestDAGOnStepCallbacksFirePerStep(t *testing.T) {
	var (
		mu       sync.Mutex
		started  []string
		finished int
	)
	runner := newDAGRunner(&dagFake{})
	runner.OnStepStart = func(s Step) {
		mu.Lock()
		started = append(started, s.Name)
		mu.Unlock()
	}
	runner.OnStepFinish = func(Result) {
		mu.Lock()
		finished++
		mu.Unlock()
	}
	p := Plan{RunID: "r-dag", Steps: []Step{dagStep(0, "a"), dagStep(1, "b", 0), dagStep(2, "c", 1)}}
	runner.Execute(context.Background(), p)

	if len(started) != 3 || finished != 3 {
		t.Fatalf("DAG 模式下回调必须每步触发一次：start=%v finish=%d", started, finished)
	}
}

// TestDAGMissingRunIDFailsVisiblyInsteadOfSilently 记录一个真实契约：
// registry 会在执行前校验 `run_id`，缺失时**不是**抛错给编排层，而是把该步骤记为失败。
// 编排层能做的就是别把它伪装成成功：失败步骤要如实为 failed，下游按汇聚规则跳过并说明原因。
func TestDAGMissingRunIDFailsVisiblyInsteadOfSilently(t *testing.T) {
	p := Plan{Steps: []Step{dagStep(0, "a"), dagStep(1, "b", 0)}}
	res := newDAGRunner(&dagFake{}).Execute(context.Background(), p)

	if res[0].Status != models.RunStatusFailed {
		t.Fatalf("缺 run_id 的步骤必须记为 failed，实际 %s", res[0].Status)
	}
	if !strings.Contains(res[0].Err.Error(), "run_id") {
		t.Fatalf("失败原因必须指向 run_id，实际 %q", res[0].Err.Error())
	}
	if res[1].Status != models.RunStatusSkipped {
		t.Fatalf("下游必须跳过而不是照跑，实际 %s", res[1].Status)
	}
}

// TestDAGFailFastIsHonoredAtStartTimeNotOnlyAtRoundBoundary 钉住一个修复：
// 同一轮里的两个兄弟分支，当并发上限把后者挡在门外时，前者失败必须让后者**不再启动**。
//
// 修复前的行为：只在轮次边界设 aborted，于是 max_parallel=1 时同一轮里的
// 后一个步骤照样会跑——急停的实际效果悄悄取决于并发度（并发 1 反而"停不住"）。
func TestDAGFailFastIsHonoredAtStartTimeNotOnlyAtRoundBoundary(t *testing.T) {
	build := func(failFast bool) Plan {
		return Plan{
			RunID:       "r-dag",
			MaxParallel: 1, // 显式串行：同一轮里的 branch-late 会被并发上限挡住
			FailFast:    failFast,
			Steps: []Step{
				dagStep(0, "root"),
				dagStep(1, "branch-bad", 0),
				dagStep(2, "branch-late", 0),
			},
		}
	}

	failFast := newDAGRunner(&dagFake{failByCmd: map[string]bool{"branch-bad": true}}).
		Execute(context.Background(), build(true))
	if got := failFast[2].Status; got != models.RunStatusSkipped {
		t.Fatalf("串行 + fail_fast：失败分支之后的步骤必须被急停，实际 %s", got)
	}
	if r := failFast[2].Artifacts["skip_reason"]; !strings.Contains(r, "fail_fast") {
		t.Fatalf("急停跳过必须说明原因，实际 %q", r)
	}

	// 不开 fail_fast：DAG 的默认语义是分支独立，后者照样跑完
	noFailFast := newDAGRunner(&dagFake{failByCmd: map[string]bool{"branch-bad": true}}).
		Execute(context.Background(), build(false))
	if got := noFailFast[2].Status; got != models.RunStatusSuccess {
		t.Fatalf("默认不 fail_fast 时其它分支应继续，实际 %s", got)
	}
}

// TestDAGSkippedStepsAreReportedToOnStepFinish 钉住落库契约：
// 被跳过的步骤也要触发 OnStepFinish，否则 job_run_step 里根本没有这一行，
// 控制台显示的步骤数会少于任务的真实步骤数，"为什么没跑"就无从回答。
//
// 同时钉住：被跳过的步骤**不得**触发 OnStepStart（它确实没有开始执行）。
func TestDAGSkippedStepsAreReportedToOnStepFinish(t *testing.T) {
	var (
		mu      sync.Mutex
		started []string
		results []Result
	)
	runner := newDAGRunner(&dagFake{failByCmd: map[string]bool{"a": true}})
	runner.OnStepStart = func(s Step) {
		mu.Lock()
		started = append(started, s.Name)
		mu.Unlock()
	}
	runner.OnStepFinish = func(res Result) {
		mu.Lock()
		results = append(results, res)
		mu.Unlock()
	}

	p := Plan{RunID: "r-dag", Steps: []Step{
		dagStep(0, "a"),
		dagStep(1, "b", 0), // all_success：上游失败 → 跳过
	}}
	runner.Execute(context.Background(), p)

	if len(results) != 2 {
		t.Fatalf("跳过的步骤也必须回调落库，实际收到 %d 条结果", len(results))
	}
	if results[1].Status != models.RunStatusSkipped || results[1].Artifacts["skip_reason"] == "" {
		t.Fatalf("跳过结果必须带状态与原因: %+v", results[1])
	}
	if results[1].Condition != ConditionOnSuccess || results[1].Join != JoinAllSuccess {
		t.Fatalf("跳过结果必须带编排属性（条件/汇聚），实际 condition=%q join=%q",
			results[1].Condition, results[1].Join)
	}
	if len(results[1].DependsOn) != 1 || results[1].DependsOn[0] != 0 {
		t.Fatalf("跳过结果必须带依赖列表，实际 %v", results[1].DependsOn)
	}
	if len(started) != 1 || started[0] != "a" {
		t.Fatalf("跳过的步骤不得触发 OnStepStart，实际 %v", started)
	}
}
