package service

import (
	"testing"

	"github.com/tmnhs/crony/common/models"
)

// 阶段 5 补全：统一审计检索的纯函数单元测试。
// 依赖数据库的合并由黑盒用例（AC-46）覆盖；这里钉死"映射/过滤/分页"这类确定性逻辑。

func TestToolDecisionResultHonest(t *testing.T) {
	cases := []struct {
		decision string
		ok       bool
		want     string
	}{
		{models.AuditDecisionDenied, false, "denied"},
		{models.AuditDecisionApprovalRequired, false, "pending"},
		{models.AuditDecisionAllowed, true, "success"},
		{models.AuditDecisionAllowed, false, "failed"},
		{models.AuditDecisionApprovalExecuted, true, "success"},
		{models.AuditDecisionApprovalExecuted, false, "failed"},
		{"something_else", true, "unknown"},
	}
	for _, c := range cases {
		got := toolDecisionResult(&models.AgentToolAudit{Decision: c.decision, OK: c.ok})
		if got != c.want {
			t.Fatalf("decision=%s ok=%v：期望 %s，实际 %s", c.decision, c.ok, c.want, got)
		}
	}
}

func TestApprovalResultMapping(t *testing.T) {
	cases := map[string]string{
		models.ApprovalStatusPending:         "pending",
		models.ApprovalStatusApproved:        "success",
		models.ApprovalStatusExecuted:        "success",
		models.ApprovalStatusRejected:        "denied",
		models.ApprovalStatusExecutionFailed: "failed",
		models.ApprovalStatusExpired:         "expired",
		models.ApprovalStatusCanceled:        "canceled",
		models.ApprovalStatusBlocked:         "blocked",
	}
	for in, want := range cases {
		if got := approvalResult(in); got != want {
			t.Fatalf("状态 %s：期望 %s，实际 %s", in, want, got)
		}
	}
}

// 合并后必须按时间倒序；来源与关键映射必须正确。
func TestBuildAuditEntriesMergesAndSorts(t *testing.T) {
	tools := []models.AgentToolAudit{
		{AuditID: "audit-1", SessionID: "sess-a", Tool: "node_status", Decision: models.AuditDecisionAllowed,
			OK: true, TargetNode: "node-1", Created: 1000, Reason: "只读巡检"},
		{AuditID: "audit-2", SessionID: "sess-a", Tool: "job_retry", Decision: models.AuditDecisionDenied,
			OK: false, Created: 3000, Reason: "策略未开启"},
	}
	approvals := []models.ApprovalRequest{
		{ApprovalID: "ap-1", SessionID: "sess-b", Tool: "job_retry", TargetNode: "node-2",
			Status: models.ApprovalStatusRejected, DecidedBy: "admin", DecidedAt: 2000, Created: 1500,
			DecisionNote: "影响面过大"},
	}
	got := buildAuditEntries(tools, approvals)
	if len(got) != 3 {
		t.Fatalf("应合并出 3 行，实际 %d", len(got))
	}
	// 时间倒序：audit-2(3000) > ap-1(2000) > audit-1(1000)
	if got[0].ID != "audit-2" || got[1].ID != "ap-1" || got[2].ID != "audit-1" {
		t.Fatalf("排序错误：%s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Result != "denied" || got[0].Source != AuditSourceToolCall {
		t.Fatalf("被拒工具调用映射错误：%+v", got[0])
	}
	if got[1].Result != "denied" || got[1].Operator != "admin" || got[1].OperatorType != "human" {
		t.Fatalf("审批行映射错误：%+v", got[1])
	}
	if got[1].TargetType != "node" || got[1].Time == "" {
		t.Fatalf("审批行目标/时间映射错误：%+v", got[1])
	}
	if got[2].Result != "success" || got[2].TargetType != "node" {
		t.Fatalf("成功工具调用映射错误：%+v", got[2])
	}
	// 未采集字段必须留空（不填样例）。
	if got[2].IP != "" {
		t.Fatalf("ip 无采集应留空，实际 %q", got[2].IP)
	}
}

// 目标节点为空时降级为工具维度，而不是编一个节点名。
func TestAuditTargetFallsBackToTool(t *testing.T) {
	got := buildAuditEntries([]models.AgentToolAudit{
		{AuditID: "a", SessionID: "s", Tool: "job_retry", Decision: models.AuditDecisionAllowed, OK: true, Created: 1},
	}, nil)
	if got[0].Target != "job_retry" || got[0].TargetType != "tool" {
		t.Fatalf("无目标节点时应回落到工具维度：%+v", got[0])
	}
}

func TestFilterAuditEntries(t *testing.T) {
	entries := buildAuditEntries([]models.AgentToolAudit{
		{AuditID: "a1", SessionID: "sess-x", Tool: "node_status", Decision: models.AuditDecisionAllowed, OK: true, Created: 10},
		{AuditID: "a2", SessionID: "sess-y", Tool: "job_retry", Decision: models.AuditDecisionDenied, Created: 20, Reason: "策略未开启"},
	}, nil)

	if got := filterAuditEntries(entries, AuditQuery{Operator: "SESS-X"}); len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("operator 过滤（大小写不敏感）错误：%+v", got)
	}
	if got := filterAuditEntries(entries, AuditQuery{Action: "job_retry"}); len(got) != 1 || got[0].ID != "a2" {
		t.Fatalf("action 过滤错误：%+v", got)
	}
	if got := filterAuditEntries(entries, AuditQuery{Result: "denied"}); len(got) != 1 || got[0].ID != "a2" {
		t.Fatalf("result 精确过滤错误：%+v", got)
	}
	if got := filterAuditEntries(entries, AuditQuery{IP: "10."}); len(got) != 0 {
		t.Fatalf("ip 无采集，过滤任何 ip 都应为空：%+v", got)
	}
	if got := filterAuditEntries(entries, AuditQuery{}); len(got) != 2 {
		t.Fatalf("空条件应返回全部：%+v", got)
	}
}

func TestPaginateAudit(t *testing.T) {
	entries := make([]AuditEntry, 0, 25)
	for i := 0; i < 25; i++ {
		entries = append(entries, AuditEntry{ID: string(rune('a' + i))})
	}
	p := paginateAudit(entries, AuditQuery{})
	if p.Page != 1 || p.PageSize != 20 || p.Total != 25 || len(p.List) != 20 {
		t.Fatalf("默认分页错误：page=%d size=%d total=%d len=%d", p.Page, p.PageSize, p.Total, len(p.List))
	}
	if p.SourceWindow != auditWindow || p.Notice == "" {
		t.Fatalf("应写明来源窗口与说明：%+v", p)
	}
	p2 := paginateAudit(entries, AuditQuery{Page: 2, PageSize: 10})
	if len(p2.List) != 10 || p2.List[0].ID != entries[10].ID {
		t.Fatalf("第二页错误：%+v", p2.List)
	}
	p3 := paginateAudit(entries, AuditQuery{Page: 99, PageSize: 10})
	if len(p3.List) != 0 || p3.Total != 25 {
		t.Fatalf("越界页应为空但 total 不变：%+v", p3)
	}
	// page_size 上限。
	if p4 := paginateAudit(entries, AuditQuery{PageSize: 9999}); p4.PageSize != 20 {
		t.Fatalf("page_size 应被收敛到 20，实际 %d", p4.PageSize)
	}
}
