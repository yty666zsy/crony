//go:build integration

package handler

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/executor"
)

// 执行协调器的真实 MySQL 集成测试（阶段 1）。
//
// 与 `runner_test.go` 的分工：
//   - runner_test.go：内存替身，验证**并发/幂等/取消语义**，不依赖外部服务；
//   - 本文件：真实 MySQL，验证**生产 SQL 路径**（MySQLRunStore 的插入/更新/查询、
//     唯一索引兜底、真实 shell 执行器落库），用于关闭 PHASE1 文档中的未验证项 1。
//
// 运行方式：
//
//	$env:CRONY_TEST_MYSQL_DSN='root:密码@tcp(127.0.0.1:3306)/crony_verify_node?charset=utf8mb4&parseTime=True&loc=Local'
//	go test -tags integration ./node/internal/handler/ -run TestMySQL -count=1 -v
func requireMySQLNode(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 集成测试")
	}
	db, err := dbclient.Init(dsn, "error", 5, 10)
	if err != nil {
		t.Fatalf("连接 MySQL 失败: %v", err)
	}
	// AutoMigrate 走真实 MySQL：唯一索引是否真的建出来，决定幂等能否兜底
	if err := db.AutoMigrate(&models.JobRun{}, &models.JobRunStep{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
}

func cleanupNodeRun(t *testing.T, runIDs ...string) {
	t.Helper()
	db := dbclient.GetMysqlDB()
	for _, runID := range runIDs {
		if err := db.Exec("delete from "+models.CronyJobRunStepTableName+" where run_id = ?", runID).Error; err != nil {
			t.Logf("清理步骤失败: %v", err)
		}
		if err := db.Exec("delete from "+models.CronyJobRunTableName+" where run_id = ?", runID).Error; err != nil {
			t.Logf("清理执行记录失败: %v", err)
		}
	}
}

// successCmd 返回一个**直接 argv 执行**（不经过 shell）的成功命令。
//
// 注意：新执行器对 Cmd 采用 RawArgv 语义（等同旧 type=1 的 exec.Command 行为），
// 因此这里必须显式给出解释器，不能写 ["echo", "ok"]（Windows 上没有 echo.exe）。
func successCmd() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/C", "echo", "crony-it-ok"}
	}
	return []string{"sh", "-c", "echo crony-it-ok"}
}

// failCmd 退出码 3：不在不可重试白名单（2/126/127/9009）内，但 RetryTimes=0 → 只跑一次。
func failCmd() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/C", "exit", "3"}
	}
	return []string{"sh", "-c", "exit 3"}
}

func itJob(id int, name string, cmd []string) *Job {
	return &Job{Job: &models.Job{
		ID:      id,
		Name:    name,
		Command: "echo crony-it-ok",
		Type:    models.JobTypeCmd,
		Spec:    "*/5 * * * *",
		RunOn:   "node-it-01",
		Timeout: 30,
		Cmd:     cmd,
	}}
}

// TestMySQLRunCoordinatorEndToEnd 真实 MySQL + 真实 shell 执行器的端到端：
// 一次执行必须落下 1 条 job_run（success）与 1 条 job_run_step（exit_code=0）。
func TestMySQLRunCoordinatorEndToEnd(t *testing.T) {
	requireMySQLNode(t)

	// 真实执行器（不是 fake）：真正 fork 一个进程
	c := NewRunCoordinator(executor.DefaultRegistry(), MySQLRunStore{})
	c.SetProcPublisher(nil) // 集成测试不连 etcd

	slot := time.Now().Truncate(time.Minute).Unix()
	job := itJob(910001, "集成测试-成功", successCmd())
	runID := models.RunIDForSlot(job.ID, slot)
	cleanupNodeRun(t, runID)
	defer cleanupNodeRun(t, runID)

	outcome := c.RunJob(job, models.TriggerScheduler, time.Unix(slot, 0))
	if outcome.Err != nil || !outcome.Succeeded() {
		t.Fatalf("执行应成功: err=%v outcome=%+v", outcome.Err, outcome)
	}
	if outcome.Run == nil || outcome.Run.ID == 0 {
		t.Fatalf("执行记录未落库: %+v", outcome.Run)
	}

	// 从库里重新读，确认不是"内存里看着对"
	stored := &models.JobRun{RunID: runID}
	if err := stored.FindByRunID(); err != nil {
		t.Fatalf("按 run_id 查不到落库的执行记录: %v", err)
	}
	if stored.Status != int(models.RunStatusSuccess) {
		t.Fatalf("库中状态应为 success(%d)，实际 %d", models.RunStatusSuccess, stored.Status)
	}
	if stored.FinishedAt == 0 {
		t.Fatalf("结束时间未落库: finished_at=%d", stored.FinishedAt)
	}
	if stored.TriggerSource != int(models.TriggerScheduler) {
		t.Fatalf("触发来源未落库: %d", stored.TriggerSource)
	}
	if stored.PolicySnapshot == "" {
		t.Fatalf("策略快照未落库（复盘时无法回答为什么跳过/重试）")
	}

	steps := make([]models.JobRunStep, 0)
	if err := dbclient.GetMysqlDB().Table(models.CronyJobRunStepTableName).
		Where("run_id = ?", runID).Order("step_index asc").Find(&steps).Error; err != nil {
		t.Fatalf("查询步骤失败: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("应有 1 条步骤记录，实际 %d", len(steps))
	}
	if steps[0].Status != int(models.RunStatusSuccess) || steps[0].ExitCode != 0 {
		t.Fatalf("步骤状态不符: status=%d exit=%d", steps[0].Status, steps[0].ExitCode)
	}
	if steps[0].Output == "" {
		t.Fatalf("步骤输出未落库")
	}
}

// TestMySQLRunCoordinatorIdempotentOnRealDB 同槽位第二次触发必须被去重，
// 且库中只有 1 条执行记录。
func TestMySQLRunCoordinatorIdempotentOnRealDB(t *testing.T) {
	requireMySQLNode(t)

	c := NewRunCoordinator(executor.DefaultRegistry(), MySQLRunStore{})
	c.SetProcPublisher(nil)

	slot := time.Now().Truncate(time.Minute).Unix()
	job := itJob(910002, "集成测试-幂等", successCmd())
	runID := models.RunIDForSlot(job.ID, slot)
	cleanupNodeRun(t, runID)
	defer cleanupNodeRun(t, runID)

	first := c.RunJob(job, models.TriggerScheduler, time.Unix(slot, 0))
	if first.Err != nil {
		t.Fatalf("首次执行应成功: %v", first.Err)
	}
	second := c.RunJob(job, models.TriggerScheduler, time.Unix(slot, 0))
	if !second.Skipped {
		t.Fatalf("同槽位第二次触发应被去重: %+v", second)
	}

	var count int64
	if err := dbclient.GetMysqlDB().Table(models.CronyJobRunTableName).
		Where("run_id = ?", runID).Count(&count).Error; err != nil {
		t.Fatalf("统计执行记录失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("同槽位只能有 1 条执行记录，实际 %d", count)
	}
}

// TestMySQLRunCoordinatorFailurePersists 失败执行也必须落库（否则"最近失败"面板是空的），
// 且退出码要能被上层读到（可重试性判定依赖它）。
func TestMySQLRunCoordinatorFailurePersists(t *testing.T) {
	requireMySQLNode(t)

	c := NewRunCoordinator(executor.DefaultRegistry(), MySQLRunStore{})
	c.SetProcPublisher(nil)

	job := itJob(910003, "集成测试-失败", failCmd())
	runID := models.NewManualRunID(job.ID, time.Now())
	cleanupNodeRun(t, runID)
	defer cleanupNodeRun(t, runID)

	outcome := c.RunJobWithID(job, models.TriggerManual, time.Now(), runID)
	if outcome.Err == nil {
		t.Fatalf("命令应当失败: %+v", outcome)
	}

	stored := &models.JobRun{RunID: runID}
	if err := stored.FindByRunID(); err != nil {
		t.Fatalf("失败执行未落库: %v", err)
	}
	if stored.Status != int(models.RunStatusFailed) {
		t.Fatalf("状态应为 failed(%d)，实际 %d", models.RunStatusFailed, stored.Status)
	}
	if stored.ExitCode == 0 {
		t.Fatalf("失败执行的退出码必须落库（可重试性判定依赖它），实际 0")
	}
	if stored.ErrorMsg == "" {
		t.Fatalf("失败原因未落库")
	}
}

// TestMySQLRunCoordinatorFailureSeedForDiagnosis 为"真实 provider 诊断"准备一条
// **根因可被外部独立核对**的失败执行。
//
// 与 TestMySQLRunCoordinatorFailurePersists 的区别：那条用 `exit 3`，库里只有退出码、
// 没有任何可分析的文本，模型再强也无从下结论。本用例执行的是**真的会失败的真实命令**，
// 要求 stdout/stderr 原文落到 job_run_step.output，这样才能检验
// "证据 → 结论"这条链路是不是真的成立（而不是靠提示词猜）。
//
// 默认命令：读取一个不存在的文件（真实的操作系统错误）。
// 可用 CRONY_SEED_CMD 覆盖为 JSON 数组形式的 argv，例如：
//
//	CRONY_SEED_CMD='["cmd","/C","E:\\MySql\\bin\\mysql.exe -h 127.0.0.1 -P 13307 -u crony_backup -pw -e \"select 1\""]'
//
// 这类命令的价值在于**根因可独立验证**：端口确实没监听、账号确实不存在，
// 因此模型说得对不对可以被外部事实判定，而不是只能"看着像对"。
func TestMySQLRunCoordinatorFailureSeedForDiagnosis(t *testing.T) {
	requireMySQLNode(t)

	argv := []string{"cmd", "/C", "type", `C:\__crony_missing_evidence_file__.sql`}
	if raw := strings.TrimSpace(os.Getenv("CRONY_SEED_CMD")); raw != "" {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("CRONY_SEED_CMD 不是合法 JSON 数组: %v", err)
		}
		if len(parsed) == 0 {
			t.Fatal("CRONY_SEED_CMD 为空数组")
		}
		argv = parsed
	}

	c := NewRunCoordinator(executor.DefaultRegistry(), MySQLRunStore{})
	c.SetProcPublisher(nil)

	job := itJob(910005, "诊断取证-真实失败", argv)
	runID := models.NewManualRunID(job.ID, time.Now())
	cleanupNodeRun(t, runID)
	// 联调取证时用 CRONY_SEED_KEEP=1 保留这次失败记录：
	// 真实 provider 的诊断要通过 HTTP 打到正在运行的 admin，必须让数据留在库里。
	if os.Getenv("CRONY_SEED_KEEP") == "1" {
		t.Logf("CRONY_SEED_KEEP=1：保留本次取证数据（不清理）")
	} else {
		defer cleanupNodeRun(t, runID)
	}

	outcome := c.RunJobWithID(job, models.TriggerManual, time.Now(), runID)
	if outcome.Err == nil {
		t.Fatalf("该命令必须失败，否则无法作为诊断证据: %+v", outcome)
	}
	stored := &models.JobRun{RunID: runID}
	if err := stored.FindByRunID(); err != nil {
		t.Fatalf("失败执行未落库: %v", err)
	}
	if stored.Status != int(models.RunStatusFailed) {
		t.Fatalf("状态应为 failed(%d)，实际 %d", models.RunStatusFailed, stored.Status)
	}

	steps := make([]models.JobRunStep, 0)
	if err := dbclient.GetMysqlDB().Table(models.CronyJobRunStepTableName).
		Where("run_id = ?", runID).Order("step_index asc").Find(&steps).Error; err != nil {
		t.Fatalf("查询步骤失败: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("失败执行没有步骤记录，诊断将失去证据")
	}
	// 证据必须真的存在：模型能引用的东西就是这两列里的文本。
	evidence := steps[0].Output + " " + steps[0].ErrorMsg
	if strings.TrimSpace(evidence) == "" {
		t.Fatalf("步骤里没有任何错误文本，无法用于诊断取证: %+v", steps[0])
	}
	t.Logf("取证失败执行已落库：run_id=%s exit_code=%d\n--- 步骤输出原文 ---\n%s\n--- 错误摘要 ---\n%s",
		runID, steps[0].ExitCode, steps[0].Output, steps[0].ErrorMsg)
	t.Logf("SEED_RUN_ID=%s", runID)
}
func TestMySQLRunCoordinatorConcurrentSameSlotOnRealDB(t *testing.T) {
	requireMySQLNode(t)

	c := NewRunCoordinator(executor.DefaultRegistry(), MySQLRunStore{})
	c.SetProcPublisher(nil)

	slot := time.Now().Truncate(time.Minute).Unix()
	job := itJob(910004, "集成测试-并发", successCmd())
	runID := models.RunIDForSlot(job.ID, slot)
	cleanupNodeRun(t, runID)
	defer cleanupNodeRun(t, runID)

	type result struct {
		executed bool
		err      error
	}
	const workers = 8
	results := make(chan result, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outcome := c.RunJob(job, models.TriggerScheduler, time.Unix(slot, 0))
			// 只有真正执行过的（非跳过/非排队）才算"执行了一次"
			results <- result{executed: !outcome.Skipped, err: outcome.Err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	ran, failed := 0, 0
	for r := range results {
		if r.executed {
			ran++
			if r.err != nil {
				failed++
			}
		}
	}
	if ran != 1 || failed != 0 {
		t.Fatalf("8 个并发同槽位触发应恰好 1 次成功执行，实际执行 %d 次（其中失败 %d 次）", ran, failed)
	}

	var count int64
	if err := dbclient.GetMysqlDB().Table(models.CronyJobRunTableName).
		Where("run_id = ?", runID).Count(&count).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("库中应只有 1 条执行记录，实际 %d", count)
	}
}
