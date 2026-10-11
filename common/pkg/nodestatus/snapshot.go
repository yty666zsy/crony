// Package nodestatus 采集本机资源水位并周期上报到 `node_metric`。
//
// 分工（阶段 3 收尾）：
//   - 本文件的 Snapshot/Collect 只负责"这一台机器现在什么水位"；
//   - collect_*.go 是平台实现（Windows / Linux）；
//   - publisher.go 负责"周期采集 + upsert 落库 + 失败不伪造"。
//
// 为什么要有这一层：admin 侧的 `node_status` 工具早就等着 `node_metric` 里有行
// （见 admin/internal/service/nodestatus.go），但**没有任何节点侧代码往里写**，
// 所以工具永远输出"未上报"。没有上报方，再好的读取方也是空的。
package nodestatus

import (
	"math"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
)

// Snapshot 一次资源采集的结果。
//
// 字段与 models.NodeMetric 一一对应，但**不依赖数据库**：采集逻辑要能在
// 没有 MySQL 的机器上被测（平台算术是最容易写错的部分）。
type Snapshot struct {
	CPUPercent  float64
	MemPercent  float64
	DiskPercent float64
	// LoadAvg 是 1/5/15 分钟负载的原始文本；Windows 没有这个概念，
	// 平台实现应留空而不是编造 0.00（"没有这个指标"≠"负载为零"）。
	LoadAvg     string
	CPUCores    int
	MemTotalMB  int64
	MemUsedMB   int64
	DiskTotalMB int64
	DiskFreeMB  int64
	// DiskPath 是磁盘水位对应的挂载点/路径（如 "/" 或 "C:\"）。
	DiskPath string
}

// Collect 采集本机资源水位。
//
// 返回 error 时调用方**必须跳过本次上报**：宁可 `node_metric` 里是旧值或没有值，
// 也不能写一行 CPUPercent=0 的假数据——admin 侧是靠 CollectedAt 区分
// "未上报"和"上报了且为零"的，写 0 会让模型得出"节点资源空闲"。
func Collect() (Snapshot, error) { return collectPlatform() }

// ToMetric 把快照转成落库行。
//
// uptimeSec 是**节点进程**已运行秒数（不是 OS uptime）：节点刚重启过是重要的
// 诊断线索（例如"任务失败是因为节点进程刚起来还没加载完任务"）。
func (s Snapshot) ToMetric(nodeUUID, hostname, version string, uptimeSec int64, runningJobs int, collectedAt time.Time) *models.NodeMetric {
	if collectedAt.IsZero() {
		collectedAt = time.Now()
	}
	return &models.NodeMetric{
		NodeUUID:    strings.TrimSpace(nodeUUID),
		Hostname:    clip(strings.TrimSpace(hostname), 64),
		Version:     clip(strings.TrimSpace(version), 64),
		CPUPercent:  round2(clampPercent(s.CPUPercent)),
		MemPercent:  round2(clampPercent(s.MemPercent)),
		DiskPercent: round2(clampPercent(s.DiskPercent)),
		LoadAvg:     clip(strings.TrimSpace(s.LoadAvg), 64),
		RunningJobs: runningJobs,
		CPUCores:    s.CPUCores,
		MemTotalMB:  s.MemTotalMB,
		MemUsedMB:   s.MemUsedMB,
		DiskTotalMB: s.DiskTotalMB,
		DiskFreeMB:  s.DiskFreeMB,
		DiskPath:    clip(strings.TrimSpace(s.DiskPath), 255),
		UptimeSec:   uptimeSec,
		// 单位是 **Unix 秒**：读取方 admin/internal/service/nodestatus.go 用
		// time.Unix(collected_at, 0) 还原，集成用例也按秒断言。
		// 写毫秒而不改读取方会得到一个约公元 58712 年的时间戳——
		// 这类"两边各自以为对方是同一单位"的错误，靠端到端用例才抓得住。
		CollectedAt: collectedAt.Unix(),
	}
}

// percentFromDelta 用两次采样的累计计数器差值算占用率。
//
// 平台无关且被两个平台共用，因为它才是真正容易写错的地方：
//   - Linux 的 /proc/stat：cpu 行给出 user/nice/system/idle/iowait/...，
//     空闲要算 idle+iowait（iowait 期间 CPU 并没有干活）；
//   - Windows 的 GetSystemTimes：idle/kernel/user 三项，其中 **kernel 已包含 idle**，
//     所以"总时间"= kernel + user，空闲 = idle。
//
// 两种口径都归约成"总增量 + 空闲增量"，算术只写一遍、只测一遍。
func percentFromDelta(totalDelta, idleDelta uint64) float64 {
	if totalDelta == 0 {
		return 0
	}
	if idleDelta > totalDelta {
		idleDelta = totalDelta // 计数器回绕/时钟异常：当作全忙，而不是算出负数
	}
	used := float64(totalDelta-idleDelta) / float64(totalDelta)
	return clampPercent(used * 100)
}

// mbFromBytes 字节 → MB（水位是给人看的，取整即可）。
func mbFromBytes(b uint64) int64 { return int64(b / (1024 * 1024)) }

// percentFrom 用 "已用/总量" 算百分比，总量为 0 时返回 0。
//
// 注意"总量为 0"与"用量为 0"是两件事：前者是**采集不到**，后者是真的空。
// 平台实现遇到总量为 0 应把它当成采集失败（返回 error）而不是调用本函数，
// 否则又会写出一行假的 0%。
func percentFrom(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return clampPercent(float64(used) / float64(total) * 100)
}

func clampPercent(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
