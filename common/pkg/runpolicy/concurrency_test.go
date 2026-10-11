package runpolicy

import (
	"testing"
	"time"
)

// TestDecide 覆盖 5 种并发策略 × (空闲 / 有在跑 / 缓冲已占用) 的决策矩阵。
// 默认策略 Skip 的语义必须与 robfig/cron 的 SkipIfStillRunning 一致：
// 上一次没跑完就跳过本次，并且**记录**一条 skipped 执行（便于回答"为什么没跑"）。
func TestDecide(t *testing.T) {
	cases := []struct {
		name     string
		policy   ConcurrencyPolicy
		active   int
		buffered int
		maxBuf   int
		want     Action
	}{
		{"skip_空闲可执行", ConcurrencySkip, 0, 0, 0, ActionRun},
		{"skip_有在跑则跳过", ConcurrencySkip, 1, 0, 0, ActionSkip},
		{"skip_多实例在跑仍跳过", ConcurrencySkip, 3, 2, 0, ActionSkip},
		{"buffer_one_空闲可执行", ConcurrencyBufferOne, 0, 0, 0, ActionRun},
		{"buffer_one_首次缓冲", ConcurrencyBufferOne, 1, 0, 0, ActionBuffer},
		{"buffer_one_缓冲满则丢弃", ConcurrencyBufferOne, 1, 1, 0, ActionSkip},
		{"buffer_all_可缓冲", ConcurrencyBufferAll, 1, 5, 16, ActionBuffer},
		{"buffer_all_达上限丢弃", ConcurrencyBufferAll, 1, 16, 16, ActionSkip},
		{"buffer_all_未达上限", ConcurrencyBufferAll, 2, 15, 16, ActionBuffer},
		{"cancel_other_取消旧执行", ConcurrencyCancelOther, 1, 0, 0, ActionCancelOthers},
		{"terminate_other_强杀旧执行", ConcurrencyTerminateOther, 2, 0, 0, ActionTerminateOthers},
		{"负数容错", ConcurrencySkip, -1, -1, 0, ActionRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.policy, tc.active, tc.buffered, tc.maxBuf)
			if got.Action != tc.want {
				t.Fatalf("Decide(%s, active=%d, buffered=%d, max=%d) = %s (%s), want %s",
					tc.policy, tc.active, tc.buffered, tc.maxBuf, got.Action, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Fatal("任何决策都必须带原因，便于审计")
			}
		})
	}
}

func TestConcurrencyPolicyStringAndParse(t *testing.T) {
	cases := []struct {
		policy ConcurrencyPolicy
		name   string
	}{
		{ConcurrencySkip, "skip"},
		{ConcurrencyBufferOne, "buffer_one"},
		{ConcurrencyBufferAll, "buffer_all"},
		{ConcurrencyCancelOther, "cancel_other"},
		{ConcurrencyTerminateOther, "terminate_other"},
	}
	for _, tc := range cases {
		if got := tc.policy.String(); got != tc.name {
			t.Errorf("%d.String() = %q, want %q", int(tc.policy), got, tc.name)
		}
		parsed, ok := ParseConcurrencyPolicy(tc.name)
		if !ok || parsed != tc.policy {
			t.Errorf("ParseConcurrencyPolicy(%q) = %v, ok=%v", tc.name, parsed, ok)
		}
	}
	if parsed, ok := ParseConcurrencyPolicy("BUFFER_ALL"); !ok || parsed != ConcurrencyBufferAll {
		t.Errorf("解析应大小写不敏感，实际 %v ok=%v", parsed, ok)
	}
	if _, ok := ParseConcurrencyPolicy("drop_everything"); ok {
		t.Error("非法策略必须返回 ok=false，由调用方回退默认值，而不是静默放行")
	}
	if got := ConcurrencyPolicy(42).String(); got != "unknown(42)" {
		t.Errorf("未知策略应打印 unknown(42)，实际 %q", got)
	}
}

func TestSlotAlignment(t *testing.T) {
	base := time.Date(2026, 10, 11, 16, 0, 0, 0, time.UTC)
	// 每分钟触发：同一分钟内的两次触发必须落在同一个槽位（幂等）
	first := Slot(base.Add(3*time.Second), time.Minute)
	second := Slot(base.Add(47*time.Second), time.Minute)
	if first != second {
		t.Fatalf("同一分钟内的触发应共享槽位：%d vs %d", first, second)
	}
	next := Slot(base.Add(61*time.Second), time.Minute)
	if next == first {
		t.Fatal("跨分钟必须产生新的槽位")
	}
	if got := Slot(base, 0); got != base.Unix() {
		t.Errorf("interval<=0 时应返回原始时间戳，实际 %d", got)
	}
}

func TestSameSlotTolerance(t *testing.T) {
	slot := time.Now().Unix()
	if !SameSlot(slot, slot, 0) {
		t.Fatal("相同槽位必须相等")
	}
	if !SameSlot(slot, slot+1, time.Second) {
		t.Fatal("1 秒内漂移应视为同一槽位（节点时钟抖动/多节点竞争）")
	}
	if SameSlot(slot, slot+5, time.Second) {
		t.Fatal("超过容差窗口的槽位不得判为同一槽位")
	}
}
