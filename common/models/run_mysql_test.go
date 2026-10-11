//go:build integration

package models

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/dbclient"
)

// 真实 MySQL 集成测试（阶段 1 的唯一"未验证项"就靠它关闭）。
//
// 为什么必须跑真库：本项目幂等的最后一道闸门是 **MySQL 唯一索引 1062**，
// 而 `IsDuplicateKey` 是按错误文本判定的（gorm v1.23 未开启 TranslateError）。
// 内存替身只能验证"我们期望的文本"，只有真库才能验证"MySQL 实际返回的文本"。
//
// 运行方式（不会自动跑，避免污染 CI/本地库）：
//
//	$env:CRONY_TEST_MYSQL_DSN='root:密码@tcp(127.0.0.1:3306)/crony_verify?charset=utf8mb4&parseTime=True&loc=Local'
//	go test -tags integration ./common/models/ -run TestMySQL -count=1 -v
//
// 需要事先建好空库（测试只建表、只删自己写入的行）：
//
//	CREATE DATABASE crony_verify DEFAULT CHARACTER SET utf8mb4;
//
// 建议一次建两个库以便并发包测试：crony_verify、crony_verify_node。
func requireMySQL(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 集成测试")
	}
	db, err := dbclient.Init(dsn, "error", 5, 10)
	if err != nil {
		t.Fatalf("连接 MySQL 失败（请检查 CRONY_TEST_MYSQL_DSN）: %v", err)
	}
	// AutoMigrate 必须在真实 MySQL 上验证：唯一索引是否真的建成，
	// 决定了"多节点抢同一槽位"能否被挡住。
	if err := db.AutoMigrate(&JobRun{}, &JobRunStep{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	// 校验唯一索引真实存在（gorm tag 写错时 AutoMigrate 会"成功"但没建索引）
	var count int64
	if err := db.Raw(`SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'uk_job_run_run_id'`,
		CronyJobRunTableName).Scan(&count).Error; err != nil {
		t.Fatalf("查询索引信息失败: %v", err)
	}
	if count == 0 {
		t.Fatalf("job_run 上没有 uk_job_run_run_id 唯一索引——幂等的最后一道闸门不存在")
	}
}

// cleanupRun 删除本用例写入的行（按 run_id 前缀，避免误删他人数据）。
func cleanupRun(t *testing.T, runID string) {
	t.Helper()
	db := dbclient.GetMysqlDB()
	if err := db.Exec("delete from "+CronyJobRunStepTableName+" where run_id = ?", runID).Error; err != nil {
		t.Logf("清理步骤失败: %v", err)
	}
	if err := db.Exec("delete from "+CronyJobRunTableName+" where run_id = ?", runID).Error; err != nil {
		t.Logf("清理执行记录失败: %v", err)
	}
}

func testRun(runID string, jobID int, slot int64) *JobRun {
	return &JobRun{
		RunID:          runID,
		JobID:          jobID,
		JobName:        "集成测试任务",
		Spec:           "*/5 * * * *",
		Command:        "echo hello",
		JobType:        1,
		NodeUUID:       "node-it-01",
		Hostname:       "it-host",
		IP:             "127.0.0.1",
		TriggerSource:  int(TriggerScheduler),
		Status:         int(RunStatusPending),
		Attempt:        1,
		ScheduledTime:  slot,
		StepCount:      1,
		PolicySnapshot: `{"concurrency":"skip"}`,
		Created:        time.Now().Unix(),
	}
}

// TestMySQLRunInsertFindUpdate 验证真实 MySQL 上的增/查/局部更新。
func TestMySQLRunInsertFindUpdate(t *testing.T) {
	requireMySQL(t)

	slot := time.Now().Truncate(time.Minute).Unix()
	runID := RunIDForSlot(900001, slot)
	cleanupRun(t, runID)
	defer cleanupRun(t, runID)

	run := testRun(runID, 900001, slot)
	if _, err := run.Insert(); err != nil {
		t.Fatalf("插入执行记录失败: %v", err)
	}
	if run.ID == 0 {
		t.Fatalf("插入后自增主键为 0（gorm 未回填 ID）")
	}

	if err := run.UpdateFields(map[string]interface{}{
		"status":      int(RunStatusSuccess),
		"exit_code":   0,
		"duration_ms": 12,
		"finished_at": time.Now().Unix(),
	}); err != nil {
		t.Fatalf("局部更新失败: %v", err)
	}

	got := &JobRun{RunID: runID}
	if err := got.FindByRunID(); err != nil {
		t.Fatalf("按 run_id 查询失败: %v", err)
	}
	if got.Status != int(RunStatusSuccess) {
		t.Fatalf("状态未更新: %d", got.Status)
	}
	if got.PolicySnapshot != `{"concurrency":"skip"}` {
		t.Fatalf("策略快照丢失: %q", got.PolicySnapshot)
	}
	if got.Created == 0 {
		t.Fatalf("created 字段未落库")
	}
}

// TestMySQLDuplicateKeyIsRecognized 幂等闸门的关键验证：
// 真库返回的 1062 文本必须被 IsDuplicateKey 认出来，否则冲突会被当成"执行失败"。
func TestMySQLDuplicateKeyIsRecognized(t *testing.T) {
	requireMySQL(t)

	slot := time.Now().Truncate(time.Minute).Unix()
	runID := RunIDForSlot(900002, slot)
	cleanupRun(t, runID)
	defer cleanupRun(t, runID)

	if _, err := testRun(runID, 900002, slot).Insert(); err != nil {
		t.Fatalf("首次插入应当成功: %v", err)
	}

	// 模拟另一个节点同时抢到同一槽位
	_, err := testRun(runID, 900002, slot).Insert()
	if err == nil {
		t.Fatalf("重复插入同一 run_id 竟然成功——唯一索引没有生效")
	}
	t.Logf("MySQL 实际返回的冲突错误: %v", err)
	if !IsDuplicateKey(err) {
		t.Fatalf("MySQL 的 1062 错误未被 IsDuplicateKey 识别，幂等兜底会失效: %v", err)
	}
	if IsRecordNotFound(err) {
		t.Fatalf("冲突错误不应被判定为 record not found")
	}
}

// TestMySQLRecordNotFoundIsDistinguishable 幂等预检必须能区分
// "没有历史记录"（可以执行）与"查询失败"（必须放弃执行）。
func TestMySQLRecordNotFoundIsDistinguishable(t *testing.T) {
	requireMySQL(t)

	missing := &JobRun{RunID: fmt.Sprintf("j900003-m%d-notexist", time.Now().UnixNano())}
	err := missing.FindByRunID()
	if err == nil {
		t.Fatalf("不存在的 run_id 查询竟然成功")
	}
	if !IsRecordNotFound(err) {
		t.Fatalf("不存在的 run_id 未被判定为 record not found: %v", err)
	}
	if IsDuplicateKey(err) {
		t.Fatalf("record not found 不应被判定为唯一索引冲突")
	}
}

// TestMySQLFindRecentRunBySlot 去重窗口（节点时钟漂移场景）。
func TestMySQLFindRecentRunBySlot(t *testing.T) {
	requireMySQL(t)

	slot := time.Now().Truncate(time.Minute).Unix()
	runID := RunIDForSlot(900004, slot)
	cleanupRun(t, runID)
	defer cleanupRun(t, runID)

	if _, err := testRun(runID, 900004, slot).Insert(); err != nil {
		t.Fatalf("插入失败: %v", err)
	}

	found, err := FindRecentRunBySlot(900004, slot, 60)
	if err != nil {
		t.Fatalf("窗口查询失败: %v", err)
	}
	if !found {
		t.Fatalf("窗口内应能查到刚写入的执行记录")
	}

	// 相邻槽位：+30 秒落在 60 秒窗口内
	if found, _ = FindRecentRunBySlot(900004, slot+30, 60); !found {
		t.Fatalf("+30s 应落在 60s 去重窗口内")
	}
	// 超出窗口：+5 分钟
	if found, _ = FindRecentRunBySlot(900004, slot+300, 60); found {
		t.Fatalf("+300s 不应落在 60s 去重窗口内")
	}
	// 不同任务互不影响
	if found, _ = FindRecentRunBySlot(900005, slot, 60); found {
		t.Fatalf("窗口查询串到了其它任务")
	}
}

// TestMySQLStepInsertAndOrder 步骤明细落库与排序（执行时间线）。
func TestMySQLStepInsertAndOrder(t *testing.T) {
	requireMySQL(t)

	slot := time.Now().Truncate(time.Minute).Unix()
	runID := RunIDForSlot(900006, slot)
	cleanupRun(t, runID)
	defer cleanupRun(t, runID)

	if _, err := testRun(runID, 900006, slot).Insert(); err != nil {
		t.Fatalf("插入执行记录失败: %v", err)
	}
	// 故意乱序写入，验证查询按 step_index 排序
	for _, idx := range []int{1, 0} {
		step := &JobRunStep{
			RunID:     runID,
			StepIndex: idx,
			Name:      fmt.Sprintf("step-%d", idx),
			Kind:      "shell",
			Condition: "on_success",
			Command:   "echo step",
			Status:    int(RunStatusSuccess),
			Output:    "utf8 输出：中文不截断",
			Created:   time.Now().Unix(),
		}
		if _, err := step.Insert(); err != nil {
			t.Fatalf("插入步骤失败: %v", err)
		}
	}

	steps := make([]JobRunStep, 0)
	if err := dbclient.GetMysqlDB().Table(CronyJobRunStepTableName).
		Where("run_id = ?", runID).Order("step_index asc").Find(&steps).Error; err != nil {
		t.Fatalf("查询步骤失败: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("步骤数不符: %d", len(steps))
	}
	if steps[0].StepIndex != 0 || steps[1].StepIndex != 1 {
		t.Fatalf("步骤未按 step_index 排序: %d, %d", steps[0].StepIndex, steps[1].StepIndex)
	}
	if steps[0].Output != "utf8 输出：中文不截断" {
		t.Fatalf("中文输出落库后被破坏: %q", steps[0].Output)
	}
}

// TestMySQLConcurrentInsertOnlyOneWins 真实的并发抢同槽位：
// 8 个协程同时插入同一个 run_id，必须**恰好一个成功**。
func TestMySQLConcurrentInsertOnlyOneWins(t *testing.T) {
	requireMySQL(t)

	slot := time.Now().Truncate(time.Minute).Unix()
	runID := RunIDForSlot(900007, slot)
	cleanupRun(t, runID)
	defer cleanupRun(t, runID)

	const workers = 8
	results := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			_, err := testRun(runID, 900007, slot).Insert()
			results <- err
		}()
	}
	close(start)

	okCount, dupCount, otherCount := 0, 0, 0
	for i := 0; i < workers; i++ {
		switch err := <-results; {
		case err == nil:
			okCount++
		case IsDuplicateKey(err):
			dupCount++
		default:
			otherCount++
			t.Logf("非预期错误: %v", err)
		}
	}
	if okCount != 1 {
		t.Fatalf("并发抢同槽位应恰好 1 个成功，实际 %d 个（幂等失效）", okCount)
	}
	if dupCount != workers-1 {
		t.Fatalf("其余 %d 个都应是唯一索引冲突，实际 %d 个", workers-1, dupCount)
	}
	if otherCount != 0 {
		t.Fatalf("出现了 %d 个非冲突错误", otherCount)
	}
}
