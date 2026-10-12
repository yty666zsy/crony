package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/selfheal"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 阶段 4-C 服务层单测：**全部依赖都是假的**，不碰数据库、不碰 etcd、不碰模型。
//
// 为什么服务层必须有这样一组测试：自愈是"会自动动手"的功能，
// 它最危险的失效模式不是报错，而是**在不该动手的时候动手**（开关没开、次数用尽、
// 任务没 opt-in、写能力没开）。这些路径在集成测试里很难稳定构造，
// 但在这里可以逐条钉死：断言"没有发生重跑调用"和断言"发生了"同样重要。

type fakeHealDeps struct {
	failedRuns []*models.JobRun
	steps      map[string][]models.JobRunStep
	job        *models.Job
	node       *models.Node
	nodeErr    error

	saved      []*models.JobHeal
	healsByRun map[string][]*models.JobHeal
	follow     *models.JobRun
	followErr  error

	retryCalls   []tools.RetryRequest
	retryRes     tools.Result
	retryErr     error
	timeoutCalls []int64
	timeoutErr   error
	writes       bool

	memObs    []models.MemoryObservation
	memIDs    []string
	memErr    error
	recallIDs []string
	notified  int

	now time.Time
}

func newFakeHealDeps() *fakeHealDeps {
	return &fakeHealDeps{
		steps:      map[string][]models.JobRunStep{},
		healsByRun: map[string][]*models.JobHeal{},
		now:        time.Unix(1700000000, 0),
		retryRes:   tools.Result{OK: true, Summary: "已投递立即执行"},
		memIDs:     []string{"mem-1"},
		job: &models.Job{
			ID: 9001, Name: "联调-自愈服务单测", SelfHeal: 1, Timeout: 60,
		},
		node: &models.Node{UUID: "node-ut-01", Status: models.NodeConnSuccess},
	}
}

func (f *fakeHealDeps) deps() HealDeps {
	return HealDeps{
		FindFailedRuns: func(since time.Time, limit int) ([]*models.JobRun, error) {
			out := make([]*models.JobRun, 0, len(f.failedRuns))
			for _, r := range f.failedRuns {
				if r.FinishedAt >= since.Unix() {
					out = append(out, r)
				}
			}
			if len(out) > limit {
				out = out[:limit]
			}
			return out, nil
		},
		FindSteps: func(runID string) ([]models.JobRunStep, error) { return f.steps[runID], nil },
		FindJob:   func(jobID int) (*models.Job, error) { return f.job, nil },
		FindNode: func(uuid string) (*models.Node, error) {
			if f.nodeErr != nil {
				return nil, f.nodeErr
			}
			return f.node, nil
		},
		HealsForSource: func(sourceRunID string) ([]*models.JobHeal, error) {
			return f.healsByRun[sourceRunID], nil
		},
		HealsForJob: func(jobID int, since time.Time) ([]*models.JobHeal, error) {
			out := make([]*models.JobHeal, 0)
			for _, h := range f.saved {
				if h.JobID == jobID && h.Created >= since.Unix() {
					out = append(out, h)
				}
			}
			return out, nil
		},
		ActiveHeals: func() ([]*models.JobHeal, error) {
			// 与生产同一口径：并发的唯一事实来源是"库里处于待观测状态的行"。
			out := make([]*models.JobHeal, 0)
			for _, h := range f.saved {
				if h.Status == models.HealStatusExecuted {
					out = append(out, h)
				}
			}
			return out, nil
		},
		SaveHeal: func(h *models.JobHeal) error {
			for i, old := range f.saved {
				if old.HealID == h.HealID {
					f.saved[i] = h
					f.healsByRun[h.SourceRunID] = replaceHeal(f.healsByRun[h.SourceRunID], h)
					return nil
				}
			}
			f.saved = append(f.saved, h)
			f.healsByRun[h.SourceRunID] = append(f.healsByRun[h.SourceRunID], h)
			return nil
		},
		FindHeal: func(healID string) (*models.JobHeal, error) {
			for _, h := range f.saved {
				if h.HealID == healID {
					return h, nil
				}
			}
			return nil, nil
		},
		SearchHeals: func(req *request.ReqJobHealSearch) ([]models.JobHeal, int64, error) {
			out := make([]models.JobHeal, 0, len(f.saved))
			for _, h := range f.saved {
				out = append(out, *h)
			}
			return out, int64(len(out)), nil
		},
		FollowRun: func(h *models.JobHeal) (*models.JobRun, error) {
			if f.followErr != nil {
				return nil, f.followErr
			}
			return f.follow, nil
		},
		UpsertMemory: func(obs models.MemoryObservation, now time.Time) ([]string, error) {
			if f.memErr != nil {
				return nil, f.memErr
			}
			f.memObs = append(f.memObs, obs)
			return f.memIDs, nil
		},
		RecallStats: func(symptom, nodeUUID string) (map[models.HealAction]selfheal.HistoryStats, []string, string) {
			if len(f.recallIDs) == 0 {
				return nil, nil, ""
			}
			return nil, f.recallIDs, "参考历史经验"
		},
		Retry: func(ctx context.Context, req tools.RetryRequest) (tools.Result, error) {
			f.retryCalls = append(f.retryCalls, req)
			return f.retryRes, f.retryErr
		},
		UpdateTimeout: func(jobID int, timeout int64) error {
			if f.timeoutErr != nil {
				return f.timeoutErr
			}
			f.timeoutCalls = append(f.timeoutCalls, timeout)
			return nil
		},
		WritesEnabled: func() bool { return f.writes },
		Notify:        func(job *models.Job, subject, body string) { f.notified++ },
		Now:           func() time.Time { return f.now },
		Logf:          func(format string, args ...interface{}) {},
	}
}

func replaceHeal(list []*models.JobHeal, h *models.JobHeal) []*models.JobHeal {
	for i, old := range list {
		if old.HealID == h.HealID {
			list[i] = h
			return list
		}
	}
	return append(list, h)
}

// failRun 构造一次失败执行。
func failRun(runID string, status models.RunStatus, exit int, errMsg string) *models.JobRun {
	return &models.JobRun{
		RunID: runID, JobID: 9001, Status: int(status), ExitCode: exit,
		ErrorMsg: errMsg, NodeUUID: "node-ut-01",
		FinishedAt: 1699999900, ScheduledTime: 1699999800,
	}
}

// allowPolicy 打开总开关并放宽闸门（单测只验证被测那一处逻辑）。
func allowPolicy() selfheal.Policy {
	p := selfheal.DefaultPolicy()
	p.Enabled = true
	return p
}

func newSvc(f *fakeHealDeps, p selfheal.Policy) *SelfHealService {
	return NewSelfHealService(f.deps(), p)
}

// TestHealDisabledByMasterSwitch：总开关关闭时只留痕、绝不动手。
func TestHealDisabledByMasterSwitch(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.failedRuns = []*models.JobRun{failRun("j9001-t1", models.RunStatusFailed, 1, "exit status 1")}
	svc := newSvc(f, selfheal.DefaultPolicy()) // Enabled=false
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描不应报错：%v", err)
	}
	if len(f.retryCalls) != 0 {
		t.Fatalf("总开关关闭时不允许任何重跑，实际调用 %d 次", len(f.retryCalls))
	}
	if rep.Scanned != 1 || rep.Denied != 1 {
		t.Fatalf("期望 扫描1/拒绝1，实际 扫描%d/拒绝%d", rep.Scanned, rep.Denied)
	}
	if len(f.saved) != 1 || f.saved[0].Status != models.HealStatusDenied {
		t.Fatalf("拒必须留痕为 denied，实际 %+v", f.saved)
	}
	d, ok := selfheal.ParseCapDecision(f.saved[0].PolicyJSON)
	if !ok || d.Rule != selfheal.CapDisabled {
		t.Fatalf("拒绝原因应可解析且为 self_heal_disabled，实际 %q ok=%v", f.saved[0].PolicyJSON, ok)
	}
}

// TestHealJobNotOptedIn：任务没开 self_heal 时不动手（升级后行为不变的关键）。
func TestHealJobNotOptedIn(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.job.SelfHeal = 0
	f.failedRuns = []*models.JobRun{failRun("j9001-t2", models.RunStatusFailed, 1, "exit status 1")}
	svc := newSvc(f, allowPolicy())
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.retryCalls) != 0 {
		t.Fatalf("任务未 opt-in 不允许重跑，实际 %d 次", len(f.retryCalls))
	}
	d, _ := selfheal.ParseCapDecision(f.saved[0].PolicyJSON)
	if rep.Denied != 1 || d.Rule != selfheal.CapJobOptIn {
		t.Fatalf("期望拒绝规则 job_not_opted_in，实际 %s", d.Rule)
	}
}

// TestHealWritesDisabled：写能力未开启时不动手（与工具网关同一判据）。
func TestHealWritesDisabled(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = false
	f.failedRuns = []*models.JobRun{failRun("j9001-t3", models.RunStatusFailed, 1, "exit status 1")}
	svc := newSvc(f, allowPolicy())
	rep, _ := svc.ScanOnce(context.Background())
	if len(f.retryCalls) != 0 {
		t.Fatalf("写能力关闭时不允许重跑，实际 %d 次", len(f.retryCalls))
	}
	d, _ := selfheal.ParseCapDecision(f.saved[0].PolicyJSON)
	if rep.Denied != 1 || d.Rule != selfheal.CapWriteOff {
		t.Fatalf("期望拒绝规则 write_capability_off，实际 %s", d.Rule)
	}
}

// TestHealAllowedRetriesAndWaitsForObservation：放行时重跑一次，
// 状态是 executed（**不是 succeeded**），且**不**沉淀经验（结果还没观测到）。
func TestHealAllowedRetriesAndWaitsForObservation(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.failedRuns = []*models.JobRun{failRun("j9001-t4", models.RunStatusFailed, 1, "exit status 1")}
	svc := newSvc(f, allowPolicy())
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.retryCalls) != 1 {
		t.Fatalf("期望重跑 1 次，实际 %d 次", len(f.retryCalls))
	}
	if rep.Executed != 1 || rep.Succeeded != 0 {
		t.Fatalf("期望 已执行1/成功0，实际 %d/%d", rep.Executed, rep.Succeeded)
	}
	h := f.saved[0]
	if h.Status != models.HealStatusExecuted {
		t.Fatalf("未观测前状态必须是 executed，实际 %s", h.Status)
	}
	if h.Deadline == 0 {
		t.Fatal("已执行必须带观测截止时间（否则永远等不到结论）")
	}
	if len(f.memObs) != 0 {
		t.Fatalf("结果未观测前不允许沉淀经验，实际 %d 条", len(f.memObs))
	}
	if h.Action != models.HealActionRetrySameNode {
		t.Fatalf("命令失败应选原节点重跑，实际 %s", h.Action)
	}
}

// TestObserveSucceededSinksMemory：观测到后续执行成功 → succeeded + 经验 outcome=success。
func TestObserveSucceededSinksMemory(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.failedRuns = []*models.JobRun{failRun("j9001-t5", models.RunStatusFailed, 1, "exit status 1")}
	svc := newSvc(f, allowPolicy())
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	executed := f.saved[0]
	// 第二轮：出现新的成功执行。
	f.failedRuns = nil
	f.follow = &models.JobRun{RunID: "j9001-t6", JobID: 9001, Status: int(models.RunStatusSuccess)}
	f.now = f.now.Add(30 * time.Second)
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Succeeded != 1 || executed.Status != models.HealStatusSucceeded {
		t.Fatalf("期望自愈成功，实际 %s（成功计数 %d）", executed.Status, rep.Succeeded)
	}
	if executed.NewRunID != "j9001-t6" {
		t.Fatalf("必须记录后续执行单号，实际 %q", executed.NewRunID)
	}
	if len(f.memObs) != 1 {
		t.Fatalf("观测出结果后必须沉淀一条经验，实际 %d", len(f.memObs))
	}
	obs := f.memObs[0]
	if obs.Outcome != models.MemoryOutcomeSuccess || obs.AgentKind != AgentKindSelfHeal || obs.Tool != ToolNameSelfHeal {
		t.Fatalf("经验归属/结果错误：%+v", obs)
	}
	if !strings.HasPrefix(obs.ActionText, "self_heal:retry_same_node") {
		t.Fatalf("动作文本必须带可反解前缀，实际 %q", obs.ActionText)
	}
	if _, ok := selfheal.ActionFromMemoryText(obs.ActionText); !ok {
		t.Fatalf("动作文本必须能被反解，实际 %q", obs.ActionText)
	}
	if executed.MemoryIDs == "" {
		t.Fatal("经验条目号必须回写到自愈记录（否则溯源断了）")
	}
}

// TestObserveStillFailing：自愈后仍失败 → failed（经验记 failed，供下次排序参考）。
func TestObserveStillFailing(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.failedRuns = []*models.JobRun{failRun("j9001-t7", models.RunStatusFailed, 1, "exit status 1")}
	svc := newSvc(f, allowPolicy())
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	executed := f.saved[0]
	f.follow = &models.JobRun{RunID: "j9001-t8", JobID: 9001, Status: int(models.RunStatusFailed), ExitCode: 2, ErrorMsg: "exit status 2"}
	f.now = f.now.Add(30 * time.Second)
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executed.Status != models.HealStatusFailed {
		t.Fatalf("仍失败应记 failed，实际 %s", executed.Status)
	}
	if len(f.memObs) != 1 || f.memObs[0].Outcome != models.MemoryOutcomeFailed {
		t.Fatalf("经验应记 failed，实际 %+v", f.memObs)
	}
}

// TestObserveTimeoutWithoutNewRun：观测窗口内没有新执行，超时后判失败（**不判成功**）。
func TestObserveTimeoutWithoutNewRun(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.failedRuns = []*models.JobRun{failRun("j9001-t9", models.RunStatusFailed, 1, "exit status 1")}
	svc := newSvc(f, allowPolicy())
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	executed := f.saved[0]
	f.follow = nil
	// 还没到截止：保持 executed（给慢成功的执行留时间）。
	f.now = f.now.Add(10 * time.Second)
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executed.Status != models.HealStatusExecuted {
		t.Fatalf("未到观测截止前不应改判，实际 %s", executed.Status)
	}
	// 越过截止：判失败。
	f.now = time.Unix(executed.Deadline+1, 0)
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executed.Status != models.HealStatusFailed {
		t.Fatalf("观测超时应判失败（不能算成功），实际 %s", executed.Status)
	}
	if !strings.Contains(executed.OutcomeNote, "没有出现新的执行") {
		t.Fatalf("结论必须说清为什么判失败，实际 %q", executed.OutcomeNote)
	}
}

// TestIdempotentSkipForSettledHeal：已成功的同一次失败不会被重复处置。
func TestIdempotentSkipForSettledHeal(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	run := failRun("j9001-t10", models.RunStatusFailed, 1, "exit status 1")
	f.failedRuns = []*models.JobRun{run}
	svc := newSvc(f, allowPolicy())
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 把第一次变成终态成功，再扫一遍。
	f.saved[0].Status = models.HealStatusSucceeded
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.retryCalls) != 1 {
		t.Fatalf("终态后不得再次动手，实际重跑 %d 次", len(f.retryCalls))
	}
	if rep.Skipped != 1 {
		t.Fatalf("期望跳过 1 条，实际 %d", rep.Skipped)
	}
}

// TestDeniedNotReDecidedAndNotBudget：被拒之后不再重复判定（避免同一条事实刷屏），
// 且拒绝不消耗次数预算（否则三轮回合就把从未尝试过的任务推给人工）。
func TestDeniedNotReDecidedAndNotBudget(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	run := failRun("j9001-t11", models.RunStatusFailed, 1, "exit status 1")
	f.failedRuns = []*models.JobRun{run}
	svc := newSvc(f, allowPolicy())
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 造一条"临时性拒绝"（冷却）：仍然不再重复判定。
	f.saved[0].Status = models.HealStatusDenied
	f.saved[0].PolicyJSON = selfheal.CapDecision{Rule: selfheal.CapCooldown, Reason: "冷却中"}.JSON()
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 {
		t.Fatalf("被拒之后应停止重复判定，实际跳过 %d", rep.Skipped)
	}
	if len(f.saved) != 1 {
		t.Fatalf("不得为同一次失败新增拒绝记录（防刷屏），实际 %d 条", len(f.saved))
	}
	// 被拒记录不占用次数预算：真实尝试数仍是 0。
	f.saved[0].Status = models.HealStatusDenied
	d := selfheal.EvalCaps(svc.Policy(), selfheal.CapInput{
		JobSelfHeal: true, WritesEnabled: true, History: f.saved, Now: f.now,
	})
	if !d.Allowed || d.Rule != selfheal.CapAllowed {
		t.Fatalf("被拒记录不得消耗预算，实际 %+v", d)
	}
}

// TestFailedHealMayRetryAfterCooldown：真正失败过的自愈在冷却过后可以再试一次。
func TestFailedHealMayRetryAfterCooldown(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	run := failRun("j9001-t11b", models.RunStatusFailed, 1, "exit status 1")
	f.failedRuns = []*models.JobRun{run}
	svc := newSvc(f, allowPolicy())
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 第一次动作判为失败。
	f.saved[0].Status = models.HealStatusFailed
	f.saved[0].Created = f.now.Unix() - 1000 // 早已过冷却
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 0 || len(f.saved) != 2 {
		t.Fatalf("失败过的自愈应可再试一次，实际 跳过%d 记录%d", rep.Skipped, len(f.saved))
	}
	if f.saved[1].Attempt != 2 {
		t.Fatalf("第二次尝试的序号应为 2，实际 %d", f.saved[1].Attempt)
	}
	// 但冷却期内不允许再试。
	f.saved[1].Status = models.HealStatusFailed
	f.saved[1].Created = f.now.Unix()
	rep, err = svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.retryCalls) != 2 {
		t.Fatalf("冷却期内不得再动手，实际重跑 %d 次", len(f.retryCalls))
	}
	if rep.Denied != 1 {
		t.Fatalf("冷却期内的处置应记一条拒绝，实际 %d", rep.Denied)
	}
}

// TestEscalateOnNodeUnreachable：节点不可达 → 转人工（不动手）+ 通知。
func TestEscalateOnNodeUnreachable(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.node.Status = models.NodeConnFail
	run := failRun("j9001-t12", models.RunStatusFailed, 1, "connection refused")
	f.failedRuns = []*models.JobRun{run}
	svc := newSvc(f, allowPolicy())
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.retryCalls) != 0 {
		t.Fatalf("节点不可达不得原节点重跑，实际 %d 次", len(f.retryCalls))
	}
	h := f.saved[0]
	if h.Status != models.HealStatusEscalated || h.FailureClass != string(selfheal.ClassNodeUnreachable) {
		t.Fatalf("期望转人工 + 节点不可达，实际 %s/%s", h.Status, h.FailureClass)
	}
	if rep.Escalated != 1 || f.notified != 1 {
		t.Fatalf("转人工必须计数并通知，实际 escalated=%d notify=%d", rep.Escalated, f.notified)
	}
	if len(f.memObs) != 1 || f.memObs[0].ActionKind != models.MemoryActionAdvice {
		t.Fatalf("转人工经验应记 advice，实际 %+v", f.memObs)
	}
}

// TestAttemptCapEscalatesNotDenies：次数用尽语义是"交给人接手"，不是"这次不许"。
func TestAttemptCapEscalatesNotDenies(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	p := allowPolicy()
	p.MaxAttemptsPerJobWindow = 2
	run := failRun("j9001-t13", models.RunStatusFailed, 1, "exit status 1")
	f.failedRuns = []*models.JobRun{run}
	// 窗口内已有两条（success/failed 都算尝试）。
	for i := 0; i < 2; i++ {
		f.saved = append(f.saved, &models.JobHeal{
			HealID: "heal-old-" + string(rune('a'+i)), JobID: 9001,
			Status: models.HealStatusFailed, Created: f.now.Unix() - 10,
		})
	}
	svc := newSvc(f, p)
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.retryCalls) != 0 {
		t.Fatalf("次数用尽不得动手，实际 %d 次", len(f.retryCalls))
	}
	if rep.Escalated != 1 || rep.Denied != 0 {
		t.Fatalf("次数用尽应转人工而非普通拒绝，实际 escalated=%d denied=%d", rep.Escalated, rep.Denied)
	}
	h := f.saved[len(f.saved)-1]
	if h.Status != models.HealStatusEscalated || h.Attempt != 3 {
		t.Fatalf("期望 escalated/attempt=3（含本次尝试的序号），实际 %s/%d", h.Status, h.Attempt)
	}
	if f.notified != 1 {
		t.Fatalf("转人工必须通知，实际 %d", f.notified)
	}
}

// TestExtendTimeoutUpdatesBeforeRetry：调超时动作必须**先改配置成功**再重跑。
func TestExtendTimeoutUpdatesBeforeRetry(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.job.Timeout = 60
	run := failRun("j9001-t14", models.RunStatusTimeout, 0, "任务执行超时")
	f.failedRuns = []*models.JobRun{run}
	svc := newSvc(f, allowPolicy())
	if _, err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := f.saved[0]
	if h.Action != models.HealActionRetryExtendTimeout {
		t.Fatalf("超时应选调大超时重跑，实际 %s", h.Action)
	}
	if len(f.timeoutCalls) != 1 || f.timeoutCalls[0] != 120 {
		t.Fatalf("期望超时 60→120，实际 %v", f.timeoutCalls)
	}
	if len(f.retryCalls) != 1 {
		t.Fatalf("调超时后必须重跑，实际 %d 次", len(f.retryCalls))
	}
	args, ok := selfheal.ParseActionJSON(h.ActionJSON)
	if !ok || args.FromTimeout != 60 || args.ToTimeout != 120 {
		t.Fatalf("动作参数必须可读回（60→120），实际 %q", h.ActionJSON)
	}
}

// TestExtendTimeoutFailureBlocksRetry：改配置失败时**不允许**照样重跑
// （否则记录会写着"已调大超时"，而实际配置没变）。
func TestExtendTimeoutFailureBlocksRetry(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.job.Timeout = 60
	f.timeoutErr = errors.New("数据库写入失败")
	f.failedRuns = []*models.JobRun{failRun("j9001-t15", models.RunStatusTimeout, 0, "任务执行超时")}
	svc := newSvc(f, allowPolicy())
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.retryCalls) != 0 {
		t.Fatalf("改配置失败后不得重跑，实际 %d 次", len(f.retryCalls))
	}
	h := f.saved[0]
	if h.Status != models.HealStatusFailed || rep.Failed != 1 {
		t.Fatalf("期望 failed，实际 %s (failed=%d)", h.Status, rep.Failed)
	}
	if !strings.Contains(h.OutcomeNote, "未执行重跑") {
		t.Fatalf("结论必须写明未重跑，实际 %q", h.OutcomeNote)
	}
}

// TestRetryDeliveryFailureIsRecorded：投递失败（例如 etcd 不可用）→ failed 且留痕原因。
func TestRetryDeliveryFailureIsRecorded(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	f.retryRes = tools.Result{OK: false, Summary: "投递失败", Err: "etcd put 超时"}
	f.failedRuns = []*models.JobRun{failRun("j9001-t16", models.RunStatusFailed, 1, "exit status 1")}
	svc := newSvc(f, allowPolicy())
	rep, _ := svc.ScanOnce(context.Background())
	if rep.Failed != 1 {
		t.Fatalf("投递失败应计 failed，实际 %d", rep.Failed)
	}
	if !strings.Contains(f.saved[0].OutcomeNote, "etcd put 超时") {
		t.Fatalf("投递失败原因必须留痕，实际 %q", f.saved[0].OutcomeNote)
	}
}

// TestHealRunManualDoesNotBypassGates：手动处置同样过闸门（手动不是后门）。
func TestHealRunManualDoesNotBypassGates(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = false // 写能力关闭
	run := failRun("j9001-t17", models.RunStatusFailed, 1, "exit status 1")
	svc := newSvc(f, allowPolicy())
	h, err := svc.HealRun(context.Background(), run, "manual:unit-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.retryCalls) != 0 {
		t.Fatalf("手动处置不得绕过写能力闸门，实际重跑 %d 次", len(f.retryCalls))
	}
	if h.Status != models.HealStatusDenied || h.ExecutedBy != "manual:unit-test" {
		t.Fatalf("期望被拒且记录操作人，实际 %s/%s", h.Status, h.ExecutedBy)
	}
}

// TestScanOnceConcurrencyGateCountsThisRound：同一轮扫描里已执行的动作要计入并发上限，
// 否则同轮两条失败会一起动手，超过 max_concurrent。
func TestScanOnceConcurrencyGateCountsThisRound(t *testing.T) {
	f := newFakeHealDeps()
	f.writes = true
	p := allowPolicy()
	p.MaxConcurrent = 1
	f.failedRuns = []*models.JobRun{
		failRun("j9001-t18", models.RunStatusFailed, 1, "exit status 1"),
		failRun("j9001-t19", models.RunStatusFailed, 1, "exit status 1"),
	}
	svc := newSvc(f, p)
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.retryCalls) != 1 {
		t.Fatalf("并发上限=1 时同轮只允许动手一次，实际 %d 次", len(f.retryCalls))
	}
	if rep.Executed != 1 || rep.Denied != 1 {
		t.Fatalf("期望 已执行1/拒绝1，实际 %d/%d", rep.Executed, rep.Denied)
	}
}

// TestActionTextRoundTrip：经验文本 → 动作的反解必须严格（乱文本不得变出动作）。
func TestActionTextRoundTrip(t *testing.T) {
	args := selfheal.ActionJSON{JobID: 1, FromTimeout: 30, ToTimeout: 60}
	for _, a := range []models.HealAction{
		models.HealActionRetrySameNode, models.HealActionRetryExtendTimeout, models.HealActionEscalate,
	} {
		text := selfheal.MemoryActionText(a, 9001, args)
		got, ok := selfheal.ActionFromMemoryText(text)
		if !ok || got != a {
			t.Fatalf("往返失败：%q → %q ok=%v", text, got, ok)
		}
	}
	for _, bad := range []string{"", "retry_same_node", "self_heal:", "self_heal:rm_rf /", "别的工具:retry_same_node"} {
		if _, ok := selfheal.ActionFromMemoryText(bad); ok {
			t.Fatalf("乱文本不得被解析成动作：%q", bad)
		}
	}
}
