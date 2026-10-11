package agentmemory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
)

// 这一组用例守的是"沉淀的是事实、不是模型的自述"这条底线：
// 结果分类必须与审计事实一一对应，且失败/拦截**绝不能**被算成执行失败。

func TestOutcomeForAuditDecisionMapping(t *testing.T) {
	cases := []struct {
		decision string
		ok       bool
		want     string
	}{
		{models.AuditDecisionAllowed, true, models.MemoryOutcomeSuccess},
		{models.AuditDecisionAllowed, false, models.MemoryOutcomeFailed},
		{models.AuditDecisionDenied, false, models.MemoryOutcomeDenied},
		{models.AuditDecisionDenied, true, models.MemoryOutcomeDenied}, // 被拒行不该有 OK=true，但即使有也不能算成功
		{models.AuditDecisionApprovalRequired, false, models.MemoryOutcomeBlocked},
		{models.AuditDecisionApprovalExecuted, true, models.MemoryOutcomeSuccess},
		{models.AuditDecisionApprovalExecuted, false, models.MemoryOutcomeFailed},
	}
	for _, c := range cases {
		if got := OutcomeForAuditDecision(c.decision, c.ok); got != c.want {
			t.Errorf("%s/ok=%v → %s，期望 %s", c.decision, c.ok, got, c.want)
		}
	}
}

func TestExtractToolMemoriesFromAudits(t *testing.T) {
	session := &models.AgentSession{
		SessionID: "diagnose:11-run1",
		Kind:      models.AgentKindDiagnose,
		JobID:     11,
		RunID:     "run1",
		Goal:      "诊断任务 11（备份任务）本次失败的原因",
		Status:    models.AgentStatusCompleted,
	}
	audits := []models.AgentToolAudit{
		{
			SessionID: "diagnose:11-run1", AgentKind: models.AgentKindDiagnose, Tool: "node_status",
			Risk: "read", ArgsJSON: `{"node_uuid":"node-it-01","api_key":"sk-abcdefghijklmnop"}`,
			Decision: models.AuditDecisionAllowed, OK: true, OutputBytes: 512, EvidenceCount: 2,
			TargetNode: "node-it-01", CreatedMS: 2000,
		},
		{
			SessionID: "diagnose:11-run1", AgentKind: models.AgentKindDiagnose, Tool: "job_retry",
			Risk: "write", ArgsJSON: `{"job_id":11}`,
			Decision: models.AuditDecisionApprovalRequired, OK: false, Reason: "写动作需要人工审批",
			TargetNode: "node-it-01", ApprovalID: "ap-1", CreatedMS: 3000,
		},
		{
			SessionID: "diagnose:11-run1", AgentKind: models.AgentKindDiagnose, Tool: "shell_readonly",
			Risk: "read", ArgsJSON: `{"command":"df -h"}`,
			Decision: models.AuditDecisionDenied, OK: false, Reason: "单会话工具调用上限（12）已用满",
			CreatedMS: 1000,
		},
	}
	obs := Extract(session, audits)
	if len(obs) != 3 {
		t.Fatalf("应抽出 3 条工具经验，实际 %d", len(obs))
	}
	byTool := map[string]models.MemoryObservation{}
	for _, o := range obs {
		byTool[o.Tool] = o
		if o.Symptom != "诊断任务 11（备份任务）本次失败的原因 （会话完成）" {
			t.Errorf("症状应来自稳定的 goal（可去重），实际 %q", o.Symptom)
		}
		if o.SymptomTokens == "" {
			t.Error("症状词元必须落库（供人工复盘「为什么算相似」）")
		}
		if o.ActionKind != models.MemoryActionTool {
			t.Errorf("工具经验的动作种类应为 tool，实际 %s", o.ActionKind)
		}
	}
	if got := byTool["node_status"]; got.Outcome != models.MemoryOutcomeSuccess {
		t.Errorf("allowed+ok 应为 success，实际 %s", got.Outcome)
	}
	if got := byTool["job_retry"]; got.Outcome != models.MemoryOutcomeBlocked {
		t.Errorf("转人工审批（未执行）应为 blocked，实际 %s", got.Outcome)
	}
	if got := byTool["shell_readonly"]; got.Outcome != models.MemoryOutcomeDenied {
		t.Errorf("被拒应为 denied，实际 %s", got.Outcome)
	}
	// 关键：转审批/被拒不能写进成功率分母，且结果文本必须自述"没有执行"。
	if got := byTool["job_retry"]; !strings.Contains(got.ResultText, "没有执行") {
		t.Errorf("转审批的结果文本必须自述「没有执行」，实际 %q", got.ResultText)
	}
	if got := byTool["shell_readonly"]; !strings.Contains(got.ResultText, "单会话工具调用上限") {
		t.Errorf("被拒必须保留原因（否则复盘只知道「被拒了」），实际 %q", got.ResultText)
	}
	// 参数必须脱敏（经验库是另一张表，不能假设上游一定干净）。
	if got := byTool["node_status"]; strings.Contains(got.ActionText, "sk-abcdefghijklmnop") {
		t.Errorf("动作描述里泄露了口令：%q", got.ActionText)
	}
}

func TestExtractAdviceMemoryFromDiagnosis(t *testing.T) {
	result, _ := json.Marshal(map[string]interface{}{
		"summary":                 "备份任务失败，磁盘水位 92%",
		"cause":                   "备份目标分区空间不足",
		"category":                "resource",
		"evidence_sufficient":     false,
		"next_data_to_collect":    []string{"目标分区 df 输出"},
		"suggested_action_titles": []string{"清理历史备份", "扩容分区"},
		"unverified_refs":         []string{"node:other"},
		"unverified_node_claims":  []string{"目标分区已满"},
	})
	session := &models.AgentSession{
		SessionID: "diagnose:11-run1", Kind: models.AgentKindDiagnose, JobID: 11, RunID: "run1",
		Goal: "诊断任务 11（备份任务）本次失败的原因", Status: models.AgentStatusCompleted,
		NodeUUID: "node-it-01", ResultJSON: string(result),
	}
	obs := Extract(session, nil)
	if len(obs) != 1 {
		t.Fatalf("无工具调用时应只抽出 1 条建议类经验，实际 %d", len(obs))
	}
	o := obs[0]
	if o.ActionKind != models.MemoryActionAdvice || o.Outcome != models.MemoryOutcomeAdvised {
		t.Fatalf("建议类经验的动作/结果不对：%+v", o)
	}
	if !strings.Contains(o.ActionText, "备份目标分区空间不足") || !strings.Contains(o.ActionText, "清理历史备份") {
		t.Errorf("动作应包含原因与建议，实际 %q", o.ActionText)
	}
	// 证据不足 + 未验证断言必须在结果文本里写明，否则下次召回会把它当成可信经验。
	for _, must := range []string{"证据不足", "未观测引用", "未验证"} {
		if !strings.Contains(o.ResultText, must) {
			t.Errorf("结果文本缺少 %q：%q", must, o.ResultText)
		}
	}
}

func TestExtractCapsMemoriesPerSession(t *testing.T) {
	session := &models.AgentSession{
		SessionID: "diagnose:11-run1", Kind: models.AgentKindDiagnose,
		Goal: "诊断任务 11 本次失败的原因", Status: models.AgentStatusCompleted,
	}
	audits := make([]models.AgentToolAudit, 0, 10)
	for i := 0; i < 10; i++ {
		audits = append(audits, models.AgentToolAudit{
			Tool: "log_search", Decision: models.AuditDecisionAllowed, OK: true,
			Reason: "有原因", CreatedMS: int64(i),
		})
	}
	obs := Extract(session, audits)
	if len(obs) != MaxToolMemories {
		t.Fatalf("工具经验应被截到 %d 条，实际 %d", MaxToolMemories, len(obs))
	}
}

func TestExtractPrefersInformativeAudits(t *testing.T) {
	session := &models.AgentSession{
		SessionID: "s", Kind: models.AgentKindDiagnose, Goal: "诊断任务 1 本次失败的原因",
		Status: models.AgentStatusFailed,
	}
	audits := []models.AgentToolAudit{
		{Tool: "a", Decision: models.AuditDecisionAllowed, OK: true, CreatedMS: 1},
		{Tool: "b", Decision: models.AuditDecisionAllowed, OK: true, CreatedMS: 2},
		{Tool: "c", Decision: models.AuditDecisionAllowed, OK: true, CreatedMS: 3},
		{Tool: "d", Decision: models.AuditDecisionDenied, Reason: "速率上限", CreatedMS: 4},
		{Tool: "e", Decision: models.AuditDecisionAllowed, OK: false, Err: "connection refused", CreatedMS: 5},
	}
	obs := Extract(session, audits)
	tools := []string{}
	for _, o := range obs {
		tools = append(tools, o.Tool)
	}
	// 有原因/有错误的（d、e）必须优先被留下——它们才是可复用的经验。
	joined := strings.Join(tools, ",")
	if !strings.Contains(joined, "d") || !strings.Contains(joined, "e") {
		t.Fatalf("信息量高的审计行应优先沉淀，实际 %v", tools)
	}
}

func TestExtractNilAndEmptyInputs(t *testing.T) {
	if obs := Extract(nil, nil); obs != nil {
		t.Fatalf("nil 会话应返回 nil，实际 %v", obs)
	}
	// 没有任何结果 JSON、没有审计行：不沉淀（不要凭空造一条"经验"）。
	obs := Extract(&models.AgentSession{SessionID: "s", Kind: models.AgentKindJob, Goal: "巡检"}, nil)
	if len(obs) != 0 {
		t.Fatalf("无事实可沉淀时应返回空，实际 %+v", obs)
	}
}

func TestParseDiagnosisBriefToleratesBadJSON(t *testing.T) {
	if b := ParseDiagnosisBrief(""); b != nil {
		t.Error("空字符串应返回 nil")
	}
	if b := ParseDiagnosisBrief("{not json"); b != nil {
		t.Error("坏 JSON 应返回 nil（沉淀退化而不是整体失败）")
	}
	if b := ParseDiagnosisBrief(`{"summary":"x"}`); b == nil || b.Summary != "x" {
		t.Errorf("正常 JSON 应解析成功，实际 %+v", b)
	}
}

// ---- Recorder ----

type memStore struct {
	rows map[string]*models.AgentMemory
	err  error
}

func newMemStore() *memStore { return &memStore{rows: map[string]*models.AgentMemory{}} }

func (s *memStore) Upsert(m *models.AgentMemory) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if old, ok := s.rows[m.DedupeKey]; ok {
		// 用 models 导出的 ApplyOutcome，与真库 ON DUPLICATE KEY UPDATE 的口径保持一致。
		old.ApplyOutcome(m.Outcome)
		// 真库去重命中时不会改 memory_id，调用方拿到的单号会被回填成已存在的那一个
		// （见 models.UpsertAgentMemory）。替身必须复刻这一点，否则"日志里的单号能查到行"
		// 这条契约在单测里是假的。
		m.MemoryID = old.MemoryID
		return false, nil
	}
	cp := *m
	s.rows[m.DedupeKey] = &cp
	return true, nil
}

func TestRecorderDeduplicatesAndAccumulates(t *testing.T) {
	store := newMemStore()
	rec := NewRecorder(store).WithLogger(func(string, ...interface{}) {})
	session := &models.AgentSession{
		SessionID: "diagnose:11-run1", Kind: models.AgentKindDiagnose, JobID: 11,
		Goal: "诊断任务 11 本次失败的原因", Status: models.AgentStatusCompleted,
	}
	audits := []models.AgentToolAudit{
		{Tool: "node_status", Decision: models.AuditDecisionAllowed, OK: true, CreatedMS: 1},
	}
	first := rec.Record(session, audits)
	if first.Created != 1 || first.Updated != 0 || len(first.MemoryIDs) != 1 {
		t.Fatalf("首次沉淀应新建 1 条，实际 %+v", first)
	}
	// 同一任务再次失败、同样动作：**累加计数**而不是新增一行
	// （否则高频故障会把低频故障挤出召回窗口）。
	session2 := *session
	session2.SessionID = "diagnose:11-run2"
	second := rec.Record(&session2, audits)
	if second.Created != 0 || second.Updated != 1 {
		t.Fatalf("第二次应命中去重键并累加，实际 %+v", second)
	}
	if len(store.rows) != 1 {
		t.Fatalf("经验库应只有 1 行（去重生效），实际 %d", len(store.rows))
	}
	// 去重命中时上报的单号必须**仍是库里那一行**：否则日志/验收里会出现查不到的 memory_id。
	if len(second.MemoryIDs) != 1 || second.MemoryIDs[0] != first.MemoryIDs[0] {
		t.Fatalf("去重后上报的单号应保持不变: %v vs %v", first.MemoryIDs, second.MemoryIDs)
	}
	for _, row := range store.rows {
		if row.OccurrenceCount != 2 || row.SuccessCount != 2 {
			t.Fatalf("计数应为 2 次出现 / 2 次成功，实际 %+v", row)
		}
	}
}

func TestRecorderNeverBlocksOnStoreFailure(t *testing.T) {
	store := newMemStore()
	store.err = errTestStore
	var logged []string
	rec := NewRecorder(store).WithLogger(func(f string, a ...interface{}) {
		logged = append(logged, f)
	})
	session := &models.AgentSession{
		SessionID: "s1", Kind: models.AgentKindDiagnose, Goal: "诊断任务 1 本次失败的原因",
		Status: models.AgentStatusFailed,
	}
	res := rec.Record(session, []models.AgentToolAudit{
		{Tool: "log_search", Decision: models.AuditDecisionAllowed, OK: true, CreatedMS: 1},
	})
	if len(res.Errors) != 1 {
		t.Fatalf("写入失败必须被记录在 Errors 里（不能静默），实际 %+v", res)
	}
	if len(logged) == 0 {
		t.Fatal("写入失败必须进日志")
	}
	// 关键：Record 不返回 error —— 经验沉淀是旁路，绝不能阻断会话落库/接口返回。
	if res.Created != 0 {
		t.Fatalf("失败时不该有成功计数，实际 %+v", res)
	}
}

func TestRecorderWithClockUsesInjectedTime(t *testing.T) {
	store := newMemStore()
	fixed := time.Unix(1800000000, 0)
	rec := NewRecorder(store).WithLogger(func(string, ...interface{}) {}).WithClock(func() time.Time { return fixed })
	rec.Record(&models.AgentSession{
		SessionID: "s", Kind: models.AgentKindDiagnose, Goal: "诊断任务 1 本次失败的原因",
	}, []models.AgentToolAudit{{Tool: "t", Decision: models.AuditDecisionAllowed, OK: true}})
	for _, row := range store.rows {
		if row.Created != fixed.Unix() || row.CreatedMS != fixed.UnixMilli() {
			t.Fatalf("双口径时间应来自注入时钟，实际 created=%d created_ms=%d", row.Created, row.CreatedMS)
		}
	}
}

var errTestStore = errTest("store 不可用")

type errTest string

func (e errTest) Error() string { return string(e) }
