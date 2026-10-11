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
	"github.com/tmnhs/crony/common/pkg/probechannel"
	"github.com/tmnhs/crony/common/pkg/tools"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestIntegrationProbeChannelAdminToNode 阶段 3 第二段的**接缝**验证。
//
// 这里跑的是"两个半场"的真实实现，一点没替身：
//   - admin 半场：common/pkg/probechannel.Source（真正被 admin 装配进 node_probe 工具的那份代码）
//   - node 半场：本包的 ProbeWorker（真正被节点 main 启动的那份代码）
//   - 中间：真 MySQL（13306 临时实例）里的 node_probe_request 表
//
// 为什么要专门测接缝：两半各自单测通过**不代表**拼得上——表的字段名、
// 状态机取值、argv 编码、谁写 finished_at，任何一处不一致都会表现为
// "admin 一直等、节点领不到"。这类 bug 只有把两半放一起跑才会暴露。
//
// 仍未覆盖（诚实说明）：真实是**两个进程**，这里同进程；跨进程只差 MySQL 事务可见性，
// 而真 MySQL 的 READ COMMITTED/REPEATABLE READ 语义在这里已经真实生效。
func TestIntegrationProbeChannelAdminToNode(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CRONY_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真库集成测试")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&models.NodeProbeRequest{}, &models.Node{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	tag := fmt.Sprintf("it-loop-%d", time.Now().UnixNano())
	nodeUUID := tag + "-node"
	defer func() {
		db.Where("node_uuid = ?", nodeUUID).Delete(&models.NodeProbeRequest{})
		db.Where("uuid = ?", nodeUUID).Delete(&models.Node{})
	}()

	// node 表必须有这台节点：探测前会校验"这台机器存在"，
	// 否则模型拿到的会是"节点不存在"而不是真实探测结果。
	node := &models.Node{UUID: nodeUUID, Hostname: "it-loop-host", Status: models.NodeConnSuccess, UpTime: time.Now().Unix()}
	if err := db.Create(node).Error; err != nil {
		t.Fatalf("写入节点失败: %v", err)
	}

	src, err := probechannel.NewSource(db)
	if err != nil {
		t.Fatalf("创建 admin 侧数据源失败: %v", err)
	}
	worker, err := NewProbeWorker(db, nodeUUID, "it-loop-host")
	if err != nil {
		t.Fatalf("创建 node 侧执行器失败: %v", err)
	}
	worker.runner = tools.DefaultShellRunner

	// admin 半场：发起探测并等待结果（真实轮询逻辑）
	type outcome struct {
		res tools.ProbeResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := src.Probe(context.Background(), tools.ProbeRequest{
			NodeUUID:  nodeUUID,
			Command:   "tasklist",
			Args:      []string{"/fi", "imagename eq mysqld.exe"},
			Timeout:   15 * time.Second,
			Reason:    "接缝测试：确认探测通道两端拼得上",
			CreatedBy: "it-suite",
		})
		done <- outcome{res, err}
	}()

	// node 半场：轮询领取（真实？不是——这里只需一轮；RunOnce 等价于 worker.Run 的一步）
	deadline := time.Now().Add(10 * time.Second)
	claimed := 0
	for time.Now().Before(deadline) {
		n, err := worker.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("节点侧执行失败: %v", err)
		}
		claimed += n
		if claimed > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond) // 等 admin 把请求写进库
	}
	if claimed == 0 {
		t.Fatal("节点侧始终没领到请求：说明两端对表/状态/字段的理解不一致")
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("admin 侧没有在时限内拿到结果")
	}
	if got.err != nil {
		t.Fatalf("admin 侧探测返回错误: %v", got.err)
	}
	if got.res.Status != models.NodeProbeDone {
		t.Fatalf("状态应为 done，实际 %s（error=%s）", got.res.Status, got.res.Error)
	}
	if got.res.ExitCode != 0 {
		t.Fatalf("exit_code 应为 0，实际 %d（error=%s output=%q）", got.res.ExitCode, got.res.Error, got.res.Output)
	}
	if !strings.Contains(got.res.Output, "tasklist") {
		t.Fatalf("输出应记录执行的命令: %q", got.res.Output)
	}
	if got.res.Hostname != "it-loop-host" {
		t.Fatalf("admin 侧应拿到执行节点主机名，实际 %q", got.res.Hostname)
	}
	if !strings.HasPrefix(got.res.RequestID, "p") {
		t.Fatalf("request_id 格式异常: %q", got.res.RequestID)
	}

	// 审计行必须完整：谁、为什么、跑了什么、什么时候完成
	row, err := models.FindProbeRequest(db, got.res.RequestID)
	if err != nil || row == nil {
		t.Fatalf("读回审计行失败: %v %v", row, err)
	}
	if row.CreatedBy != "it-suite" || row.Reason == "" {
		t.Fatalf("审计行必须记录发起人与原因: %+v", row)
	}
	if row.Command != "tasklist" || !strings.Contains(row.ArgvJSON, "imagename eq mysqld.exe") {
		t.Fatalf("审计行必须记录命令与参数: %s %s", row.Command, row.ArgvJSON)
	}
	if row.FinishedAt == 0 || row.ClaimedAt == 0 {
		t.Fatalf("时间戳缺失: claimed=%d finished=%d", row.ClaimedAt, row.FinishedAt)
	}
}

// TestIntegrationProbeChannelNobodyClaims 没人领取时必须**如实**报 expired。
//
// 这个用例守的是"别把'节点没来'说成'命令失败'"——两者对诊断结论的含义完全不同：
// 前者说明证据没拿到（不能下结论），后者说明命令跑了但结果异常。
func TestIntegrationProbeChannelNobodyClaims(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CRONY_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真库集成测试")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&models.NodeProbeRequest{}, &models.Node{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	tag := fmt.Sprintf("it-noclaim-%d", time.Now().UnixNano())
	defer func() {
		db.Where("node_uuid = ?", tag).Delete(&models.NodeProbeRequest{})
		db.Where("uuid = ?", tag).Delete(&models.Node{})
	}()
	if err := db.Create(&models.Node{UUID: tag, Hostname: "it-noclaim-host",
		Status: models.NodeConnFail, UpTime: time.Now().Unix()}).Error; err != nil {
		t.Fatalf("写入节点失败: %v", err)
	}

	src, err := probechannel.NewSource(db)
	if err != nil {
		t.Fatalf("创建数据源失败: %v", err)
	}
	// 缩短等待，避免测试拖 15 秒
	src.SetWaitTimeout(600 * time.Millisecond)

	// 刻意不启动节点执行器：模拟"节点离线 / 探测通道未启用"
	start := time.Now()
	res, err := src.Probe(context.Background(), tools.ProbeRequest{
		NodeUUID: tag, Command: "netstat", Args: []string{"-ano"}, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("探测应返回结果（而非 error），实际: %v", err)
	}
	if res.Status != models.NodeProbeExpired {
		t.Fatalf("没人领取应报 expired，实际 %s", res.Status)
	}
	if !strings.Contains(res.Error, "不等于命令执行失败") {
		t.Fatalf("必须澄清「不是命令失败」: %q", res.Error)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("等待上限没生效（说明 SetWaitTimeout 未作用于轮询）")
	}
	// 审计行也要落到 expired，不能永远停在 pending
	rows := []models.NodeProbeRequest{}
	if err := db.Where("node_uuid = ?", tag).Find(&rows).Error; err != nil {
		t.Fatalf("查询审计行失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != models.NodeProbeExpired {
		t.Fatalf("审计行应标记 expired，实际 %+v", rows)
	}
	if rows[0].FinishedAt == 0 {
		t.Fatal("expired 也要写 finished_at（否则无法区分'还在等'和'已放弃'）")
	}
	// 审计行必须**自己说明为什么没有输出**：只写 status=expired 的话，事后回看
	// 无法区分"节点离线"与"命令跑了没输出"——实测中模型正是靠"没有输出"
	// 编造了 netstat 结果（live session r54，见 PHASE2 §3.2）。
	if !strings.Contains(rows[0].Error, "没有领取") || rows[0].ExitCode != -1 {
		t.Fatalf("expired 审计行必须记录原因与退出码，实际 error=%q exit_code=%d", rows[0].Error, rows[0].ExitCode)
	}
}

// TestIntegrationProbeChannelUnknownNodeFailsFast 不存在的节点必须**立刻**失败。
//
// 否则请求会挂到超时，模型读到的是"节点没响应"——而事实是"根本没有这台机器"。
func TestIntegrationProbeChannelUnknownNodeFailsFast(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CRONY_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真库集成测试")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&models.NodeProbeRequest{}, &models.Node{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	src, err := probechannel.NewSource(db)
	if err != nil {
		t.Fatalf("创建数据源失败: %v", err)
	}
	src.SetWaitTimeout(200 * time.Millisecond)
	missing := fmt.Sprintf("it-missing-%d", time.Now().UnixNano())
	start := time.Now()
	_, err = src.Probe(context.Background(), tools.ProbeRequest{
		NodeUUID: missing, Command: "netstat", Args: []string{"-ano"},
	})
	if err == nil {
		t.Fatal("不存在的节点必须直接报错")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误应说清\"节点不存在\": %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("应快速失败，而不是等到超时")
	}
	// 不存在的节点不该留下垃圾请求行
	var count int64
	db.Model(&models.NodeProbeRequest{}).Where("node_uuid = ?", missing).Count(&count)
	if count != 0 {
		t.Fatalf("不应为空想节点写入请求行，实际 %d 行", count)
	}
}
