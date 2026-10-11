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
	"github.com/tmnhs/crony/common/pkg/nodestatus"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 水位链路端到端：**真采集 → upsert 落库 → 读取方 → 工具输出**。
//
// 为什么必须串起来测：单测分别覆盖了算术（percentFromDelta）、映射（ToMetric）
// 与 upsert，但"写入方与读取方对 collected_at 单位是不是同一个理解"只有把
// 两段接起来才看得见——写毫秒 + 读秒会得到一个约公元 58712 年的时间戳，
// 两边各自的测试都是绿的（阶段 3 收尾时正是这么发现的）。
//
// 运行：设置 CRONY_TEST_MYSQL_DSN 并加 -tags integration；未设置则跳过。
func TestIntegrationNodeMetricPublisherEndToEnd(t *testing.T) {
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过水位链路端到端测试")
	}
	if _, err := dbclient.Init(dsn, "silent", 1, 5); err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db := dbclient.GetMysqlDB()
	if err := db.AutoMigrate(&models.Node{}, &models.NodeMetric{}); err != nil {
		t.Fatalf("建表失败（测试库需要 CREATE 权限）: %v", err)
	}

	tag := fmt.Sprintf("it-nodemetric-%d", time.Now().UnixNano())
	now := time.Now().Unix()
	defer func() {
		db.Exec("DELETE FROM "+models.CronyNodeMetricTableName+" WHERE node_uuid LIKE ?", tag+"%")
		db.Exec("DELETE FROM "+models.CronyNodeTableName+" WHERE uuid LIKE ?", tag+"%")
	}()
	if err := db.Table(models.CronyNodeTableName).Create(&models.Node{
		PID: "1", IP: "10.0.0.9", Hostname: "it-metric-host", UUID: tag,
		Version: "v9.9.9", Status: models.NodeConnSuccess, UpTime: now - 120,
	}).Error; err != nil {
		t.Fatalf("写节点失败: %v", err)
	}

	// 用**真实采集器**（本机 Windows/Linux 资源），不是假数据：
	// 这条用例要证明的是"这台机器上的采集能落成一行可用的水位"。
	pub := nodestatus.NewPublisher(db, tag, "it-metric-host", "v9.9.9").
		SetStartedAt(time.Unix(now-120, 0))
	written, err := pub.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("水位上报失败: %v", err)
	}
	if !written {
		t.Fatal("RunOnce 未写入水位行")
	}

	// 1) 落库内容：容量必须有真值（采集不到就应该是 error 而不是 0）
	var row models.NodeMetric
	if err := db.Table(models.CronyNodeMetricTableName).Where("node_uuid = ?", tag).First(&row).Error; err != nil {
		t.Fatalf("水位行没写进去: %v", err)
	}
	if row.CPUCores <= 0 || row.MemTotalMB <= 0 || row.DiskTotalMB <= 0 {
		t.Fatalf("容量字段为 0，说明采集没成功却仍然落库了（不得伪造 0）: %+v", row)
	}
	if row.CollectedAt <= 0 {
		t.Fatalf("collected_at 未写: %+v", row)
	}
	if row.UptimeSec <= 0 {
		t.Fatalf("uptime_sec 应来自 SetStartedAt（节点进程运行时长），实际 %d", row.UptimeSec)
	}

	// 2) 读取方：时间戳必须"就在刚才"，而不是公元 58712 年
	st, err := (&NodeStatusSource{}).NodeStatus(context.Background(), tag)
	if err != nil {
		t.Fatalf("读取节点状态失败: %v", err)
	}
	if st.CollectedAt.IsZero() {
		t.Fatal("读取方没有识别到水位行（CollectedAt 零值）")
	}
	if d := time.Since(st.CollectedAt); d < 0 || d > 2*time.Minute {
		t.Fatalf("采集时间口径不一致（写入方与读取方对 collected_at 单位理解不同）: %v（距今 %s）", st.CollectedAt, d)
	}
	if st.CPUCores != row.CPUCores || st.MemTotalMB != row.MemTotalMB || st.DiskTotalMB != row.DiskTotalMB {
		t.Fatalf("读取方丢容量字段: status=%+v row=%+v", st, row)
	}

	// 3) 工具输出：模型真能看到容量，而不是只看到百分比
	res, err := tools.NewNodeStatusTool(&NodeStatusSource{}).Execute(context.Background(), map[string]interface{}{"node": tag})
	if err != nil {
		t.Fatalf("调用 node_status 工具失败: %v", err)
	}
	if !res.OK {
		t.Fatalf("node_status 未返回成功: %+v", res)
	}
	if !strings.Contains(res.Output, "容量：") || !strings.Contains(res.Output, "核") {
		t.Fatalf("工具输出缺少容量信息（模型仍然只知道百分比）:\n%s", res.Output)
	}
	if reported, _ := res.Meta["metrics_reported"].(bool); !reported {
		t.Fatalf("meta.metrics_reported 应为 true: %+v", res.Meta)
	}
	if cores, _ := res.Meta["cpu_cores"].(int); cores <= 0 {
		t.Fatalf("meta 里缺少 cpu_cores: %+v", res.Meta)
	}

	// 4) 二次上报是 upsert：仍然只有一行，且 collected_at 前进
	prev := row.CollectedAt
	time.Sleep(1100 * time.Millisecond) // collected_at 单位是秒，跨秒才有得比
	if _, err := pub.RunOnce(context.Background()); err != nil {
		t.Fatalf("二次上报失败: %v", err)
	}
	var count int64
	db.Table(models.CronyNodeMetricTableName).Where("node_uuid = ?", tag).Count(&count)
	if count != 1 {
		t.Fatalf("二次上报应 upsert 到同一行，实际 %d 行", count)
	}
	var row2 models.NodeMetric
	if err := db.Table(models.CronyNodeMetricTableName).Where("node_uuid = ?", tag).First(&row2).Error; err != nil {
		t.Fatalf("读取二次上报结果失败: %v", err)
	}
	if row2.CollectedAt <= prev {
		t.Fatalf("二次上报的 collected_at 没有前进: %d → %d", prev, row2.CollectedAt)
	}
}

// 采集失败时必须**不写库**：否则库里会出现一行 CPU=0 的假水位，
// 而 admin 正是靠 collected_at 区分"未上报"与"上报为零"。
func TestIntegrationNodeMetricPublisherSkipsOnCollectError(t *testing.T) {
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过水位链路端到端测试")
	}
	if _, err := dbclient.Init(dsn, "silent", 1, 5); err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db := dbclient.GetMysqlDB()
	if err := db.AutoMigrate(&models.NodeMetric{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	tag := fmt.Sprintf("it-nodemetric-err-%d", time.Now().UnixNano())
	defer db.Exec("DELETE FROM "+models.CronyNodeMetricTableName+" WHERE node_uuid LIKE ?", tag+"%")

	pub := nodestatus.NewPublisher(db, tag, "it-metric-host", "v9.9.9").
		SetCollector(func() (nodestatus.Snapshot, error) {
			return nodestatus.Snapshot{}, fmt.Errorf("模拟采集失败（例如平台未实现）")
		})
	written, err := pub.RunOnce(context.Background())
	if err == nil {
		t.Fatal("采集失败时 RunOnce 必须返回 error")
	}
	if written {
		t.Fatal("采集失败时不得写入水位行")
	}
	var count int64
	db.Table(models.CronyNodeMetricTableName).Where("node_uuid = ?", tag).Count(&count)
	if count != 0 {
		t.Fatalf("采集失败却写了 %d 行水位（会污染成 CPU=0 的假数据）", count)
	}
}
