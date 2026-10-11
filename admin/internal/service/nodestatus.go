package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// NodeStatusSource 是 admin 侧的 `node_status` 数据源。
//
// 为什么放在 admin：数据都在共享 MySQL 里（节点表 + 运行表），控制面直接可读，
// 不需要节点暴露监听端口。这是"admin→node 证据通道"的第一段：
//
//	节点存活 / 心跳 / 主机名 / 版本 / 该节点当前在跑多少个任务  ← 共享库可读（本实现）
//	CPU / 内存 / 磁盘 / load 水位                            ← 需要节点侧采集上报（见下）
//
// 水位字段的来源是 `node_metric` 表（节点侧周期上报，见 common/models/node_metric.go）。
// 该表/该行不存在时**保持零值并让工具显式输出"未上报"**，而不是编造 0%：
// 把"没有采集数据"渲染成 `cpu_percent=0.0` 会被模型读成"节点空闲"，
// 那就是一次凭空的证据捏造。
type NodeStatusSource struct{}

// 编译期确认接口实现。
var _ tools.NodeStatusSource = (*NodeStatusSource)(nil)

// NodeStatus 实现 tools.NodeStatusSource。
//
// nodeUUID 为空时返回明确错误而不是猜一个节点：
// admin 是控制面，没有"当前节点"语义（节点侧才有），猜测等于把证据指向错误的主机。
func (s *NodeStatusSource) NodeStatus(ctx context.Context, nodeUUID string) (tools.NodeStatus, error) {
	uuid := strings.TrimSpace(nodeUUID)
	if uuid == "" {
		return tools.NodeStatus{}, fmt.Errorf("node_uuid 不能为空：admin 侧没有\"当前节点\"，必须显式指定要查询的节点")
	}
	if err := ctx.Err(); err != nil {
		return tools.NodeStatus{}, err
	}

	node := &models.Node{UUID: uuid}
	if err := node.FindByUUID(); err != nil {
		if models.IsRecordNotFound(err) {
			return tools.NodeStatus{}, fmt.Errorf("节点 %s 不存在（可能已下线、被删除，或从未注册）", uuid)
		}
		return tools.NodeStatus{}, err
	}

	st := tools.NodeStatus{
		NodeUUID: uuid,
		Hostname: node.Hostname,
		Version:  node.Version,
		Online:   node.Status == models.NodeConnSuccess,
	}
	if node.UpTime > 0 {
		st.LastHeartbeat = time.Unix(node.UpTime, 0)
	}

	// 该节点当前在跑多少个任务：判断"失败是任务自身问题还是节点被压垮"的第一手线索。
	var running int64
	if err := dbclient.GetMysqlDB().Table(models.CronyJobRunTableName).
		Where("node_uuid = ? AND status = ?", uuid, models.RunStatusRunning).
		Count(&running).Error; err != nil {
		return tools.NodeStatus{}, fmt.Errorf("统计节点在跑任务失败: %w", err)
	}
	st.RunningJobs = int(running)

	// 资源水位：可选数据源，读不到就保持零值（工具会显式说"未上报"）。
	if m, err := latestNodeMetric(uuid); err == nil && m != nil {
		st.CPUPercent = m.CPUPercent
		st.MemPercent = m.MemPercent
		st.DiskPercent = m.DiskPercent
		st.LoadAvg = m.LoadAvg
		// 容量信息一起带出去：只给百分比，模型没法判断"90% 是 4GB 还是 512GB"。
		st.CPUCores = m.CPUCores
		st.MemTotalMB = m.MemTotalMB
		st.MemUsedMB = m.MemUsedMB
		st.DiskTotalMB = m.DiskTotalMB
		st.DiskFreeMB = m.DiskFreeMB
		st.DiskPath = m.DiskPath
		if m.CollectedAt > 0 {
			st.CollectedAt = time.Unix(m.CollectedAt, 0)
		}
	}
	return st, nil
}

// latestNodeMetric 读取某节点最新一次资源水位；没有记录时返回 (nil, nil)。
//
// 表不存在（老库未迁移）也不报错：水位是增强信息，不能因为它让整个诊断失败。
func latestNodeMetric(nodeUUID string) (*models.NodeMetric, error) {
	m := &models.NodeMetric{}
	err := dbclient.GetMysqlDB().Table(models.CronyNodeMetricTableName).
		Where("node_uuid = ?", nodeUUID).Order("id DESC").First(m).Error
	if err != nil {
		if models.IsRecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}
