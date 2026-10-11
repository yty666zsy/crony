//go:build integration

package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agent"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/tools"
	"gorm.io/gorm"
)

// 真实 MySQL 集成测试（默认跳过）
//
// 为什么必须有这一层：单元测试用的是内存结构，无法证明
//   - agent_session.trace_json 的 longtext 真的装得下完整轨迹；
//   - uk_agent_session_id 唯一索引真的生效（重复写入必须被拒）；
//   - job_run/job_run_step/job_log 的字段类型能被 agentruntime 的检索条件命中
//     （时间条件是 Unix 秒、LIKE 大小写、run_id 关联）。
//
// 运行方式（**使用只读/临时库，不要指向生产库**，与阶段 1 的
// common/models/run_mysql_test.go 保持同一约定）：
//
//	$env:CRONY_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/crony_verify?charset=utf8mb4&parseTime=True&loc=Local'
//	go test -tags integration ./common/pkg/agentruntime/ -run TestIntegration -count=1 -v
//
// 未设置该环境变量时全部跳过（CI 与本地默认不依赖 MySQL）。
// 本测试只写入自己创建的行（名称带唯一前缀），结束时按主键删除，不执行任何 drop/truncate。

const integrationDSNEnv = "CRONY_TEST_MYSQL_DSN"

func integrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(integrationDSNEnv))
	if dsn == "" {
		t.Skipf("未设置 %s，跳过真实 MySQL 集成测试", integrationDSNEnv)
	}
	db, err := dbclient.Init(dsn, "silent", 1, 5)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if db == nil {
		t.Fatal("dbclient.Init 返回了空连接")
	}
	if err := db.AutoMigrate(
		&models.Job{}, &models.JobRun{}, &models.JobRunStep{}, &models.JobLog{},
		&models.AgentSession{}, &models.AgentStep{},
	); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	for _, table := range []string{models.CronyAgentSessionTableName, models.CronyAgentStepTableName} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("表 %s 未被创建", table)
		}
	}
	return db
}

// uniqueTag 生成本次测试的唯一前缀，便于精确清理自己写的行。
func uniqueTag(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestIntegrationAgentSessionStore 覆盖持久化层的关键不变式。
func TestIntegrationAgentSessionStore(t *testing.T) {
	db := integrationDB(t)
	tag := uniqueTag("itstore")
	runID := tag + "-run"

	// 1) longtext 必须装得下完整轨迹（真实模型轨迹常见数十 KB）
	bigTrace := `{"version":1,"session_id":"` + tag + `","steps":[{"seq":1,"kind":"model","content":"` +
		strings.Repeat("长文本观测 ", 12000) + `"}]}`
	session := &models.AgentSession{
		SessionID:     tag,
		Kind:          models.AgentKindDiagnose,
		JobID:         900002,
		RunID:         runID,
		NodeUUID:      "it-node",
		Goal:          "集成测试：验证会话持久化",
		Status:        models.AgentStatusCompleted,
		StopReason:    string(agent.StopCompleted),
		Provider:      "offline",
		Model:         "offline-model",
		PromptVersion: agent.PromptVersionDiagnoseV1,
		SystemPrompt:  agent.PromptDiagnoseV1,
		ModelCalls:    2,
		ToolCalls:     3,
		TotalTokens:   1234,
		DurationMs:    4321,
		WarnCount:     1,
		EvidenceCount: 2,
		ContextJSON:   `["任务：it（id=900002）","运行 run 失败"]`,
		WarningsJSON:  `["证据不足"]`,
		TraceJSON:     bigTrace,
		ResultJSON:    `{"summary":"集成测试结论","category":"unknown","cause":"c","confidence":0.5,"evidence":[],"assumptions":[],"ruled_out":[],"suggested_actions":[],"evidence_sufficient":false,"next_data_to_collect":["更多日志"]}`,
		Created:       time.Now().Unix(),
	}
	id, err := session.Insert()
	if err != nil {
		t.Fatalf("插入会话失败: %v", err)
	}
	if id <= 0 {
		t.Fatalf("插入会话应返回自增主键，实际 %d", id)
	}
	defer func() {
		_ = db.Exec("delete from "+models.CronyAgentSessionTableName+" where session_id = ?", tag).Error
	}()

	got := &models.AgentSession{SessionID: tag}
	if err := got.FindBySessionID(); err != nil {
		t.Fatalf("按 session_id 查询失败: %v", err)
	}
	if got.TraceJSON != bigTrace {
		t.Fatalf("轨迹 round-trip 不一致（longtext 容量或字符集问题）：len=%d want=%d", len(got.TraceJSON), len(bigTrace))
	}
	if got.RunID != runID || got.Kind != models.AgentKindDiagnose || got.TotalTokens != 1234 {
		t.Fatalf("字段 round-trip 不一致: %+v", got)
	}

	// 2) 唯一索引必须真的生效：同一 session_id 重复写入要被数据库挡下
	dup := *session
	dup.ID = 0
	if _, err := dup.Insert(); err == nil {
		t.Fatal("重复 session_id 必须被唯一索引拒绝")
	} else if !models.IsDuplicateKey(err) {
		t.Fatalf("重复写入的错误应可被 IsDuplicateKey 识别，实际 %v", err)
	}

	// 3) 不存在的会话必须是"未找到"而不是其他错误
	missing := &models.AgentSession{SessionID: tag + "-nope"}
	if err := missing.FindBySessionID(); err == nil || !models.IsRecordNotFound(err) {
		t.Fatalf("不存在的会话应返回 not found，实际 %v", err)
	}

	// 4) 步骤按 seq 有序返回
	for i := 1; i <= 3; i++ {
		step := &models.AgentStep{
			SessionID: tag, Seq: i, Kind: string(agent.StepModel),
			Provider: "offline", Model: "m", Content: fmt.Sprintf("step-%d", i),
			ToolCallsJSON: `[{"ID":"call_1","Name":"log_search","Arguments":"{\"keyword\":\"x\"}"}]`,
			Created:       time.Now().Unix(),
		}
		if i == 2 {
			step.Kind = string(agent.StepTool)
			step.ToolName = "log_search"
			step.ToolArgsJSON = `{"keyword":"x","job_id":900002}`
			step.Observation = "命中 1 条"
			step.EvidenceJSON = `[{"source":"job_log","ref":"job_log:1","quote":"connection refused"}]`
		}
		if _, err := step.Insert(); err != nil {
			t.Fatalf("插入步骤 %d 失败: %v", i, err)
		}
	}
	defer func() { _ = db.Exec("delete from "+models.CronyAgentStepTableName+" where session_id = ?", tag).Error }()

	steps, err := models.ListAgentSteps(tag)
	if err != nil {
		t.Fatalf("查询步骤失败: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("步骤数应为 3，实际 %d", len(steps))
	}
	for i, st := range steps {
		if st.Seq != i+1 {
			t.Fatalf("步骤必须按 seq 升序，实际 %d 位于第 %d 位", st.Seq, i)
		}
	}

	// 5) 列表查询与过滤（契约 §1 的查询条件）
	list, total, err := models.ListAgentSessions(models.AgentSessionQuery{RunID: runID, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("按 run_id 查询失败: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].SessionID != tag {
		t.Fatalf("按 run_id 过滤结果不符: total=%d list=%d", total, len(list))
	}
	if _, totalKind, err := models.ListAgentSessions(models.AgentSessionQuery{Kind: models.AgentKindNLJob, RunID: runID, Page: 1, PageSize: 10}); err != nil || totalKind != 0 {
		t.Fatalf("kind 过滤应生效（期望 0 条），实际 %d，err=%v", totalKind, err)
	}
	// 分页参数归一化：page_size 超上限必须被收敛，而不是把全表拉出来
	_, _, err = models.ListAgentSessions(models.AgentSessionQuery{RunID: runID, Page: 0, PageSize: 100000})
	if err != nil {
		t.Fatalf("分页归一化不应报错: %v", err)
	}

	// 6) 取最近一次诊断会话
	latest, err := models.FindLatestAgentSession(runID, models.AgentKindDiagnose)
	if err != nil {
		t.Fatalf("FindLatestAgentSession 失败: %v", err)
	}
	if latest == nil || latest.SessionID != tag {
		t.Fatalf("最新诊断会话不符: %+v", latest)
	}
}

// TestIntegrationDiagnosisAgainstRealMySQL 是全阶段最有价值的一条验收：
// 用真实 MySQL 里的"失败执行"作为证据，跑完 Agent 诊断并落库，再回放。
func TestIntegrationDiagnosisAgainstRealMySQL(t *testing.T) {
	db := integrationDB(t)
	tag := uniqueTag("itdiag")
	now := time.Now()

	// 1) 造一次真实的失败执行（job + job_run + steps + job_log）
	job := &models.Job{
		Name: "integration-fail-" + tag, Command: "/usr/local/bin/backup.sh --full",
		Type: models.JobTypeCmd, Status: models.JobStatusAssigned, RunOn: "it-node",
		Spec: "0 0 2 * * *", NotifyType: 0, Created: now.Unix(),
	}
	jobID, err := job.Insert()
	if err != nil {
		t.Fatalf("插入任务失败: %v", err)
	}
	defer func() { _ = db.Exec("delete from "+models.CronyJobTableName+" where id = ?", jobID).Error }()

	slot := now.Add(-10 * time.Minute).Unix()
	runID := models.RunIDForSlot(jobID, slot)
	run := &models.JobRun{
		RunID: runID, JobID: jobID, JobName: job.Name, Spec: job.Spec, Command: job.Command,
		JobType: int(models.JobTypeCmd), NodeUUID: "it-node", Hostname: "it-host", IP: "10.0.0.9",
		Status: int(models.RunStatusFailed), Attempt: 1, ScheduledTime: slot,
		StartedAt: now.Add(-10 * time.Minute).Unix(), FinishedAt: now.Add(-9 * time.Minute).Unix(),
		DurationMs: 60000, ExitCode: 1, StepCount: 2, SuccessSteps: 1, FailedSteps: 1,
		ErrorMsg: "步骤 2 退出码 1：mysqldump: Got error: 1045 Access denied for user 'backup'@'10.0.0.9'",
		Output:   "备份失败", Created: now.Unix(),
	}
	if _, err := run.Insert(); err != nil {
		t.Fatalf("插入执行记录失败: %v", err)
	}
	defer func() { _ = db.Exec("delete from "+models.CronyJobRunTableName+" where run_id = ?", runID).Error }()

	steps := []*models.JobRunStep{
		{RunID: runID, StepIndex: 0, Name: "准备目录", Kind: "shell", Condition: "on_success",
			Command: "mkdir -p /backup", Status: int(models.RunStatusSuccess), ExitCode: 0,
			StartedAt: now.Add(-10 * time.Minute).Unix(), FinishedAt: now.Add(-10 * time.Minute).Unix(),
			DurationMs: 1000, Output: "ok", Created: now.Add(-10 * time.Minute).Unix()},
		{RunID: runID, StepIndex: 1, Name: "导出数据库", Kind: "shell", Condition: "on_success",
			Command: "mysqldump -u backup db", Status: int(models.RunStatusFailed), ExitCode: 1,
			StartedAt: now.Add(-10 * time.Minute).Unix(), FinishedAt: now.Add(-9 * time.Minute).Unix(),
			DurationMs: 59000, Output: "mysqldump: Got error: 1045 Access denied for user 'backup'@'10.0.0.9'",
			ErrorMsg: "exit status 1", Created: now.Add(-9 * time.Minute).Unix()},
	}
	for _, s := range steps {
		if _, err := s.Insert(); err != nil {
			t.Fatalf("插入步骤失败: %v", err)
		}
	}
	defer func() { _ = db.Exec("delete from "+models.CronyJobRunStepTableName+" where run_id = ?", runID).Error }()

	logRow := &models.JobLog{
		Name: job.Name, JobId: jobID, Command: job.Command, IP: "10.0.0.9", Hostname: "it-host",
		NodeUUID: "it-node", Success: false,
		Output: "mysqldump: Got error: 1045 Access denied for user 'backup'@'10.0.0.9'",
		Spec:   job.Spec, StartTime: now.Add(-9 * time.Minute).Unix(), EndTime: now.Unix(),
	}
	logID, err := logRow.Insert()
	if err != nil {
		t.Fatalf("插入日志失败: %v", err)
	}
	defer func() { _ = db.Exec("delete from "+models.CronyJobLogTableName+" where id = ?", logID).Error }()

	// 2) 装配 Agent 运行时（离线 provider：确定性，不依赖外部模型）
	logRef := fmt.Sprintf("job_log:%d", logID)
	stepRef := fmt.Sprintf("job_run_step:%s#1", runID)
	response := fmt.Sprintf(`{"summary":"数据库账号权限不足导致备份失败",`+
		`"category":"permission","cause":"备份账号 backup 缺少 mysqldump 所需权限（1045 Access denied）",`+
		`"confidence":80,`+
		`"evidence":[{"ref":%q,"quote":"Access denied for user","why":"直接给出数据库拒绝的原文"},`+
		`{"ref":%q,"quote":"exit status 1","why":"说明失败发生在导出步骤而不是准备步骤"}],`+
		`"assumptions":["目标库账号策略近期未变更"],`+
		`"ruled_out":[{"hypothesis":"磁盘空间不足","reason":"准备步骤成功且报错信息是权限类"}],`+
		`"suggested_actions":[{"title":"补授权限","detail":"为 backup 账号授予 SELECT/LOCK TABLES 等权限","risk":"medium","rollback":"回收授权"}],`+
		`"evidence_sufficient":true,"next_data_to_collect":[]}`, logRef, stepRef)

	rt, err := New(Options{
		Config:   offlineConfig(fmt.Sprintf(`{"name":"log_search","arguments":{"keyword":"Access denied","job_id":%d}}`, jobID), response),
		Policy:   tools.DefaultPolicy(),
		Sources:  tools.Sources{Logs: &DBLogSource{}},
		NodeUUID: "it-node",
	})
	if err != nil {
		t.Fatalf("装配运行时失败: %v", err)
	}

	// 3) 先验证材料装配：诊断上下文必须来自真实表
	rc, err := LoadRunContext(runID)
	if err != nil {
		t.Fatalf("装配运行上下文失败: %v", err)
	}
	if rc.JobID != jobID || rc.RunID != runID {
		t.Fatalf("上下文任务/运行标识错误: %+v", rc)
	}
	if !strings.Contains(strings.Join(rc.StepSummaries, " "), "导出数据库") {
		t.Fatalf("步骤摘要必须来自真实 job_run_step（含失败步骤名），实际 %v", rc.StepSummaries)
	}
	if rc.Command == "" || rc.Status == "" {
		t.Fatalf("运行上下文必须带上当时的命令与状态: %+v", rc)
	}

	// 4) 跑诊断
	res, err := rt.RunDiagnosis(context.Background(), DiagnosisRequest{RunID: runID})
	if err != nil {
		t.Fatalf("诊断失败: %v", err)
	}
	if res == nil || res.Diagnosis.Summary == "" {
		t.Fatalf("诊断结果为空: %+v", res)
	}
	if res.PersistError != "" {
		t.Fatalf("诊断会话必须落库成功，实际 persist_error=%s", res.PersistError)
	}
	if !res.Diagnosis.AdvisoryOnly {
		t.Fatal("诊断结果必须标记为仅建议")
	}
	if len(res.Diagnosis.Evidence) == 0 {
		t.Fatal("结论必须带证据链")
	}
	if refs := res.Diagnosis.UnverifiedRefs; len(refs) > 0 {
		// 证据定位来自工具观测（DB 检索），不应被判为幻觉
		t.Fatalf("真实 MySQL 证据不应被判为未观测引用: %v", refs)
	}

	// 5) 落库校验：会话 + 步骤都能查到，且轨迹可解析
	session := &models.AgentSession{SessionID: res.SessionID}
	if err := session.FindBySessionID(); err != nil {
		t.Fatalf("查询落库会话失败: %v", err)
	}
	if session.Kind != models.AgentKindDiagnose || session.RunID != runID {
		t.Fatalf("落库会话字段错误: kind=%s run_id=%s", session.Kind, session.RunID)
	}
	if session.EvidenceCount == 0 {
		t.Fatal("落库会话必须记录证据条数")
	}
	var trace agent.Trace
	if err := json.Unmarshal([]byte(session.TraceJSON), &trace); err != nil {
		t.Fatalf("落库轨迹不可解析: %v", err)
	}
	if len(trace.Steps) == 0 {
		t.Fatal("轨迹必须包含步骤")
	}
	dbSteps, err := models.ListAgentSteps(res.SessionID)
	if err != nil {
		t.Fatalf("查询落库步骤失败: %v", err)
	}
	var toolStep *models.AgentStep
	for _, st := range dbSteps {
		if st.Kind == string(agent.StepTool) && st.ToolName == "log_search" {
			toolStep = st
		}
	}
	if toolStep == nil {
		t.Fatalf("轨迹里必须留有 log_search 工具步骤，实际 %d 个步骤", len(dbSteps))
	}
	if !strings.Contains(toolStep.Observation, logRef) && !strings.Contains(toolStep.Observation, "命中") {
		t.Fatalf("工具观测应体现真实命中（含 %s），实际 %q", logRef, toolStep.Observation)
	}
	defer func() {
		_ = db.Exec("delete from "+models.CronyAgentSessionTableName+" where session_id = ?", res.SessionID).Error
		_ = db.Exec("delete from "+models.CronyAgentStepTableName+" where session_id = ?", res.SessionID).Error
	}()

	// 6) 回放：不加 force 时必须直接返回库里的结论，不再花 token
	replay, err := rt.RunDiagnosis(context.Background(), DiagnosisRequest{RunID: runID})
	if err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	if !replay.Replayed || replay.SessionID != res.SessionID {
		t.Fatalf("应回放同一会话（replayed=%v session=%s）", replay.Replayed, replay.SessionID)
	}
	if replay.Diagnosis.Summary != res.Diagnosis.Summary {
		t.Fatalf("回放结论不一致: %q vs %q", replay.Diagnosis.Summary, res.Diagnosis.Summary)
	}

	// 7) 不存在的运行记录必须是可识别的业务错误
	if _, err := rt.RunDiagnosis(context.Background(), DiagnosisRequest{RunID: tag + "-ghost"}); err == nil {
		t.Fatal("不存在的 run_id 必须报错")
	} else if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误信息应说明运行记录不存在，实际 %v", err)
	}
}
