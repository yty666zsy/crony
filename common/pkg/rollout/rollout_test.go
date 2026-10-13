package rollout

import (
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/tools"
)

// 放量闸门的判定是纯函数，所以这里可以**穷举**它：坏信号、缺证据、过期证据、
// 全跳过、超上限、非法档位——每一条都要能被单独证伪，而不是"跑一遍看起来对"。
func TestBlockers(t *testing.T) {
	cases := []struct {
		name  string
		ev    Evidence
		count int      // 期望的坏信号条数
		kw    []string // 期望在坏信号里出现的关键词
	}{
		{
			name:  "全绿：没有坏信号",
			ev:    Evidence{EvalRunID: 9, EvalPassed: 4},
			count: 0,
		},
		{
			name:  "评测判红：算坏信号",
			ev:    Evidence{EvalRunID: 9, EvalPassed: 3, EvalFailed: 1},
			count: 1,
			kw:    []string{"不通过", "run_id=9"},
		},
		{
			name:  "全部跳过：不是坏信号（没考到≠做错了）",
			ev:    Evidence{EvalRunID: 9, EvalSkipped: 4},
			count: 0,
		},
		{
			name:  "SLO 违约：算坏信号",
			ev:    Evidence{SloTotal: 3, SloBreaches: 2},
			count: 1,
			kw:    []string{"SLO 违约", "2/3"},
		},
		{
			name:  "还没跑过批：不是坏信号（是缺证据）",
			ev:    Evidence{},
			count: 0,
		},
	}
	for _, tc := range cases {
		got := Blockers(tc.ev)
		if len(got) != tc.count {
			t.Fatalf("%s：坏信号条数 = %d %v，期望 %d", tc.name, len(got), got, tc.count)
		}
		// 关键词按整体包含校验：一条原因里往往同时含多个关键信息。
		joined := strings.Join(got, " | ")
		for _, kw := range tc.kw {
			if !strings.Contains(joined, kw) {
				t.Fatalf("%s：坏信号 %q 未包含 %q", tc.name, joined, kw)
			}
		}
	}
}

func TestWidenReasons(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	freshEval := Evidence{EvalRunID: 12, EvalPassed: 4, EvalSkipped: 0, EvalScore: 1, EvalAt: now.Unix() - 60}
	freshSlo := func(ev Evidence) Evidence { ev.SloTotal, ev.SloAt = 2, now.Unix()-30; return ev }
	cfg := DefaultConfig()

	type want struct {
		reasons int // 期望的拒绝原因条数（-1 = 不校验条数，只校验"非空/为空"）
		has     string
	}
	cases := []struct {
		name  string
		stage tools.AutonomyLevel
		ev    Evidence
		cfg   Config
		want  want
	}{
		{
			name:  "证据齐全且干净：允许加宽到 L2",
			stage: tools.AutonomyL2,
			ev:    freshSlo(freshEval),
			cfg:   cfg,
			want:  want{reasons: 0},
		},
		{
			name:  "允许加宽到 L1",
			stage: tools.AutonomyL1,
			ev:    freshSlo(freshEval),
			cfg:   cfg,
			want:  want{reasons: 0},
		},
		{
			name:  "超过放量上限 L3：拒绝（要改配置，不靠放量）",
			stage: tools.AutonomyL3,
			ev:    freshSlo(freshEval),
			cfg:   cfg,
			want:  want{reasons: 1, has: "最高档 L2"},
		},
		{
			name:  "L0 不是放量目标：拒绝并指向 tighten",
			stage: tools.AutonomyL0,
			ev:    freshSlo(freshEval),
			cfg:   cfg,
			want:  want{reasons: 1, has: "tighten"},
		},
		{
			name:  "非法档位：拒绝",
			stage: tools.AutonomyLevel("L9"),
			ev:    freshSlo(freshEval),
			cfg:   cfg,
			want:  want{reasons: 1, has: "未知放量档位"},
		},
		{
			name:  "没有任何证据：拒绝（缺评测 + 缺 SLO）",
			stage: tools.AutonomyL2,
			ev:    Evidence{},
			cfg:   cfg,
			want:  want{reasons: 2, has: "缺少评测证据"},
		},
		{
			name:  "评测证据过期：拒绝",
			stage: tools.AutonomyL2,
			ev:    freshSlo(Evidence{EvalRunID: 12, EvalPassed: 4, EvalAt: now.Unix() - int64(25*time.Hour/time.Second)}),
			cfg:   cfg,
			want:  want{reasons: 1, has: "已过期"},
		},
		{
			name:  "全部跳过：拒绝（跳过不等于通过）",
			stage: tools.AutonomyL2,
			ev:    freshSlo(Evidence{EvalRunID: 12, EvalSkipped: 4, EvalAt: now.Unix() - 60}),
			cfg:   cfg,
			want:  want{reasons: 1, has: "没有有效样本"},
		},
		{
			name:  "有坏信号：拒绝，且坏信号排在缺证据之前",
			stage: tools.AutonomyL2,
			ev:    Evidence{EvalRunID: 12, EvalFailed: 1, EvalAt: now.Unix() - 60},
			cfg:   cfg,
			want:  want{reasons: 2, has: "不通过"},
		},
		{
			name:  "干净证据卡在 SLO 违约上：拒绝",
			stage: tools.AutonomyL1,
			ev:    Evidence{EvalRunID: 12, EvalPassed: 4, EvalAt: now.Unix() - 60, SloTotal: 1, SloBreaches: 1, SloAt: now.Unix() - 30},
			cfg:   cfg,
			want:  want{reasons: 1, has: "SLO 违约"},
		},
		{
			name:  "放宽要求（不需要 SLO 证据）：只凭评测即可",
			stage: tools.AutonomyL2,
			ev:    freshEval,
			cfg:   Config{EvidenceTTL: time.Hour, RequireEval: true, RequireSLO: false, MaxStage: tools.AutonomyL2},
			want:  want{reasons: 0},
		},
	}
	for _, tc := range cases {
		got := WidenReasons(tc.stage, tc.ev, tc.cfg, now)
		if tc.want.reasons == 0 {
			if len(got) != 0 {
				t.Fatalf("%s：期望可以加宽，实际被拒: %v", tc.name, got)
			}
			continue
		}
		if len(got) != tc.want.reasons {
			t.Fatalf("%s：拒绝原因条数 = %d %v，期望 %d", tc.name, len(got), got, tc.want.reasons)
		}
		if !strings.Contains(got[0], tc.want.has) {
			t.Fatalf("%s：首条原因 %q 未包含 %q", tc.name, got[0], tc.want.has)
		}
	}
}

func TestAutoTarget(t *testing.T) {
	// 没有坏信号：不自动收紧（ok=false）。
	if stage, reasons, ok := AutoTarget(Evidence{EvalRunID: 3, EvalPassed: 4}); ok {
		t.Fatalf("干净证据不该触发自动降级，却给了 stage=%s reasons=%v", stage, reasons)
	}
	// 有坏信号：一律 L0，且必须说明为什么。
	for _, ev := range []Evidence{
		{EvalRunID: 3, EvalFailed: 2},
		{SloTotal: 2, SloBreaches: 1},
	} {
		stage, reasons, ok := AutoTarget(ev)
		if !ok {
			t.Fatalf("坏信号 %+v 必须触发自动降级", ev)
		}
		if stage != tools.AutonomyL0 {
			t.Fatalf("自动降级目标必须是 L0，实际 %s", stage)
		}
		if len(reasons) == 0 {
			t.Fatal("自动降级必须写明原因（否则运维只看到一个没有解释的 L0）")
		}
	}
}

// 证据有效期是"放量"与"曾经好过"的分界线，单独钉一条。
func TestEvidenceTTLBoundary(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	cfg := Config{EvidenceTTL: time.Hour, RequireEval: true, RequireSLO: false, MaxStage: tools.AutonomyL2}
	// 正好一小时：不算过期（用的是 >，边界不抖动）。
	ev := Evidence{EvalRunID: 1, EvalPassed: 1, EvalAt: now.Unix() - 3600}
	if got := WidenReasons(tools.AutonomyL1, ev, cfg, now); len(got) != 0 {
		t.Fatalf("边界（正好等于 TTL）不该判过期: %v", got)
	}
	// 一小时零一秒：过期。
	ev.EvalAt = now.Unix() - 3601
	got := WidenReasons(tools.AutonomyL1, ev, cfg, now)
	if len(got) != 1 || !strings.Contains(got[0], "已过期") {
		t.Fatalf("超过 TTL 必须判过期，实际: %v", got)
	}
}
