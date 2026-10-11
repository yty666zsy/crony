package tools

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tmnhs/crony/common/pkg/llm"
)

// stubRetryExecutor 记录被执行的请求，并可注入失败。
type stubRetryExecutor struct {
	calls   int32
	last    RetryRequest
	failErr error
}

func (s *stubRetryExecutor) RetryJob(_ context.Context, req RetryRequest) (Result, error) {
	atomic.AddInt32(&s.calls, 1)
	s.last = req
	if s.failErr != nil {
		return Result{}, s.failErr
	}
	return Result{OK: true, Output: "已投递重跑", Evidence: []Evidence{{Source: "job", Ref: "job:21", Quote: "重跑已投递"}}}, nil
}

func (s *stubRetryExecutor) count() int { return int(atomic.LoadInt32(&s.calls)) }

// lastApproval 取最近一张审批单（stubApprover 定义在 gate_test.go，这里只做取值适配）。
func lastApproval(a *stubApprover) ApprovalInput {
	if len(a.got) == 0 {
		return ApprovalInput{}
	}
	return a.got[len(a.got)-1]
}

// writeTestPolicy 开启写工具的仓库级测试策略（与 tools_test.go 的 writePolicy 同源）。
func writeTestPolicy() Policy {
	p := writePolicy()
	return p.Normalize()
}

// 写工具必须自洽：ReadOnly=false 与 Risk=write 同时成立，且能给出目标。
func TestJobRetryToolDeclaresWriteRiskAndTargets(t *testing.T) {
	tool := NewJobRetryTool(&stubRetryExecutor{})
	if tool.ReadOnly() {
		t.Fatalf("写工具的 ReadOnly() 必须为 false")
	}
	if tool.Risk() != RiskWrite {
		t.Fatalf("job_retry 的风险级别应为 write，实际 %s", tool.Risk())
	}
	if !tool.Risk().IsWrite() {
		t.Fatalf("RiskWrite.IsWrite() 应为 true")
	}
	targets := TargetsOf(tool, map[string]interface{}{"job_id": float64(21)})
	if len(targets) != 1 || targets[0] != "job:21" {
		t.Fatalf("目标必须渲染成 job:21（爆炸半径预算的输入），实际 %v", targets)
	}
}

// Schema 是写动作的第一道防线：没有理由的写动作不允许通过参数校验。
func TestJobRetrySchemaRequiresJobIDAndReason(t *testing.T) {
	tool := NewJobRetryTool(&stubRetryExecutor{})
	if err := tool.Schema().Validate(map[string]interface{}{"job_id": float64(21), "reason": "失败原因未查清，重跑取证"}); err != nil {
		t.Fatalf("合法参数不应报错: %v", err)
	}
	if err := tool.Schema().Validate(map[string]interface{}{"job_id": float64(21)}); err == nil {
		t.Fatalf("缺少 reason 必须被拒")
	}
	if err := tool.Schema().Validate(map[string]interface{}{"reason": "只是想说点什么"}); err == nil {
		t.Fatalf("缺少 job_id 必须被拒")
	}
	if err := tool.Schema().Validate(map[string]interface{}{"job_id": float64(0), "reason": "重跑一次试试"}); err == nil {
		t.Fatalf("job_id=0 必须被拒（范围 1 起）")
	}
	if err := tool.Schema().Validate(map[string]interface{}{"job_id": "21", "reason": "重跑一次试试"}); err == nil {
		t.Fatalf("job_id 传字符串必须被拒（类型不符）")
	}
}

// 执行器缺失时必须显式拒绝：注册了却没人能执行，绝不能静默成功。
func TestJobRetryWithoutExecutorIsRejected(t *testing.T) {
	tool := NewJobRetryTool(nil)
	_, err := tool.Execute(context.Background(), map[string]interface{}{"job_id": float64(21), "reason": "重跑一次试试"})
	if err == nil || !errors.Is(err, ErrToolDenied) {
		t.Fatalf("未装配执行器时应返回 ErrToolDenied，实际 %v", err)
	}
}

// 执行器返回 error 要转成"失败观测"而不是把错误抛出去（否则模型会反复重试）。
func TestJobRetryExecutorErrorBecomesFailedResult(t *testing.T) {
	exec := &stubRetryExecutor{failErr: errors.New("调度器不可用")}
	tool := NewJobRetryTool(exec)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"job_id": float64(21), "reason": "重跑一次试试"})
	if err != nil {
		t.Fatalf("执行器故障不应作为 error 抛出: %v", err)
	}
	if res.OK || !strings.Contains(res.Err, "调度器不可用") {
		t.Fatalf("必须返回失败观测并带原因，实际 %+v", res)
	}
}

// 默认（只读）策略下，写工具**根本不注册**：模型看不到，也就不会去调。
func TestRegisterWriteToolsSkipsWhenPolicyIsReadonly(t *testing.T) {
	reg := NewRegistry(DefaultPolicy())
	names, err := RegisterWriteTools(reg, WriteSources{Retry: &stubRetryExecutor{}})
	if err != nil {
		t.Fatalf("只读策略不应报错: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("只读策略下不应注册写工具，实际 %v", names)
	}
	if _, ok := reg.Get("job_retry"); ok {
		t.Fatalf("只读策略下 job_retry 不得出现在注册表里")
	}
}

// L0 自治级别下同样不注册（"只提供建议"）。
func TestRegisterWriteToolsSkipsAtAutonomyL0(t *testing.T) {
	p := writeTestPolicy()
	p.Autonomy = AutonomyL0
	reg := NewRegistry(p)
	names, err := RegisterWriteTools(reg, WriteSources{Retry: &stubRetryExecutor{}})
	if err != nil {
		t.Fatalf("L0 不应报错: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("L0 下不应注册写工具，实际 %v", names)
	}
}

// 没有执行器时不注册（不是"注册了再失败"）。
func TestRegisterWriteToolsSkipsWithoutExecutor(t *testing.T) {
	reg := NewRegistry(writeTestPolicy())
	names, err := RegisterWriteTools(reg, WriteSources{})
	if err != nil {
		t.Fatalf("无执行器不应报错: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("无执行器时不应注册写工具，实际 %v", names)
	}
}

// 开启写 + 有执行器：注册成功，且参数 Schema 会随 Specs 暴露给模型。
func TestRegisterWriteToolsRegistersWhenAllowed(t *testing.T) {
	reg := NewRegistry(writeTestPolicy())
	names, err := RegisterWriteTools(reg, WriteSources{Retry: &stubRetryExecutor{}})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if len(names) != 1 || names[0] != "job_retry" {
		t.Fatalf("应注册 job_retry，实际 %v", names)
	}
	spec, ok := specByName(reg.Specs(), "job_retry")
	if !ok {
		t.Fatalf("Specs 里必须有 job_retry（否则模型不知道有这个能力）")
	}
	if spec.Description == "" || spec.Parameters == nil {
		t.Fatalf("job_retry 的 Spec 必须带说明与参数 Schema: %+v", spec)
	}
	if !strings.Contains(spec.Description, "审批") {
		t.Fatalf("写工具的说明必须告诉模型「会先转人工审批」，实际 %q", spec.Description)
	}
}

// 核心不变式：默认 L1 下写工具被调用时**只生成审批单、不执行**，并且审计为 approval_required。
func TestWriteToolCallRequiresApprovalAndDoesNotExecute(t *testing.T) {
	exec := &stubRetryExecutor{}
	approver := &stubApprover{}
	var records []CallRecord
	reg := NewRegistry(writeTestPolicy(), WithApprover(approver), WithObserver(func(r CallRecord) { records = append(records, r) }))
	if _, err := RegisterWriteTools(reg, WriteSources{Retry: exec}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	ctx := WithRunMeta(context.Background(), RunMeta{AgentKind: "diagnose", RunID: "run-1"})
	res, err := reg.Call(ctx, "s-1", "job_retry", map[string]interface{}{
		"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证",
	})
	if err != nil {
		t.Fatalf("转审批不应返回错误: %v", err)
	}
	if exec.count() != 0 {
		t.Fatalf("未经批准不得执行，实际执行了 %d 次", exec.count())
	}
	if !res.OK {
		t.Fatalf("转审批必须是成功观测（模型需要如实写「等待批准」）: %+v", res)
	}
	if res.Meta["executed"] != false || res.Meta["approval_status"] != "pending" {
		t.Fatalf("观测必须写明尚未执行，实际 Meta=%v", res.Meta)
	}
	if !strings.Contains(res.Output, "尚未执行") {
		t.Fatalf("观测正文必须包含「尚未执行」，实际 %q", res.Output)
	}
	gotAp := lastApproval(approver)
	if gotAp.ArgsHash != ArgsHash(map[string]interface{}{"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证"}) {
		t.Fatalf("审批单必须带参数哈希（TOCTOU 校验的输入），实际 %q", gotAp.ArgsHash)
	}
	if gotAp.Tool != "job_retry" || gotAp.Risk != RiskWrite || len(gotAp.Targets) != 1 {
		t.Fatalf("审批单必须带工具/风险/目标，实际 %+v", gotAp)
	}
	if len(records) != 1 || records[0].Decision != DecisionApprovalRequired || records[0].ApprovalID == "" {
		t.Fatalf("必须落一条 approval_required 审计并带审批单号，实际 %+v", records)
	}
	if records[0].OK != true {
		t.Fatalf("转审批的审计行必须标记为「未失败」（它没有副作用），实际 OK=%v", records[0].OK)
	}
}

// 没有审批通道时按安全默认**拒绝**，并且不执行、不消耗会话预算。
func TestWriteToolWithoutApproverIsDeniedFailClosed(t *testing.T) {
	exec := &stubRetryExecutor{}
	var records []CallRecord
	reg := NewRegistry(writeTestPolicy(), WithObserver(func(r CallRecord) { records = append(records, r) }))
	if _, err := RegisterWriteTools(reg, WriteSources{Retry: exec}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	_, err := reg.Call(context.Background(), "s-1", "job_retry", map[string]interface{}{
		"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证",
	})
	if err == nil || !errors.Is(err, ErrApprovalUnavailable) {
		t.Fatalf("缺审批通道必须失败（fail closed），实际 %v", err)
	}
	if exec.count() != 0 {
		t.Fatalf("缺审批通道时更不得执行，实际执行 %d 次", exec.count())
	}
	if stats := reg.SessionStats("s-1"); stats.Calls != 0 {
		t.Fatalf("被拒绝的调用不得消耗会话预算，实际 calls=%d", stats.Calls)
	}
	if len(records) != 1 || records[0].Decision != DecisionDenied || records[0].Reason != ReasonApprovalNoChannel {
		t.Fatalf("必须落一条带原因的 denied 审计，实际 %+v", records)
	}
}

// L2 + 命中白名单：免审批直接执行（但审计仍是 allowed，且由网关真的调用执行器）。
func TestAutoApprovedWriteExecutesWithoutApproval(t *testing.T) {
	exec := &stubRetryExecutor{}
	approver := &stubApprover{}
	p := writeTestPolicy()
	p.Autonomy = AutonomyL2
	p.AutoApprove = []string{"job_retry@job:21"}
	// 本例观察的是"白名单是否精确命中"，不是爆炸半径：默认半径是"单会话 1 个目标"，
	// 这里要在一个会话里试两个不同目标（job:21 与 job:22），因此放宽到 2。
	// 半径本身另有专测（TestBlastRadiusGate / TestWriteActionsPerSessionBudget）。
	p.MaxAffectedHostsPerSession = 2
	var records []CallRecord
	reg := NewRegistry(p, WithApprover(approver), WithObserver(func(r CallRecord) { records = append(records, r) }))
	if _, err := RegisterWriteTools(reg, WriteSources{Retry: exec}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	res, err := reg.Call(context.Background(), "s-1", "job_retry", map[string]interface{}{
		"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证",
	})
	if err != nil {
		t.Fatalf("免审批调用不应报错: %v", err)
	}
	if exec.count() != 1 {
		t.Fatalf("命中白名单必须真的执行，实际 %d 次", exec.count())
	}
	if len(approver.got) != 0 {
		t.Fatalf("命中白名单不应生成审批单，实际 %d 张", len(approver.got))
	}
	if !res.OK || res.Meta["executed"] != true {
		t.Fatalf("执行成功的观测必须写明 executed=true，实际 %+v", res)
	}
	if len(records) != 1 || records[0].Decision != DecisionAllowed {
		t.Fatalf("免审批执行也要落 allowed 审计，实际 %+v", records)
	}
	// 同一会话里白名单只覆盖 job:21，换个目标必须回到审批通道。
	if _, err := reg.Call(context.Background(), "s-1", "job_retry", map[string]interface{}{
		"job_id": float64(22), "reason": "另一个任务也需要重跑一次",
	}); err != nil {
		t.Fatalf("换目标后应转审批（不报错）: %v", err)
	}
	if exec.count() != 1 {
		t.Fatalf("白名单外的目标不得直接执行，实际执行 %d 次", exec.count())
	}
	if len(approver.got) != 1 {
		t.Fatalf("白名单外的目标必须生成审批单，实际 %d 张", len(approver.got))
	}
}

// 自治级别 L0 时写工具不注册，因此调用会落到"未知工具"这条分支（可解释）。
func TestAutonomyL0WriteToolIsNotCallable(t *testing.T) {
	p := writeTestPolicy()
	p.Autonomy = AutonomyL0
	reg := NewRegistry(p)
	if _, err := RegisterWriteTools(reg, WriteSources{Retry: &stubRetryExecutor{}}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	_, err := reg.Call(context.Background(), "s-1", "job_retry", map[string]interface{}{
		"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证",
	})
	if err == nil || !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("L0 下 job_retry 应不可见（未知工具），实际 %v", err)
	}
}

// 审批单生成失败同样不执行（且把已占用的配额还回去）。
func TestApproverFailureDoesNotExecute(t *testing.T) {
	exec := &stubRetryExecutor{}
	approver := &stubApprover{err: errors.New("写审批单失败")}
	reg := NewRegistry(writeTestPolicy(), WithApprover(approver))
	if _, err := RegisterWriteTools(reg, WriteSources{Retry: exec}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	_, err := reg.Call(context.Background(), "s-1", "job_retry", map[string]interface{}{
		"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证",
	})
	if err == nil || !errors.Is(err, ErrApprovalUnavailable) {
		t.Fatalf("审批单生成失败必须拒绝执行，实际 %v", err)
	}
	if exec.count() != 0 {
		t.Fatalf("审批单没生成，绝不能执行，实际 %d 次", exec.count())
	}
	if stats := reg.SessionStats("s-1"); stats.Calls != 0 {
		t.Fatalf("失败路径不得消耗会话预算，实际 calls=%d", stats.Calls)
	}
}

// 写动作会话预算：同一会话内的写动作次数受 max_write_actions_per_session 限制。
func TestWriteActionsPerSessionBudget(t *testing.T) {
	exec := &stubRetryExecutor{}
	approver := &stubApprover{}
	p := writeTestPolicy()
	p.MaxWriteActionsPerSession = 1
	// 关掉"每工具上限"的干扰，只看写动作预算。
	p.MaxCallsPerTool = 10
	var records []CallRecord
	reg := NewRegistry(p, WithApprover(approver), WithObserver(func(r CallRecord) { records = append(records, r) }))
	if _, err := RegisterWriteTools(reg, WriteSources{Retry: exec}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	args := map[string]interface{}{"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证"}
	if _, err := reg.Call(context.Background(), "s-1", "job_retry", args); err != nil {
		t.Fatalf("第一次转审批不应报错: %v", err)
	}
	_, err := reg.Call(context.Background(), "s-1", "job_retry", map[string]interface{}{
		"job_id": float64(22), "reason": "第二个任务也需要重跑一次",
	})
	if err == nil {
		t.Fatalf("超过写动作预算必须被拒")
	}
	last := records[len(records)-1]
	if last.Decision != DecisionDenied || last.Reason != ReasonBlastRadius {
		t.Fatalf("超预算必须落 denied 审计并写明爆炸半径原因，实际 %+v", last)
	}
}

// ExecuteApproved：已经拿到审批单的执行路径——跳过审批闸、但其余闸门照走，
// 审计记为 approval_executed 且带审批单号，并且**不重复扣**写动作预算。
func TestExecuteApprovedRunsAndAuditsOnce(t *testing.T) {
	exec := &stubRetryExecutor{}
	approver := &stubApprover{}
	p := writeTestPolicy()
	// 半径与写动作预算都卡在 1：如果执行时重复扣预算，这里会直接被拒，用例就红了。
	p.MaxAffectedHostsPerSession = 1
	p.MaxWriteActionsPerSession = 1
	var records []CallRecord
	reg := NewRegistry(p, WithApprover(approver), WithObserver(func(r CallRecord) { records = append(records, r) }))
	if _, err := RegisterWriteTools(reg, WriteSources{Retry: exec}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	args := map[string]interface{}{"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证"}

	// 第一步：Call 生成审批单（这一步消耗预算）。
	if _, err := reg.Call(context.Background(), "s-1", "job_retry", args); err != nil {
		t.Fatalf("转审批不应报错: %v", err)
	}
	if exec.count() != 0 {
		t.Fatalf("转审批阶段不得执行")
	}
	// 第二步：带着审批单号执行（不得因为预算已扣而被拒）。
	res, err := reg.ExecuteApproved(context.Background(), "s-1", "job_retry", args, "ap-1")
	if err != nil {
		t.Fatalf("已批准的执行不应被预算拒掉（预算在转审批时已扣）: %v", err)
	}
	if exec.count() != 1 {
		t.Fatalf("已批准必须真的执行一次，实际 %d 次", exec.count())
	}
	if !res.OK || res.Meta["executed"] != true {
		t.Fatalf("执行结果必须标记 executed=true，实际 %+v", res)
	}
	last := records[len(records)-1]
	if last.Decision != DecisionApprovalExecuted || last.ApprovalID != "ap-1" {
		t.Fatalf("审计必须记为 approval_executed 且带审批单号，实际 %+v", last)
	}
	if len(approver.got) != 1 {
		t.Fatalf("执行路径不得再生成审批单，实际 %d 张", len(approver.got))
	}
}

// 已批准的只读工具调用必须被拒：审批单只对写动作有意义。
func TestExecuteApprovedRejectsReadonlyTool(t *testing.T) {
	var records []CallRecord
	reg := NewRegistry(writeTestPolicy(), WithObserver(func(r CallRecord) { records = append(records, r) }))
	reg.MustRegister(newStub("read_thing"))
	if _, err := reg.ExecuteApproved(context.Background(), "s-1", "read_thing", map[string]interface{}{"q": "x"}, "ap-1"); err == nil {
		t.Fatalf("拿审批单执行只读工具必须被拒")
	}
	if len(records) != 1 || records[0].Reason != ReasonApprovalMissed {
		t.Fatalf("必须落一条带原因的 denied 审计，实际 %+v", records)
	}
}

// 审批单号为空不得执行（防止"没有单子也走执行路径"）。
func TestExecuteApprovedRequiresApprovalID(t *testing.T) {
	exec := &stubRetryExecutor{}
	reg := NewRegistry(writeTestPolicy())
	if _, err := RegisterWriteTools(reg, WriteSources{Retry: exec}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if _, err := reg.ExecuteApproved(context.Background(), "s-1", "job_retry",
		map[string]interface{}{"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证"}, "  "); err == nil {
		t.Fatalf("空审批单号必须被拒")
	}
	if exec.count() != 0 {
		t.Fatalf("没有审批单不得执行")
	}
}

// 网关本身不做幂等（这是刻意的分层）：同一张单子再执行一次，网关会照做。
// 这个"会照做"正是把幂等责任明确压给控制面 CAS 的依据（见 admin 的 ApprovalService）。
func TestExecuteApprovedIsNotIdempotentByItself(t *testing.T) {
	exec := &stubRetryExecutor{}
	reg := NewRegistry(writeTestPolicy())
	if _, err := RegisterWriteTools(reg, WriteSources{Retry: exec}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	args := map[string]interface{}{"job_id": float64(21), "reason": "上次失败原因未查清，重跑取证"}
	for i := 1; i <= 2; i++ {
		if _, err := reg.ExecuteApproved(context.Background(), "s-1", "job_retry", args, "ap-1"); err != nil {
			t.Fatalf("第 %d 次执行不应报错: %v", i, err)
		}
	}
	if exec.count() != 2 {
		t.Fatalf("网关不做幂等（设计如此）：预期执行 2 次，实际 %d 次", exec.count())
	}
}

// specByName 从 Specs 里按名取一条（测试辅助）。
func specByName(specs []llm.ToolSpec, name string) (llm.ToolSpec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return s, true
		}
	}
	return llm.ToolSpec{}, false
}
