package tools

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// 阶段 5 补全：写动作结果回写自治信用（让信用自己会动）
// ---------------------------------------------------------------------------

type outcomeRec struct {
	tool  string
	ok    bool
	count int
}

// TestAutonomyOutcomeRecordedOnSuccess 执行成功的写动作必须回写（成功证据）。
func TestAutonomyOutcomeRecordedOnSuccess(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL2
	p.AutoApprove = []string{"job_retry@node-1"}
	p.MaxAffectedHostsPerSession = 2
	r := NewRegistry(p, WithApprover(&stubApprover{}))
	rec := &outcomeRec{}
	r.SetAutonomyOutcomeRecorder(func(tool string, ok bool) { rec.tool, rec.ok, rec.count = tool, ok, rec.count+1 })
	r.MustRegister(newWriteStub("job_retry", "node-1"))

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("命中白名单应直接执行: %v", err)
	}
	if rec.count != 1 || rec.tool != "job_retry" || !rec.ok {
		t.Fatalf("执行成功应回写一次成功，实际 %+v", rec)
	}
}

// TestAutonomyOutcomeRecordedOnFailure 执行失败/超时的写动作必须回写为不良后果。
func TestAutonomyOutcomeRecordedOnFailure(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL2
	p.AutoApprove = []string{"job_retry@node-1"}
	p.MaxAffectedHostsPerSession = 2
	r := NewRegistry(p, WithApprover(&stubApprover{}))
	rec := &outcomeRec{}
	r.SetAutonomyOutcomeRecorder(func(tool string, ok bool) { rec.tool, rec.ok, rec.count = tool, ok, rec.count+1 })
	wt := newWriteStub("job_retry", "node-1")
	wt.fn = func(context.Context, map[string]interface{}) (Result, error) {
		return Result{OK: false, Err: "boom"}, nil
	}
	r.MustRegister(wt)

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("工具返回失败不应变成网关错误: %v", err)
	}
	if rec.count != 1 || rec.ok {
		t.Fatalf("执行失败应回写一次不良后果，实际 %+v", rec)
	}
}

// TestAutonomyOutcomeNotRecordedWhenNotExecuted "没有执行"的路径一律不记：
// 转人工审批不是动作结果，记了等于凭空给信用加减分。
func TestAutonomyOutcomeNotRecordedWhenApprovalPending(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL1 // L1：所有写动作都要审批
	ap := &stubApprover{}
	r := NewRegistry(p, WithApprover(ap))
	rec := &outcomeRec{}
	r.SetAutonomyOutcomeRecorder(func(tool string, ok bool) { rec.count++ })
	wt := newWriteStub("job_retry", "node-1")
	r.MustRegister(wt)

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("应转审批而不是报错: %v", err)
	}
	if wt.calls != 0 || rec.count != 0 {
		t.Fatalf("转审批没有执行，不得回写信用: calls=%d recorded=%d", wt.calls, rec.count)
	}
}

// TestAutonomyOutcomeNotRecordedForReadTools 只读工具不消耗写动作信用。
func TestAutonomyOutcomeNotRecordedForReadTools(t *testing.T) {
	r := NewRegistry(DefaultPolicy())
	rec := &outcomeRec{}
	r.SetAutonomyOutcomeRecorder(func(tool string, ok bool) { rec.count++ })
	r.MustRegister(newStub("node_status"))

	if _, err := r.Call(context.Background(), "s1", "node_status", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("只读工具应可直接执行: %v", err)
	}
	if rec.count != 0 {
		t.Fatalf("只读工具不得回写写动作信用，实际 recorded=%d", rec.count)
	}
}

// TestAutonomyOutcomeNotRecordedForUnknownTool 工具不存在（没有执行）不记。
func TestAutonomyOutcomeNotRecordedForUnknownTool(t *testing.T) {
	r := NewRegistry(writePolicy())
	rec := &outcomeRec{}
	r.SetAutonomyOutcomeRecorder(func(tool string, ok bool) { rec.count++ })

	if _, err := r.Call(context.Background(), "s1", "no_such_tool", map[string]interface{}{"q": "x"}); err == nil {
		t.Fatal("未知工具应被拒绝")
	}
	if rec.count != 0 {
		t.Fatalf("未知工具没有执行，不得回写: recorded=%d", rec.count)
	}
}

// TestAutonomyOutcomeNotRecordedWhenOff 未装配回写回调时，一切照旧（零回归）。
func TestAutonomyOutcomeNotRecordedWhenOff(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL2
	p.AutoApprove = []string{"job_retry@node-1"}
	p.MaxAffectedHostsPerSession = 2
	r := NewRegistry(p, WithApprover(&stubApprover{}))
	r.MustRegister(newWriteStub("job_retry", "node-1"))

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("未装配回写不应影响调用: %v", err)
	}
}

// TestSetAutonomyOutcomeRecorderCannotSwap 回写目标只允许装配一次（不允许热换口径）。
func TestSetAutonomyOutcomeRecorderCannotSwap(t *testing.T) {
	r := NewRegistry(writePolicy())
	n1, n2 := 0, 0
	r.SetAutonomyOutcomeRecorder(func(string, bool) { n1++ })
	r.SetAutonomyOutcomeRecorder(func(string, bool) { n2++ })
	r.recordAutonomyOutcome(RiskWrite, "job_retry", true)
	if n1 != 1 || n2 != 0 {
		t.Fatalf("第二次装配应被忽略（第一次生效），实际 first=%d second=%d", n1, n2)
	}
}
