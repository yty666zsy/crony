package service

import (
	"testing"

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
