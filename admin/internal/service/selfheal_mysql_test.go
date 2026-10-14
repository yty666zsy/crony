//go:build integration

package service

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/selfheal"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 阶段 4-C 真实 MySQL 集成测试：自愈的**落库口径**必须用真库验。
//
// 这一层要覆盖的正是"假源测不到"的部分：
//   - FindFailedRuns 的 status in (failed,timeout) + finished_at 窗口 + limit；
//   - HealsForSource / HealsForJob 的幂等与次数统计；
//   - FollowRun 的"source run 之后同一个 job 的第一条 run"（按自增 id，不是时间）；
//   - saveHealRow 的"插入/更新"两分支与 memory_ids / new_run_id / finished_at 回写；
//   - UpdateTimeout 只改 timeout 一列（不整行覆盖）；
//   - UpsertAgentMemory 的去重（dedupe_key）与 recall 端"只认得出动作的才计数"；
//   - searchHeals 的筛选与分页。
//
// 只 stub 两个**对外效应**：Retry（投递）与 WritesEnabled（能力面判据）——
// 它们不产生 SQL 事实，且 etcd 在本环境不可用（投递路径由验收用例另行覆盖）。
func TestIntegrationSelfHealAgainstRealMySQL(t *testing.T) {
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 的自愈测试")
	}
	if _, err := dbclient.Init(dsn, "silent", 1, 5); err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db := dbclient.GetMysqlDB()
	if err := db.AutoMigrate(&models.Job{}, &models.JobRun{}, &models.JobRunStep{},
		&models.JobHeal{}, &models.AgentMemory{}, &models.Node{}); err != nil {
		t.Fatalf("建表失败（测试库需要 CREATE 权限）: %v", err)
	}

	tag := fmt.Sprintf("it-heal-%d", time.Now().UnixNano())
	jobID := 910040
	jobIDTimeout := 910041
	nodeUUID := tag + "-node"
	runID := tag + "-r1"
	node := &models.Node{
		PID: "1", IP: "127.0.0.1", Hostname: "it-heal-host", UUID: nodeUUID,
		Version: "v9.9.9", Status: models.NodeConnSuccess, UpTime: time.Now().Unix(),
	}
	if err := db.Table(models.CronyNodeTableName).Create(node).Error; err != nil {
		t.Fatalf("写节点失败: %v", err)
	}
	jobs := []*models.Job{
		{ID: jobID, Name: tag + "-job", Command: "echo hi", Type: models.JobTypeCmd,
			Spec: "0 0 1 1 * *", Status: models.JobStatusAssigned, Timeout: 60, SelfHeal: 1, RunOn: nodeUUID},
		{ID: jobIDTimeout, Name: tag + "-job-timeout", Command: "sleep 100", Type: models.JobTypeCmd,
			Spec: "0 0 1 1 * *", Status: models.JobStatusAssigned, Timeout: 60, SelfHeal: 1, RunOn: nodeUUID},
	}
	for _, j := range jobs {
		if err := db.Table(models.CronyJobTableName).Create(j).Error; err != nil {
			t.Fatalf("写任务失败: %v", err)
		}
	}
	defer func() {
		db.Exec("DELETE FROM "+models.CronyJobHealTableName+" WHERE job_id IN (?,?)", jobID, jobIDTimeout)
		db.Exec("DELETE FROM "+models.CronyJobRunTableName+" WHERE run_id LIKE ?", tag+"%")
		db.Exec("DELETE FROM "+models.CronyJobTableName+" WHERE id IN (?,?)", jobID, jobIDTimeout)
		db.Exec("DELETE FROM "+models.CronyAgentMemoryTableName+" WHERE run_id LIKE ?", tag+"%")
		db.Exec("DELETE FROM "+models.CronyNodeTableName+" WHERE uuid LIKE ?", tag+"%")
	}()

	now := time.Now()
	createRun := func(runID string, jobID int, status models.RunStatus, exit int, errMsg string) *models.JobRun {
		r := &models.JobRun{
			RunID: runID, JobID: jobID, Status: int(status), ExitCode: exit, ErrorMsg: errMsg,
			NodeUUID: nodeUUID, ScheduledTime: now.Unix() - 60, StartedAt: now.Unix() - 50,
			FinishedAt: now.Unix() - 10, Created: now.Unix() - 60,
		}
		if err := db.Table(models.CronyJobRunTableName).Create(r).Error; err != nil {
			t.Fatalf("写执行记录失败: %v", err)
		}
		return r
	}
	createRun(runID, jobID, models.RunStatusFailed, 1, "exit status 1")
	timeoutRunID := tag + "-r-t1"
	createRun(timeoutRunID, jobIDTimeout, models.RunStatusTimeout, 0, "任务执行超时")

	// 装配：真实依赖 + 只 stub 对外效应。
	policy := selfheal.DefaultPolicy()
	policy.Enabled = true
	policy.CooldownSeconds = 0 // 集成测试要连做多轮，冷却由策略单测覆盖
	svc := NewDefaultSelfHealService(policy)
	retryCalls := 0
	retried := map[int]int{}
	var lastRetry tools.RetryRequest
	svc.deps.WritesEnabled = func() bool { return true }
	svc.deps.Retry = func(ctx context.Context, req tools.RetryRequest) (tools.Result, error) {
		retryCalls++
		retried[req.JobID]++
		lastRetry = req
		return tools.Result{OK: true, Summary: "stub 投递成功"}, nil
	}
	// 经验召回走真实 sqlite/MySQL 通路（GormMemoryStore）。
	SetMemoryService(NewMemoryService(NewGormMemoryStore()))
	defer SetMemoryService(nil)

	// ---------- 1) 第一轮：分类 + 决策 + 闸门 + 投递（真库落 job_heal） ----------
	rep, err := svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("第一轮扫描失败: %v", err)
	}
	var heals []models.JobHeal
	if err := db.Table(models.CronyJobHealTableName).Where("job_id IN (?,?)", jobID, jobIDTimeout).
		Order("id asc").Find(&heals).Error; err != nil {
		t.Fatalf("读自愈记录失败: %v", err)
	}
	if rep.Executed != 2 || retryCalls != 2 {
		for _, h := range heals {
			t.Logf("heal=%s job=%d status=%s action=%s attempt=%d policy=%s note=%s",
				h.HealID, h.JobID, h.Status, h.Action, h.Attempt, h.PolicyJSON, h.OutcomeNote)
		}
		t.Fatalf("期望两条失败各处置一次，实际 executed=%d retry=%d rep=%+v", rep.Executed, retryCalls, rep)
	}
	// 两个任务各被投递一次（顺序按 job_run.id 倒序，断言集合而不是顺序）。
	if retried[jobID] != 1 || retried[jobIDTimeout] != 1 {
		t.Fatalf("期望两个任务各投递一次，实际 %v（last=%d/%s）", retried, lastRetry.JobID, lastRetry.Reason)
	}
	if lastRetry.RunID == "" || lastRetry.Reason == "" {
		t.Fatalf("投递请求必须带来源执行与理由（审计与幂等都要），实际 %+v", lastRetry)
	}
	if len(heals) != 2 {
		t.Fatalf("期望 2 条自愈记录，实际 %d", len(heals))
	}
	for _, h := range heals {
		if h.Status != models.HealStatusExecuted {
			t.Fatalf("未观测前必须是 executed，实际 %s（heal=%s）", h.Status, h.HealID)
		}
		if h.Deadline == 0 || h.FinishedAt != 0 {
			t.Fatalf("executed 必须带观测截止且不写结束时间，实际 deadline=%d finished=%d", h.Deadline, h.FinishedAt)
		}
		if h.SourceRunID == "" || h.Symptom == "" || h.Reason == "" {
			t.Fatalf("自愈记录必须自含症状/理由/来源执行: %+v", h)
		}
	}
	// 超时那条必须是"调大超时后重跑"，且**真库里 timeout 已被改**。
	var timeoutHeal models.JobHeal
	for _, h := range heals {
		if h.JobID == jobIDTimeout {
			timeoutHeal = h
		}
	}
	if timeoutHeal.Action != models.HealActionRetryExtendTimeout {
		t.Fatalf("超时应选调大超时重跑，实际 %s", timeoutHeal.Action)
	}
	args, ok := selfheal.ParseActionJSON(timeoutHeal.ActionJSON)
	if !ok || args.FromTimeout != 60 || args.ToTimeout != 120 {
		t.Fatalf("动作参数应为 60→120，实际 %q", timeoutHeal.ActionJSON)
	}
	var timeoutNow int64
	db.Table(models.CronyJobTableName).Select("timeout").Where("id = ?", jobIDTimeout).Scan(&timeoutNow)
	if timeoutNow != 120 {
		t.Fatalf("真库里 timeout 应已被改为 120，实际 %d", timeoutNow)
	}
	// 观测前不得沉淀经验。
	var memCount int64
	db.Table(models.CronyAgentMemoryTableName).Where("run_id LIKE ?", tag+"%").Count(&memCount)
	if memCount != 0 {
		t.Fatalf("观测出结果前不得沉淀经验，实际 %d 条", memCount)
	}

	// ---------- 2) 第二轮：出现后续成功执行 → 观测为成功 + 沉淀经验 ----------
	createRun(tag+"-r2", jobID, models.RunStatusSuccess, 0, "")
	createRun(tag+"-r-t2", jobIDTimeout, models.RunStatusSuccess, 0, "")
	rep, err = svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("第二轮扫描失败: %v", err)
	}
	if rep.Succeeded != 2 || rep.Observed != 2 {
		t.Fatalf("期望观测到 2 次成功，实际 rep=%+v", rep)
	}
	var cmdHeal models.JobHeal
	for _, h := range heals {
		if h.JobID == jobID {
			cmdHeal = h
		}
	}
	var fresh models.JobHeal
	if err := db.Table(models.CronyJobHealTableName).Where("heal_id = ?", cmdHeal.HealID).First(&fresh).Error; err != nil {
		t.Fatalf("重读自愈记录失败: %v", err)
	}
	if fresh.Status != models.HealStatusSucceeded || fresh.NewRunID != tag+"-r2" || fresh.FinishedAt == 0 {
		t.Fatalf("观测结论落库不符: status=%s new_run=%s finished=%d", fresh.Status, fresh.NewRunID, fresh.FinishedAt)
	}
	if fresh.MemoryIDs == "" || fresh.MemoryIDs == "null" {
		t.Fatalf("经验条目号必须回写，实际 %q", fresh.MemoryIDs)
	}
	var mem models.AgentMemory
	if err := db.Table(models.CronyAgentMemoryTableName).Where("run_id = ?", runID).First(&mem).Error; err != nil {
		t.Fatalf("应沉淀自愈经验（run_id=%s）: %v", runID, err)
	}
	if mem.AgentKind != AgentKindSelfHeal || mem.Tool != ToolNameSelfHeal ||
		mem.Outcome != models.MemoryOutcomeSuccess || mem.Risk != "write" {
		t.Fatalf("经验归属/结果不符: %+v", mem)
	}
	action, ok := selfheal.ActionFromMemoryText(mem.ActionText)
	if !ok || action != models.HealActionRetrySameNode {
		t.Fatalf("动作文本必须可反解，实际 %q → %q ok=%v", mem.ActionText, action, ok)
	}
	if mem.DedupeKey == "" || mem.MemoryID == "" {
		t.Fatalf("经验必须有去重键与条目号: %+v", mem)
	}

	// ---------- 3) 第三轮：幂等跳过（不新增任何行） ----------
	rep, err = svc.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("第三轮扫描失败: %v", err)
	}
	if rep.Skipped != 2 || rep.Executed != 0 || retryCalls != 2 {
		t.Fatalf("已处置的失败不得重复动手，实际 rep=%+v retry=%d", rep, retryCalls)
	}
	var healCount int64
	db.Table(models.CronyJobHealTableName).Where("job_id IN (?,?)", jobID, jobIDTimeout).Count(&healCount)
	if healCount != 2 {
		t.Fatalf("幂等轮不得新增自愈记录，实际 %d 条", healCount)
	}

	// ---------- 4) 次数上限：窗口内已有 3 次真实尝试 → 转人工且不动手 ----------
	capPolicy := selfheal.DefaultPolicy()
	capPolicy.Enabled = true
	capPolicy.CooldownSeconds = 0
	capPolicy.MaxAttemptsPerJobWindow = 3
	capSvc := NewDefaultSelfHealService(capPolicy)
	capRetry := 0
	capSvc.deps.WritesEnabled = func() bool { return true }
	capSvc.deps.Retry = func(ctx context.Context, req tools.RetryRequest) (tools.Result, error) {
		capRetry++
		return tools.Result{OK: true, Summary: "stub"}, nil
	}
	// 造 3 条真实尝试（failed），再加一条新的失败执行。
	base := time.Now().Unix()
	for i := 0; i < 3; i++ {
		if err := db.Table(models.CronyJobHealTableName).Create(&models.JobHeal{
			HealID: fmt.Sprintf("heal-%s-cap-%d", tag, i), SourceRunID: tag + "-cap-src-" + fmt.Sprint(i),
			JobID: jobID, JobName: tag + "-job", Status: models.HealStatusFailed,
			Action: models.HealActionRetrySameNode, Created: base - int64(i*5), Updated: base,
		}).Error; err != nil {
			t.Fatalf("写历史自愈失败记录失败: %v", err)
		}
	}
	createRun(tag+"-r3", jobID, models.RunStatusFailed, 1, "exit status 1")
	rep, err = capSvc.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("次数上限轮扫描失败: %v", err)
	}
	if capRetry != 0 {
		t.Fatalf("次数用尽不得动手，实际重跑 %d 次", capRetry)
	}
	if rep.Escalated != 1 || rep.Denied != 0 {
		t.Fatalf("次数用尽应转人工，实际 rep=%+v", rep)
	}
	var esc models.JobHeal
	if err := db.Table(models.CronyJobHealTableName).
		Where("source_run_id = ?", tag+"-r3").First(&esc).Error; err != nil {
		t.Fatalf("转人工必须留痕: %v", err)
	}
	if esc.Status != models.HealStatusEscalated || esc.Attempt != 5 {
		// 窗口内真实尝试 = 本步第 1 轮那条已成功执行的记录（1）+ 上面 3 条合成失败（3）= 4，
		// 本次决策是"含本次的第 5 次" → 触发上限并转人工。
		// 这里同时验证了"成功过的尝试也算尝试"（否则次数会算少，同一任务会被反复重跑）。
		t.Fatalf("期望 escalated/attempt=5，实际 %s/%d", esc.Status, esc.Attempt)
	}

	// ---------- 5) 查询口径：search/find/by_run ----------
	searcher := &request.ReqJobHealSearch{SourceRunID: runID}
	searcher.Page = 1
	searcher.PageSize = 10
	rows, total, err := svc.Search(searcher)
	if err != nil {
		t.Fatalf("按执行单号查询自愈记录失败: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].HealID != cmdHeal.HealID {
		t.Fatalf("按 source_run_id 查询口径不符: total=%d rows=%d", total, len(rows))
	}
	got, err := svc.Find(cmdHeal.HealID)
	if err != nil || got.HealID != cmdHeal.HealID {
		t.Fatalf("按单号查详情失败: %v", err)
	}
	if _, err := svc.Find("heal-does-not-exist-" + tag); err == nil {
		t.Fatal("不存在的自愈单号必须返回 ErrHealNotFound")
	}
	byRun, err := svc.HealBySourceRun(runID)
	if err != nil || len(byRun) != 1 {
		t.Fatalf("by_run 查询失败: %v len=%d", err, len(byRun))
	}
}
