package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
)

// 阶段 5 补全：统一审计检索（只读）。
//
// 本平台**没有**一张"万能审计表"，但已有两个真实的"谁在什么时候做了什么"事实源：
//  1. `agent_tool_audit` —— 工具网关每次调用一行（含被拒的、转审批的，append-only）；
//  2. `approval_request` —— 写动作审批单（谁批的、结果如何）。
//
// `/audit/search` 把这两者**合并**成统一审计视图，并如实标注每行来源（`source`）。
//
// 三条纪律：
//  1. **不合并没有的事实源**：例如"用户登录审计"当前不存在，就不显示（不编造）；
//  2. **未知字段留空**：`ip` 目前无采集，恒为空字符串，不用样例值填充；
//  3. **合并有窗口上限**并在响应里写明，避免"检索不到"被误读成"从没发生"。

const (
	// auditWindow 合并前每个来源最多加载的行数（控制内存与查询成本）。
	auditWindow = 500
	// AuditSourceToolCall / AuditSourceApproval 审计行的来源标注。
	AuditSourceToolCall = "tool_audit"
	AuditSourceApproval = "approval"
)

// AuditQuery 审计检索条件（字段全部可选，空串 = 不过滤）。
type AuditQuery struct {
	Operator string
	Action   string
	Target   string
	Result   string
	IP       string
	Page     int
	PageSize int
}

// AuditEntry 统一审计行（字段与前端 AuditView 的约定一致）。
type AuditEntry struct {
	ID           string `json:"id"`
	Time         string `json:"time"`
	Operator     string `json:"operator"`
	OperatorType string `json:"operator_type"` // agent | human | system
	Action       string `json:"action"`
	ActionText   string `json:"action_text"`
	Target       string `json:"target"`
	TargetType   string `json:"target_type"` // node | tool
	Result       string `json:"result"`      // success | failed | denied | pending | ...
	Detail       string `json:"detail"`
	// IP 当前无采集，恒为空（诚实留空，不填样例）。
	IP      string `json:"ip"`
	TraceID string `json:"trace_id"`
	Source  string `json:"source"`

	// ts 仅用于排序（不对外暴露）：两个来源的时间单位与语义不完全一致，
	// 合并排序必须用一个统一数值，而不是解析字符串。
	ts int64
}

// AuditPage 分页结果（与后端分页契约一致）。
type AuditPage struct {
	List     []AuditEntry `json:"list"`
	Total    int          `json:"total"`
	Page     int          `json:"page"`
	PageSize int          `json:"page_size"`
	// SourceWindow 每个来源最多加载的行数（诚实口径）。
	SourceWindow int `json:"source_window"`
	// Notice 数据来源说明。
	Notice string `json:"notice"`
}

// AuditService 统一审计（只读）。
type AuditService struct{}

// DefaultAuditService 进程内单例。
var DefaultAuditService = new(AuditService)

// Search 从两个真实来源合并后按条件过滤 + 分页。
func (s *AuditService) Search(q AuditQuery) (AuditPage, error) {
	db := dbclient.GetMysqlDB()
	if db == nil {
		return AuditPage{}, fmt.Errorf("数据库未初始化，审计不可用")
	}
	var tools []models.AgentToolAudit
	if err := db.Table(models.CronyAgentToolAuditTableName).
		Order("created_ms desc, id desc").Limit(auditWindow).Find(&tools).Error; err != nil {
		return AuditPage{}, err
	}
	var approvals []models.ApprovalRequest
	if err := db.Table(models.CronyApprovalRequestTableName).
		Order("created_ms desc, id desc").Limit(auditWindow).Find(&approvals).Error; err != nil {
		return AuditPage{}, err
	}
	entries := buildAuditEntries(tools, approvals)
	filtered := filterAuditEntries(entries, q)
	return paginateAudit(filtered, q), nil
}

// buildAuditEntries 把两类事实映射成统一审计行并按时间倒序。
func buildAuditEntries(tools []models.AgentToolAudit, approvals []models.ApprovalRequest) []AuditEntry {
	out := make([]AuditEntry, 0, len(tools)+len(approvals))
	for i := range tools {
		out = append(out, toolAuditEntry(&tools[i]))
	}
	for i := range approvals {
		out = append(out, approvalEntry(&approvals[i]))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ts == out[j].ts {
			return out[i].ID > out[j].ID
		}
		return out[i].ts > out[j].ts
	})
	return out
}

func toolAuditEntry(a *models.AgentToolAudit) AuditEntry {
	target, targetType := a.Tool, "tool"
	if n := strings.TrimSpace(a.TargetNode); n != "" {
		target, targetType = n, "node"
	}
	op := strings.TrimSpace(a.SessionID)
	if op == "" {
		op = "unknown"
	}
	return AuditEntry{
		ID:           a.AuditID,
		Time:         unixToRFC3339(a.Created),
		Operator:     op,
		OperatorType: "agent",
		Action:       "tool." + a.Tool,
		ActionText:   fmt.Sprintf("调用工具 %s（%s）", a.Tool, toolDecisionText(a.Decision)),
		Target:       target,
		TargetType:   targetType,
		Result:       toolDecisionResult(a),
		Detail:       firstNonEmpty(a.Reason, a.Err, a.ApprovalID),
		TraceID:      firstNonEmpty(a.RunID, a.AuditID),
		Source:       AuditSourceToolCall,
		ts:           a.Created,
	}
}

func approvalEntry(a *models.ApprovalRequest) AuditEntry {
	target, targetType := a.Tool, "tool"
	if n := strings.TrimSpace(a.TargetNode); n != "" {
		target, targetType = n, "node"
	}
	op, opType := strings.TrimSpace(a.DecidedBy), "human"
	if op == "" {
		if req := strings.TrimSpace(a.RequestedBy); req != "" {
			op, opType = req, "agent"
		} else {
			op, opType = strings.TrimSpace(a.SessionID), "agent"
		}
	}
	if op == "" {
		op, opType = "unknown", "system"
	}
	// 已决策的单以决策时间为准，未决策的以创建时间为准。
	ts := a.Created
	if a.DecidedAt > 0 {
		ts = a.DecidedAt
	}
	return AuditEntry{
		ID:           a.ApprovalID,
		Time:         unixToRFC3339(ts),
		Operator:     op,
		OperatorType: opType,
		Action:       "approval." + a.Status,
		ActionText:   fmt.Sprintf("写动作审批 %s（%s）", a.Tool, approvalStatusText(a.Status)),
		Target:       target,
		TargetType:   targetType,
		Result:       approvalResult(a.Status),
		Detail:       firstNonEmpty(a.DecisionNote, a.ExecutionResult, a.Impact),
		TraceID:      firstNonEmpty(a.RunID, a.ApprovalID),
		Source:       AuditSourceApproval,
		ts:           ts,
	}
}

// 工具调用的决策 → 人话。
func toolDecisionText(d string) string {
	switch d {
	case models.AuditDecisionAllowed:
		return "已执行"
	case models.AuditDecisionDenied:
		return "被拒绝"
	case models.AuditDecisionApprovalRequired:
		return "转人工审批"
	case models.AuditDecisionApprovalExecuted:
		return "审批后执行"
	}
	return d
}

// 工具调用结果：只报**观测到的**事实（denied 不是 failed，是"没动手"）。
func toolDecisionResult(a *models.AgentToolAudit) string {
	switch a.Decision {
	case models.AuditDecisionDenied:
		return "denied"
	case models.AuditDecisionApprovalRequired:
		return "pending"
	case models.AuditDecisionAllowed, models.AuditDecisionApprovalExecuted:
		if a.OK {
			return "success"
		}
		return "failed"
	}
	return "unknown"
}

// 审批状态 → 人话。
func approvalStatusText(s string) string {
	switch s {
	case models.ApprovalStatusPending:
		return "待审批"
	case models.ApprovalStatusApproved:
		return "已批准"
	case models.ApprovalStatusRejected:
		return "已拒绝"
	case models.ApprovalStatusExpired:
		return "已过期"
	case models.ApprovalStatusCanceled:
		return "已撤销"
	case models.ApprovalStatusExecuted:
		return "已执行"
	case models.ApprovalStatusExecutionFailed:
		return "执行失败"
	case models.ApprovalStatusBlocked:
		return "执行前复核未通过"
	}
	return s
}

// 审批状态 → 统一结果取值。
func approvalResult(s string) string {
	switch s {
	case models.ApprovalStatusPending:
		return "pending"
	case models.ApprovalStatusApproved, models.ApprovalStatusExecuted:
		return "success"
	case models.ApprovalStatusRejected:
		return "denied"
	case models.ApprovalStatusExecutionFailed:
		return "failed"
	case models.ApprovalStatusExpired:
		return "expired"
	case models.ApprovalStatusCanceled:
		return "canceled"
	case models.ApprovalStatusBlocked:
		return "blocked"
	}
	return "unknown"
}

// filterAuditEntries 应用检索条件（operator/action/target 模糊；result 精确；ip 模糊）。
func filterAuditEntries(entries []AuditEntry, q AuditQuery) []AuditEntry {
	op := strings.ToLower(strings.TrimSpace(q.Operator))
	action := strings.ToLower(strings.TrimSpace(q.Action))
	target := strings.ToLower(strings.TrimSpace(q.Target))
	result := strings.TrimSpace(q.Result)
	ip := strings.TrimSpace(q.IP)
	if op == "" && action == "" && target == "" && result == "" && ip == "" {
		return entries
	}
	out := make([]AuditEntry, 0, len(entries))
	for _, e := range entries {
		if op != "" && !strings.Contains(strings.ToLower(e.Operator), op) {
			continue
		}
		if action != "" && !strings.Contains(strings.ToLower(e.Action), action) &&
			!strings.Contains(strings.ToLower(e.ActionText), action) {
			continue
		}
		if target != "" && !strings.Contains(strings.ToLower(e.Target), target) {
			continue
		}
		if result != "" && e.Result != result {
			continue
		}
		if ip != "" && !strings.Contains(e.IP, ip) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func paginateAudit(entries []AuditEntry, q AuditQuery) AuditPage {
	page := q.Page
	if page <= 0 {
		page = 1
	}
	size := q.PageSize
	if size <= 0 || size > 200 {
		size = 20
	}
	total := len(entries)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	page1 := make([]AuditEntry, 0, end-start)
	page1 = append(page1, entries[start:end]...)
	return AuditPage{
		List:         page1,
		Total:        total,
		Page:         page,
		PageSize:     size,
		SourceWindow: auditWindow,
		Notice: fmt.Sprintf("合并自 %s 与 %s 两个真实来源（各取最近 %d 行）；ip 当前无采集恒为空。",
			models.CronyAgentToolAuditTableName, models.CronyApprovalRequestTableName, auditWindow),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func unixToRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).Format(time.RFC3339)
}
