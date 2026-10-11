package nodestatus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"gorm.io/gorm"
)

// Collector 采集函数类型。
//
// 之所以可注入：验收"采集失败**不落库**"这条规则需要在没有坏机器的前提下可测。
type Collector func() (Snapshot, error)

// DefaultInterval 默认上报周期。
//
// 30 秒的理由：`node_metric` 是"当前值"语义（一个节点一行），诊断关心的是
// "这会儿节点能不能干活"；再密一点只是白写数据库，再疏一点就可能在诊断期间
// 只看到十几分钟前的旧水位。
const DefaultInterval = 30 * time.Second

const (
	minInterval = 5 * time.Second
	maxInterval = time.Hour
)

// Publisher 周期把本机资源水位 upsert 到 `node_metric`。
type Publisher struct {
	db          *gorm.DB
	nodeUUID    string
	hostname    string
	version     string
	interval    time.Duration
	collect     Collector
	startedAt   time.Time
	runningJobs func() int
	logf        func(format string, args ...any)
	now         func() time.Time
}

// NewPublisher 创建发布器（默认：真实采集 + 30s 周期 + 静默日志）。
func NewPublisher(db *gorm.DB, nodeUUID, hostname, version string) *Publisher {
	return &Publisher{
		db:       db,
		nodeUUID: strings.TrimSpace(nodeUUID),
		hostname: hostname,
		version:  version,
		interval: DefaultInterval,
		collect:  Collect,
		now:      time.Now,
	}
}

// SetInterval 设置上报周期（小于 5s 或大于 1h 会被夹回合法区间）。
func (p *Publisher) SetInterval(d time.Duration) *Publisher {
	if d < minInterval || d > maxInterval {
		d = DefaultInterval
	}
	p.interval = d
	return p
}

// SetCollector 注入采集函数（测试用）。
func (p *Publisher) SetCollector(c Collector) *Publisher {
	if c != nil {
		p.collect = c
	}
	return p
}

// SetStartedAt 设置节点**进程**启动时间，用于计算 uptime_sec。
func (p *Publisher) SetStartedAt(t time.Time) *Publisher { p.startedAt = t; return p }

// SetRunningJobs 注入"本节点当前在跑任务数"的统计函数（可选）。
func (p *Publisher) SetRunningJobs(fn func() int) *Publisher { p.runningJobs = fn; return p }

// SetLogger 注入告警输出（默认丢弃：common 包不依赖具体日志实现）。
func (p *Publisher) SetLogger(fn func(format string, args ...any)) *Publisher { p.logf = fn; return p }

// SetClock 注入时钟（测试用，便于断言 collected_at）。
func (p *Publisher) SetClock(fn func() time.Time) *Publisher {
	if fn != nil {
		p.now = fn
	}
	return p
}

// RunOnce 采集一次并落库；返回是否真的写入了一行。
//
// 采集失败时返回 (false, err) **且不写库**：宁可库里是旧值/没有值，
// 也不能写一行 0%，因为 admin 侧是用 collected_at 区分"未上报"和"上报为零"的。
func (p *Publisher) RunOnce(ctx context.Context) (bool, error) {
	if p.db == nil {
		return false, fmt.Errorf("nodestatus: 数据库未初始化，无法上报水位")
	}
	if p.nodeUUID == "" {
		return false, fmt.Errorf("nodestatus: node_uuid 为空，水位无法归属到节点")
	}
	snap, err := p.collect()
	if err != nil {
		p.warn("资源水位采集失败，本次跳过上报（不写假数据）: %s", err.Error())
		return false, err
	}
	jobs := 0
	if p.runningJobs != nil {
		jobs = p.runningJobs()
	}
	now := p.now()
	var uptime int64
	if !p.startedAt.IsZero() {
		if sec := int64(now.Sub(p.startedAt).Seconds()); sec > 0 {
			uptime = sec
		}
	}
	m := snap.ToMetric(p.nodeUUID, p.hostname, p.version, uptime, jobs, now)
	if err := models.SaveNodeMetric(p.db.WithContext(ctx), m); err != nil {
		return false, fmt.Errorf("写入资源水位失败: %w", err)
	}
	return true, nil
}

// Run 阻塞循环：**先立刻上报一次**（刚启动的节点也应该尽快有水位，
// 否则诊断在启动后头 30 秒里看到的还是"未上报"），随后按周期上报。
func (p *Publisher) Run(ctx context.Context) {
	interval := p.interval
	if interval < minInterval || interval > maxInterval {
		interval = DefaultInterval
	}
	p.publish(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.publish(ctx)
		}
	}
}

func (p *Publisher) publish(ctx context.Context) {
	if _, err := p.RunOnce(ctx); err != nil {
		// 采集/落库失败只告警：水位是增强信息，不能让它把节点进程带崩，
		// 也不能让它刷屏（周期 30s，失败会自然重试）。
		p.warn("[nodestatus] 水位上报未成功（将在下个周期重试）: %s", err.Error())
	}
}

func (p *Publisher) warn(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}
