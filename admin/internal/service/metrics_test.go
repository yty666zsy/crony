package service

import (
	"math"
	"testing"

	"github.com/tmnhs/crony/common/models"
)

// ---------- 指标口径的穷举测试 ----------
//
// 这里测的全是"分母"：指标看板真正会错的地方不是算术，而是**哪些行算进分母**。
// 分母错了页面照样好看，只是数字从此没人能信。

func TestRatioOrNoSample(t *testing.T) {
	cases := []struct {
		name   string
		num    int64
		den    int64
		want   float64
		reason string
	}{
		{"分母为 0", 0, 0, NoSample, "没有样本 → 无样本，不是 0%"},
		{"有分子没分母", 5, 0, NoSample, "没观测到样本时分子不该有值，仍然只能是无样本"},
		{"负分母", 5, -3, NoSample, "脏数据不得变成负数比率"},
		{"分子为 0 但有样本", 0, 5, 0, "全是坏的 = 0%，与无样本必须区分开"},
		{"一半", 1, 2, 0.5, ""},
		{"全通过", 7, 7, 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RatioOrNoSample(c.num, c.den)
			if math.Abs(got-c.want) > 1e-12 {
				t.Fatalf("RatioOrNoSample(%d,%d)=%v，want %v（%s）", c.num, c.den, got, c.want, c.reason)
			}
		})
	}
}

func TestFoldHealRates(t *testing.T) {
	S, E, F := string(models.HealStatusSucceeded), string(models.HealStatusEscalated), string(models.HealStatusFailed)
	D, P, X := string(models.HealStatusDenied), string(models.HealStatusPlanned), string(models.HealStatusExecuted)

	cases := []struct {
		name                       string
		by                         map[string]int64
		wantDecided                int64
		wantAuto, wantTake, wantMi float64
		why                        string
	}{
		{
			name: "一条记录都没有", by: map[string]int64{},
			wantDecided: 0, wantAuto: NoSample, wantTake: NoSample, wantMi: NoSample,
			why: "无样本必须是 NoSample，不能是 0%",
		},
		{
			name: "只有 denied", by: map[string]int64{D: 5},
			wantDecided: 0, wantAuto: NoSample, wantTake: NoSample, wantMi: NoSample,
			why: "策略拒绝 = 系统正确地没动手，把它算进分母等于惩罚正确行为",
		},
		{
			name: "只有 planned/executed", by: map[string]int64{P: 2, X: 3},
			wantDecided: 0, wantAuto: NoSample, wantTake: NoSample, wantMi: NoSample,
			why: "还没出结论的处置不构成样本",
		},
		{
			name: "denied 与结论混合", by: map[string]int64{D: 9, S: 1},
			wantDecided: 1, wantAuto: 1, wantTake: 0, wantMi: 0,
			why: "9 条拒绝不得稀释 1 条成功：自动修复率仍是 100%",
		},
		{
			name: "成功与失败各一", by: map[string]int64{S: 1, F: 1},
			wantDecided: 2, wantAuto: 0.5, wantTake: 0, wantMi: 0.5,
		},
		{
			name: "三态齐全", by: map[string]int64{S: 2, E: 1, F: 1},
			wantDecided: 4, wantAuto: 0.5, wantTake: 0.25, wantMi: 0.25,
		},
		{
			name: "全部转人工", by: map[string]int64{E: 3},
			wantDecided: 3, wantAuto: 0, wantTake: 1, wantMi: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FoldHealRates(c.by)
			if got.Decided != c.wantDecided {
				t.Fatalf("decided=%d want %d", got.Decided, c.wantDecided)
			}
			if got.Decided != got.Succeeded+got.Escalated+got.Failed {
				t.Fatalf("decided 必须等于三态之和：%+v", got)
			}
			for _, p := range []struct {
				name string
				got  float64
				want float64
			}{
				{"自动修复率", got.AutoRepair, c.wantAuto},
				{"人工接管率", got.Takeover, c.wantTake},
				{"误动作率", got.Misaction, c.wantMi},
			} {
				if math.Abs(p.got-p.want) > 1e-12 {
					t.Fatalf("%s=%v want %v（%s）", p.name, p.got, p.want, c.why)
				}
			}
		})
	}
}

func TestFoldApprovalRates(t *testing.T) {
	AP, EX, EF, BL := models.ApprovalStatusApproved, models.ApprovalStatusExecuted,
		models.ApprovalStatusExecutionFailed, models.ApprovalStatusBlocked
	RJ, PD, XP, CN := models.ApprovalStatusRejected, models.ApprovalStatusPending,
		models.ApprovalStatusExpired, models.ApprovalStatusCanceled

	cases := []struct {
		name        string
		by          map[string]int64
		wantApprove int64
		wantDecided int64
		wantRate    float64
		why         string
	}{
		{
			name: "空表", by: map[string]int64{},
			wantApprove: 0, wantDecided: 0, wantRate: NoSample,
		},
		{
			name: "全是没人管的", by: map[string]int64{PD: 3, XP: 2},
			wantApprove: 0, wantDecided: 0, wantRate: NoSample,
			why: "待审批与超时未处理都不是'不通过'，否则看板会把'没人管'报成'全被拒'",
		},
		{
			name: "撤回不算不通过", by: map[string]int64{CN: 4},
			wantApprove: 0, wantDecided: 0, wantRate: NoSample,
			why: "发起方撤回没有对'该不该做'下结论",
		},
		{
			name: "批一拒一", by: map[string]int64{AP: 1, RJ: 1},
			wantApprove: 1, wantDecided: 2, wantRate: 0.5,
		},
		{
			name: "批准后已流转的四种状态都算通过", by: map[string]int64{AP: 1, EX: 1, EF: 1, BL: 1, RJ: 2},
			wantApprove: 4, wantDecided: 6, wantRate: 4.0 / 6.0,
			why: "只数 approved 会让通过率随执行推进假性下跌（批了 10 单执行完变 20%）",
		},
		{
			name: "待审批不参与", by: map[string]int64{AP: 2, PD: 8},
			wantApprove: 2, wantDecided: 2, wantRate: 1,
			why: "8 条待审批不得把通过率拉成 20%",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FoldApprovalRates(c.by)
			if got.Approved != c.wantApprove || got.Decided != c.wantDecided {
				t.Fatalf("approved=%d decided=%d，want %d/%d", got.Approved, got.Decided, c.wantApprove, c.wantDecided)
			}
			if got.Decided != got.Approved+got.Rejected {
				t.Fatalf("decided 必须等于 通过+不通过：%+v", got)
			}
			if math.Abs(got.PassRate-c.wantRate) > 1e-12 {
				t.Fatalf("通过率=%v want %v（%s）", got.PassRate, c.wantRate, c.why)
			}
		})
	}
}

func TestFoldSessionTokenCost(t *testing.T) {
	cases := []struct {
		name        string
		tokens      []int64
		wantCount   int64
		wantTotal   int64
		wantAverage float64
		why         string
	}{
		{"没有会话", nil, 0, 0, NoSample, ""},
		{"会话都没记账", []int64{0, 0}, 0, 0, NoSample,
			"token 记账之前的历史会话是 0，摊进分母只会得到一个好看但没意义的平均成本"},
		{"混合（含未记账）", []int64{0, 100, 300}, 2, 400, 200, "只算有记账的两条"},
		{"负数脏数据被忽略", []int64{-5, 120}, 1, 120, 120, "脏数据不得把平均值拉下去"},
		{"全部有记账", []int64{50, 150, 400}, 3, 600, 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FoldSessionTokenCost(c.tokens)
			if got.Sessions != c.wantCount || got.Total != c.wantTotal {
				t.Fatalf("sessions=%d total=%d，want %d/%d", got.Sessions, got.Total, c.wantCount, c.wantTotal)
			}
			if math.Abs(got.AvgTokens-c.wantAverage) > 1e-12 {
				t.Fatalf("平均 token=%v want %v（%s）", got.AvgTokens, c.wantAverage, c.why)
			}
		})
	}
}
