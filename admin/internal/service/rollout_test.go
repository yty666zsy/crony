package service

import (
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 闸门是三层取最严（配置 ∩ 信用 ∩ 放量），其中"信用 ∩ 放量"这一折由 foldCaps 负责。
// 这是**安全关键**的一小段：一旦它变成"取更宽的那个"，放量闸门就名存实亡，
// 所以这里把四种组合都钉死。
func TestFoldCaps(t *testing.T) {
	cases := []struct {
		name      string
		credit    tools.AutonomyLevel
		creditOK  bool
		cap       tools.AutonomyLevel
		capOK     bool
		want      tools.AutonomyLevel
		wantKnown bool
	}{
		{
			name:   "两者都表态：取更严的那个（信用 L1 比放量 L2 严）",
			credit: tools.AutonomyL1, creditOK: true,
			cap: tools.AutonomyL2, capOK: true,
			want: tools.AutonomyL1, wantKnown: true,
		},
		{
			name:   "两者都表态：取更严的那个（放量 L0 比信用 L2 严）",
			credit: tools.AutonomyL2, creditOK: true,
			cap: tools.AutonomyL0, capOK: true,
			want: tools.AutonomyL0, wantKnown: true,
		},
		{
			name:   "只有信用表态：听信用（放量不表态 = 沿用）",
			credit: tools.AutonomyL2, creditOK: true,
			cap: tools.AutonomyL3, capOK: false,
			want: tools.AutonomyL2, wantKnown: true,
		},
		{
			name:   "只有放量表态：听放量（信用无历史 = 沿用）",
			credit: tools.AutonomyL3, creditOK: false,
			cap: tools.AutonomyL0, capOK: true,
			want: tools.AutonomyL0, wantKnown: true,
		},
		{
			name:   "都不表态：unknown，由网关回落到配置级别",
			credit: tools.AutonomyL2, creditOK: false,
			cap: tools.AutonomyL2, capOK: false,
			want: "", wantKnown: false,
		},
	}
	for _, tc := range cases {
		got, known := foldCaps(tc.credit, tc.creditOK, tc.cap, tc.capOK)
		if known != tc.wantKnown || (known && got != tc.want) {
			t.Fatalf("%s: foldCaps = (%q, %v)，期望 (%q, %v)", tc.name, got, known, tc.want, tc.wantKnown)
		}
	}
}

// nextStage 只用于"再放一档"的解释文案，钉住它的边界：L0->L1->L2->L3，L3 不再上。
func TestNextStage(t *testing.T) {
	cases := map[tools.AutonomyLevel]tools.AutonomyLevel{
		tools.AutonomyL0: tools.AutonomyL1,
		tools.AutonomyL1: tools.AutonomyL2,
		tools.AutonomyL2: tools.AutonomyL3,
		tools.AutonomyL3: tools.AutonomyL3,
	}
	for cur, want := range cases {
		if got := nextStage(cur); got != want {
			t.Fatalf("nextStage(%s) = %s，期望 %s", cur, got, want)
		}
	}
}

// AutoEvalSkip 是自动降级循环的守卫内核：它决定"这一轮要不要真的去收回放量"。
// 这里把它当安全边界来钉 —— 尤其是"没有记录不新造"与"已是 L0 不重复收回"两条，
// 一旦放宽，"打开开关"就会从零回归变成全平台行为突变。
func TestAutoEvalSkip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := func(stage string, tightenedAt int64) *models.RolloutState {
		return &models.RolloutState{Env: "default", Stage: stage, TightenedAt: tightenedAt}
	}
	cases := []struct {
		name       string
		enabled    bool
		state      *models.RolloutState
		cooldown   time.Duration
		wantSkip   bool
		wantReason string // 非空时要求原因包含该子串
	}{
		{name: "开关关闭：跳过（零回归）", enabled: false, state: st("L2", 0), cooldown: time.Hour, wantSkip: true, wantReason: "未开启"},
		{name: "没有记录：跳过（只收回、不新造）", enabled: true, state: nil, cooldown: time.Hour, wantSkip: true, wantReason: "还没有放量记录"},
		{name: "已是 L0：跳过（不重复收回）", enabled: true, state: st("L0", 0), cooldown: time.Hour, wantSkip: true, wantReason: "已是 L0"},
		{name: "有放量且从未收紧：放行", enabled: true, state: st("L2", 0), cooldown: time.Hour, wantSkip: false},
		{name: "距上次收紧在冷却内：跳过", enabled: true, state: st("L2", now.Unix()-10), cooldown: time.Hour, wantSkip: true, wantReason: "未到冷却"},
		{name: "距上次收紧已过冷却（边界外）：放行", enabled: true, state: st("L2", now.Unix()-7200), cooldown: time.Hour, wantSkip: false},
		{name: "冷却边界（恰好等于）：放行", enabled: true, state: st("L2", now.Unix()-3600), cooldown: time.Hour, wantSkip: false},
		{name: "冷却为 0：不做退避", enabled: true, state: st("L2", now.Unix()), cooldown: 0, wantSkip: false},
		{name: "收紧时间在未来（时钟漂移）：不放行过度拦截", enabled: true, state: st("L2", now.Unix()+600), cooldown: time.Hour, wantSkip: false},
	}
	for _, tc := range cases {
		skip, reason := AutoEvalSkip(tc.enabled, tc.state, now, tc.cooldown)
		if skip != tc.wantSkip {
			t.Fatalf("%s: skip = %v，期望 %v（reason=%q）", tc.name, skip, tc.wantSkip, reason)
		}
		if tc.wantSkip && tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
			t.Fatalf("%s: 跳过原因 %q 应包含 %q", tc.name, reason, tc.wantReason)
		}
		if !tc.wantSkip && reason != "" {
			t.Fatalf("%s: 放行时不应给出跳过原因，得到 %q", tc.name, reason)
		}
	}
}
