package handler

import (
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/middlerware"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 本文件是阶段 4.2（写动作审批）的接口层：
//   - /agent/approval/{search,find,approve,reject,cancel}：人工审批台；
//   - /agent/audit/search：工具调用审计（网关每次调用一条，含被拒的）。
//
// 两条硬约束贯穿本文件：
//  1. **操作人来自 token，不来自请求体**。请求体里的 decided_by 一律忽略：
//     谁批的必须能追到人，而"让客户端自报身份"等于不设防。
//  2. **参数对外一律脱敏**（tools.ArgsJSON 原文只用于执行校验）：
//     库里存事实，接口做裁剪。

// ---------- 视图 ----------

type agentApprovalView struct {
	ApprovalID   string `json:"approval_id"`
	SessionID    string `json:"session_id"`
	RunID        string `json:"run_id"`
	Tool         string `json:"tool"`
	Risk         string `json:"risk"`
	RiskText     string `json:"risk_text"`
	Reason       string `json:"reason"`
	ArgsText     string `json:"args_text"`
	ArgsHash     string `json:"args_hash"`
	TargetNode   string `json:"target_node"`
	Impact       string `json:"impact"`
	RequestedBy  string `json:"requested_by"`
	Status       string `json:"status"`
	StatusText   string `json:"status_text"`
	DecidedBy    string `json:"decided_by"`
	DecidedAt    string `json:"decided_at"`
	DecisionNote string `json:"decision_note"`
	ExecutedAt   string `json:"executed_at"`
	ExecResult   string `json:"execution_result"`
	// ExpiresAt/RemainSeconds 必须给前端：审批台的核心动作是"快过期的先看"。
	ExpiresAt     int64 `json:"expires_at"`
	RemainSeconds int64 `json:"remain_seconds"`
	Created       int64 `json:"created"`
	CreatedMS     int64 `json:"created_ms"`
	// Executed 是否真的执行过（前端据此区分"批准了但没跑"与"跑了"）。
	Executed bool `json:"executed"`
}

func approvalView(a *models.ApprovalRequest, nowUnix int64) agentApprovalView {
	if a == nil {
		return agentApprovalView{}
	}
	remain := int64(0)
	if a.Status == models.ApprovalStatusPending && a.ExpiresAt > 0 {
		remain = a.ExpiresAt - nowUnix
		if remain < 0 {
			remain = 0
		}
	}
	executed := a.Status == models.ApprovalStatusExecuted || a.Status == models.ApprovalStatusExecutionFailed
	return agentApprovalView{
		ApprovalID:    a.ApprovalID,
		SessionID:     a.SessionID,
		RunID:         a.RunID,
		Tool:          a.Tool,
		Risk:          a.Risk,
		RiskText:      approvalRiskText(a.Risk),
		Reason:        a.Reason,
		ArgsText:      llm.Redact(a.ArgsJSON),
		ArgsHash:      a.ArgsHash,
		TargetNode:    a.TargetNode,
		Impact:        a.Impact,
		RequestedBy:   a.RequestedBy,
		Status:        a.Status,
		StatusText:    approvalStatusText(a.Status),
		DecidedBy:     a.DecidedBy,
		DecidedAt:     unixText(a.DecidedAt),
		DecisionNote:  a.DecisionNote,
		ExecutedAt:    unixText(a.ExecutedAt),
		ExecResult:    a.ExecutionResult,
		ExpiresAt:     a.ExpiresAt,
		RemainSeconds: remain,
		Created:       a.Created,
		CreatedMS:     a.CreatedMS,
		Executed:      executed,
	}
}

// approvalStatusText 状态的中文解释必须由后端给（前端只渲染不推断）。
func approvalStatusText(s string) string {
	switch s {
	case models.ApprovalStatusPending:
		return "待审批"
	case models.ApprovalStatusApproved:
		return "已批准（尚未执行）"
	case models.ApprovalStatusRejected:
		return "已驳回"
	case models.ApprovalStatusExpired:
		return "已过期（超时未决策，不可再执行）"
	case models.ApprovalStatusCanceled:
		return "已撤销"
	case models.ApprovalStatusExecuted:
		return "已执行"
	case models.ApprovalStatusExecutionFailed:
		return "已执行但失败"
	case models.ApprovalStatusBlocked:
		// 与"已执行但失败"必须能区分：这一类的动作从未发生（executed_at 为空）。
		return "已拦截（执行前复核未通过，未执行）"
	}
	return s
}

func approvalRiskText(r string) string {
	switch tools.RiskLevel(r) {
	case tools.RiskWrite:
		return "可逆写（有副作用，可回退）"
	case tools.RiskDestructive:
		return "不可逆写（可能无法回退）"
	case tools.RiskRead:
		return "只读（不应出现在审批单里）"
	}
	return r
}

// unixText 秒级时间戳 → 可读文本（0 表示"未发生"，必须显示成空而不是 1970）。
// 复用 agent_view.go 的 rfc3339OrZero：同一套时间口径，前端才能统一按浏览器时区渲染。
func unixText(sec int64) string { return rfc3339OrZero(sec) }

// nowUnix 当前秒级时间戳（视图层计算"还剩多少秒过期"用）。
func nowUnix() int64 { return time.Now().Unix() }

// pageOr/pageSizeOr 回显分页参数（models 层会兜默认值，这里保持与查询一致）。
func pageOr(p int) int {
	if p <= 0 {
		return 1
	}
	return p
}

func pageSizeOr(p int) int {
	if p <= 0 || p > 200 {
		return 20
	}
	return p
}

type agentAuditView struct {
	AuditID       string `json:"audit_id"`
	SessionID     string `json:"session_id"`
	AgentKind     string `json:"agent_kind"`
	RunID         string `json:"run_id"`
	Tool          string `json:"tool"`
	Risk          string `json:"risk"`
	Decision      string `json:"decision"`
	DecisionText  string `json:"decision_text"`
	Reason        string `json:"reason"`
	ApprovalID    string `json:"approval_id"`
	TargetNode    string `json:"target_node"`
	OK            bool   `json:"ok"`
	Err           string `json:"err"`
	DurationMS    int64  `json:"duration_ms"`
	OutputBytes   int    `json:"output_bytes"`
	EvidenceCount int    `json:"evidence_count"`
	ArgsText      string `json:"args_text"`
	Created       int64  `json:"created"`
	CreatedMS     int64  `json:"created_ms"`
}

func auditView(a *models.AgentToolAudit) agentAuditView {
	if a == nil {
		return agentAuditView{}
	}
	return agentAuditView{
		AuditID:       a.AuditID,
		SessionID:     a.SessionID,
		AgentKind:     a.AgentKind,
		RunID:         a.RunID,
		Tool:          a.Tool,
		Risk:          a.Risk,
		Decision:      a.Decision,
		DecisionText:  auditDecisionText(a.Decision),
		Reason:        a.Reason,
		ApprovalID:    a.ApprovalID,
		TargetNode:    a.TargetNode,
		OK:            a.OK,
		Err:           a.Err,
		DurationMS:    a.DurationMS,
		OutputBytes:   a.OutputBytes,
		EvidenceCount: a.EvidenceCount,
		ArgsText:      llm.Redact(a.ArgsJSON),
		Created:       a.Created,
		CreatedMS:     a.CreatedMS,
	}
}

// auditDecisionText 决策的解释：注意 approval_required 是**成功**的一步
// （它没有副作用），前端不能把它渲染成错误。
func auditDecisionText(d string) string {
	switch d {
	case tools.DecisionAllowed:
		return "已放行并执行"
	case tools.DecisionDenied:
		return "被拒绝（未执行）"
	case tools.DecisionApprovalRequired:
		return "已转人工审批（尚未执行）"
	case tools.DecisionApprovalExecuted:
		return "按审批单执行"
	}
	return d
}

// ---------- §4.2 审批台 ----------

// requireApprovalService 取审批控制面（未启用 Agent 能力时按 1007/1011 如实返回）。
func requireApprovalService(c *gin.Context, tag string) (*service.ApprovalService, bool) {
	svc := service.DefaultApprovalService()
	if svc != nil {
		return svc, true
	}
	code, msg := agentDisabledCode()
	resp.FailWithMessage(code, tag+" "+msg, c)
	return nil, false
}

// ApprovalSearch 审批台列表（默认"待审批优先、最快到期优先"，由 models 排序保证）。
func (a *AgentRouter) ApprovalSearch(c *gin.Context) {
	var req struct {
		Status     string `json:"status"`
		Tool       string `json:"tool"`
		TargetNode string `json:"target_node"`
		Page       int    `json:"page"`
		PageSize   int    `json:"page_size"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/approval/search] 请求参数错误: "+err.Error(), c)
		return
	}
	svc, ok := requireApprovalService(c, "[agent/approval/search]")
	if !ok {
		return
	}
	list, total, err := svc.List(strings.TrimSpace(req.Status), strings.TrimSpace(req.Tool),
		strings.TrimSpace(req.TargetNode), req.Page, req.PageSize)
	if err != nil {
		logger.GetLogger().Error("[agent/approval/search] 查询审批单失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/approval/search] 查询审批单失败", c)
		return
	}
	views := make([]agentApprovalView, 0, len(list))
	for i := range list {
		views = append(views, approvalView(&list[i], nowUnix()))
	}
	resp.OkWithData(gin.H{"list": views, "total": total, "page": pageOr(req.Page), "page_size": pageSizeOr(req.PageSize)}, c)
}

// ApprovalFind 查一张单子（含执行结果）。
func (a *AgentRouter) ApprovalFind(c *gin.Context) {
	var req struct {
		ApprovalID string `json:"approval_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/approval/find] 请求参数错误: "+err.Error(), c)
		return
	}
	if strings.TrimSpace(req.ApprovalID) == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/approval/find] approval_id 不能为空", c)
		return
	}
	svc, ok := requireApprovalService(c, "[agent/approval/find]")
	if !ok {
		return
	}
	row, err := svc.Find(req.ApprovalID)
	if err != nil {
		resp.FailWithMessage(approvalErrorCode(err), "[agent/approval/find] "+approvalErrorMsg(err), c)
		return
	}
	resp.OkWithData(gin.H{"approval": approvalView(row, nowUnix())}, c)
}

// ApprovalApprove 批准并执行（一次请求走完"批 + 执行"）。
func (a *AgentRouter) ApprovalApprove(c *gin.Context) { a.decide(c, true) }

// ApprovalReject 驳回（不执行）。
func (a *AgentRouter) ApprovalReject(c *gin.Context) { a.decide(c, false) }

// decide 审批决策的公共实现。
//
// decided_by 一律取登录用户：请求体里的同名字段被刻意忽略（见文件头约束 1）。
func (a *AgentRouter) decide(c *gin.Context, approve bool) {
	tag := "[agent/approval/reject]"
	if approve {
		tag = "[agent/approval/approve]"
	}
	var req struct {
		ApprovalID string `json:"approval_id"`
		Note       string `json:"note"`
		// DecidedBy 仅用于兼容旧前端；**不采信**（见文件头约束 1）。
		DecidedBy string `json:"decided_by"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	if strings.TrimSpace(req.ApprovalID) == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" approval_id 不能为空", c)
		return
	}
	operator := middlerware.CurrentUserName(c)
	if operator == "" {
		resp.FailWithMessage(resp.ErrorForbidden, tag+" 无法从登录态取得操作人，拒绝执行（谁批的必须能追到人）", c)
		return
	}
	svc, ok := requireApprovalService(c, tag)
	if !ok {
		return
	}
	res, err := svc.Decide(c.Request.Context(), req.ApprovalID, approve, operator, strings.TrimSpace(req.Note))
	if err != nil {
		logger.GetLogger().Warn(tag + " 决策未生效: " + err.Error())
		code := approvalErrorCode(err)
		// 执行已发生但状态回写有问题时：**不能**只报错，必须把执行事实一并返回。
		data := gin.H{"executed": false}
		if res != nil {
			data["approval"] = approvalView(res.Request, nowUnix())
			data["executed"] = res.Executed
			if res.Warning != "" {
				data["warning"] = res.Warning
			}
		}
		resp.Result(code, data, tag+" "+approvalErrorMsg(err), c)
		return
	}
	data := gin.H{
		"approval": approvalView(res.Request, nowUnix()),
		"executed": res.Executed,
		"result":   toolResultView(res.Result),
	}
	if res.Warning != "" {
		data["warning"] = res.Warning
	}
	// 执行失败是可解释的**业务结果**：HTTP 仍是 200，code 指出是哪一类失败。
	if res.Executed && !res.Result.OK {
		resp.Result(resp.ErrorAgentApprovalExecFailed, data, tag+" 已执行但失败: "+approvalErrorMsg(errors.New(res.Result.Err)), c)
		return
	}
	if approve {
		resp.Result(resp.SUCCESS, data, tag+" 已批准并执行", c)
		return
	}
	resp.Result(resp.SUCCESS, data, tag+" 已驳回（未执行）", c)
}

// ApprovalCancel 撤销一张待审批单。
func (a *AgentRouter) ApprovalCancel(c *gin.Context) {
	var req struct {
		ApprovalID string `json:"approval_id"`
		Note       string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/approval/cancel] 请求参数错误: "+err.Error(), c)
		return
	}
	if strings.TrimSpace(req.ApprovalID) == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/approval/cancel] approval_id 不能为空", c)
		return
	}
	operator := middlerware.CurrentUserName(c)
	if operator == "" {
		resp.FailWithMessage(resp.ErrorForbidden, "[agent/approval/cancel] 无法从登录态取得操作人", c)
		return
	}
	svc, ok := requireApprovalService(c, "[agent/approval/cancel]")
	if !ok {
		return
	}
	row, err := svc.Cancel(req.ApprovalID, operator, strings.TrimSpace(req.Note))
	if err != nil {
		code := approvalErrorCode(err)
		data := gin.H{}
		if row != nil {
			data["approval"] = approvalView(row, nowUnix())
		}
		resp.Result(code, data, "[agent/approval/cancel] "+approvalErrorMsg(err), c)
		return
	}
	resp.OkWithData(gin.H{"approval": approvalView(row, nowUnix())}, c)
}

// ---------- §4.2 审计 ----------

// AuditSearch 工具调用审计查询。
//
// 为什么它很重要：网关**每次**调用都落一条（含被拒的），所以它能回答
// "模型试过什么、被哪道闸拒的、有没有执行"——这是"AI 做了什么"的唯一事实来源。
func (a *AgentRouter) AuditSearch(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id"`
		Tool      string `json:"tool"`
		Decision  string `json:"decision"`
		Page      int    `json:"page"`
		PageSize  int    `json:"page_size"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/audit/search] 请求参数错误: "+err.Error(), c)
		return
	}
	db := dbclient.GetMysqlDB()
	if db == nil {
		resp.FailWithMessage(resp.ERROR, "[agent/audit/search] 数据库未初始化，审计不可用", c)
		return
	}
	list, total, err := models.ListAgentToolAudits(db, strings.TrimSpace(req.SessionID),
		strings.TrimSpace(req.Tool), strings.TrimSpace(req.Decision), req.Page, req.PageSize)
	if err != nil {
		logger.GetLogger().Error("[agent/audit/search] 查询审计失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/audit/search] 查询审计失败", c)
		return
	}
	views := make([]agentAuditView, 0, len(list))
	for i := range list {
		views = append(views, auditView(&list[i]))
	}
	resp.OkWithData(gin.H{"list": views, "total": total, "page": pageOr(req.Page), "page_size": pageSizeOr(req.PageSize)}, c)
}

// ---------- 错误映射 ----------

// approvalErrorCode 审批错误 → 业务码。每一种拒绝都要有自己的码：
// 前端据此决定"是刷新列表（冲突/已决策）还是提示重新发起（过期）"。
func approvalErrorCode(err error) int {
	switch {
	case err == nil:
		return resp.SUCCESS
	case errors.Is(err, service.ErrApprovalNotFound):
		return resp.ErrorAgentApprovalNotFound
	case errors.Is(err, service.ErrApprovalConflict):
		return resp.ErrorAgentApprovalConflict
	case errors.Is(err, service.ErrApprovalExpired):
		return resp.ErrorAgentApprovalExpired
	case errors.Is(err, service.ErrApprovalMismatch):
		return resp.ErrorAgentApprovalMismatch
	case errors.Is(err, service.ErrApprovalToolGone),
		errors.Is(err, service.ErrApprovalPolicyChanged),
		errors.Is(err, service.ErrApprovalNoExecutor):
		return resp.ErrorAgentApprovalBlocked
	default:
		return resp.ERROR
	}
}

func approvalErrorMsg(err error) string {
	if err == nil {
		return ""
	}
	return llm.Redact(err.Error())
}

// toolResultView 工具执行观测的展示视图（截断，原文在审计表里）。
func toolResultView(r tools.Result) gin.H {
	evidence := make([]gin.H, 0, len(r.Evidence))
	for _, e := range r.Evidence {
		evidence = append(evidence, gin.H{"source": e.Source, "ref": e.Ref, "quote": llm.Truncate(e.Quote, 500)})
	}
	return gin.H{
		"ok":          r.OK,
		"summary":     r.Summary,
		"output":      llm.Truncate(r.Output, 4000),
		"err":         llm.Redact(r.Err),
		"truncated":   r.Truncated,
		"duration_ms": r.DurationMS,
		"evidence":    evidence,
		"meta":        r.Meta,
	}
}
