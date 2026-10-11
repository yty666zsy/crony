package models

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CronyNodeMetricTableName 节点资源水位表名。
const CronyNodeMetricTableName = "node_metric"

// NodeMetric 节点资源水位（节点侧周期上报的"最新值"，**一个节点一行**）。
//
// 为什么是"最新值"而不是历史序列：
//   - `node_status` 工具的参数只有 node_uuid（没有时间点），诊断要回答的是
//     "这个节点现在能不能干活"，最新值就够；
//   - 历史水位（"故障发生时节点是否已经过载"）确实更有价值，但它需要按
//     (node_uuid, collected_at) 建时序表 + 保留策略，属于阶段 3 后续项，
//     届时再加 `node_metric_history`，本表保持"当前值"语义不变。
//
// 为什么单独一张表而不是复用 `node` 表：`node` 是"节点注册信息"（由 admin 维护），
// 本表是"节点自报的运行水位"（由 node 进程周期写入）。写入者不同、生命周期不同
// （节点重启/重注册不应抹掉水位，水位过期也不应影响节点在不在线），混在一张表里
// 会让两条语义互相污染。
type NodeMetric struct {
	ID       int    `json:"id" gorm:"column:id;primary_key;auto_increment"`
	NodeUUID string `json:"node_uuid" gorm:"size:128;column:node_uuid;not null;uniqueIndex:uk_node_metric_uuid"`
	// Hostname/Version 冗余一份：便于"水位行还在、但节点记录被删/被改名"时仍可归因。
	Hostname    string  `json:"hostname" gorm:"size:64;column:hostname;default:''"`
	Version     string  `json:"version" gorm:"size:64;column:version;default:''"`
	CPUPercent  float64 `json:"cpu_percent" gorm:"column:cpu_percent;default:0"`
	MemPercent  float64 `json:"mem_percent" gorm:"column:mem_percent;default:0"`
	DiskPercent float64 `json:"disk_percent" gorm:"column:disk_percent;default:0"`
	LoadAvg     string  `json:"load_avg" gorm:"size:64;column:load_avg;default:''"`
	RunningJobs int     `json:"running_jobs" gorm:"column:running_jobs;default:0"`
	// 容量信息：只有百分比时模型无法判断"90% 是 4GB 里的 90% 还是 512GB 里的 90%"。
	CPUCores    int   `json:"cpu_cores" gorm:"column:cpu_cores;default:0"`
	MemTotalMB  int64 `json:"mem_total_mb" gorm:"column:mem_total_mb;default:0"`
	MemUsedMB   int64 `json:"mem_used_mb" gorm:"column:mem_used_mb;default:0"`
	DiskTotalMB int64 `json:"disk_total_mb" gorm:"column:disk_total_mb;default:0"`
	DiskFreeMB  int64 `json:"disk_free_mb" gorm:"column:disk_free_mb;default:0"`
	// DiskPath 是上面磁盘水位对应的挂载点/路径（例如 "/" 或 "C:\"）。
	DiskPath string `json:"disk_path" gorm:"size:255;column:disk_path;default:''"`
	// UptimeSec 节点**进程**已运行秒数（不是 OS uptime）：节点刚重启过是重要的诊断线索。
	UptimeSec int64 `json:"uptime_sec" gorm:"column:uptime_sec;default:0"`
	// CollectedAt 是本次采集时刻，单位 **Unix 秒**（与 node.up_time 一致；
	// 注意本项目的 node_probe_request.*_at 用的是毫秒，两者不要混）。
	// 读取方靠它区分"未上报（0）"与"上报为零"，所以写库时绝不能漏写。
	CollectedAt int64 `json:"collected_at" gorm:"column:collected_at;not null;default:0;index:idx_node_metric_collected"`
}

// TableName 实现 gorm 的表名约定。
func (m *NodeMetric) TableName() string { return CronyNodeMetricTableName }

// SaveNodeMetric 按 node_uuid upsert 写入一条水位。
//
// 用 upsert 而不是先查后写：节点可能多协程/多进程并发上报，先查后写会撞唯一索引。
func SaveNodeMetric(db *gorm.DB, m *NodeMetric) error {
	return db.Table(CronyNodeMetricTableName).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_uuid"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"hostname", "version", "cpu_percent", "mem_percent", "disk_percent",
			"load_avg", "running_jobs", "cpu_cores", "mem_total_mb", "mem_used_mb",
			"disk_total_mb", "disk_free_mb", "disk_path", "uptime_sec", "collected_at",
		}),
	}).Create(m).Error
}
