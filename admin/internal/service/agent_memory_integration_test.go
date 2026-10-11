//go:build integration

// 阶段 4-A 经验库的**真库**集成测试。
//
// 为什么必须是真库：
//  1. 去重靠 dedupe_key 的**唯一索引 + ON DUPLICATE KEY UPDATE**。内存替身里
//     "先查后写"看起来也对，但并发下会撞 1062；只有真库能证明"同一条经验
//     会被累加而不是新增"，这正是经验库不退化成流水账的前提；
//  2. 召回候选集的"只取启用行"过滤在 SQL 里；停用过滤写错的话，被撤下的坏经验
//     仍然会影响模型，而这种错误在单测里（手写候选集）永远暴露不出来；
//  3. 统计口径是从 SQL 聚合出来的（sum/group by），"成功率分母只含执行过的动作"
//     必须用真库的数来对账。
//
// 运行方式同其它集成测试：CRONY_TEST_MYSQL_DSN 指向临时库（13306），未设置即跳过。

package service

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agentmemory"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"gorm.io/gorm"
)

// itMemDB 连真库并保证经验表存在（独立于审批的 itDB：本文件的用例不关心审批表）。
func itMemDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CRONY_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 集成测试")
	}
	db, err := dbclient.Init(dsn, "silent", 1, 5)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if db == nil {
		t.Fatal("dbclient.Init 返回了空连接")
	}
	if err := db.AutoMigrate(&models.AgentMemory{}); err != nil {
		t.Fatalf("迁移经验表失败: %v", err)
	}
	if !db.Migrator().HasTable(models.CronyAgentMemoryTableName) {
		t.Fatalf("表 %s 未被创建", models.CronyAgentMemoryTableName)
	}
	return db
}

// itMemTag 生成本轮用例的唯一标记（症状里带它，保证召回只命中本轮的行走）。
func itMemTag(suffix string) string {
	return fmt.Sprintf("itmem-%s-%s", time.Now().Format("150405.000000"), suffix)
}

// itMemObs 造一条观测（去重键由 symptom+action+target 决定）。
func itMemObs(tag, symptom, action, target, outcome string, now time.Time) models.MemoryObservation {
	return models.MemoryObservation{
		SessionID:  tag + "-sess",
		RunID:      tag + "-run",
		JobID:      910101,
		AgentKind:  models.AgentKindDiagnose,
		Symptom:    symptom,
		ActionKind: models.MemoryActionTool,
		ActionText: action,
		Tool:       strings.Fields(action)[0],
		TargetNode: target,
		Risk:       "read",
		Outcome:    outcome,
		ResultText: "集成测试结果：" + outcome,
	}
}

// itMemUpsert 落一条并返回 memory_id（失败即 Fatal）。
func itMemUpsert(t *testing.T, db *gorm.DB, obs models.MemoryObservation, now time.Time) (string, bool) {
	t.Helper()
	mem := obs.ToAgentMemory(now)
	created, err := models.UpsertAgentMemory(db, mem)
	if err != nil {
		t.Fatalf("落库经验失败: %v", err)
	}
	return mem.MemoryID, created
}

// itMemCleanup 按会话前缀清理本轮的行。
func itMemCleanup(t *testing.T, db *gorm.DB, tag string) {
	t.Helper()
	if err := db.Exec("delete from "+models.CronyAgentMemoryTableName+" where session_id like ?", tag+"%").Error; err != nil {
		t.Errorf("清理经验行失败（验收库可能残留 %s）: %v", tag, err)
	}
}

// TestIntegrationMemoryDedupeAccumulates 同一条经验被反复观测时**累加而不是新增**。
func TestIntegrationMemoryDedupeAccumulates(t *testing.T) {
	db := itMemDB(t)
	prefix := itMemTag("dedupe")
	svc := NewMemoryService(NewGormMemoryStore())
	t.Cleanup(func() { itMemCleanup(t, db, prefix) })

	base := time.Now()
	symptom := prefix + " 备份任务失败 磁盘水位 92%"
	obs := itMemObs(prefix, symptom, "disk_df /data", "node-it-01", models.MemoryOutcomeFailed, base)

	id1, created1 := itMemUpsert(t, db, obs, base)
	if !created1 {
		t.Fatal("第一次落库必须是新建")
	}
	// 第二次：同样的症状/动作/目标，但这次成功了（模拟"同一个处置这次奏效"）。
	obs2 := obs
	obs2.Outcome = models.MemoryOutcomeSuccess
	obs2.SessionID = prefix + "-sess2"
	id2, created2 := itMemUpsert(t, db, obs2, base.Add(time.Minute))
	if created2 {
		t.Fatal("同 dedupe_key 的第二次落库不能新建行（否则经验库会退化成流水账）")
	}
	if id1 != id2 {
		t.Fatalf("去重命中时上报的单号必须回填为库里那一行: %s vs %s", id1, id2)
	}

	row, err := svc.Find(id1)
	if err != nil || row == nil {
		t.Fatalf("回读失败: %v", err)
	}
	if row.OccurrenceCount != 2 {
		t.Fatalf("出现次数应累加为 2，实际 %d", row.OccurrenceCount)
	}
	if row.SuccessCount != 1 || row.FailCount != 1 {
		t.Fatalf("成功/失败计数应为 1/1，实际 %d/%d", row.SuccessCount, row.FailCount)
	}
	if row.Outcome != models.MemoryOutcomeSuccess {
		t.Fatalf("最近一次结果应更新为 success，实际 %s", row.Outcome)
	}
	if row.LastSeen <= base.Unix() {
		t.Fatalf("last_seen 应被刷新到第二次观测时间，实际 %d", row.LastSeen)
	}
	rate, known := row.MemorySuccessRate()
	if !known || rate <= 0 || rate >= 1 {
		t.Fatalf("成功 1 失败 1 时成功率应为 0.5 且已知，实际 %v/%v", rate, known)
	}

	// 库里只应有 1 行同 dedupe_key 的实体。
	var cnt int64
	if err := db.Table(models.CronyAgentMemoryTableName).Where("dedupe_key = ?", row.DedupeKey).Count(&cnt).Error; err != nil {
		t.Fatalf("统计 dedupe 行数失败: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("同 dedupe_key 应只有 1 行，实际 %d", cnt)
	}
}

// TestIntegrationMemoryRecallHitsRelatedAndSkipsDisabled 召回：命中相关、跳过无关、**跳过停用**。
func TestIntegrationMemoryRecallHitsRelatedAndSkipsDisabled(t *testing.T) {
	db := itMemDB(t)
	prefix := itMemTag("recall")
	svc := NewMemoryService(NewGormMemoryStore())
	t.Cleanup(func() { itMemCleanup(t, db, prefix) })

	now := time.Now()
	related := prefix + " 备份任务失败 mysqldump 鉴权失败 1045 账号密码过期"
	unrelated := prefix + " 定时任务失败 通知发送超时 短信网关不可达"
	disabled := prefix + " 备份任务失败 mysqldump 鉴权失败 1045 账号密码过期 另一处置"

	idRelated, _ := itMemUpsert(t, db, itMemObs(prefix, related, "log_search keyword=1045", "node-it-01", models.MemoryOutcomeFailed, now), now)
	idUnrelated, _ := itMemUpsert(t, db, itMemObs(prefix+"-u", unrelated, "notify_test channel=sms", "node-it-02", models.MemoryOutcomeSuccess, now), now)
	idDisabled, _ := itMemUpsert(t, db, itMemObs(prefix+"-d", disabled, "cred_rotate user=backup", "node-it-01", models.MemoryOutcomeSuccess, now.Add(time.Second)), now.Add(time.Second))

	// 停用掉"另一处置"：它文本上同样相似，但**不允许**再影响模型。
	if _, err := svc.SetEnabled(idDisabled, false, "it-tester", "该处置在真实环境验证为无效，撤下以免误导"); err != nil {
		t.Fatalf("停用失败: %v", err)
	}

	query := prefix + " 备份任务失败 mysqldump 鉴权失败 1045 账号密码已轮换"
	res := svc.RecallForDiagnose(query, models.AgentKindDiagnose, "node-it-01", 3)
	if res.AdvisoryOnly != true || res.UsableAsEvidence != false {
		t.Fatalf("召回必须声明仅线索、不可作证据：advisory_only=%v usable_as_evidence=%v", res.AdvisoryOnly, res.UsableAsEvidence)
	}
	if res.Considered == 0 {
		t.Fatal("候选集不该为空（库里至少有本轮 3 条启用项，其中 2 条启用）")
	}

	got := map[string]bool{}
	for _, h := range res.Hits {
		got[h.MemoryID] = true
	}
	if !got[idRelated] {
		t.Fatalf("相关经验必须被召回，实际命中 %v（query=%s）", res.Hits, query)
	}
	if got[idUnrelated] {
		t.Fatal("无关经验（短信网关）不该被召回")
	}
	if got[idDisabled] {
		t.Fatal("**停用的经验绝不允许进入召回结果**（否则撤下动作是假的）")
	}
	// 打分必须可解释：每条命中都要给出命中词与理由。
	for _, h := range res.Hits {
		if len(h.Matched) == 0 || len(h.Reasons) == 0 {
			t.Fatalf("命中缺少可解释信息: %+v", h)
		}
		if h.Score <= 0 || h.Score > 1.5 {
			t.Fatalf("相似度应在 (0,1.5] 内（同类/同目标有加成，上限 1.10 + 容差）: %v", h.Score)
		}
	}
	// 渲染出来的提示行必须自带"条目号 + 相似度"，供人复盘注入内容。
	lines := agentmemory.RenderHintLines(res)
	if len(lines) != len(res.Hits) {
		t.Fatalf("提示行数与命中数应一致: %d vs %d", len(lines), len(res.Hits))
	}
	for _, l := range lines {
		if !strings.Contains(l, "[") || !strings.Contains(l, "相似度") {
			t.Fatalf("提示行缺少条目号/相似度: %s", l)
		}
	}
}

// TestIntegrationMemoryDisableCASAndReasonRequired 治理动作的三条硬约束。
func TestIntegrationMemoryDisableCASAndReasonRequired(t *testing.T) {
	db := itMemDB(t)
	prefix := itMemTag("disable")
	svc := NewMemoryService(NewGormMemoryStore())
	t.Cleanup(func() { itMemCleanup(t, db, prefix) })

	now := time.Now()
	id, _ := itMemUpsert(t, db, itMemObs(prefix, prefix+" 磁盘水位高 清理临时目录",
		"log_search keyword=no space", "node-it-01", models.MemoryOutcomeSuccess, now), now)

	// 1) 停用必须给原因。
	if _, err := svc.SetEnabled(id, false, "it-tester", "   "); err != ErrMemoryReasonRequired {
		t.Fatalf("停用未写原因必须被拒，实际 %v", err)
	}
	row, _ := svc.Find(id)
	if row == nil || !row.Enabled {
		t.Fatal("被拒的停用不得改变状态")
	}

	// 2) 正常停用：谁/为什么/何时三件事都要落库。
	at := now.Add(2 * time.Minute)
	svc.SetClock(func() time.Time { return at })
	updated, err := svc.SetEnabled(id, false, "it-tester", "该经验对应的处置已废弃")
	if err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if updated.Enabled || updated.DisabledBy != "it-tester" ||
		updated.DisabledReason != "该经验对应的处置已废弃" || updated.DisabledAt != at.Unix() {
		t.Fatalf("停用事实不完整: enabled=%v by=%q reason=%q at=%d",
			updated.Enabled, updated.DisabledBy, updated.DisabledReason, updated.DisabledAt)
	}

	// 3) 重复停用必须 CAS 失败（重复操作要能被识别，而不是静默成功）。
	if _, err := svc.SetEnabled(id, false, "it-tester", "再停一次"); err != ErrMemoryConflict {
		t.Fatalf("重复停用应返回冲突，实际 %v", err)
	}

	// 4) 重新启用：清空停用审计字段（否则启用的行会带着"上次为什么停用"的残留）。
	re, err := svc.SetEnabled(id, true, "it-tester", "")
	if err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	if !re.Enabled || re.DisabledBy != "" || re.DisabledReason != "" || re.DisabledAt != 0 {
		t.Fatalf("启用后必须清空停用审计字段: %+v", re)
	}

	// 5) 不存在的单号：必须是"未找到"而不是"冲突"。
	if _, err := svc.SetEnabled(prefix+"-nonexistent", false, "it-tester", "x"); err != ErrMemoryNotFound {
		t.Fatalf("不存在的条目应返回未找到，实际 %v", err)
	}
}

// TestIntegrationMemoryStatsSelfConsistency 统计口径自洽：分组和 == 总数，成功率分母只含执行过的样本。
func TestIntegrationMemoryStatsSelfConsistency(t *testing.T) {
	db := itMemDB(t)
	prefix := itMemTag("stats")
	store := NewGormMemoryStore()
	svc := NewMemoryService(store)
	t.Cleanup(func() { itMemCleanup(t, db, prefix) })

	now := time.Now()
	// 症状刻意各不相同 → dedupe_key 不同 → 4 行独立经验。
	specs := []struct {
		outcome string
		symptom string
		action  string
	}{
		{models.MemoryOutcomeSuccess, prefix + " 场景A 端口未监听", "probe_port port=13307"},
		{models.MemoryOutcomeSuccess, prefix + " 场景B 磁盘水位高", "disk_df /data"},
		{models.MemoryOutcomeFailed, prefix + " 场景C 备份失败", "log_search keyword=1045"},
		{models.MemoryOutcomeBlocked, prefix + " 场景D 需要重跑", "job_retry job_id=910101"},
		{models.MemoryOutcomeDenied, prefix + " 场景E 越权尝试", "job_retry job_id=910102"},
		{models.MemoryOutcomeAdvised, prefix + " 场景F 只给建议", "建议：检查 DNS 解析"},
	}
	for i, s := range specs {
		obs := itMemObs(prefix, s.symptom, s.action, "node-it-01", s.outcome, now.Add(time.Duration(i)*time.Millisecond))
		if s.outcome == models.MemoryOutcomeAdvised {
			obs.ActionKind = models.MemoryActionAdvice
			obs.Tool = ""
		}
		itMemUpsert(t, db, obs, now.Add(time.Duration(i)*time.Millisecond))
	}

	stats, err := svc.Stats()
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	// 分组和 == 总行数（真库聚合的自洽性）。
	var sum int64
	for _, n := range stats.ByOutcome {
		sum += n
	}
	if sum != stats.Total {
		t.Fatalf("by_outcome 之和 %d 应等于 total %d", sum, stats.Total)
	}
	if stats.Enabled+stats.Disabled != stats.Total {
		t.Fatalf("enabled(%d)+disabled(%d) 应等于 total(%d)", stats.Enabled, stats.Disabled, stats.Total)
	}
	// 本轮贡献：成功 2、失败 1；被拒/未执行/只建议**不得**进失败数。
	if stats.SuccessSum < 2 || stats.FailSum < 1 {
		t.Fatalf("成功/失败累计至少应为 2/1，实际 %d/%d", stats.SuccessSum, stats.FailSum)
	}
	if stats.SuccessRate == nil {
		t.Fatal("有执行样本时必须给出成功率")
	}
	want := float64(stats.SuccessSum) / float64(stats.SuccessSum+stats.FailSum)
	if diff := *stats.SuccessRate - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("成功率应等于 成功/(成功+失败)=%.6f，实际 %.6f", want, *stats.SuccessRate)
	}
	// blocked/denied/advised 三类必须**存在且不计入分母**：分母 == success+fail。
	if _, ok := stats.ByOutcome[models.MemoryOutcomeBlocked]; !ok {
		t.Fatalf("本轮落了 blocked 经验，by_outcome 里必须能看到: %+v", stats.ByOutcome)
	}
	if _, ok := stats.ByOutcome[models.MemoryOutcomeDenied]; !ok {
		t.Fatalf("本轮落了 denied 经验，by_outcome 里必须能看到: %+v", stats.ByOutcome)
	}

	// 三态检查：本轮某条经验只有 blocked 计数、没有执行样本 → 必须"无数据"而不是 0%。
	rows, _, err := store.List(models.MemoryQuery{SessionID: prefix + "-sess", PageSize: 50})
	if err != nil {
		t.Fatalf("列表查询失败: %v", err)
	}
	if len(rows) != len(specs) {
		t.Fatalf("本轮应有 %d 条经验，实际 %d", len(specs), len(rows))
	}
	for _, r := range rows {
		rate, known := r.MemorySuccessRate()
		if r.Outcome == models.MemoryOutcomeBlocked || r.Outcome == models.MemoryOutcomeDenied ||
			r.Outcome == models.MemoryOutcomeAdvised {
			if known {
				t.Fatalf("未执行的动作不得有成功率：outcome=%s rate=%v", r.Outcome, rate)
			}
		}
	}
}

// TestIntegrationMemoryRecorderEndToEnd 会话 + 审计 → 规则抽取 → 真库沉淀 → 召回。
//
// 这是本阶段唯一一条"从会话事实到下一次诊断输入"的完整链路验证。
func TestIntegrationMemoryRecorderEndToEnd(t *testing.T) {
	db := itMemDB(t)
	prefix := itMemTag("e2e")
	svc := NewMemoryService(NewGormMemoryStore())
	t.Cleanup(func() { itMemCleanup(t, db, prefix) })

	now := time.Now()
	session := &models.AgentSession{
		SessionID: prefix + "-sess",
		Kind:      models.AgentKindDiagnose,
		RunID:     prefix + "-run",
		JobID:     910101,
		NodeUUID:  "node-it-01",
		Goal:      prefix + " 诊断任务 910101（备份任务）本次失败的原因",
		Status:    models.AgentStatusFailed,
		ResultJSON: `{"summary":"备份账号密码过期","cause":"mysqldump 鉴权失败 1045",` +
			`"evidence_sufficient":true,"suggested_action_titles":["更新备份账号凭据"]}`,
	}
	audits := []models.AgentToolAudit{
		{
			AuditID: prefix + "-a1", SessionID: session.SessionID, AgentKind: models.AgentKindDiagnose,
			Tool: "log_search", Risk: "read", Decision: models.AuditDecisionAllowed, OK: false,
			Err: "命令执行失败：日志源不可达", TargetNode: "node-it-01", CreatedMS: now.UnixMilli(),
		},
		{
			AuditID: prefix + "-a2", SessionID: session.SessionID, AgentKind: models.AgentKindDiagnose,
			Tool: "job_retry", Risk: "write", Decision: models.AuditDecisionApprovalRequired, OK: false,
			Reason: "写动作需要人工审批", ApprovalID: "ap-it-1", TargetNode: "node-it-01",
			CreatedMS: now.UnixMilli() + 1,
		},
		{
			AuditID: prefix + "-a3", SessionID: session.SessionID, AgentKind: models.AgentKindDiagnose,
			Tool: "job_retry", Risk: "write", Decision: models.AuditDecisionDenied, OK: false,
			Reason: "策略不允许写动作", TargetNode: "node-it-01", CreatedMS: now.UnixMilli() + 2,
		},
	}

	rec := agentmemory.NewRecorder(NewGormMemoryStore())
	res := rec.Record(session, audits)
	if len(res.Errors) > 0 {
		t.Fatalf("沉淀不应报错: %v", res.Errors)
	}
	// 3 条工具审计 + 1 条会话结论 = 4 条观测（上限 5 内）。
	if res.Extracted != 4 {
		t.Fatalf("应抽取 4 条经验（3 工具 + 1 结论），实际 %d（%v）", res.Extracted, res.MemoryIDs)
	}
	// 但**行数只有 3**：a2（转审批）与 a3（被拒）是同一个工具、同一组空参数、
	// 同一个目标节点，去重键完全相同 → 合并成一行并用计数列分别记账。
	// 这是设计决定（动作相同 → 同一行，便于统计"这个动作真正跑起来时成功率多少"），
	// 所以必须显式钉住：如果哪天键变了，这条断言会立刻提醒"经验库的粒度变了"。
	if res.Created != 3 || res.Updated != 1 {
		t.Fatalf("首次沉淀应为 3 新建 + 1 去重合并，实际 created=%d updated=%d", res.Created, res.Updated)
	}
	// 上报的单号必须都能查到行（去重命中的那条要回填真实单号）。
	for _, id := range res.MemoryIDs {
		row, err := svc.Find(id)
		if err != nil || row == nil {
			t.Fatalf("沉淀上报的单号必须可查: %s（err=%v）", id, err)
		}
	}

	// 结果映射必须落在事实上：被拒=denied、转审批=blocked（且文本写明"没有执行"）。
	rows, _, err := svc.Search(models.MemoryQuery{SessionID: session.SessionID, PageSize: 50})
	if err != nil || len(rows) != 3 {
		t.Fatalf("回读沉淀结果失败（%d 行）: %v", len(rows), err)
	}
	byOutcome := map[string]models.AgentMemory{}
	for _, r := range rows {
		byOutcome[r.Outcome] = r
	}
	if _, ok := byOutcome[models.MemoryOutcomeFailed]; !ok {
		t.Fatal("allowed+ok=false 必须映射为 failed")
	}
	// 合并行：最近一次结果是 denied，但 blocked 计数必须还在（历史不能被覆盖掉）。
	collided, ok := byOutcome[models.MemoryOutcomeDenied]
	if !ok {
		t.Fatal("denied 必须映射为 denied")
	}
	if collided.BlockedCount != 1 || collided.DeniedCount != 1 || collided.OccurrenceCount != 2 {
		t.Fatalf("同一动作的两种闸门结果必须分别记账: occurrence=%d blocked=%d denied=%d",
			collided.OccurrenceCount, collided.BlockedCount, collided.DeniedCount)
	}
	if collided.SuccessCount+collided.FailCount != 0 {
		t.Fatalf("被拒/未执行的动作不得计入成功率分子/分母: %+v", collided)
	}
	if !strings.Contains(collided.ResultText, "没有执行") && !strings.Contains(collided.ResultText, "被拦截") {
		t.Fatalf("未执行的结果文本必须点明'没有执行'（否则会被读成执行失败）: %s", collided.ResultText)
	}
	advised, ok := byOutcome[models.MemoryOutcomeAdvised]
	if !ok {
		t.Fatal("会话结论必须沉淀为 advised 经验")
	}
	if advised.ActionKind != models.MemoryActionAdvice || advised.Tool != "" {
		t.Fatalf("建议类经验的形状不对: kind=%s tool=%q", advised.ActionKind, advised.Tool)
	}
	if !strings.Contains(advised.ResultText, "未执行") {
		t.Fatalf("建议类经验必须写明'未执行': %s", advised.ResultText)
	}

	// 第二次同样的会话（同 goal/status → 同 symptom，同工具/目标 → 同 dedupe_key）
	// 必须**累加**而不是新增：这是"反复发生的故障"能被计数的基础。
	// 注意 upsert 会把 session_id 刷新为最近一次会话，所以下面按 sess2 查能找到全部 3 行。
	session2 := *session
	session2.SessionID = prefix + "-sess2"
	res2 := rec.Record(&session2, audits)
	if res2.Created != 0 || res2.Extracted != 4 {
		t.Fatalf("第二次沉淀应命中去重（created=0）：created=%d extracted=%d", res2.Created, res2.Updated)
	}
	rows2, _, err := svc.Search(models.MemoryQuery{SessionID: session2.SessionID, PageSize: 50})
	if err != nil {
		t.Fatalf("二次沉淀回读失败: %v", err)
	}
	if len(rows2) != 3 {
		t.Fatalf("去重后总行数不应增加，实际 %d", len(rows2))
	}
	for _, r := range rows2 {
		if r.OccurrenceCount < 2 {
			t.Fatalf("去重命中的行出现次数应 >=2: %s -> %d", r.MemoryID, r.OccurrenceCount)
		}
	}

	// 召回：用同一个 goal 文本查，必须命中本轮沉淀的行。
	recall := svc.RecallForDiagnose(session.Goal, models.AgentKindDiagnose, "node-it-01", 5)
	if len(recall.Hits) == 0 {
		t.Fatal("同一任务的症状必须能召回刚沉淀的经验")
	}
	hitIDs := map[string]bool{}
	for _, h := range recall.Hits {
		hitIDs[h.MemoryID] = true
	}
	matched := 0
	for _, r := range rows2 {
		if hitIDs[r.MemoryID] {
			matched++
		}
	}
	if matched == 0 {
		t.Fatalf("召回结果里没有任何本轮沉淀的条目: %+v", recall.Hits)
	}
}
