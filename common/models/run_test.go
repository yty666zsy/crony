package models

import (
	"testing"
	"time"
)

func TestRunStatusStringAndFinal(t *testing.T) {
	cases := []struct {
		status RunStatus
		name   string
		final  bool
	}{
		{RunStatusPending, "pending", false},
		{RunStatusRunning, "running", false},
		{RunStatusSuccess, "success", true},
		{RunStatusFailed, "failed", true},
		{RunStatusCanceled, "canceled", true},
		{RunStatusSkipped, "skipped", true},
		{RunStatusTimeout, "timeout", true},
	}
	for _, tc := range cases {
		if got := tc.status.String(); got != tc.name {
			t.Errorf("RunStatus(%d).String() = %q, want %q", int(tc.status), got, tc.name)
		}
		if got := tc.status.IsFinal(); got != tc.final {
			t.Errorf("%s.IsFinal() = %v, want %v", tc.name, got, tc.final)
		}
	}
	if got := RunStatus(99).String(); got != "unknown(99)" {
		t.Errorf("未知状态应打印 unknown(99)，实际 %q", got)
	}
}

// TestCanTransition 固化状态机：终态不可变更，非法迁移必须被拒绝，
// 否则审计记录会被后续写入静默覆盖（这正是 job_log 旧模型的问题）。
func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to RunStatus
		want     bool
	}{
		{RunStatusPending, RunStatusRunning, true},
		{RunStatusPending, RunStatusSkipped, true},
		{RunStatusPending, RunStatusCanceled, true},
		{RunStatusPending, RunStatusSuccess, false}, // 不能跳过 running 直接成功
		{RunStatusRunning, RunStatusSuccess, true},
		{RunStatusRunning, RunStatusFailed, true},
		{RunStatusRunning, RunStatusTimeout, true},
		{RunStatusRunning, RunStatusCanceled, true},
		{RunStatusRunning, RunStatusPending, false},
		{RunStatusSuccess, RunStatusFailed, false}, // 终态不可改
		{RunStatusFailed, RunStatusSuccess, false},
		{RunStatusCanceled, RunStatusRunning, false},
		{RunStatusTimeout, RunStatusSuccess, false},
		{RunStatusSkipped, RunStatusRunning, false},
	}
	for _, tc := range cases {
		if got := CanTransition(tc.from, tc.to); got != tc.want {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestRunIDForSlotIsDeterministic 是幂等性的基础：
// 同一任务同一调度槽位必须得到同一个 run_id（唯一索引据此挡住重复执行）。
func TestRunIDForSlotIsDeterministic(t *testing.T) {
	slot := time.Date(2026, 10, 11, 16, 0, 0, 0, time.UTC).Unix()
	a := RunIDForSlot(42, slot)
	b := RunIDForSlot(42, slot)
	if a != b {
		t.Fatalf("同一槽位生成了不同的 run_id: %q vs %q", a, b)
	}
	if a == RunIDForSlot(43, slot) {
		t.Fatal("不同任务不得共用 run_id")
	}
	if a == RunIDForSlot(42, slot+60) {
		t.Fatal("不同槽位不得共用 run_id")
	}
	if !IsSlotRunID(a) {
		t.Fatalf("调度槽位 run_id %q 应被识别为幂等键", a)
	}
}

func TestNewManualRunIDIsUnique(t *testing.T) {
	now := time.Now()
	a := NewManualRunID(7, now)
	b := NewManualRunID(7, now)
	if a == b {
		t.Fatal("手动触发的 run_id 必须唯一（不能因为同秒触发而碰撞）")
	}
	if IsSlotRunID(a) {
		t.Fatalf("手动 run_id %q 不应被当作调度槽位幂等键", a)
	}
}

func TestTriggerSourceString(t *testing.T) {
	cases := map[TriggerSource]string{
		TriggerScheduler: "scheduler",
		TriggerManual:    "manual",
		TriggerRetry:     "retry",
		TriggerAPI:       "api",
		TriggerAgent:     "agent",
		TriggerSelfHeal:  "self_heal",
	}
	for source, want := range cases {
		if got := source.String(); got != want {
			t.Errorf("TriggerSource(%d).String() = %q, want %q", int(source), got, want)
		}
	}
	if got := TriggerSource(99).String(); got != "unknown(99)" {
		t.Errorf("未知触发来源应打印 unknown(99)，实际 %q", got)
	}
}

func TestRunTableNames(t *testing.T) {
	run := &JobRun{}
	step := &JobRunStep{}
	if run.TableName() != CronyJobRunTableName || CronyJobRunTableName != "job_run" {
		t.Fatalf("job_run 表名不符: %q", run.TableName())
	}
	if step.TableName() != CronyJobRunStepTableName || CronyJobRunStepTableName != "job_run_step" {
		t.Fatalf("job_run_step 表名不符: %q", step.TableName())
	}
}
