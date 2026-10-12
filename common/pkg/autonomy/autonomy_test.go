package autonomy

import "testing"

func TestPromoteAfterConsecutiveSuccess(t *testing.T) {
	cfg := Config{PromoteAfter: 3, MaxLevel: LevelFull, DemoteOnMistake: 1, FreezeOnMistake: true}
	c := Credit{Level: LevelAdvisory}
	for i := 0; i < 3; i++ {
		c = Observe(c, true, cfg)
	}
	if c.Level != LevelApproval {
		t.Fatalf("连续 3 次成功应升到 L1，实际 %s", c.Level.Name())
	}
	// 再 3 次 → L2。
	for i := 0; i < 3; i++ {
		c = Observe(c, true, cfg)
	}
	if c.Level != LevelWhitelist {
		t.Fatalf("再 3 次成功应升到 L2，实际 %s", c.Level.Name())
	}
}

func TestMistakeDemotesAndFreezes(t *testing.T) {
	cfg := DefaultConfig()
	c := Credit{Level: LevelWhitelist, Streak: 4}
	c = Observe(c, false, cfg)
	if c.Level != LevelApproval {
		t.Fatalf("一次不良后果应降一级（L2→L1），实际 %s", c.Level.Name())
	}
	if !c.Frozen {
		t.Fatalf("一次不良后果必须冻结")
	}
	if c.Streak != 0 {
		t.Fatalf("不良后果必须清零连续成功")
	}
}

func TestFrozenNeverPromotes(t *testing.T) {
	cfg := DefaultConfig()
	c := Credit{Level: LevelApproval, Frozen: true}
	for i := 0; i < 100; i++ {
		c = Observe(c, true, cfg)
	}
	if c.Level != LevelApproval {
		t.Fatalf("冻结状态永远不得升级，实际 %s", c.Level.Name())
	}
}

func TestUnfreezeDoesNotChangeLevel(t *testing.T) {
	c := Credit{Level: LevelApproval, Frozen: true, FrozenReason: "x", Streak: 3}
	c = Unfreeze(c)
	if c.Frozen || c.FrozenReason != "" {
		t.Fatalf("解冻应清除冻结标记与原因")
	}
	if c.Level != LevelApproval {
		t.Fatalf("解冻不改变等级，实际 %s", c.Level.Name())
	}
}

func TestEffectiveFallsBackWhenFrozen(t *testing.T) {
	if got := Effective(Credit{Level: LevelFull, Frozen: true}); got != LevelApproval {
		t.Fatalf("冻结时应回落 L1，实际 %s", got.Name())
	}
	if got := Effective(Credit{Level: LevelWhitelist}); got != LevelWhitelist {
		t.Fatalf("未冻结时应返回自身等级，实际 %s", got.Name())
	}
}

// TestEnforcementLevelHasApprovalFloor 锁死"信用不能决定要不要写"这一条：
// 等级 0（只建议）在放行判定里等价于 L1，绝不让"刚成功过一次"的动作反被全盘拒绝。
func TestEnforcementLevelHasApprovalFloor(t *testing.T) {
	cases := []struct {
		in   Credit
		want Level
	}{
		{Credit{Level: LevelAdvisory}, LevelApproval},                    // L0 → 底线 L1
		{Credit{Level: LevelApproval}, LevelApproval},                    // L1 保持
		{Credit{Level: LevelWhitelist}, LevelWhitelist},                  // L2 保持
		{Credit{Level: LevelFull}, LevelFull},                            // L3 保持（是否真放行由配置上限再夹）
		{Credit{Level: LevelAdvisory, Frozen: true}, LevelApproval},      // 冻结 → L1
		{Credit{Level: LevelFull, Frozen: true}, LevelApproval},          // 冻结 → L1（不是 L3）
	}
	for _, c := range cases {
		if got := EnforcementLevel(c.in); got != c.want {
			t.Fatalf("EnforcementLevel(%s, frozen=%v) = %s，期望 %s",
				c.in.Level.Name(), c.in.Frozen, got.Name(), c.want.Name())
		}
	}
}

func TestMaxLevelCapsPromotion(t *testing.T) {
	cfg := Config{PromoteAfter: 1, MaxLevel: LevelWhitelist, DemoteOnMistake: 1, FreezeOnMistake: true}
	c := Credit{Level: LevelAdvisory}
	for i := 0; i < 10; i++ {
		c = Observe(c, true, cfg)
	}
	if c.Level != LevelWhitelist {
		t.Fatalf("自动升级不得越过 MaxLevel（L2），实际 %s", c.Level.Name())
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]Level{"L0": LevelAdvisory, "l2": LevelWhitelist, "3": LevelFull} {
		got, ok := ParseLevel(in)
		if !ok || got != want {
			t.Fatalf("ParseLevel(%q)=%v,%v 期望 %v", in, got, ok, want)
		}
	}
	if _, ok := ParseLevel("L9"); ok {
		t.Fatalf("非法等级应返回 ok=false")
	}
}
