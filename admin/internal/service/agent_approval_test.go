package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// ---------- 测试替身 ----------

// memApprovalStore 内存审批单表，**忠实复刻** models 里的 CAS 语义。
//
// 为什么替身要复刻 CAS 而不是"能用就行"：本项目里"一次批准一次执行"是核心不变式，
// 如果替身宽松（先查后写也放行），单元测试就会给出虚假的安全感。
type memApprovalStore struct {
	mu   sync.Mutex
	rows map[string]*models.ApprovalRequest
	// markExecutedOK 强制 CAS 失败（用于观察"执行了但状态没写上"的告警路径）。
	markExecutedOK *bool
	decideCalls    int32
	markCalls      int32
}

func newMemStore() *memApprovalStore {
	return &memApprovalStore{rows: map[string]*models.ApprovalRequest{}}
}

func (m *memApprovalStore) Create(req *models.ApprovalRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *req
	m.rows[req.ApprovalID] = &cp
	return nil
}

func (m *memApprovalStore) Find(approvalID string) (*models.ApprovalRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[approvalID]
	if !ok {
		return nil, errors.New("record not found")
	}
	cp := *row
	return &cp, nil
}

func (m *memApprovalStore) List(status, _, _ string, _, _ int) ([]models.ApprovalRequest, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []models.ApprovalRequest{}
	for _, r := range m.rows {
		if status == "" || r.Status == status {
			out = append(out, *r)
		}
	}
	return out, int64(len(out)), nil
}

func (m *memApprovalStore) Decide(approvalID, toStatus, decidedBy, note string, now int64) (bool, error) {
	atomic.AddInt32(&m.decideCalls, 1)
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[approvalID]
	if !ok || row.Status != models.ApprovalStatusPending {
		return false, nil // CAS 未抢到
	}
	row.Status = toStatus
	row.DecidedBy, row.DecidedAt, row.DecisionNote = decidedBy, now, note
	return true, nil
}

func (m *memApprovalStore) MarkExecuted(approvalID, status, result string, now int64) (bool, error) {
	atomic.AddInt32(&m.markCalls, 1)
	if m.markExecutedOK != nil && !*m.markExecutedOK {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[approvalID]
	if !ok || row.Status != models.ApprovalStatusApproved {
		return false, nil
	}
	row.Status = status
	row.ExecutedAt, row.ExecutionResult = now, result
	return true, nil
}

// MarkBlocked 复刻 models.MarkApprovalBlocked 的语义：approved → blocked，**不动 executed_at**。
func (m *memApprovalStore) MarkBlocked(approvalID, result string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[approvalID]
	if !ok || row.Status != models.ApprovalStatusApproved {
		return false, nil
	}
	row.Status = models.ApprovalStatusBlocked
	row.ExecutionResult = result
	return true, nil
}

func (m *memApprovalStore) ExpirePending(now int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, r := range m.rows {
		if r.Status == models.ApprovalStatusPending && r.ExpiresAt > 0 && r.ExpiresAt <= now {
			r.Status = models.ApprovalStatusExpired
			r.DecisionNote = "审批超时未决策（系统自动作废）"
			n++
		}
	}
	return n, nil
}

// tamperArgs 模拟"批准与执行之间参数被改"（TOCTOU 攻击/上游 bug）。
func (m *memApprovalStore) tamperArgs(approvalID string, args map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[approvalID]; ok {
		b, _ := json.Marshal(args)
		row.ArgsJSON = string(b)
	}
}

func (m *memApprovalStore) statusOf(approvalID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[approvalID]; ok {
		return row.Status
	}
	return ""
}

// fakeApprovalExecutor 记录执行调用，可注入故障/能力面。
type fakeApprovalExecutor struct {
	mu       sync.Mutex
	calls    int32
	lastArgs map[string]interface{}
	lastID   string
	lastMeta tools.RunMeta
	names    []string
	failErr  error
	result   tools.Result
}

func (f *fakeApprovalExecutor) ExecuteApprovedTool(_ context.Context, meta tools.RunMeta, _, tool string,
	args map[string]interface{}, approvalID string) (tools.Result, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.lastArgs, f.lastID, f.lastMeta = args, approvalID, meta
	f.mu.Unlock()
	if f.failErr != nil {
		return tools.Result{}, f.failErr
	}
	if f.result.Summary != "" || f.result.Err != "" {
		return f.result, nil
	}
	return tools.Result{OK: true, Summary: "已触发任务 " + tool + " 的重跑"}, nil
}

func (f *fakeApprovalExecutor) ToolNames() []string {
	if f.names == nil {
		return []string{"job_retry", "shell_readonly"}
	}
	return f.names
}

func (f *fakeApprovalExecutor) count() int { return int(atomic.LoadInt32(&f.calls)) }

// 编译期契约检查：实现是否真的满足接口（接口漂移会在这里断掉）。
var (
	_ tools.Approver      = (*ApprovalService)(nil)
	_ ApprovalStore       = (*GormApprovalStore)(nil)
	_ ApprovalExecutor    = (*fakeApprovalExecutor)(nil)
	_ tools.RetryExecutor = (*JobRetryExecutor)(nil)
)

// approvePolicy 允许写动作的策略（与运行时同源）。
func approvePolicy() tools.Policy {
	p := tools.DefaultPolicy()
	p.AllowWrite = true
	return p.Normalize()
}

// newTestService 装配一套"服务 + 内存库 + 假执行器"。
func newTestService(t *testing.T) (*ApprovalService, *memApprovalStore, *fakeApprovalExecutor) {
	t.Helper()
	store := newMemStore()
	exec := &fakeApprovalExecutor{}
	svc := NewApprovalService(store, exec, approvePolicy())
	return svc, store, exec
}

// requestRetry 走一遍"模型请求写动作"→ 落审批单。
func requestRetry(t *testing.T, svc *ApprovalService, jobID int) tools.ApprovalOutcome {
	t.Helper()
	args := map[string]interface{}{"job_id": float64(jobID), "reason": "上次执行失败原因未查清，重跑取证"}
	out, err := svc.RequestApproval(context.Background(), tools.ApprovalInput{
		SessionID:   "diagnose:s-1",
		RunID:       "run-1",
		AgentKind:   "diagnose",
		Tool:        "job_retry",
		Risk:        tools.RiskWrite,
		Args:        args,
		ArgsHash:    tools.ArgsHash(args),
		Reason:      "上次执行失败原因未查清，重跑取证",
		Targets:     []string{"job:21"},
		Impact:      "1 个任务 / 1 台节点",
		RequestedBy: "diagnose",
	})
	if err != nil {
		t.Fatalf("落单失败: %v", err)
	}
	if out.ApprovalID == "" || out.Status != models.ApprovalStatusPending {
		t.Fatalf("落单结果必须带单号且为 pending，实际 %+v", out)
	}
	return out
}

// ---------- 用例 ----------

// 落单必须把"模型的理由、参数哈希、影响面、TTL"一起存下来：
// 人批的是理由，执行校验用的是哈希。
func TestRequestApprovalStoresReasonHashAndTTL(t *testing.T) {
	svc, store, _ := newTestService(t)
	out := requestRetry(t, svc, 21)

	row, err := store.Find(out.ApprovalID)
	if err != nil {
		t.Fatalf("查单失败: %v", err)
	}
	if row.Status != models.ApprovalStatusPending || row.Tool != "job_retry" || row.Risk != "write" {
		t.Fatalf("落单字段不符: %+v", row)
	}
	if !strings.Contains(row.Reason, "重跑取证") {
		t.Fatalf("必须存档模型自述理由，实际 %q", row.Reason)
	}
	if row.ArgsHash != tools.ArgsHash(map[string]interface{}{"job_id": float64(21), "reason": "上次执行失败原因未查清，重跑取证"}) {
		t.Fatalf("参数哈希不符: %s", row.ArgsHash)
	}
	if row.TargetNode != "job:21" || row.Impact == "" {
		t.Fatalf("目标与影响面必须落库: %+v", row)
	}
	if row.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("TTL 必须落在未来，实际 %d", row.ExpiresAt)
	}
	if row.Created == 0 || row.CreatedMS == 0 {
		t.Fatalf("时间戳必须落库（Created=%d CreatedMS=%d）", row.Created, row.CreatedMS)
	}
}

// 驳回：不执行，状态落 rejected。
func TestRejectDoesNotExecute(t *testing.T) {
	svc, store, exec := newTestService(t)
	out := requestRetry(t, svc, 21)

	res, err := svc.Decide(context.Background(), out.ApprovalID, false, "ops-zhang", "现场已恢复，不需要重跑")
	if err != nil {
		t.Fatalf("驳回不应报错: %v", err)
	}
	if res.Executed || exec.count() != 0 {
		t.Fatalf("驳回绝不能执行: executed=%v calls=%d", res.Executed, exec.count())
	}
	if got := store.statusOf(out.ApprovalID); got != models.ApprovalStatusRejected {
		t.Fatalf("状态应为 rejected，实际 %s", got)
	}
	row, _ := store.Find(out.ApprovalID)
	if row.DecidedBy != "ops-zhang" || row.DecisionNote == "" || row.DecidedAt == 0 {
		t.Fatalf("决策人与意见必须落库: %+v", row)
	}
}

// 批准即执行：执行一次、参数是**存档的那份**、状态落 executed。
func TestApproveExecutesOnceWithStoredArgs(t *testing.T) {
	svc, store, exec := newTestService(t)
	out := requestRetry(t, svc, 21)

	res, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", "确认现场仍是同一故障，同意重跑")
	if err != nil {
		t.Fatalf("批准执行不应报错: %v", err)
	}
	if !res.Executed || exec.count() != 1 {
		t.Fatalf("批准必须执行一次: executed=%v calls=%d", res.Executed, exec.count())
	}
	if exec.lastID != out.ApprovalID {
		t.Fatalf("执行必须带审批单号（审计要能追到单子），实际 %q", exec.lastID)
	}
	if exec.lastArgs["job_id"] != float64(21) {
		t.Fatalf("执行参数必须来自存档参数，实际 %+v", exec.lastArgs)
	}
	if exec.lastMeta.RunID != "run-1" || exec.lastMeta.AgentKind != "diagnose" {
		t.Fatalf("执行必须带上会话归属（审计行要能对齐诊断会话），实际 %+v", exec.lastMeta)
	}
	if got := store.statusOf(out.ApprovalID); got != models.ApprovalStatusExecuted {
		t.Fatalf("状态应为 executed，实际 %s", got)
	}
	row, _ := store.Find(out.ApprovalID)
	if row.ExecutedAt == 0 || row.ExecutionResult == "" {
		t.Fatalf("执行事实必须回写: %+v", row)
	}
}

// 一次批准一次执行：已执行的单子再批准必须失败，且**不得**再执行。
func TestSecondApproveIsConflictAndNeverExecutesAgain(t *testing.T) {
	svc, _, exec := newTestService(t)
	out := requestRetry(t, svc, 21)
	if _, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", ""); err != nil {
		t.Fatalf("首次批准应成功: %v", err)
	}
	_, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-wang", "再点一次")
	if err == nil || !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("重复批准必须报冲突，实际 %v", err)
	}
	if exec.count() != 1 {
		t.Fatalf("一次批准只能执行一次，实际 %d 次", exec.count())
	}
}

// 并发批准：CAS 保证只有一个赢家，执行恰好一次。
func TestConcurrentApproveExecutesExactlyOnce(t *testing.T) {
	svc, _, exec := newTestService(t)
	out := requestRetry(t, svc, 21)

	var wg sync.WaitGroup
	var okCount, conflictCount int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-"+string(rune('a'+i)), "")
			if err == nil {
				atomic.AddInt32(&okCount, 1)
				return
			}
			if errors.Is(err, ErrApprovalConflict) {
				atomic.AddInt32(&conflictCount, 1)
			}
		}(i)
	}
	wg.Wait()
	if okCount != 1 {
		t.Fatalf("并发批准必须恰好一个成功，实际 %d", okCount)
	}
	if conflictCount != 3 {
		t.Fatalf("其余必须报冲突，实际 %d", conflictCount)
	}
	if exec.count() != 1 {
		t.Fatalf("并发下一个批准只能执行一次，实际 %d 次", exec.count())
	}
}

// 过期：TTL 之后人工再点批准必须失败，且不执行。
func TestExpiredApprovalCannotBeApproved(t *testing.T) {
	svc, store, exec := newTestService(t)
	now := time.Now()
	svc.SetClock(func() time.Time { return now })
	out := requestRetry(t, svc, 21)
	svc.SetClock(func() time.Time { return now.Add(models.DefaultApprovalTTL + time.Minute) })

	_, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", "来晚了")
	if err == nil || !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("过期后批准必须被拒，实际 %v", err)
	}
	if exec.count() != 0 {
		t.Fatalf("过期单绝不能执行，实际 %d 次", exec.count())
	}
	if got := store.statusOf(out.ApprovalID); got != models.ApprovalStatusExpired {
		t.Fatalf("过期应由惰性清理落成 expired，实际 %s", got)
	}
	// 过期原因必须自述（回看时能区分"没人处理"与"处理了没生效"）。
	row, _ := store.Find(out.ApprovalID)
	if !strings.Contains(row.DecisionNote, "超时") {
		t.Fatalf("过期必须带自述原因，实际 %q", row.DecisionNote)
	}
}

// TOCTOU：批准的是 A，执行时参数被改成 B → 必须拒绝且不执行。
func TestTamperedArgsAreRejected(t *testing.T) {
	svc, store, exec := newTestService(t)
	out := requestRetry(t, svc, 21)
	// 攻击者（或上游 bug）在批准前把参数换成另一个任务。
	store.tamperArgs(out.ApprovalID, map[string]interface{}{
		"job_id": float64(999), "reason": "换个任务重跑",
	})

	_, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", "看起来没问题")
	if err == nil || !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("参数被改必须被拒（TOCTOU），实际 %v", err)
	}
	if exec.count() != 0 {
		t.Fatalf("参数不符绝不能执行，实际 %d 次", exec.count())
	}
	if got := store.statusOf(out.ApprovalID); got != models.ApprovalStatusBlocked {
		t.Fatalf("复核失败应落 blocked（动作从未执行），实际 %s", got)
	}
	// blocked 与 execution_failed 的分界就在这里：不得留下执行时间戳。
	if row, _ := store.Find(out.ApprovalID); row.ExecutedAt != 0 {
		t.Fatalf("复核失败不得写 executed_at（否则等于伪造「执行过了」）：%+v", row)
	}
}

// 批准之后工具下线/策略收紧：不得执行。
func TestToolGoneAndPolicyTightenedBlockExecution(t *testing.T) {
	t.Run("工具下线", func(t *testing.T) {
		svc, store, exec := newTestService(t)
		exec.names = []string{"shell_readonly"} // 能力面里已经没有 job_retry
		out := requestRetry(t, svc, 21)
		_, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", "")
		if err == nil || !errors.Is(err, ErrApprovalToolGone) {
			t.Fatalf("工具下线必须被拒，实际 %v", err)
		}
		if exec.count() != 0 {
			t.Fatalf("不得执行")
		}
		if got := store.statusOf(out.ApprovalID); got != models.ApprovalStatusBlocked {
			t.Fatalf("应落 blocked（未执行），实际 %s", got)
		}
	})
	t.Run("策略收紧", func(t *testing.T) {
		store := newMemStore()
		exec := &fakeApprovalExecutor{}
		p := approvePolicy()
		svc := NewApprovalService(store, exec, p)
		out := requestRetry(t, svc, 21)
		// 批准之前运维把 allow_write 关掉（改配置重启/热更）。
		p2 := approvePolicy()
		p2.AllowWrite = false
		svc.policy = p2.Normalize()
		_, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", "")
		if err == nil || !errors.Is(err, ErrApprovalPolicyChanged) {
			t.Fatalf("策略收紧必须被拒，实际 %v", err)
		}
		if exec.count() != 0 {
			t.Fatalf("不得执行")
		}
		if got := store.statusOf(out.ApprovalID); got != models.ApprovalStatusBlocked {
			t.Fatalf("策略收紧后应落 blocked（未执行），实际 %s", got)
		}
	})
}

// 执行失败：状态落 execution_failed，但**已执行**这个事实必须保留。
func TestExecutorFailureMarksExecutionFailed(t *testing.T) {
	svc, store, exec := newTestService(t)
	exec.failErr = errors.New("etcd 不可用")
	out := requestRetry(t, svc, 21)

	res, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", "")
	if err == nil {
		t.Fatalf("执行失败必须把错误往上报（审批人要知道没跑成）")
	}
	if !res.Executed {
		t.Fatalf("执行失败仍然代表「尝试执行过」，Executed 必须为 true")
	}
	if got := store.statusOf(out.ApprovalID); got != models.ApprovalStatusExecutionFailed {
		t.Fatalf("状态应为 execution_failed，实际 %s", got)
	}
	row, _ := store.Find(out.ApprovalID)
	if !strings.Contains(row.ExecutionResult, "etcd") {
		t.Fatalf("失败原因必须回写，实际 %q", row.ExecutionResult)
	}
}

// 业务失败（Result.OK=false）也算 execution_failed——不能因为 HTTP 层没报错就记成功。
func TestBusinessFailureIsNotSuccess(t *testing.T) {
	svc, store, _ := newTestService(t)
	out := requestRetry(t, svc, 21)
	svc.exec.(*fakeApprovalExecutor).result = tools.Result{OK: false, Summary: "任务不存在", Err: "job 21 not found"}
	_, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", "")
	if err != nil {
		t.Fatalf("业务失败不应作为 error 抛出（它是可解释的结果）: %v", err)
	}
	if got := store.statusOf(out.ApprovalID); got != models.ApprovalStatusExecutionFailed {
		t.Fatalf("业务失败应落 execution_failed，实际 %s", got)
	}
}

// 状态回写失败：执行已发生 → 必须带 Warning 往上报，不能静默成成功。
func TestMarkExecutedFailureSurfacesWarning(t *testing.T) {
	svc, store, _ := newTestService(t)
	out := requestRetry(t, svc, 21)
	no := false
	store.markExecutedOK = &no

	res, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", "")
	if err != nil {
		t.Fatalf("回写失败不应改变执行事实: %v", err)
	}
	if !res.Executed || res.Warning == "" {
		t.Fatalf("必须带告警（执行了但状态没写上会让单子被再执行）: %+v", res)
	}
	if !strings.Contains(res.Warning, "CAS") {
		t.Fatalf("告警要说明是 CAS 未生效，实际 %q", res.Warning)
	}
}

// 撤销：只有 pending 能撤；执行过的单子不能撤。
func TestCancelOnlyPending(t *testing.T) {
	svc, store, exec := newTestService(t)
	out := requestRetry(t, svc, 21)
	if _, err := svc.Decide(context.Background(), out.ApprovalID, true, "ops-li", ""); err != nil {
		t.Fatalf("批准应成功: %v", err)
	}
	if _, err := svc.Cancel(out.ApprovalID, "ops-li", "撤回"); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("已执行单不可撤销，实际 %v", err)
	}
	if exec.count() != 1 {
		t.Fatalf("撤销失败不得影响已执行事实")
	}
	// 另一张 pending 单可以撤。
	out2 := requestRetry(t, svc, 22)
	row, err := svc.Cancel(out2.ApprovalID, "ops-li", "不用了")
	if err != nil || row.Status != models.ApprovalStatusCanceled {
		t.Fatalf("pending 应可撤销，实际 %v %+v", err, row)
	}
	if got := store.statusOf(out2.ApprovalID); got != models.ApprovalStatusCanceled {
		t.Fatalf("状态应为 canceled，实际 %s", got)
	}
}

// 缺少操作人必须拒绝：没有操作人的"批准"在事后审计里等于没有批准人。
func TestDecideRequiresOperator(t *testing.T) {
	svc, _, exec := newTestService(t)
	out := requestRetry(t, svc, 21)
	if _, err := svc.Decide(context.Background(), out.ApprovalID, true, "  ", ""); err == nil {
		t.Fatalf("缺少操作人必须被拒")
	}
	if exec.count() != 0 {
		t.Fatalf("不得执行")
	}
}

// 单号不存在：明确的 not found，而不是笼统错误。
func TestDecideUnknownApproval(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Decide(context.Background(), "ap-nope", true, "ops-li", ""); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("未知单号应报 not found，实际 %v", err)
	}
}

// ---------- 重跑执行器（admin 侧） ----------

// 未分配节点的任务不能重跑：换节点执行同一任务已经不是"重跑"而是"再跑一次"。
func TestRetryExecutorRefusesUnassignedJob(t *testing.T) {
	e := &JobRetryExecutor{
		Once:    func(*request.ReqJobOnce) error { t.Fatalf("未分配节点不得投递"); return nil },
		FindJob: func(int) (*models.Job, error) { return &models.Job{ID: 21, RunOn: "", Status: 0}, nil },
		FindNode: func(string) (*models.Node, error) {
			t.Fatalf("没有节点就不该去查节点")
			return nil, nil
		},
	}
	res, err := e.RetryJob(context.Background(), tools.RetryRequest{JobID: 21, Reason: "重跑取证"})
	if err != nil {
		t.Fatalf("业务失败不应抛 error: %v", err)
	}
	if res.OK || res.Err == "" {
		t.Fatalf("必须返回失败结果并带原因，实际 %+v", res)
	}
}

// 节点连接异常时不得投递（投了也是白投，还会让审批者以为跑过了）。
func TestRetryExecutorRefusesUnreachableNode(t *testing.T) {
	delivered := 0
	e := &JobRetryExecutor{
		Once:    func(*request.ReqJobOnce) error { delivered++; return nil },
		FindJob: func(int) (*models.Job, error) { return &models.Job{ID: 21, RunOn: "node-it-01"}, nil },
		FindNode: func(string) (*models.Node, error) {
			return &models.Node{UUID: "node-it-01", Status: models.NodeConnFail}, nil
		},
	}
	res, _ := e.RetryJob(context.Background(), tools.RetryRequest{JobID: 21, Reason: "重跑取证"})
	if res.OK || delivered != 0 {
		t.Fatalf("节点异常不得投递: res=%+v delivered=%d", res, delivered)
	}
}

// 正常路径：投递到原节点一次，并给出带证据的成功结果。
func TestRetryExecutorDeliversToOriginalNode(t *testing.T) {
	var got *request.ReqJobOnce
	e := &JobRetryExecutor{
		Once:     func(r *request.ReqJobOnce) error { got = r; return nil },
		FindJob:  func(int) (*models.Job, error) { return &models.Job{ID: 21, RunOn: "node-it-01"}, nil },
		FindNode: func(string) (*models.Node, error) { return &models.Node{UUID: "node-it-01"}, nil },
	}
	res, err := e.RetryJob(context.Background(), tools.RetryRequest{JobID: 21, Reason: "上次失败原因未查清"})
	if err != nil || !res.OK {
		t.Fatalf("正常路径应成功: err=%v res=%+v", err, res)
	}
	if got == nil || got.JobId != 21 || got.NodeUUID != "node-it-01" {
		t.Fatalf("投递内容不符: %+v", got)
	}
	if len(res.Evidence) == 0 || res.Evidence[0].Ref != "job:21" {
		t.Fatalf("成功必须带证据（结论挂证据），实际 %+v", res.Evidence)
	}
}
