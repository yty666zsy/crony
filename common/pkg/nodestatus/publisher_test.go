package nodestatus

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 发布器的**不需要数据库**的规则测试。
// 需要真库的 upsert / 端到端断言在 admin/internal/service 的 integration 用例里
// （那里能同时看到写入方与读取方，才能发现 collected_at 口径不一致）。

func TestPublisherUnitRejectsInvalidConfig(t *testing.T) {
	// 没有数据库：不能静默成功（静默成功 = 水位永远不落库，却没人知道）
	if _, err := NewPublisher(nil, "n1", "h1", "v1").RunOnce(context.Background()); err == nil {
		t.Fatal("db 为 nil 时必须报错")
	}
	// 没有 node_uuid：水位无法归属，宁可报错也不要写一行无主数据
	p := NewPublisher(nil, "   ", "h1", "v1")
	if _, err := p.RunOnce(context.Background()); err == nil {
		t.Fatal("node_uuid 为空时必须报错")
	}
}

func TestPublisherUnitIntervalClamping(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, DefaultInterval},
		{time.Second, DefaultInterval},       // 太密：白写数据库
		{24 * time.Hour, DefaultInterval},    // 太疏：诊断只会看到旧水位
		{10 * time.Second, 10 * time.Second}, // 合法
		{time.Minute, time.Minute},           // 合法
	}
	for _, c := range cases {
		if got := NewPublisher(nil, "n", "h", "v").SetInterval(c.in).interval; got != c.want {
			t.Fatalf("SetInterval(%s) = %s，期望 %s", c.in, got, c.want)
		}
	}
}

// 配置错误要在采集之前报出来：db 没初始化时先跑一遍平台采集毫无意义。
// "采集失败不落库"这条规则由真库 integration 用例断言（见
// admin/internal/service/nodestatus_metric_e2e_mysql_test.go）。
func TestPublisherUnitValidatesConfigBeforeCollecting(t *testing.T) {
	called := false
	p := NewPublisher(nil, "n1", "h1", "v1").SetCollector(func() (Snapshot, error) {
		called = true
		return Snapshot{}, fmt.Errorf("平台未实现")
	})
	if _, err := p.RunOnce(context.Background()); err == nil {
		t.Fatal("db 为 nil 时必须报错")
	}
	if called {
		t.Fatal("db 未初始化时不应先做平台采集")
	}
}

func TestPublisherUnitUptimeFromStartedAt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	snap := Snapshot{CPUPercent: 10, MemPercent: 20, DiskPercent: 30, CPUCores: 4}
	m := snap.ToMetric("n1", "h1", "v1", 3600, 2, now)
	if m.UptimeSec != 3600 {
		t.Fatalf("uptime_sec 应原样带出（进程运行时长），实际 %d", m.UptimeSec)
	}
	if m.CollectedAt != now.Unix() {
		t.Fatalf("collected_at 单位必须是 Unix 秒（读取方按秒还原），实际 %d", m.CollectedAt)
	}
	if m.RunningJobs != 2 {
		t.Fatalf("running_jobs 应为 2，实际 %d", m.RunningJobs)
	}
	if m.NodeUUID != "n1" || m.Hostname != "h1" || m.Version != "v1" {
		t.Fatalf("归属字段不符: %+v", m)
	}
}

// SetClock 让"周期上报"这件事在没有真睡眠的情况下可测。
func TestPublisherUnitClockIsInjectable(t *testing.T) {
	fixed := time.Unix(1_600_000_000, 0)
	p := NewPublisher(nil, "n1", "h1", "v1").SetClock(func() time.Time { return fixed })
	if got := p.now(); !got.Equal(fixed) {
		t.Fatalf("SetClock 未生效: %v", got)
	}
}
