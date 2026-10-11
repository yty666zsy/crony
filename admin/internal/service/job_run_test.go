package service

import (
	"math"
	"testing"

	"github.com/tmnhs/crony/common/models"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestSummarizeRunsSuccessRateExcludesSkipped 成功率口径：
// 被并发策略跳过不是失败，不能进成功率的分母，否则正常的并发保护会被显示成故障。
func TestSummarizeRunsSuccessRateExcludesSkipped(t *testing.T) {
	rows := []RunStatusCount{
		{Status: int(models.RunStatusSuccess), Count: 8, Retry: 3},
		{Status: int(models.RunStatusFailed), Count: 1, Retry: 2},
		{Status: int(models.RunStatusTimeout), Count: 1, Retry: 0},
		{Status: int(models.RunStatusCanceled), Count: 0, Retry: 0},
		{Status: int(models.RunStatusSkipped), Count: 5, Retry: 0},
		{Status: int(models.RunStatusRunning), Count: 1, Retry: 0},
		{Status: int(models.RunStatusPending), Count: 0, Retry: 0},
	}
	got := SummarizeRuns(rows, 1700000000)

	if got.Total != 16 {
		t.Fatalf("total = %d, want 16", got.Total)
	}
	if got.Success != 8 || got.Failed != 1 || got.Timeout != 1 || got.Skipped != 5 || got.Running != 1 {
		t.Fatalf("状态计数不符: %+v", got)
	}
	if got.RetryTotal != 5 {
		t.Fatalf("重试次数合计 = %d, want 5", got.RetryTotal)
	}
	// 分母 = 16 - 5(skipped) = 11；8/11
	if !approx(got.SuccessRate, 8.0/11.0) {
		t.Fatalf("success_rate = %v, want %v（跳过不得计入分母）", got.SuccessRate, 8.0/11.0)
	}
	// 跳过率 = 5/16
	if !approx(got.SkipRate, 5.0/16.0) {
		t.Fatalf("skip_rate = %v, want %v", got.SkipRate, 5.0/16.0)
	}
	if got.ByStatus["success"] != 8 || got.ByStatus["skipped"] != 5 {
		t.Fatalf("by_status 不符: %+v", got.ByStatus)
	}
	if got.Since != 1700000000 {
		t.Fatalf("since = %d", got.Since)
	}
}

// TestSummarizeRunsEmptyAndAllSkipped 边界：不能出现除零（NaN）。
func TestSummarizeRunsEmptyAndAllSkipped(t *testing.T) {
	empty := SummarizeRuns(nil, 0)
	if empty.Total != 0 || empty.SuccessRate != 0 || empty.SkipRate != 0 {
		t.Fatalf("空结果应为全 0: %+v", empty)
	}

	allSkipped := SummarizeRuns([]RunStatusCount{{Status: int(models.RunStatusSkipped), Count: 4}}, 0)
	if allSkipped.Total != 4 || allSkipped.Skipped != 4 {
		t.Fatalf("计数不符: %+v", allSkipped)
	}
	if allSkipped.SuccessRate != 0 {
		t.Fatalf("全部跳过时成功率应为 0（且不能是 NaN），实际 %v", allSkipped.SuccessRate)
	}
	if !approx(allSkipped.SkipRate, 1.0) {
		t.Fatalf("全部跳过时 skip_rate 应为 1，实际 %v", allSkipped.SkipRate)
	}
}

// TestSummarizeRunsUnknownStatusIsCounted 未知状态值也要计入 total（不能丢数据）。
func TestSummarizeRunsUnknownStatusIsCounted(t *testing.T) {
	got := SummarizeRuns([]RunStatusCount{{Status: 99, Count: 3}}, 0)
	if got.Total != 3 {
		t.Fatalf("未知状态也必须计入 total，实际 %+v", got)
	}
	if got.ByStatus["unknown(99)"] != 3 {
		t.Fatalf("by_status 键应可读: %+v", got.ByStatus)
	}
}
