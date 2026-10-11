//go:build integration

package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
)

// 真实 MySQL 下的 node_status 数据源测试（阶段 3 第一段：admin→node 证据通道）。
//
// 为什么必须用真库：这里全部是 SQL 口径问题——node 表的状态常量、
// job_run 的 `status = 1`（running）统计、node_metric 的 upsert 与"没有水位行"的判定。
// 假源测不到这些。
//
// 运行：设置 CRONY_TEST_MYSQL_DSN 并加 -tags integration；未设置则跳过。
func TestIntegrationNodeStatusSourceAgainstRealMySQL(t *testing.T) {
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 的 node_status 测试")
	}
	if _, err := dbclient.Init(dsn, "silent", 1, 5); err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db := dbclient.GetMysqlDB()
	if err := db.AutoMigrate(&models.Node{}, &models.JobRun{}, &models.NodeMetric{}); err != nil {
		t.Fatalf("建表失败（测试库需要 CREATE 权限）: %v", err)
	}

	tag := fmt.Sprintf("it-nodestatus-%d", time.Now().UnixNano())
	uuid := tag
	otherUUID := tag + "-other"
	now := time.Now().Unix()
	defer func() {
		db.Exec("DELETE FROM "+models.CronyJobRunTableName+" WHERE run_id LIKE ?", tag+"%")
		db.Exec("DELETE FROM "+models.CronyNodeMetricTableName+" WHERE node_uuid LIKE ?", tag+"%")
		db.Exec("DELETE FROM "+models.CronyNodeTableName+" WHERE uuid LIKE ?", tag+"%")
	}()

	// 节点记录：在线（NodeConnSuccess=1），心跳=20 秒前
	if err := db.Table(models.CronyNodeTableName).Create(&models.Node{
		PID: "1", IP: "10.0.0.1", Hostname: "it-host", UUID: uuid,
		Version: "v9.9.9", Status: models.NodeConnSuccess, UpTime: now - 20,
	}).Error; err != nil {
		t.Fatalf("写节点失败: %v", err)
	}
	// 该节点 2 个 running + 1 个 success；另一个节点 1 个 running（不得被算进来）
	runs := []*models.JobRun{
		{RunID: tag + "-r1", JobID: 900201, NodeUUID: uuid, Status: int(models.RunStatusRunning)},
		{RunID: tag + "-r2", JobID: 900202, NodeUUID: uuid, Status: int(models.RunStatusRunning)},
		{RunID: tag + "-r3", JobID: 900203, NodeUUID: uuid, Status: int(models.RunStatusSuccess)},
		{RunID: tag + "-r4", JobID: 900204, NodeUUID: otherUUID, Status: int(models.RunStatusRunning)},
	}
	for _, r := range runs {
		if err := db.Table(models.CronyJobRunTableName).Create(r).Error; err != nil {
			t.Fatalf("写运行记录失败: %v", err)
		}
	}

	src := &NodeStatusSource{}

	// 1) 没有水位行：水位必须保持"未上报"（CollectedAt 零值），但存活/在跑数是真实的
	st, err := src.NodeStatus(context.Background(), uuid)
	if err != nil {
		t.Fatalf("查询节点状态失败: %v", err)
	}
	if !st.Online || st.Hostname != "it-host" || st.Version != "v9.9.9" {
		t.Fatalf("节点基本信息不符: %+v", st)
	}
	if st.LastHeartbeat.Unix() != now-20 {
		t.Fatalf("心跳时间应从 node.up 还原，实际 %v", st.LastHeartbeat)
	}
	if st.RunningJobs != 2 {
		t.Fatalf("在跑任务数应为 2（只算该节点且 status=running），实际 %d", st.RunningJobs)
	}
	if !st.CollectedAt.IsZero() {
		t.Fatalf("没有水位行时 CollectedAt 必须是零值（工具据此输出\"未上报\"），实际 %v", st.CollectedAt)
	}

	// 2) 写入水位（upsert）：应被读到，且"更新"不产生第二行
	m := &models.NodeMetric{
		NodeUUID: uuid, Hostname: "it-host", Version: "v9.9.9",
		CPUPercent: 12.5, MemPercent: 63.25, DiskPercent: 95.5, LoadAvg: "1.20 0.90 0.70",
		RunningJobs: 2, CPUCores: 8, MemTotalMB: 16384, MemUsedMB: 10362,
		DiskTotalMB: 51200, DiskFreeMB: 2304, DiskPath: "/", UptimeSec: 3600,
		CollectedAt: now,
	}
	if err := models.SaveNodeMetric(db, m); err != nil {
		t.Fatalf("写水位失败: %v", err)
	}
	m2 := *m
	m2.CPUPercent = 40.5
	m2.CollectedAt = now + 5
	if err := models.SaveNodeMetric(db, &m2); err != nil {
		t.Fatalf("二次写水位（upsert）失败: %v", err)
	}
	var metricRows int64
	db.Table(models.CronyNodeMetricTableName).Where("node_uuid = ?", uuid).Count(&metricRows)
	if metricRows != 1 {
		t.Fatalf("upsert 后应只有 1 行水位，实际 %d 行（唯一索引或 OnConflict 配置有问题）", metricRows)
	}

	st, err = src.NodeStatus(context.Background(), uuid)
	if err != nil {
		t.Fatalf("查询节点状态失败: %v", err)
	}
	if st.CPUPercent != 40.5 || st.MemPercent != 63.25 || st.DiskPercent != 95.5 {
		t.Fatalf("水位应按最新一次上报取值: %+v", st)
	}
	if st.LoadAvg != "1.20 0.90 0.70" {
		t.Fatalf("load 不符: %q", st.LoadAvg)
	}
	if st.CollectedAt.Unix() != now+5 {
		t.Fatalf("采集时间不符: %v", st.CollectedAt)
	}

	// 3) 未知节点：必须报"不存在"，不能返回一个空快照（那会让模型以为节点在线但没数据）
	if _, err := src.NodeStatus(context.Background(), tag+"-ghost"); err == nil ||
		!strings.Contains(err.Error(), "不存在") {
		t.Fatalf("未知节点应报不存在，实际 %v", err)
	}

	// 4) 空 node_uuid：admin 侧没有"当前节点"语义，必须显式报错而不是猜
	if _, err := src.NodeStatus(context.Background(), "  "); err == nil ||
		!strings.Contains(err.Error(), "node_uuid") {
		t.Fatalf("空 node_uuid 应报错，实际 %v", err)
	}

	// 5) 离线节点：Status != NodeConnSuccess 时 Online 必须为 false
	if err := db.Table(models.CronyNodeTableName).Where("uuid = ?", uuid).
		Update("status", models.NodeConnFail).Error; err != nil {
		t.Fatalf("改为离线失败: %v", err)
	}
	st, err = src.NodeStatus(context.Background(), uuid)
	if err != nil {
		t.Fatalf("查询离线节点失败: %v", err)
	}
	if st.Online {
		t.Fatalf("status=%d 应判为离线: %+v", models.NodeConnFail, st)
	}
}
