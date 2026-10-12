package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/slo"
)

// 阶段 5 补全：SLO 违约处置内核的单元测试。
//
// 为什么这层必须有单元测试（而不是只靠黑盒）：黑盒接口 `Allowed=true` 是**人工触发**，
// 碰不到"自动开关关闭时绝不动手"这条最关键的安全边界；而这条边界正是
// "升级在跑的系统行为不变"的落点。依赖全部注入，故这里能把开关、冷却、通道可用性
// 逐一钉死。

func breachEval(id, jobID int, name, action string, breached bool) SloEvaluation {
	return SloEvaluation{
		Policy: models.SloPolicy{
			ID: id, JobID: jobID, Name: name, BreachAction: action, Enabled: true,
		},
		JobName: "任务-" + name,
		Report: slo.Report{
			Breached: breached,
			Total:    10, Success: 5, SuccessRate: 0.5,
			Reasons: []string{"成功率 50% 低于目标 99%"},
		},
	}
}

type breachRecorder struct {
	saved    []models.SloBreach
	notified int
	diagRuns []string
}

func newTestBreachService(rec *breachRecorder, opts ...func(*SloBreachDeps)) *SloBreachService {
	deps := SloBreachDeps{
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0) },
		AutoEnabled: func() bool { return true },
		Cooldown:    func() time.Duration { return 30 * time.Minute },
		LastActionAt: func(int) (int64, error) {
			return 0, nil
		},
		SaveBreach: func(b *models.SloBreach) error {
			rec.saved = append(rec.saved, *b)
			return nil
		},
		FindJob: func(jobID int) (*models.Job, error) {
			return &models.Job{ID: jobID, Name: "任务"}, nil
		},
		Notify: func(*models.Job, string, string) int {
			rec.notified++
			return 1
		},
		LatestFailedRun: func(jobID int) (*models.JobRun, error) {
			return &models.JobRun{RunID: "run-x", JobID: jobID, Status: int(models.RunStatusFailed)}, nil
		},
		Diagnose: func(_ context.Context, runID string) DiagnoseOutcome {
			rec.diagRuns = append(rec.diagRuns, runID)
			return DiagnoseOutcome{Available: true, OK: true, SessionID: "sess-x", Detail: "诊断完成"}
		},
	}
	for _, o := range opts {
		o(&deps)
	}
	return NewSloBreachService(deps)
}

// 自动开关关闭时：只计数跳过、不落库、不发通知、不诊断。
func TestSloBreachDisabledDoesNothing(t *testing.T) {
	rec := &breachRecorder{}
	svc := newTestBreachService(rec, func(d *SloBreachDeps) { d.AutoEnabled = func() bool { return false } })

	rep := svc.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(1, 910007, "p1", "diagnose", true)},
		SloBreachOptions{Allowed: false, ExecutedBy: "auto"})

	if rep.Breached != 1 || rep.Skipped != 1 || rep.Acted != 0 {
		t.Fatalf("关闭时必须跳过且不动手，实际 %+v", rep)
	}
	if len(rec.saved) != 0 {
		t.Fatalf("关闭时不得落库（与接线前完全一致），实际落库 %d 行", len(rec.saved))
	}
	if rec.notified != 0 || len(rec.diagRuns) != 0 {
		t.Fatalf("关闭时不得发通知/诊断，实际 notify=%d diag=%v", rec.notified, rec.diagRuns)
	}
}

// 未违约的契约不处理（口径与 slo.Evaluate 一致）。
func TestSloBreachIgnoresNonBreached(t *testing.T) {
	rec := &breachRecorder{}
	svc := newTestBreachService(rec)
	rep := svc.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(1, 910007, "p1", "notify", false)},
		SloBreachOptions{Allowed: true})
	if rep.Breached != 0 || rep.Acted != 0 || rep.Observed != 0 || len(rec.saved) != 0 {
		t.Fatalf("未违约不应产生任何动作，实际 %+v", rep)
	}
}

// notify 动作：有收件人 → acted；无收件人 → observed（未发送，如实说明）。
func TestSloBreachNotify(t *testing.T) {
	rec := &breachRecorder{}
	svc := newTestBreachService(rec)
	rep := svc.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(1, 910007, "p1", "notify", true)},
		SloBreachOptions{Allowed: true, ExecutedBy: "manual"})
	if rep.Acted != 1 || len(rec.saved) != 1 {
		t.Fatalf("有收件人应 acted 且落库，实际 %+v saved=%d", rep, len(rec.saved))
	}
	row := rec.saved[0]
	if row.Status != models.SloBreachActed || row.ExecutedBy != "manual" {
		t.Fatalf("落库行状态/触发方不对: %+v", row)
	}
	if !strings.Contains(row.Detail, "收件人") {
		t.Fatalf("处置说明应写明发送结果: %q", row.Detail)
	}
	if strings.TrimSpace(row.Reason) == "" {
		t.Fatalf("违约必须记录原因")
	}
	if strings.TrimSpace(row.ReportJSON) == "" {
		t.Fatalf("违约必须留下评估快照")
	}

	// 无收件人：Notify 返回 0 → observed（不能假装发过）。
	rec2 := &breachRecorder{}
	svc2 := newTestBreachService(rec2, func(d *SloBreachDeps) {
		d.Notify = func(*models.Job, string, string) int { return 0 }
	})
	rep2 := svc2.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(1, 910007, "p1", "notify", true)},
		SloBreachOptions{Allowed: true})
	if rep2.Observed != 1 || rep2.Acted != 0 {
		t.Fatalf("无收件人应 observed，实际 %+v", rep2)
	}
	if !strings.Contains(rec2.saved[0].Detail, "未配置") {
		t.Fatalf("应如实说明未发送: %q", rec2.saved[0].Detail)
	}
}

// diagnose 动作：通道不可用 → 降级为通知并写明（绝不假装诊断过）。
func TestSloBreachDiagnoseDegrades(t *testing.T) {
	rec := &breachRecorder{}
	svc := newTestBreachService(rec, func(d *SloBreachDeps) {
		d.Diagnose = func(context.Context, string) DiagnoseOutcome {
			return DiagnoseOutcome{Available: false, Detail: "Agent 运行时未启用（未配置 llm）"}
		}
	})
	rep := svc.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(1, 910007, "p1", "diagnose", true)},
		SloBreachOptions{Allowed: true})
	if rep.Acted != 1 {
		t.Fatalf("有收件人时降级为通知应 acted，实际 %+v", rep)
	}
	row := rec.saved[0]
	if !strings.Contains(row.Detail, "降级为通知") || !strings.Contains(row.Detail, "未配置 llm") {
		t.Fatalf("降级必须写明原因: %q", row.Detail)
	}
	if rec.notified != 1 || len(rec.diagRuns) != 0 {
		t.Fatalf("降级后应发通知且不诊断，实际 notify=%d diag=%v", rec.notified, rec.diagRuns)
	}
}

// diagnose 动作：通道可用 → acted，落库带回会话号；且诊断的是"最近一次失败执行"。
func TestSloBreachDiagnoseOK(t *testing.T) {
	rec := &breachRecorder{}
	svc := newTestBreachService(rec)
	rep := svc.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(7, 910007, "p7", "diagnose", true)},
		SloBreachOptions{Allowed: true})
	if rep.Acted != 1 {
		t.Fatalf("诊断成功应 acted，实际 %+v", rep)
	}
	if len(rec.diagRuns) != 1 || rec.diagRuns[0] != "run-x" {
		t.Fatalf("应对最近失败执行触发诊断，实际 %v", rec.diagRuns)
	}
	if !strings.Contains(rec.saved[0].Detail, "sess-x") {
		t.Fatalf("诊断会话号应落库（可回放）: %q", rec.saved[0].Detail)
	}
}

// 全局契约（job_id=0）没有可诊断/可通知的具体对象：只留痕，不做无效动作。
func TestSloBreachGlobalPolicyObserves(t *testing.T) {
	rec := &breachRecorder{}
	svc := newTestBreachService(rec)
	rep := svc.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(3, 0, "global", "diagnose", true)},
		SloBreachOptions{Allowed: true})
	if rep.Observed != 1 || rep.Acted != 0 {
		t.Fatalf("全局契约应 observed，实际 %+v", rep)
	}
	if rec.notified != 0 || len(rec.diagRuns) != 0 {
		t.Fatalf("全局契约不得发通知/诊断")
	}
	if !strings.Contains(rec.saved[0].Detail, "全局契约") {
		t.Fatalf("应说明为什么没动手: %q", rec.saved[0].Detail)
	}
}

// 未知动作一律保守降级为 none（闭合白名单：绝不让"看不懂的配置"变成别的动作）。
func TestSloBreachUnknownActionIsNone(t *testing.T) {
	rec := &breachRecorder{}
	svc := newTestBreachService(rec)
	rep := svc.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(1, 910007, "p1", "rm -rf /", true)},
		SloBreachOptions{Allowed: true})
	if rep.Observed != 1 || rep.Acted != 0 {
		t.Fatalf("未知动作应 observed(none)，实际 %+v", rep)
	}
	row := rec.saved[0]
	if row.Action != models.SloBreachActionNone {
		t.Fatalf("未知动作必须归一为 none，实际 %q", row.Action)
	}
	if !strings.Contains(row.Detail, "未知") {
		t.Fatalf("应说明动作未知: %q", row.Detail)
	}
	if rec.notified != 0 || len(rec.diagRuns) != 0 {
		t.Fatalf("未知动作不得触发任何外部动作")
	}
}

// 冷却：冷却期内跳过；force 绕过。
func TestSloBreachCooldown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	recent := func(d *SloBreachDeps) {
		d.Now = func() time.Time { return now }
		d.LastActionAt = func(int) (int64, error) { return now.Add(-1 * time.Minute).Unix(), nil }
	}

	rec := &breachRecorder{}
	svc := newTestBreachService(rec, recent)
	rep := svc.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(1, 910007, "p1", "notify", true)},
		SloBreachOptions{Allowed: true})
	if rep.Skipped != 1 || rep.Acted != 0 || len(rec.saved) != 0 {
		t.Fatalf("冷却期内应跳过且不落库，实际 %+v", rep)
	}

	rec2 := &breachRecorder{}
	svc2 := newTestBreachService(rec2, recent)
	rep2 := svc2.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(1, 910007, "p1", "notify", true)},
		SloBreachOptions{Allowed: true, BypassCooldown: true})
	if rep2.Acted != 1 {
		t.Fatalf("force 应绕过冷却，实际 %+v", rep2)
	}
}

// 冷却查询出错：计入 Failed 并留错误，不误发动作。
func TestSloBreachCooldownError(t *testing.T) {
	rec := &breachRecorder{}
	svc := newTestBreachService(rec, func(d *SloBreachDeps) {
		d.LastActionAt = func(int) (int64, error) { return 0, errors.New("db down") }
	})
	rep := svc.HandleBreaches(context.Background(),
		[]SloEvaluation{breachEval(1, 910007, "p1", "notify", true)},
		SloBreachOptions{Allowed: true})
	if rep.Failed != 1 || len(rep.Errors) == 0 || rec.notified != 0 {
		t.Fatalf("查询冷却出错必须计入失败且不发动作，实际 %+v", rep)
	}
}

// 报告计数：多个契约混合时，各状态计数自洽。
func TestSloBreachReportCounts(t *testing.T) {
	rec := &breachRecorder{}
	svc := newTestBreachService(rec)
	rep := svc.HandleBreaches(context.Background(), []SloEvaluation{
		breachEval(1, 910007, "n1", "notify", true),  // acted
		breachEval(2, 0, "g1", "diagnose", true),     // observed（全局）
		breachEval(3, 910007, "n2", "notify", false), // 未违约，不计
	}, SloBreachOptions{Allowed: true})
	if rep.Evaluated != 3 || rep.Breached != 2 || rep.Acted != 1 || rep.Observed != 1 {
		t.Fatalf("计数不自洽: %+v", rep)
	}
	if len(rep.Rows) != 2 {
		t.Fatalf("应落 2 行（未违约不落库）: %d", len(rep.Rows))
	}
}
