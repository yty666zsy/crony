//go:build integration

package handler

import (
	"encoding/json"
	"fmt"
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

// cleanupNodeRunJob 清理某个**测试专用 job_id** 下的全部执行记录。
//
// 为什么需要它：并发用例里只有 1 个 worker 真正执行（run_id = RunIDForSlot），
// 其余 worker 走"同槽位已被占用"分支，会写一条 run_id 形如
// `j<jobID>-m<纳秒>-skip` 的记录——它的 run_id **不是** RunIDForSlot 推导出来的，
// 所以 cleanupNodeRun(runID) 永远删不到它。实测每跑一轮留 7 行，
// 35 行残留在库里（job_id=910004 在 job 表中并不存在，纯测试脏数据）。
//
// 只按测试专用 job_id 清理，不碰其他任何数据。
func cleanupNodeRunJob(t *testing.T, jobID int) {
	t.Helper()
	prefix := fmt.Sprintf("j%d-%%", jobID)
	db := dbclient.GetMysqlDB()
	if err := db.Exec("delete from "+models.CronyJobRunStepTableName+" where run_id like ?", prefix).Error; err != nil {
		t.Logf("清理步骤失败: %v", err)
	}
	if err := db.Exec("delete from "+models.CronyJobRunTableName+" where run_id like ?", prefix).Error; err != nil {
		t.Logf("清理执行记录失败: %v", err)
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
	// 被跳过的 worker 写的是 `…-skip` 记录（run_id 与 slot 无关），必须按 job_id 清
	cleanupNodeRunJob(t, job.ID)
	defer cleanupNodeRunJob(t, job.ID)

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

// ---------- 阶段 4-B：多步骤编排（DAG）的真实执行 ----------

// dagStepJSON 生成一个步骤的编排 JSON。
//
// 用显式 args 而不是拼一条命令行：Windows/Linux 的解释器不同（cmd /C 与 sh -c），
// 拼字符串既容易踩引号转义的坑，也掩盖了"编排里命令怎么切分"这件事。
func dagStepJSON(index int, name string, cmd []string, dependsOn, join string) string {
	args, _ := json.Marshal(cmd[1:])
	dep := ""
	if dependsOn != "" {
		dep = fmt.Sprintf(`,"depends_on":%q`, dependsOn)
	}
	joinPart := ""
	if join != "" {
		joinPart = fmt.Sprintf(`,"join":%q`, join)
	}
	return fmt.Sprintf(`{"index":%d,"name":%q,"command":%q,"args":%s%s%s}`,
		index, name, cmd[0], string(args), dep, joinPart)
}

func dagJob(id int, name, stepsJSON string) *Job {
	return &Job{Job: &models.Job{
		ID:      id,
		Name:    name,
		Command: "echo fallback-must-not-run",
		Type:    models.JobTypeCmd,
		Spec:    "*/5 * * * *",
		RunOn:   "node-it-01",
		Timeout: 30,
		// 关键：StepsJSON 非空时 BuildPlan 必须走编排路径，绝不回退到这条 command。
		StepsJSON: stepsJSON,
	}}
}

func readRunSteps(t *testing.T, runID string) []models.JobRunStep {
	t.Helper()
	steps := make([]models.JobRunStep, 0)
	if err := dbclient.GetMysqlDB().Table(models.CronyJobRunStepTableName).
		Where("run_id = ?", runID).Order("step_index asc").Find(&steps).Error; err != nil {
		t.Fatalf("查询步骤失败: %v", err)
	}
	return steps
}

// TestMySQLRunCoordinatorMultiStepDAG 真实 MySQL + 真实进程的菱形编排：
// 两条分支并行执行，汇聚步骤在上游都成功后执行；库中必须落下 4 条步骤记录，
// 且顺序按 step_index 而不是完成先后。
func TestMySQLRunCoordinatorMultiStepDAG(t *testing.T) {
	requireMySQLNode(t)

	c := NewRunCoordinator(executor.DefaultRegistry(), MySQLRunStore{})
	c.SetProcPublisher(nil) // 集成测试不连 etcd

	stepsJSON := "{\"max_parallel\":2,\"steps\":[" + strings.Join([]string{
		dagStepJSON(0, "extract", successCmd(), "", ""),
		dagStepJSON(1, "transform", successCmd(), "0", ""),
		dagStepJSON(2, "enrich", successCmd(), "0", ""),
		dagStepJSON(3, "load", successCmd(), "1,2", "all_success"),
	}, ",") + "]}"

	slot := time.Now().Truncate(time.Minute).Unix()
	job := dagJob(910010, "集成测试-DAG菱形", stepsJSON)
	runID := models.RunIDForSlot(job.ID, slot)
	cleanupNodeRun(t, runID)
	defer cleanupNodeRun(t, runID)

	outcome := c.RunJob(job, models.TriggerScheduler, time.Unix(slot, 0))
	if outcome.Err != nil || !outcome.Succeeded() {
		t.Fatalf("菱形编排应执行成功: err=%v outcome=%+v", outcome.Err, outcome)
	}

	stored := &models.JobRun{RunID: runID}
	if err := stored.FindByRunID(); err != nil {
		t.Fatalf("按 run_id 查不到执行记录: %v", err)
	}
	if stored.StepCount != 4 || stored.SuccessSteps != 4 || stored.FailedSteps != 0 {
		t.Fatalf("执行记录计数不符: step_count=%d success=%d failed=%d",
			stored.StepCount, stored.SuccessSteps, stored.FailedSteps)
	}

	steps := readRunSteps(t, runID)
	if len(steps) != 4 {
		t.Fatalf("应有 4 条步骤记录，实际 %d", len(steps))
	}
	wantNames := []string{"extract", "transform", "enrich", "load"}
	for i, s := range steps {
		if s.StepIndex != i || s.Name != wantNames[i] {
			t.Fatalf("第 %d 行步骤错位: index=%d name=%q", i, s.StepIndex, s.Name)
		}
		if s.Status != int(models.RunStatusSuccess) || s.ExitCode != 0 {
			t.Fatalf("步骤 %q 状态不符: status=%d exit=%d", s.Name, s.Status, s.ExitCode)
		}
		if s.Output == "" {
			t.Fatalf("步骤 %q 输出未落库", s.Name)
		}
	}
}

// TestMySQLRunCoordinatorDAGSkipsDownstreamOnFailure all_success 汇聚在
// 上游失败时必须跳过下游，且**跳过原因必须落库**（阶段 4-B 的核心可解释性要求）。
func TestMySQLRunCoordinatorDAGSkipsDownstreamOnFailure(t *testing.T) {
	requireMySQLNode(t)

	c := NewRunCoordinator(executor.DefaultRegistry(), MySQLRunStore{})
	c.SetProcPublisher(nil)

	stepsJSON := "{\"steps\":[" + strings.Join([]string{
		dagStepJSON(0, "extract", successCmd(), "", ""),
		dagStepJSON(1, "transform", failCmd(), "0", ""),
		dagStepJSON(2, "load", successCmd(), "1", "all_success"),
	}, ",") + "]}"

	slot := time.Now().Truncate(time.Minute).Unix()
	job := dagJob(910011, "集成测试-DAG上游失败", stepsJSON)
	runID := models.RunIDForSlot(job.ID, slot)
	cleanupNodeRun(t, runID)
	defer cleanupNodeRun(t, runID)

	outcome := c.RunJob(job, models.TriggerScheduler, time.Unix(slot, 0))
	if outcome.Err == nil {
		t.Fatal("上游失败时整次执行必须失败（不能因为下游被跳过就报成功）")
	}

	stored := &models.JobRun{RunID: runID}
	if err := stored.FindByRunID(); err != nil {
		t.Fatalf("按 run_id 查不到执行记录: %v", err)
	}
	if stored.Status != int(models.RunStatusFailed) {
		t.Fatalf("库中状态应为 failed，实际 %s", models.RunStatus(stored.Status))
	}
	if stored.ErrorMsg == "" {
		t.Fatal("失败原因未落库")
	}

	steps := readRunSteps(t, runID)
	if len(steps) != 3 {
		t.Fatalf("被跳过的步骤也必须落库（否则没人知道它没跑），实际 %d 条", len(steps))
	}
	if steps[0].Status != int(models.RunStatusSuccess) {
		t.Fatalf("extract 应成功，实际 %d", steps[0].Status)
	}
	if steps[1].Status != int(models.RunStatusFailed) || steps[1].ExitCode == 0 {
		t.Fatalf("transform 应失败且有非零退出码: status=%d exit=%d", steps[1].Status, steps[1].ExitCode)
	}
	skipped := steps[2]
	if skipped.Status != int(models.RunStatusSkipped) {
		t.Fatalf("load 应被跳过（all_success 上游失败），实际 %s", models.RunStatus(skipped.Status))
	}
	if !strings.Contains(skipped.ErrorMsg, "上游") {
		t.Fatalf("跳过原因必须写进 error_msg（否则控制台只是一句「跳过」）: %q", skipped.ErrorMsg)
	}
	if !strings.Contains(skipped.Artifacts, "all_success") || !strings.Contains(skipped.Artifacts, "depends_on") {
		t.Fatalf("跳过步骤的 artifacts 必须带汇聚策略与依赖: %s", skipped.Artifacts)
	}
}

// TestMySQLRunCoordinatorRejectsInvalidPlanOnRealDB 库里存着一份非法编排时，
// 节点必须**拒绝执行并留下 failed 记录**，而不是"跳过坏步骤跑剩下的"。
func TestMySQLRunCoordinatorRejectsInvalidPlanOnRealDB(t *testing.T) {
	requireMySQLNode(t)

	c := NewRunCoordinator(executor.DefaultRegistry(), MySQLRunStore{})
	c.SetProcPublisher(nil)

	// 成环：0 → 2 → 1 → 0
	stepsJSON := "{\"steps\":[" + strings.Join([]string{
		dagStepJSON(0, "a", successCmd(), "2", ""),
		dagStepJSON(1, "b", successCmd(), "0", ""),
		dagStepJSON(2, "c", successCmd(), "1", ""),
	}, ",") + "]}"

	slot := time.Now().Truncate(time.Minute).Unix()
	job := dagJob(910012, "集成测试-非法编排", stepsJSON)
	runID := models.RunIDForSlot(job.ID, slot)
	cleanupNodeRun(t, runID)
	defer cleanupNodeRun(t, runID)

	outcome := c.RunJob(job, models.TriggerScheduler, time.Unix(slot, 0))
	if outcome.Err == nil {
		t.Fatal("非法编排必须返回错误")
	}
	if !strings.Contains(outcome.Err.Error(), "编排") {
		t.Fatalf("错误信息应指出是编排问题: %q", outcome.Err.Error())
	}

	stored := &models.JobRun{RunID: runID}
	if err := stored.FindByRunID(); err != nil {
		t.Fatalf("拒绝执行也必须留痕（否则没人知道它没跑）: %v", err)
	}
	if stored.Status != int(models.RunStatusFailed) {
		t.Fatalf("拒绝执行应记为 failed，实际 %s", models.RunStatus(stored.Status))
	}
	if !strings.Contains(stored.ErrorMsg, "编排") {
		t.Fatalf("拒绝原因未落库: %q", stored.ErrorMsg)
	}
	if steps := readRunSteps(t, runID); len(steps) != 0 {
		t.Fatalf("被拒绝的执行不应有任何步骤记录，实际 %d 条", len(steps))
	}
}

// TestMySQLRunCoordinatorDAGFailFastStopsPendingBranches fail_fast 打开时，
// 一条分支失败不再启动其它待启动步骤，但 all_done 收集步骤仍要执行。
func TestMySQLRunCoordinatorDAGFailFastStopsPendingBranches(t *testing.T) {
	requireMySQLNode(t)

	c := NewRunCoordinator(executor.DefaultRegistry(), MySQLRunStore{})
	c.SetProcPublisher(nil)

	stepsJSON := "{\"max_parallel\":1,\"fail_fast\":true,\"steps\":[" + strings.Join([]string{
		dagStepJSON(0, "root", successCmd(), "root", ""),
		dagStepJSON(1, "branch-bad", failCmd(), "0", ""),
		dagStepJSON(2, "branch-late", successCmd(), "0", ""),
		dagStepJSON(3, "collector", successCmd(), "1,2", "all_done"),
	}, ",") + "]}"

	slot := time.Now().Truncate(time.Minute).Unix()
	job := dagJob(910013, "集成测试-DAG急停", stepsJSON)
	runID := models.RunIDForSlot(job.ID, slot)
	cleanupNodeRun(t, runID)
	defer cleanupNodeRun(t, runID)

	outcome := c.RunJob(job, models.TriggerScheduler, time.Unix(slot, 0))
	if outcome.Err == nil {
		t.Fatal("有分支失败时整次执行必须失败")
	}

	steps := readRunSteps(t, runID)
	if len(steps) != 4 {
		t.Fatalf("急停也必须把每个步骤的结局落库，实际 %d 条", len(steps))
	}
	if steps[1].Status != int(models.RunStatusFailed) {
		t.Fatalf("branch-bad 应失败，实际 %s", models.RunStatus(steps[1].Status))
	}
	// max_parallel=1 + fail_fast：branch-late 排在失败分支之后，必须被急停跳过
	if steps[2].Status != int(models.RunStatusSkipped) {
		t.Fatalf("branch-late 应因 fail_fast 被跳过，实际 %s（skip_reason=%s）",
			models.RunStatus(steps[2].Status), steps[2].ErrorMsg)
	}
	if !strings.Contains(steps[2].ErrorMsg, "fail_fast") {
		t.Fatalf("急停跳过必须说明原因: %q", steps[2].ErrorMsg)
	}
	if steps[3].Status != int(models.RunStatusSuccess) {
		t.Fatalf("all_done 收集步骤必须照常执行，实际 %s", models.RunStatus(steps[3].Status))
	}
}
