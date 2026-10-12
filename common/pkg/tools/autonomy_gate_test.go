package tools

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// 阶段 5：自治信用接入放行判定（配置是上限，信用只可收紧）
// ---------------------------------------------------------------------------

// TestTightenOnlyLowers 锁死核心不变式：min(ceiling, want)，信用永远无法越权。
func TestTightenOnlyLowers(t *testing.T) {
	cases := []struct {
		ceiling, want, expect AutonomyLevel
	}{
		{AutonomyL2, AutonomyL1, AutonomyL1}, // 收紧生效
		{AutonomyL2, AutonomyL3, AutonomyL2}, // 想升级 → 被夹住
		{AutonomyL1, AutonomyL0, AutonomyL0}, // 更严 → 生效
		{AutonomyL3, AutonomyL2, AutonomyL2}, // 收紧
		{AutonomyL1, AutonomyL1, AutonomyL1}, // 相等
		{AutonomyL0, AutonomyL3, AutonomyL0}, // 上限 L0 → 谁也别想动
	}
	for _, c := range cases {
		if got := Tighten(c.ceiling, c.want); got != c.expect {
			t.Fatalf("Tighten(%s, %s) = %s，期望 %s", c.ceiling, c.want, got, c.expect)
		}
	}
}

// TestAutonomyCreditsOffByDefault 未装配解析器时，行为与阶段 4 完全一致（零回归）。
func TestAutonomyCreditsOffByDefault(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL2
	p.AutoApprove = []string{"job_retry@node-1"}
	p.MaxAffectedHostsPerSession = 2 // 本例只看白名单，半径另有专测
	ap := &stubApprover{}
	r := NewRegistry(p, WithApprover(ap))
	wt := newWriteStub("job_retry", "node-1")
	r.MustRegister(wt)

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("命中白名单应直接执行: %v", err)
	}
	if wt.calls != 1 {
		t.Fatalf("未接入信用时命中白名单必须自动放行: calls=%d", wt.calls)
	}
	if r.AutonomyCreditEnabled() {
		t.Fatal("未装配解析器时 AutonomyCreditEnabled 必须为 false")
	}
}

// TestAutonomyResolverTightensWhitelist 信用收紧 → 命中白名单的写动作改回人工审批。
//
// 这是把"出了不良后果就自动回到审批"落地的核心行为。
func TestAutonomyResolverTightensWhitelist(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL2
	p.AutoApprove = []string{"job_retry@node-1"}
	p.MaxAffectedHostsPerSession = 2
	ap := &stubApprover{}
	r := NewRegistry(p, WithApprover(ap), WithAutonomyResolver(func(string) (AutonomyLevel, bool) {
		return AutonomyL1, true // 信用：该动作已回落到 L1
	}))
	wt := newWriteStub("job_retry", "node-1")
	r.MustRegister(wt)

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("应转审批而不是报错: %v", err)
	}
	if wt.calls != 0 {
		t.Fatalf("信用收紧后不得自动执行: calls=%d", wt.calls)
	}
	if len(ap.got) != 1 {
		t.Fatalf("应生成 1 张审批单: %d", len(ap.got))
	}
	if got := r.EffectiveAutonomyForTool("job_retry"); got != AutonomyL1 {
		t.Fatalf("生效级别应为 L1，实际 %s", got)
	}
}

// TestAutonomyResolverCannotEscalate 解析器想升级也过不了配置上限。
func TestAutonomyResolverCannotEscalate(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL1 // 上限就是 L1
	p.AutoApprove = []string{"job_retry@node-1"}
	ap := &stubApprover{}
	r := NewRegistry(p, WithApprover(ap), WithAutonomyResolver(func(string) (AutonomyLevel, bool) {
		return AutonomyL3, true // 信用：全自动（应被夹到 L1）
	}))
	wt := newWriteStub("job_retry", "node-1")
	r.MustRegister(wt)

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("应转审批而不是报错: %v", err)
	}
	if wt.calls != 0 {
		t.Fatalf("上限 L1 下不得自动执行: calls=%d", wt.calls)
	}
	if len(ap.got) != 1 {
		t.Fatalf("应生成 1 张审批单: %d", len(ap.got))
	}
	if got := r.EffectiveAutonomyForTool("job_retry"); got != AutonomyL1 {
		t.Fatalf("生效级别必须被上限夹在 L1，实际 %s", got)
	}
}

// TestAutonomyResolverNoOpinionKeepsConfig 无历史（ok=false）→ 沿用配置。
func TestAutonomyResolverNoOpinionKeepsConfig(t *testing.T) {
	p := writePolicy()
	p.Autonomy = AutonomyL2
	p.AutoApprove = []string{"job_retry@node-1"}
	p.MaxAffectedHostsPerSession = 2
	ap := &stubApprover{}
	r := NewRegistry(p, WithApprover(ap), WithAutonomyResolver(func(string) (AutonomyLevel, bool) {
		return "", false // 没有该动作的历史
	}))
	wt := newWriteStub("job_retry", "node-1")
	r.MustRegister(wt)

	if _, err := r.Call(context.Background(), "s1", "job_retry", map[string]interface{}{"q": "x"}); err != nil {
		t.Fatalf("命中白名单应直接执行: %v", err)
	}
	if wt.calls != 1 {
		t.Fatalf("无历史时应沿用配置（自动放行）: calls=%d", wt.calls)
	}
}

// TestSetAutonomyResolverCannotSwap 解析器只允许装配一次（不允许热换判据）。
func TestSetAutonomyResolverCannotSwap(t *testing.T) {
	r := NewRegistry(writePolicy())
	first := func(string) (AutonomyLevel, bool) { return AutonomyL1, true }
	second := func(string) (AutonomyLevel, bool) { return AutonomyL2, true }
	r.SetAutonomyResolver(first)
	r.SetAutonomyResolver(second)
	if got := r.EffectiveAutonomyForTool("any"); got != AutonomyL1 {
		t.Fatalf("第二次装配应被忽略，生效级别应仍来自第一个解析器，实际 %s", got)
	}
}
