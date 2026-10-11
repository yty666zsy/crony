package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 审批控制面的哨兵错误：调用方（handler）据此映射成业务码。
//
// 这些错误全部是**可解释的拒绝**：每一个都对应一种"为什么不能执行"，
// 而不是一个笼统的 500。写动作的失败如果不可解释，运维就只能靠猜。
var (
	// ErrApprovalNotFound 单号不存在。
	ErrApprovalNotFound = errors.New("审批单不存在")
	// ErrApprovalConflict 单子已被别人决策 / 已执行（并发或重复点击）。
	ErrApprovalConflict = errors.New("审批单状态已变更，本次决策未生效")
	// ErrApprovalExpired 已过期：写动作的合理性依赖当时的现场。
	ErrApprovalExpired = errors.New("审批单已过期，不允许再执行")
	// ErrApprovalMismatch 参数哈希对不上（批准 A、执行 B 的 TOCTOU 尝试）。
	ErrApprovalMismatch = errors.New("审批单参数与待执行参数不一致，拒绝执行")
	// ErrApprovalToolGone 工具已不在能力面内（策略收紧或工具下线）。
	ErrApprovalToolGone = errors.New("审批单对应的工具当前不可用，拒绝执行")
	// ErrApprovalPolicyChanged 批准之后策略被收紧（allow_write 关闭 / 自治级别降级）。
	ErrApprovalPolicyChanged = errors.New("批准后策略已收紧（写动作不再被允许），拒绝执行")
	// ErrApprovalNoExecutor 执行器未装配（按安全默认拒绝）。
	ErrApprovalNoExecutor = errors.New("审批执行器未装配，拒绝执行")
)

// ApprovalStore 审批单持久化（真实现是 gorm，测试用内存替身）。
type ApprovalStore interface {
	Create(req *models.ApprovalRequest) error
	Find(approvalID string) (*models.ApprovalRequest, error)
	List(status, tool, targetNode string, page, pageSize int) ([]models.ApprovalRequest, int64, error)
	// Decide 是 CAS：只有 pending → approved/rejected/canceled 会生效，返回"是否抢到"。
	Decide(approvalID, toStatus, decidedBy, note string, now int64) (bool, error)
	// MarkExecuted 是 CAS：只有 approved → executed/execution_failed 会生效。
	MarkExecuted(approvalID, status, result string, now int64) (bool, error)
	// MarkBlocked 是 CAS：approved → blocked，且**不写 executed_at**
	// （复核未通过 = 动作从未执行，不能留下执行时间戳）。
	MarkBlocked(approvalID, result string) (bool, error)
	// ExpirePending 惰性清理过期单（返回生效行数）。
	ExpirePending(now int64) (int64, error)
}

// ApprovalExecutor 已批准写动作的执行方（真实现是 Agent 运行时的工具网关）。
//
// 注意接口里**没有**"直接执行工具"的口子：执行必须经过 tools.Registry.ExecuteApproved，
// 这样审计里才会有 approval_executed 这条事实。
type ApprovalExecutor interface {
	ExecuteApprovedTool(ctx context.Context, meta tools.RunMeta, sessionID, tool string,
		args map[string]interface{}, approvalID string) (tools.Result, error)
	// ToolNames 当前能力面（用于"批准的工具是否还在"这一项复核）。
	ToolNames() []string
}

// DecisionResult 一次人工决策的结果（含"批准并执行"的执行事实）。
type DecisionResult struct {
	Request *models.ApprovalRequest `json:"request"`
	// Executed 是否真的执行了（驳回/取消/过期时为 false）。
	Executed bool `json:"executed"`
	// Result 执行观测（Executed=true 时才有意义）。
	Result tools.Result `json:"result"`
	// Warning 决策生效但过程有意外时的提示（例如：执行成功但状态回写失败）。
	// 它必须往上报：把"执行了但记录没写上"静默成成功，是最坏的一种乐观。
	Warning string `json:"warning,omitempty"`
}

// ApprovalService 写动作审批控制面。
//
// 它承担四件事，每一件都对应一个真实事故模式：
//  1. **落单**：把模型的自述理由、参数哈希、影响面存下来（人批的是"理由"而不是工具名）；
//  2. **CAS 决策**：两个人同时点批准，只有一个成功（否则一次批准变两次执行）；
//  3. **执行前复核**：参数哈希、工具是否仍在能力面、策略是否仍允许（TOCTOU）；
//  4. **一次批准一次执行**：approved → executed 也是 CAS，执行结果无论成败都回写。
type ApprovalService struct {
	store  ApprovalStore
	exec   ApprovalExecutor
	policy tools.Policy
	// now 可注入（测试要观察 TTL 边界与过期行为）。
	now func() time.Time
	// execTimeout 单次执行的上限；避免一次执行把 HTTP 请求拖死。
	execTimeout time.Duration
}

// NewApprovalService 装配审批控制面（policy 传运行时生效的策略，用于执行前复核）。
func NewApprovalService(store ApprovalStore, exec ApprovalExecutor, policy tools.Policy) *ApprovalService {
	return &ApprovalService{
		store:       store,
		exec:        exec,
		policy:      policy.Normalize(),
		now:         time.Now,
		execTimeout: 45 * time.Second,
	}
}

// SetClock 注入时钟（仅测试用）。
func (s *ApprovalService) SetClock(now func() time.Time) { s.now = now }

// RequestApproval 实现 tools.Approver：生成一张待审批单。
//
// 返回的成功观测由工具网关负责包装（见 tools.approvalObservation），
// 所以这里只关心"单子有没有落下来"。
func (s *ApprovalService) RequestApproval(_ context.Context, in tools.ApprovalInput) (tools.ApprovalOutcome, error) {
	if s == nil || s.store == nil {
		return tools.ApprovalOutcome{}, fmt.Errorf("审批服务未初始化")
	}
	tool := strings.TrimSpace(in.Tool)
	if tool == "" {
		return tools.ApprovalOutcome{}, fmt.Errorf("审批单缺少工具名")
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = models.DefaultApprovalTTL
	}
	now := s.now()
	argsJSON, err := marshalArgs(in.Args)
	if err != nil {
		return tools.ApprovalOutcome{}, fmt.Errorf("审批单参数无法序列化: %w", err)
	}
	req := &models.ApprovalRequest{
		ApprovalID:  models.NewApprovalID(),
		SessionID:   in.SessionID,
		Tool:        tool,
		Risk:        string(in.Risk),
		ArgsJSON:    argsJSON,
		ArgsHash:    in.ArgsHash,
		Reason:      llm.Truncate(in.Reason, 2000),
		TargetNode:  firstOrEmpty(in.Targets),
		Impact:      llm.Truncate(in.Impact, 512),
		RequestedBy: llm.Truncate(in.RequestedBy, 128),
		Status:      models.ApprovalStatusPending,
		ExpiresAt:   now.Add(ttl).Unix(),
		Created:     now.Unix(),
		CreatedMS:   now.UnixMilli(),
		RunID:       in.RunID,
	}
	if err := s.store.Create(req); err != nil {
		return tools.ApprovalOutcome{}, fmt.Errorf("写入审批单失败: %w", err)
	}
	return tools.ApprovalOutcome{
		ApprovalID: req.ApprovalID,
		Status:     req.Status,
		ExpiresAt:  req.ExpiresAt,
		Summary: fmt.Sprintf("已提交人工审批（单号 %s，%d 分钟后过期）：%s",
			req.ApprovalID, int(ttl.Minutes()), llm.Truncate(in.Impact, 200)),
	}, nil
}

// Decide 人工决策。approve=true 时**批准即执行**（一条 HTTP 请求走完"批 + 执行"）。
//
// 为什么要合成一步而不是"批准"与"执行"两个接口：
// 分开会让"批准了但从没执行"成为常态——那种状态在控制台上无法区分
// "运维还没点执行"和"执行静默失败了"，于是"批准"就变成了心理安慰。
func (s *ApprovalService) Decide(ctx context.Context, approvalID string, approve bool,
	decidedBy, note string) (*DecisionResult, error) {
	if s == nil || s.store == nil {
		return nil, ErrApprovalNotFound
	}
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return nil, fmt.Errorf("%w: 缺少单号", ErrApprovalNotFound)
	}
	decidedBy = strings.TrimSpace(decidedBy)
	if decidedBy == "" {
		// 谁批的必须能追到人：没有操作人的"批准"在事后审计里等于没有批准人。
		return nil, fmt.Errorf("缺少操作人")
	}
	now := s.now()
	// 惰性过期：读之前顺手清一遍（与 models.ExpirePendingApprovals 的设计一致）。
	_, _ = s.store.ExpirePending(now.Unix())

	req, err := s.store.Find(approvalID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrApprovalNotFound, approvalID)
	}
	switch req.Status {
	case models.ApprovalStatusPending:
	case models.ApprovalStatusExpired:
		return &DecisionResult{Request: req}, fmt.Errorf("%w: %s", ErrApprovalExpired, approvalID)
	default:
		// 已批准/已执行/已驳回……都不允许再决策（尤其"已执行"后再批准 = 放大器）。
		return &DecisionResult{Request: req}, fmt.Errorf("%w: 当前状态 %s", ErrApprovalConflict, req.Status)
	}
	if req.ExpiresAt > 0 && now.Unix() >= req.ExpiresAt {
		// 兜底：惰性清理没覆盖到的边界（刚好在这一刻过期）。
		return &DecisionResult{Request: req}, fmt.Errorf("%w: %s", ErrApprovalExpired, approvalID)
	}

	if !approve {
		ok, err := s.store.Decide(approvalID, models.ApprovalStatusRejected, decidedBy, note, now.Unix())
		if err != nil {
			return nil, err
		}
		if !ok {
			// CAS 没抢到：别人先决策了。**不执行**，并如实告知。
			return nil, fmt.Errorf("%w: %s", ErrApprovalConflict, approvalID)
		}
		req.Status = models.ApprovalStatusRejected
		req.DecidedBy, req.DecidedAt, req.DecisionNote = decidedBy, now.Unix(), note
		return &DecisionResult{Request: req, Executed: false}, nil
	}

	// 批准：CAS 抢决策权（抢不到就绝不允许执行）。
	ok, err := s.store.Decide(approvalID, models.ApprovalStatusApproved, decidedBy, note, now.Unix())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrApprovalConflict, approvalID)
	}
	req.Status = models.ApprovalStatusApproved
	req.DecidedBy, req.DecidedAt, req.DecisionNote = decidedBy, now.Unix(), note

	// 复核：批准发生在过去，现在是否还能执行必须重新判断。
	args, verifyErr := s.verifyBeforeExecute(req)
	if verifyErr != nil {
		s.markBlocked(req, verifyErr, now)
		return &DecisionResult{Request: req, Executed: false}, verifyErr
	}
	if s.exec == nil {
		s.markBlocked(req, ErrApprovalNoExecutor, now)
		return &DecisionResult{Request: req, Executed: false}, ErrApprovalNoExecutor
	}

	execCtx, cancel := context.WithTimeout(ctx, s.execTimeout)
	defer cancel()
	// 审计归属：把 AgentKind/RunID 带上，控制台上"审批执行的审计行"才能与诊断会话对齐。
	meta := tools.RunMeta{AgentKind: req.RequestedBy, RunID: req.RunID}
	res, execErr := s.exec.ExecuteApprovedTool(execCtx, meta, req.SessionID, req.Tool, args, req.ApprovalID)

	finalStatus := models.ApprovalStatusExecuted
	resultText := summarizeResult(res)
	if execErr != nil {
		finalStatus = models.ApprovalStatusExecutionFailed
		resultText = llm.Redact(execErr.Error())
	} else if !res.OK {
		finalStatus = models.ApprovalStatusExecutionFailed
		if resultText == "" {
			resultText = res.Err
		}
	}
	marked, err := s.store.MarkExecuted(approvalID, finalStatus, llm.Truncate(resultText, 4000), s.now().Unix())
	out := &DecisionResult{Request: req, Executed: true, Result: res}
	if err != nil {
		out.Warning = fmt.Sprintf("执行已发生，但状态回写失败（单号 %s 可能仍显示 approved）: %v", approvalID, err)
		return out, nil
	}
	if !marked {
		// approved → executed 的 CAS 没生效：理论上不会发生（我们刚拿到 approved），
		// 真发生了就必须报警而不是静默——因为"执行了但没有终态记录"会让同一张单子被再执行。
		out.Warning = fmt.Sprintf("执行已发生，但状态 CAS 未生效（单号 %s 当前状态可能已变化），请人工核查是否重复执行", approvalID)
		return out, nil
	}
	req.Status = finalStatus
	req.ExecutedAt = s.now().Unix()
	req.ExecutionResult = resultText
	if execErr != nil {
		return out, execErr
	}
	return out, nil
}

// Cancel 发起方主动撤销（只有 pending 能撤）。
func (s *ApprovalService) Cancel(approvalID, by, note string) (*models.ApprovalRequest, error) {
	if s == nil || s.store == nil {
		return nil, ErrApprovalNotFound
	}
	now := s.now()
	_, _ = s.store.ExpirePending(now.Unix())
	req, err := s.store.Find(strings.TrimSpace(approvalID))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrApprovalNotFound, approvalID)
	}
	if req.Status != models.ApprovalStatusPending {
		return req, fmt.Errorf("%w: 当前状态 %s", ErrApprovalConflict, req.Status)
	}
	ok, err := s.store.Decide(req.ApprovalID, models.ApprovalStatusCanceled, by, note, now.Unix())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrApprovalConflict, approvalID)
	}
	req.Status = models.ApprovalStatusCanceled
	req.DecidedBy, req.DecidedAt, req.DecisionNote = by, now.Unix(), note
	return req, nil
}

// Find 查一张单子（读之前惰性清理过期）。
func (s *ApprovalService) Find(approvalID string) (*models.ApprovalRequest, error) {
	if s == nil || s.store == nil {
		return nil, ErrApprovalNotFound
	}
	_, _ = s.store.ExpirePending(s.now().Unix())
	req, err := s.store.Find(strings.TrimSpace(approvalID))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrApprovalNotFound, approvalID)
	}
	return req, nil
}

// List 审批台列表（读之前惰性清理过期，保证"过期"不是等到有人点才出现）。
func (s *ApprovalService) List(status, tool, targetNode string, page, pageSize int) ([]models.ApprovalRequest, int64, error) {
	if s == nil || s.store == nil {
		return nil, 0, nil
	}
	_, _ = s.store.ExpirePending(s.now().Unix())
	return s.store.List(status, tool, targetNode, page, pageSize)
}

// verifyBeforeExecute 执行前的复核（TOCTOU 的正面拦截）。
//
// 逐项说明为什么要复核：
//  1. 参数哈希：库里存的是被批准的那份参数；任何人（含模型）在批准与执行之间改了参数，
//     哈希就对不上——**批准 A 执行 B** 是审批制里最经典的越权方式；
//  2. 工具仍在能力面：策略可能在批准后被收紧（allow_write 关掉），工具可能下线；
//  3. 策略仍允许写：与 2 同理，但失败原因要说清是"策略收紧"而不是"工具没了"。
func (s *ApprovalService) verifyBeforeExecute(req *models.ApprovalRequest) (map[string]interface{}, error) {
	args := map[string]interface{}{}
	if strings.TrimSpace(req.ArgsJSON) != "" {
		if err := json.Unmarshal([]byte(req.ArgsJSON), &args); err != nil {
			return nil, fmt.Errorf("%w: 存档参数无法解析（%v）", ErrApprovalMismatch, err)
		}
	}
	if got := tools.ArgsHash(args); got != req.ArgsHash {
		return nil, fmt.Errorf("%w: 存档哈希 %s，实算 %s", ErrApprovalMismatch, llm.Truncate(req.ArgsHash, 16), llm.Truncate(got, 16))
	}
	if s.exec != nil {
		names := s.exec.ToolNames()
		found := false
		for _, n := range names {
			if n == req.Tool {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: %s 已不在能力面内（当前: %s）", ErrApprovalToolGone, req.Tool, strings.Join(names, ", "))
		}
	}
	if !s.policy.AllowWrite || !s.policy.Autonomy.AllowsWriteTools() {
		return nil, fmt.Errorf("%w: 当前策略 allow_write=%v autonomy=%s",
			ErrApprovalPolicyChanged, s.policy.AllowWrite, s.policy.Autonomy)
	}
	return args, nil
}

// markBlocked 把"执行前复核未通过"写到单子上（approved → blocked）。
//
// 刻意**不写 executed_at**：复核未通过 = 动作从未交给执行器。
// 这一条是验收 AC-18 抓出来的：初版复用了 execution_failed 并写了执行时间戳，
// 于是库里出现"状态=已执行但失败 + 有时间戳"的行，而事实上什么都没发生。
func (s *ApprovalService) markBlocked(req *models.ApprovalRequest, cause error, _ time.Time) {
	if s.store == nil {
		return
	}
	reason := llm.Redact("执行前复核未通过: " + cause.Error())
	_, _ = s.store.MarkBlocked(req.ApprovalID, llm.Truncate(reason, 4000))
	req.Status = models.ApprovalStatusBlocked
	req.ExecutionResult = reason
}

// marshalArgs 参数序列化（保持稳定：同一份参数永远同一串字节，
// 否则哈希校验会随字段顺序抖动而误报 TOCTOU）。
func marshalArgs(args map[string]interface{}) (string, error) {
	if args == nil {
		return "{}", nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// summarizeResult 执行结果的存档文本（人看的一行 + 关键事实）。
func summarizeResult(res tools.Result) string {
	parts := []string{}
	if res.Summary != "" {
		parts = append(parts, res.Summary)
	}
	if !res.OK && res.Err != "" {
		parts = append(parts, "错误: "+res.Err)
	}
	if res.Output != "" {
		parts = append(parts, "输出: "+llm.Truncate(res.Output, 1500))
	}
	if len(parts) == 0 {
		if res.OK {
			return "已执行（无输出）"
		}
		return "执行失败（无原因输出）"
	}
	return strings.Join(parts, " | ")
}

// firstOrEmpty 取第一个目标（审批单的 TargetNode 字段是单值，多目标时取首个并保留影响面摘要）。
func firstOrEmpty(targets []string) string {
	for _, t := range targets {
		if strings.TrimSpace(t) != "" {
			return llm.Truncate(t, 128)
		}
	}
	return ""
}
