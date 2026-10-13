package handler

import (
	"strings"
	"testing"

	"github.com/tmnhs/crony/common/models"
)

// 成功率的**三态**文本必须三处（列表行 / 召回命中 / 全局统计）同口径。
//
// 这条测试存在的理由：后端早前只有"列表行"给了 success_rate_text，召回命中与
// 统计接口没给，前端因此在真机上要么显示空白、要么只能退化成"无执行样本"。
// 而"没有执行样本"与"0% 成功率"是两句结论完全相反的话，不能混。
func TestMemoryRateTextThreeStates(t *testing.T) {
	none := memoryRateText(0, false, 0, 0)
	if !strings.Contains(none, "无执行样本") {
		t.Fatalf("没有执行样本时必须明确说\"无执行样本\"，而不是 0%%: %q", none)
	}
	if strings.Contains(none, "0.0%") {
		t.Fatalf("无样本不得渲染成 0%%（会被读成\"从来没成功过\"）: %q", none)
	}

	got := memoryRateText(0.25, true, 16, 48)
	want := "25.0%（成功 16 / 失败 48）"
	if got != want {
		t.Fatalf("有样本时的文案应为 %q，实际 %q", want, got)
	}

	// 0% 是"有样本但都失败了"，必须与"无样本"区分开。
	zero := memoryRateText(0, true, 0, 4)
	if !strings.Contains(zero, "0.0%") {
		t.Fatalf("有 4 次失败样本时必须显示 0.0%%，实际 %q", zero)
	}
}

// memorySuccessRateText 是列表行用的包装，必须与 memoryRateText 同口径。
func TestMemorySuccessRateTextMatchesRateText(t *testing.T) {
	m := &models.AgentMemory{SuccessCount: 3, FailCount: 1}
	if got, want := memorySuccessRateText(0.75, true, m), memoryRateText(0.75, true, 3, 1); got != want {
		t.Fatalf("列表行文案与统一口径不一致: %q vs %q", got, want)
	}
	m2 := &models.AgentMemory{}
	if got := memorySuccessRateText(0, false, m2); !strings.Contains(got, "无执行样本") {
		t.Fatalf("无样本行必须显示无执行样本: %q", got)
	}
}
