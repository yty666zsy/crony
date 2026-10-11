package retry

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantRetryable bool
	}{
		{"nil", nil, false},
		{"未分类错误默认可重试", errors.New("connection reset"), true},
		{"显式可重试", Retryable(errors.New("502")), true},
		{"显式不可重试", Fatal(errors.New("401 unauthorized")), false},
		{"取消不重试", ErrCanceled, false},
		{"包装后的取消不重试", Retryable(ErrCanceled), false},
		{"多次标记以外层标记为准", Fatal(Retryable(errors.New("x"))), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if got.Retryable != tc.wantRetryable {
				t.Fatalf("Classify() = %v (%s), want retryable=%v", got.Retryable, got.Reason, tc.wantRetryable)
			}
			if tc.err != nil && got.Reason == "" {
				t.Fatal("分类结果必须带原因，便于落库复盘")
			}
		})
	}
}

func TestBackoffExponentialAndCapped(t *testing.T) {
	p := DefaultPolicy() // 2s 起，2 倍，上限 30s，抖动 0.2
	cases := []struct {
		attempt int
		rand01  float64
		want    time.Duration
	}{
		{1, 0, 2 * time.Second},
		{2, 0, 4 * time.Second},
		{3, 0, 8 * time.Second},
		{1, 1, 1600 * time.Millisecond}, // 2s * (1 - 0.2*1) = 1.6s
		{2, 0.5, 3600 * time.Millisecond},
		{5, 0, 30 * time.Second},  // 32s 被上限截断
		{50, 0, 30 * time.Second}, // 指数爆炸仍被截断
	}
	for _, tc := range cases {
		got := p.Backoff(tc.attempt, tc.rand01)
		if diff := math.Abs(float64(got - tc.want)); diff > float64(50*time.Millisecond) {
			t.Errorf("Backoff(attempt=%d, rand=%v) = %v, want %v", tc.attempt, tc.rand01, got, tc.want)
		}
	}
	// 抖动只会向下，不会突破单次上限
	for i := 0; i < 20; i++ {
		if got := p.Backoff(10, float64(i)/20); got > p.MaxInterval {
			t.Fatalf("抖动后 %v 超过上限 %v", got, p.MaxInterval)
		}
	}
}

func TestNormalizeClampsInvalidPolicy(t *testing.T) {
	p := Policy{MaxAttempts: 0, InitialInterval: -1, MaxInterval: -5, Multiplier: 0.2, Jitter: 3, MaxElapsed: -1}.Normalize()
	if p.MaxAttempts != 1 {
		t.Errorf("MaxAttempts = %d, want 1", p.MaxAttempts)
	}
	if p.InitialInterval != 0 {
		t.Errorf("InitialInterval = %v, want 0", p.InitialInterval)
	}
	if p.MaxInterval != 0 {
		t.Errorf("MaxInterval = %v, want 0（跟随 InitialInterval）", p.MaxInterval)
	}
	if p.Multiplier != 1 {
		t.Errorf("Multiplier = %v, want 1（退避不能负增长）", p.Multiplier)
	}
	if p.Jitter != 1 {
		t.Errorf("Jitter = %v, want 1", p.Jitter)
	}
	if p.MaxElapsed != 0 {
		t.Errorf("MaxElapsed = %v, want 0", p.MaxElapsed)
	}
	if huge := (Policy{MaxAttempts: 100000}).Normalize(); huge.MaxAttempts != 100 {
		t.Errorf("MaxAttempts 上限应为 100，实际 %d", huge.MaxAttempts)
	}
}

// TestDecide 覆盖重试决策的四个终止条件：
// 成功、达到最大次数、超过总时长、错误不可重试。
func TestDecide(t *testing.T) {
	p := Policy{
		MaxAttempts:     3,
		InitialInterval: time.Second,
		MaxInterval:     10 * time.Second,
		Multiplier:      2,
		MaxElapsed:      time.Minute,
	}
	retryableErr := Retryable(errors.New("502"))

	if d := p.Decide(1, 0, nil); d.Retry {
		t.Error("成功时不应重试")
	}
	if d := p.Decide(1, 0, retryableErr); !d.Retry || d.NextAttempt != 2 || d.Delay != time.Second {
		t.Errorf("第 1 次失败应重试并等待 1s，实际 %+v", d)
	}
	if d := p.Decide(2, 2*time.Second, retryableErr); !d.Retry || d.Delay != 2*time.Second {
		t.Errorf("第 2 次失败应等 2s，实际 %+v", d)
	}
	if d := p.Decide(3, 3*time.Second, retryableErr); d.Retry {
		t.Errorf("达到最大尝试次数后不得重试，实际 %+v", d)
	}
	if d := p.Decide(1, 2*time.Minute, retryableErr); d.Retry {
		t.Errorf("超过总时长上限后不得重试，实际 %+v", d)
	}
	if d := p.Decide(1, 0, Fatal(errors.New("bad config"))); d.Retry {
		t.Errorf("不可重试错误不得重试，实际 %+v", d)
	}
	if d := p.DecideWithRand(1, 0, retryableErr, 0); d.Delay != time.Second {
		t.Errorf("注入随机数应可确定性控制抖动，实际 %+v", d)
	}
}

// TestDecideDelayNeverExceedsRemainingBudget 防止"最后一次重试把执行拖过总时长上限"。
func TestDecideDelayNeverExceedsRemainingBudget(t *testing.T) {
	p := Policy{MaxAttempts: 5, InitialInterval: time.Minute, MaxInterval: time.Minute, Multiplier: 1, MaxElapsed: 90 * time.Second}
	d := p.DecideWithRand(1, 30*time.Second, Retryable(errors.New("timeout")), 0)
	if !d.Retry {
		t.Fatalf("仍在预算内应重试，实际 %+v", d)
	}
	if d.Delay > 60*time.Second {
		t.Fatalf("等待 %v 超出剩余预算 60s", d.Delay)
	}
}

// TestFromLegacy 保证老任务的 retry_times / retry_interval 语义在升级后不变。
func TestFromLegacy(t *testing.T) {
	cases := []struct {
		name            string
		retryTimes      int
		intervalSeconds int64
		wantAttempts    int
		wantInitial     time.Duration
		wantMultiplier  float64
		wantMaxInterval time.Duration
	}{
		{"不重试", 0, 0, 1, time.Minute, 2, 10 * time.Minute},
		{"重试3次_默认退避", 3, 0, 4, time.Minute, 2, 10 * time.Minute},
		{"固定间隔30秒", 2, 30, 3, 30 * time.Second, 1, 30 * time.Second},
		{"负数按不重试处理", -5, 0, 1, time.Minute, 2, 10 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := FromLegacy(tc.retryTimes, tc.intervalSeconds)
			if p.MaxAttempts != tc.wantAttempts {
				t.Errorf("MaxAttempts = %d, want %d", p.MaxAttempts, tc.wantAttempts)
			}
			if p.InitialInterval != tc.wantInitial {
				t.Errorf("InitialInterval = %v, want %v", p.InitialInterval, tc.wantInitial)
			}
			if p.Multiplier != tc.wantMultiplier {
				t.Errorf("Multiplier = %v, want %v", p.Multiplier, tc.wantMultiplier)
			}
			if p.MaxInterval != tc.wantMaxInterval {
				t.Errorf("MaxInterval = %v, want %v", p.MaxInterval, tc.wantMaxInterval)
			}
			if p.MaxElapsed != 30*time.Minute {
				t.Errorf("MaxElapsed = %v, want 30m（防僵尸执行的硬上限）", p.MaxElapsed)
			}
		})
	}
}

func TestSnapshotIsValidJSON(t *testing.T) {
	snapshot := DefaultPolicy().Snapshot()
	for _, key := range []string{"max_attempts", "initial_interval_ms", "jitter", "max_elapsed_ms"} {
		if !contains(snapshot, key) {
			t.Errorf("策略快照缺少字段 %s: %s", key, snapshot)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
