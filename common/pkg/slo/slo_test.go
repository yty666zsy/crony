package slo

import (
	"testing"
	"time"
)

func obs(run string, status int, sched, started, finished int64) Observation {
	return Observation{RunID: run, Status: status, ScheduledTime: sched, StartedAt: started, FinishedAt: finished}
}

func TestSuccessRateBreach(t *testing.T) {
	now := time.Now().Unix()
	// 10 次里 6 次成功 = 60%，目标 90%。
	obsList := []Observation{}
	for i := 0; i < 10; i++ {
		st := 2
		if i < 4 {
			st = 3
		}
		obsList = append(obsList, obs("r", st, now, now, now))
	}
	r := Evaluate(Targets{TargetSuccessRate: 0.9}, obsList)
	if !r.Breached {
		t.Fatalf("60%% < 90%% 应违约，实际 %+v", r)
	}
	if r.Success != 6 || r.Total != 10 {
		t.Fatalf("统计错误：success=%d total=%d", r.Success, r.Total)
	}
	if len(r.Reasons) == 0 {
		t.Fatalf("违约必须给出原因")
	}
}

func TestOnTimeUsesGrace(t *testing.T) {
	now := time.Now().Unix()
	// 一次准点（晚 1 分钟，在 5 分钟宽限内），一次迟到（晚 10 分钟）。
	good := obs("a", 2, now, now+60, now+120)
	late := obs("b", 2, now, now+600, now+660)
	r := Evaluate(Targets{TargetOnTimeRate: 1.0, Grace: 5 * time.Minute}, []Observation{good, late})
	if r.OnTimeEligible != 2 || r.OnTime != 1 {
		t.Fatalf("准时口径错误：eligible=%d ontime=%d", r.OnTimeEligible, r.OnTime)
	}
	if !r.Breached {
		t.Fatalf("准时率 50%% < 100%% 应违约")
	}
}

// 手动/API 触发没有计划时间：不该拉低准时率，也不该被判准时率违约。
func TestManualRunsExcludedFromOnTime(t *testing.T) {
	now := time.Now().Unix()
	manual := obs("m", 2, 0, now, now+10) // 无计划时间
	r := Evaluate(Targets{TargetOnTimeRate: 1.0}, []Observation{manual})
	if r.OnTimeEligible != 0 {
		t.Fatalf("无计划时间不应计入准时样本，实际 %d", r.OnTimeEligible)
	}
	if r.Breached {
		t.Fatalf("只有手动触发时不应因准时率违约（没有可评估样本）")
	}
}

// 无样本：既不违约也不达标。
func TestNoSamplesIsUnverifiable(t *testing.T) {
	r := Evaluate(Targets{TargetSuccessRate: 0.9, TargetOnTimeRate: 0.9}, nil)
	if !r.Unverifiable {
		t.Fatalf("无样本必须标记 unverifiable")
	}
	if r.Breached {
		t.Fatalf("无样本不得判违约（否则误报风暴）")
	}
	if r.SuccessRate != -1 {
		t.Fatalf("无样本成功率应为 -1（区别于 0%%），实际 %v", r.SuccessRate)
	}
}

// 未结束的执行不进分母。
func TestUnfinishedExcluded(t *testing.T) {
	now := time.Now().Unix()
	running := obs("r", 1, now, now, 0) // finished=0
	done := obs("d", 2, now, now, now+1)
	r := Evaluate(Targets{}, []Observation{running, done})
	if r.Total != 1 {
		t.Fatalf("未结束的执行不该进统计，实际 total=%d", r.Total)
	}
}
