//go:build integration

package service

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/tools"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestIntegrationNodeProbeLoopAgainstRealMySQL 端到端验证 admin→node 只读探测通道。
//
// 跑的是**真实一整套**：真 MySQL（13306 临时实例）+ 真命令执行（不注入假执行器）
// + 真表结构与真状态机。断言的是通道的**安全语义**而不只是"能跑通"：
//
//  1. 只领取本节点的请求（别的节点的请求必须原地不动）；
//  2. 白名单外的命令在节点侧被拒绝，且**没有真的执行**；
//  3. 正常命令真的执行并把输出+来源信息+退出码写回；
//  4. 输出里带节点主机名与命令名（证据可归属）；
//  5. 领取是原子的（第二次 RunOnce 不会重复执行已完成的行）。
//
// 为什么不用假执行器：这个通道的全部价值就是"在目标机器上真的看到端口/进程"，
// 用假执行器测等于把最需要验证的部分（真的跑得起来吗）留空。
func TestIntegrationNodeProbeLoopAgainstRealMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CRONY_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真库集成测试")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&models.NodeProbeRequest{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	tag := fmt.Sprintf("it-probe-%d", time.Now().UnixNano())
	nodeUUID := tag + "-node"
	otherNode := tag + "-other"
	defer func() {
		// 只清理自己造的行，绝不 TRUNCATE（共享测试库里有别人的数据）
		db.Where("node_uuid IN ?", []string{nodeUUID, otherNode}).Delete(&models.NodeProbeRequest{})
	}()

	// 本机真实可用的只读命令（按平台选择，两边都在白名单内）
	cmd, args := "ps", []string{"-ef"}
	if runtime.GOOS == "windows" {
		cmd, args = "netstat", []string{"-ano"}
	}

	mine := &models.NodeProbeRequest{
		RequestID:      tag + "-1",
		NodeUUID:       nodeUUID,
		Command:        cmd,
		ArgvJSON:       `["` + strings.Join(args, `","`) + `"]`,
		TimeoutSeconds: 10,
		Status:         models.NodeProbePending,
		Reason:         "集成测试：确认探测通道可用",
		CreatedBy:      "it-suite",
		Created:        time.Now().UnixMilli(),
	}
	foreign := &models.NodeProbeRequest{
		RequestID:      tag + "-2",
		NodeUUID:       otherNode,
		Command:        cmd,
		ArgvJSON:       `["` + strings.Join(args, `","`) + `"]`,
		TimeoutSeconds: 10,
		Status:         models.NodeProbePending,
		Created:        time.Now().UnixMilli(),
	}
	// 写动作：即使被塞进共享库，节点侧也必须拒绝执行
	forbidden := &models.NodeProbeRequest{
		RequestID:      tag + "-3",
		NodeUUID:       nodeUUID,
		Command:        "sc",
		ArgvJSON:       `["stop","MySQL84"]`,
		TimeoutSeconds: 10,
		Status:         models.NodeProbePending,
		Created:        time.Now().UnixMilli(),
	}
	for _, r := range []*models.NodeProbeRequest{mine, foreign, forbidden} {
		if err := db.Create(r).Error; err != nil {
			t.Fatalf("写入探测请求失败: %v", err)
		}
	}

	worker, err := NewProbeWorker(db, nodeUUID, "it-host")
	if err != nil {
		t.Fatalf("创建探测执行器失败: %v", err)
	}
	worker.runner = tools.DefaultShellRunner // 真执行，不注入假执行器

	n, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("执行一轮失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("本轮应领取 2 条（本节点待处理），实际 %d", n)
	}

	// 1) 正常命令：真的执行并写回
	got, err := models.FindProbeRequest(db, mine.RequestID)
	if err != nil || got == nil {
		t.Fatalf("读回探测请求失败: %v %v", got, err)
	}
	if got.Status != models.NodeProbeDone {
		t.Fatalf("状态应为 done，实际 %s（error=%s）", got.Status, got.Error)
	}
	if got.ExitCode != 0 {
		t.Fatalf("%s %v 应返回 0，实际 %d（error=%s output=%q）", cmd, args, got.ExitCode, got.Error, got.Output)
	}
	if len(strings.TrimSpace(got.Output)) == 0 {
		t.Fatal("输出不能为空（空输出会让模型以为'没有发现'）")
	}
	if !strings.Contains(got.Output, "it-host") || !strings.Contains(got.Output, cmd) {
		t.Fatalf("输出必须带节点与命令来源: %q", got.Output)
	}
	if got.NodeHostname != "it-host" {
		t.Fatalf("应记录执行节点主机名，实际 %q", got.NodeHostname)
	}
	if got.ClaimedAt == 0 || got.FinishedAt == 0 {
		t.Fatalf("领取/完成时间戳缺失: claimed=%d finished=%d", got.ClaimedAt, got.FinishedAt)
	}
	if runtime.GOOS == "windows" && !strings.Contains(got.Output, "TCP") {
		t.Fatalf("Windows 下 netstat -ano 应输出 TCP 段:\n%s", got.Output)
	}

	// 2) 别的节点的请求：不能被领取
	other, err := models.FindProbeRequest(db, foreign.RequestID)
	if err != nil || other == nil {
		t.Fatalf("读回他人请求失败: %v %v", other, err)
	}
	if other.Status != models.NodeProbePending {
		t.Fatalf("不得领取别的节点的请求，实际状态 %s", other.Status)
	}

	// 3) 写动作：节点侧拒绝，且没有任何输出（说明根本没执行）
	denied, err := models.FindProbeRequest(db, forbidden.RequestID)
	if err != nil || denied == nil {
		t.Fatalf("读回被拒请求失败: %v %v", denied, err)
	}
	if denied.Status != models.NodeProbeFailed {
		t.Fatalf("sc stop 必须被拒绝，实际 %s", denied.Status)
	}
	if !strings.Contains(denied.Error, "白名单") {
		t.Fatalf("拒绝原因应说明白名单，实际 %q", denied.Error)
	}
	if strings.TrimSpace(denied.Output) != "" {
		t.Fatalf("被拒命令不得有任何输出（说明真的没执行）: %q", denied.Output)
	}
	if denied.ExitCode != -1 {
		t.Fatalf("未执行的命令退出码应为 -1，实际 %d", denied.ExitCode)
	}

	// 4) 幂等：再跑一轮不应该重复执行已完成的行（只剩别的节点那条不动）
	n2, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("第二轮不应再领取任何请求，实际 %d", n2)
	}
	again, _ := models.FindProbeRequest(db, mine.RequestID)
	if again.Status != models.NodeProbeDone || again.FinishedAt != got.FinishedAt {
		t.Fatalf("已完成的行不应被重跑: status=%s finished=%d→%d", again.Status, got.FinishedAt, again.FinishedAt)
	}
}
