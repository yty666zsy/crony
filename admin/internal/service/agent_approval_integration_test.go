//go:build integration

// 阶段 4.2 审批闭环的**真库**集成测试。
//
// 为什么这些用例必须是集成而不是单元：
//  1. "一次批准一次执行"的最终保证在数据库的 CAS 上（`update ... where status='pending'`），
//     内存替身再忠实也只是复刻；只有真库才能证明两个并发请求里恰好一个赢；
//  2. 审批 → 执行 → 审计是一条跨三张表的链路（approval_request / agent_tool_audit /
//     执行器副作用），单测里各层都对、接起来错，是这类功能最常见的翻车方式；
//  3. 这里刻意**打开写权限**（allow_write=true + 自治 L2 且白名单为空）：
//     黑盒验收跑的是只读部署，只能验证"不执行"那一侧；"批准后真的执行一次"
//     只能在这里验。两个测试的分工是有意的，不是覆盖漏洞。
//
// 唯一的替身是"重跑如何投递到节点"（RetryExecutor）：真实投递要写 etcd，
// 集成测试环境没有 etcd。副作用用调用计数观测——它证明的是
// "网关确实把动作交到了执行器手里、且只交了一次"。
//
// 运行方式同其它集成测试：设置 CRONY_TEST_MYSQL_DSN 指向临时库（13306），未设置即跳过。

package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agentruntime"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/tools"
	"gorm.io/gorm"
)

const itApprovalDSNEnv = "CRONY_TEST_MYSQL_DSN"

// itRetryStub 记录"重跑被投递了几次"，并可注入失败。
type itRetryStub struct {
	calls  int32
	last   tools.RetryRequest
	failIt error
}

func (s *itRetryStub) RetryJob(_ context.Context, req tools.RetryRequest) (tools.Result, error) {
	atomic.AddInt32(&s.calls, 1)
	s.last = req
	if s.failIt != nil {
		return tools.Result{}, s.failIt
	}
	return tools.Result{
		OK:      true,
		Summary: "已向原节点投递一次重跑",
		Evidence: []tools.Evidence{
			{Source: "job", Ref: "job:910007"},
		},
		Meta: map[string]interface{}{"job_id": req.JobID},
	}, nil
}

func (s *itRetryStub) count() int { return int(atomic.LoadInt32(&s.calls)) }

// regExecutor 把 *tools.Registry 适配成 ApprovalExecutor。
//
// 生产接线里 ApprovalExecutor 是 *agentruntime.Runtime（它有 ToolNames 与
// ExecuteApprovedTool），内部就是把两个调用转给注册表。测试里直接用注册表，
// 只差一个"名字叫什么"的桥：Registry 的名字方法叫 Names()。
// 这个适配器存在的价值是让集成测试**不依赖 runtime 的模型配置**就能覆盖
// "网关 → 审批单 → 执行 → 审计"这条链，而不是绕开被测逻辑。
type regExecutor struct{ reg *tools.Registry }

func (e regExecutor) ExecuteApprovedTool(ctx context.Context, meta tools.RunMeta, sessionID, tool string,
	args map[string]interface{}, approvalID string) (tools.Result, error) {
	return e.reg.ExecuteApproved(ctx, sessionID, tool, args, approvalID)
}

func (e regExecutor) ToolNames() []string { return e.reg.Names() }

// itDB 连真实库并保证两张阶段 4 的表存在。
func itDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(itApprovalDSNEnv))
	if dsn == "" {
		t.Skipf("未设置 %s，跳过真实 MySQL 集成测试", itApprovalDSNEnv)
	}
	db, err := dbclient.Init(dsn, "silent", 1, 5)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if db == nil {
		t.Fatal("dbclient.Init 返回了空连接")
	}
	if err := db.AutoMigrate(&models.AgentToolAudit{}, &models.ApprovalRequest{}); err != nil {
		t.Fatalf("迁移审批/审计表失败: %v", err)
	}
	for _, table := range []string{models.CronyApprovalRequestTableName, models.CronyAgentToolAuditTableName} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("表 %s 未被创建", table)
		}
	}
	return db
}

// writePolicy 打开写动作能力，但**不放行任何免审批白名单**：
// 自治 L2 是写工具被注册的前提（L1 下写能力面为空），而 AutoApprove 为空
// 保证每一次写调用都必须走人工审批——正是本文件要验的路径。
func writePolicy() tools.Policy {
	p := tools.DefaultPolicy()
	p.AllowWrite = true
	p.Autonomy = tools.AutonomyL2
	p.AutoApprove = nil
	return p.Normalize()
}

// approvalRuntime 装配一套"真注册表 + 真审计观察者 + 真库审批服务"。
//
// 返回注册表、审批服务、执行器替身，以及清理函数（用完删除本会话的事实行）。
func approvalRuntime(t *testing.T, db *gorm.DB) (*tools.Registry, *ApprovalService, *itRetryStub, string) {
	t.Helper()
	tag := "itappr-" + time.Now().Format("150405.000000")
	sessionID := tag + "-sess"
	stub := &itRetryStub{}

	policy := writePolicy()
	reg := tools.NewRegistry(policy, tools.WithObserver(agentruntime.ToolAuditObserver()))
	if err := reg.Register(tools.NewJobRetryTool(stub)); err != nil {
		t.Fatalf("注册写工具失败: %v", err)
	}
	if !reg.HasWriteTools() {
		t.Fatal("打开写权限后注册表里必须有写工具（否则审批链路无从触发）")
	}
	store := NewGormApprovalStore()
	svc := NewApprovalService(store, regExecutor{reg: reg}, policy)
	reg.SetApprover(svc)

	t.Cleanup(func() {
		if err := db.Exec("delete from "+models.CronyAgentToolAuditTableName+" where session_id = ?", sessionID).Error; err != nil {
			t.Errorf("清理审计行失败（验收库可能残留 %s）: %v", sessionID, err)
		}
		if err := db.Exec("delete from "+models.CronyApprovalRequestTableName+" where session_id = ?", sessionID).Error; err != nil {
			t.Errorf("清理审批单失败（验收库可能残留 %s）: %v", sessionID, err)
		}
	})
	return reg, svc, stub, sessionID
}

// itRetryArgs 一次合法的重跑参数。
func itRetryArgs() map[string]interface{} {
	return map[string]interface{}{
		"job_id": float64(910007),
		"reason": "集成测试：确认执行失败原因后重跑一次",
	}
}

// findApprovalBySession 按会话找审批单（一次调用一张单）。
func findApprovalBySession(t *testing.T, db *gorm.DB, sessionID string) models.ApprovalRequest {
	t.Helper()
	var row models.ApprovalRequest
	if err := db.Table(models.CronyApprovalRequestTableName).
		Where("session_id = ?", sessionID).Order("id desc").First(&row).Error; err != nil {
		t.Fatalf("按会话 %s 查审批单失败: %v", sessionID, err)
	}
	return row
}

// auditRows 读本次会话的审计行（按发生顺序）。
func auditRows(t *testing.T, db *gorm.DB, sessionID string) []models.AgentToolAudit {
	t.Helper()
	var rows []models.AgentToolAudit
	if err := db.Table(models.CronyAgentToolAuditTableName).
		Where("session_id = ?", sessionID).Order("id asc").Find(&rows).Error; err != nil {
		t.Fatalf("查审计行失败: %v", err)
	}
	return rows
}

// TestIntegrationWriteCallRequiresApprovalAndExecutesOnce 是审批闭环的主用例：
//
//	写调用 → 网关不给过、转人工审批（未执行）→ 落真审批单 → 人工批准
//	→ 复核参数哈希 → 执行器被调用**一次** → 审计两行（approval_required / approval_executed）
//	→ 单子落 executed → 再次批准被 CAS 拒绝且不再执行。
func TestIntegrationWriteCallRequiresApprovalAndExecutesOnce(t *testing.T) {
	db := itDB(t)
	reg, svc, stub, sessionID := approvalRuntime(t, db)

	ctx := tools.WithRunMeta(context.Background(), tools.RunMeta{
		AgentKind: models.AgentKindDiagnose, RunID: sessionID + "-run",
	})

	// 1) 模型请求写动作：必须得到"转审批"的**成功观测**，而不是错误，也不是执行。
	res, err := reg.Call(ctx, sessionID, "job_retry", itRetryArgs())
	if err != nil {
		t.Fatalf("转人工审批不应作为错误返回（它是可解释的一步）: %v", err)
	}
	if stub.count() != 0 {
		t.Fatalf("审批之前绝不能有副作用，实际执行器被调用 %d 次", stub.count())
	}
	if asStrMeta(res.Meta, "approval_status") != models.ApprovalStatusPending {
		t.Fatalf("观测里必须写明审批状态 pending，实际 %+v", res.Meta)
	}
	if asBoolMeta(res.Meta, "executed") {
		t.Fatalf("转审批阶段不能标记 executed=true（尚未执行）: %+v", res.Meta)
	}
	if len(res.Evidence) == 0 || !strings.HasPrefix(res.Evidence[0].Ref, "approval:") {
		t.Fatalf("转审批的证据必须指向审批单，实际 %+v", res.Evidence)
	}
	rows := auditRows(t, db, sessionID)
	if len(rows) != 1 || rows[0].Decision != tools.DecisionApprovalRequired {
		t.Fatalf("应恰好一条 approval_required 审计行，实际 %+v", rows)
	}
	if rows[0].Reason == "" {
		t.Fatalf("转审批必须写明原因: %+v", rows[0])
	}

	// 2) 真库里必须有一张待审批单，且参数哈希与请求一致（执行时要靠它复验）。
	pending := findApprovalBySession(t, db, sessionID)
	if pending.Status != models.ApprovalStatusPending {
		t.Fatalf("审批单状态应为 pending，实际 %s", pending.Status)
	}
	if pending.ArgsHash != tools.ArgsHash(itRetryArgs()) {
		t.Fatalf("参数哈希与请求不一致：批准 A 执行 B 的门就关不上了（%s）", pending.ArgsHash)
	}
	if pending.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("审批单 TTL 必须落在未来: %d", pending.ExpiresAt)
	}
	if !strings.Contains(pending.Reason, "重跑一次") {
		t.Fatalf("模型自述理由必须原样存档: %q", pending.Reason)
	}

	// 3) 人工批准：这一次要真的执行。
	decision, err := svc.Decide(context.Background(), pending.ApprovalID, true, "ops-li", "确认现场一致，同意重跑")
	if err != nil {
		t.Fatalf("批准执行失败: %v", err)
	}
	if !decision.Executed {
		t.Fatalf("批准后必须真的执行: %+v", decision)
	}
	if stub.count() != 1 {
		t.Fatalf("一次批准只能投递一次，实际 %d 次", stub.count())
	}
	if stub.last.JobID != 910007 {
		t.Fatalf("投递的任务必须是审批单里的那个，实际 %+v", stub.last)
	}
	executed := findApprovalBySession(t, db, sessionID)
	if executed.Status != models.ApprovalStatusExecuted {
		t.Fatalf("执行后状态应为 executed，实际 %s", executed.Status)
	}
	if executed.ExecutedAt == 0 || executed.ExecutionResult == "" {
		t.Fatalf("执行事实必须回写（含结果）：%+v", executed)
	}
	if executed.DecidedBy != "ops-li" {
		t.Fatalf("审批人必须落库，实际 %q", executed.DecidedBy)
	}

	rows = auditRows(t, db, sessionID)
	if len(rows) != 2 {
		t.Fatalf("应恰好两条审计行（approval_required + approval_executed），实际 %d 条", len(rows))
	}
	if rows[1].Decision != tools.DecisionApprovalExecuted {
		t.Fatalf("执行必须留 approval_executed 事实，实际 %s", rows[1].Decision)
	}
	if rows[1].ApprovalID != pending.ApprovalID {
		t.Fatalf("执行审计行必须带审批单号（否则审查看不出是谁批的），实际 %q", rows[1].ApprovalID)
	}
	if !rows[1].OK {
		t.Fatalf("成功执行的行 ok 应为 true: %+v", rows[1])
	}
	if rows[1].Risk != string(tools.RiskWrite) {
		t.Fatalf("写动作的审计行必须标明风险级别，实际 %q", rows[1].Risk)
	}

	// 4) 再次批准：必须被拒绝，且**不再执行**（这是"一次批准一次执行"的最终保证）。
	_, err = svc.Decide(context.Background(), pending.ApprovalID, true, "ops-wang", "再点一次")
	if !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("重复批准必须报冲突，实际 %v", err)
	}
	if stub.count() != 1 {
		t.Fatalf("重复批准不得产生第二次执行，实际 %d 次", stub.count())
	}
	if got := auditRows(t, db, sessionID); len(got) != 2 {
		t.Fatalf("重复批准不得新增审计行，实际 %d 条", len(got))
	}
}

// TestIntegrationConcurrentApproveExecutesExactlyOnce 并发批准：
// 真正的仲裁者是数据库 CAS，不是应用层的"先查再改"。
func TestIntegrationConcurrentApproveExecutesExactlyOnce(t *testing.T) {
	db := itDB(t)
	reg, svc, stub, sessionID := approvalRuntime(t, db)

	ctx := tools.WithRunMeta(context.Background(), tools.RunMeta{AgentKind: models.AgentKindDiagnose, RunID: sessionID + "-run"})
	if _, err := reg.Call(ctx, sessionID, "job_retry", itRetryArgs()); err != nil {
		t.Fatalf("转审批失败: %v", err)
	}
	pending := findApprovalBySession(t, db, sessionID)

	// 8 个并发：4 个时 MySQL 的死锁复现率约 1/4，8 个更容易把回归露出来
	// （修复前这套用例大约一半概率失败，见报告 §3）。
	const workers = 8
	var wg sync.WaitGroup
	var won, conflict, other int32
	var mu sync.Mutex
	otherErrs := []string{}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Decide(context.Background(), pending.ApprovalID, true, "ops-"+string(rune('a'+i)), "")
			switch {
			case err == nil:
				atomic.AddInt32(&won, 1)
			case errors.Is(err, ErrApprovalConflict):
				atomic.AddInt32(&conflict, 1)
			default:
				atomic.AddInt32(&other, 1)
				// 把"其它错误"原文留下来：只报一个计数会让人完全无法定位，
				// 而这条路径恰恰是并发下最容易出问题的地方。
				mu.Lock()
				otherErrs = append(otherErrs, err.Error())
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if won != 1 {
		t.Fatalf("并发批准必须恰好一个赢家，实际 %d（冲突 %d / 其它 %d：%v）", won, conflict, other, otherErrs)
	}
	if conflict != workers-1 || other != 0 {
		t.Fatalf("其余请求必须报冲突且不能是别的错误：冲突 %d 其它 %d：%v", conflict, other, otherErrs)
	}
	if stub.count() != 1 {
		t.Fatalf("并发下执行必须恰好一次，实际 %d 次", stub.count())
	}
	if got := findApprovalBySession(t, db, sessionID); got.Status != models.ApprovalStatusExecuted {
		t.Fatalf("终态应为 executed，实际 %s", got.Status)
	}
}

// TestIntegrationExpiredApprovalNeverExecutes 过期单子：惰性清理落 expired，
// 人工再点批准只会看到"过期"，执行器一次都不会被调用。
func TestIntegrationExpiredApprovalNeverExecutes(t *testing.T) {
	db := itDB(t)
	reg, svc, stub, sessionID := approvalRuntime(t, db)

	ctx := tools.WithRunMeta(context.Background(), tools.RunMeta{AgentKind: models.AgentKindDiagnose, RunID: sessionID + "-run"})
	if _, err := reg.Call(ctx, sessionID, "job_retry", itRetryArgs()); err != nil {
		t.Fatalf("转审批失败: %v", err)
	}
	pending := findApprovalBySession(t, db, sessionID)

	// 把 TTL 拨到过去（模拟"人隔了一天才来看"）：直接改库比 sleep 一分钟现实。
	if err := db.Exec("update "+models.CronyApprovalRequestTableName+
		" set expires_at = ? where approval_id = ?", time.Now().Add(-time.Minute).Unix(), pending.ApprovalID).Error; err != nil {
		t.Fatalf("把审批单置为过期失败: %v", err)
	}

	_, err := svc.Decide(context.Background(), pending.ApprovalID, true, "ops-li", "来晚了")
	if !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("过期后批准必须报过期，实际 %v", err)
	}
	if stub.count() != 0 {
		t.Fatalf("过期单子绝不能执行，实际 %d 次", stub.count())
	}
	row := findApprovalBySession(t, db, sessionID)
	if row.Status != models.ApprovalStatusExpired {
		t.Fatalf("过期单子应落 expired（与 execution_failed 语义不同），实际 %s", row.Status)
	}
	if row.ExecutedAt != 0 {
		t.Fatalf("过期单子不得有执行时间戳: %+v", row)
	}
	// 审计只有"转审批"那一行：批准被拒不该在工具网关留下任何事实。
	if got := auditRows(t, db, sessionID); len(got) != 1 || got[0].Decision != tools.DecisionApprovalRequired {
		t.Fatalf("过期批准不得新增审计行，实际 %+v", got)
	}
}

// TestIntegrationPolicyTightenedAfterApprovalBlocksExecution 批准之后策略收紧：
// 复核必须在执行前再读一次策略——"批的时候允许"不等于"执行的时候还允许"。
func TestIntegrationPolicyTightenedAfterApprovalBlocksExecution(t *testing.T) {
	db := itDB(t)
	reg, svc, stub, sessionID := approvalRuntime(t, db)

	ctx := tools.WithRunMeta(context.Background(), tools.RunMeta{AgentKind: models.AgentKindDiagnose, RunID: sessionID + "-run"})
	if _, err := reg.Call(ctx, sessionID, "job_retry", itRetryArgs()); err != nil {
		t.Fatalf("转审批失败: %v", err)
	}
	pending := findApprovalBySession(t, db, sessionID)

	tightened := writePolicy()
	tightened.AllowWrite = false
	svc.policy = tightened.Normalize()

	_, err := svc.Decide(context.Background(), pending.ApprovalID, true, "ops-li", "批准")
	if !errors.Is(err, ErrApprovalPolicyChanged) {
		t.Fatalf("策略收紧后批准必须被拒（1017 类），实际 %v", err)
	}
	if stub.count() != 0 {
		t.Fatalf("策略收紧后绝不能执行，实际 %d 次", stub.count())
	}
	row := findApprovalBySession(t, db, sessionID)
	if row.Status != models.ApprovalStatusBlocked {
		t.Fatalf("复核不通过应落 blocked（动作从未执行，与 execution_failed 语义不同），实际 %s", row.Status)
	}
	if row.ExecutedAt != 0 {
		t.Fatalf("复核不通过不得写执行时间戳（否则等于伪造执行事实）: %+v", row)
	}
	if strings.TrimSpace(row.ExecutionResult) == "" {
		t.Fatalf("必须写明是哪类复核失败：%+v", row)
	}
}

// asStrMeta / asBoolMeta 只用于读观测 Meta 里的约定字段。
func asStrMeta(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func asBoolMeta(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}
