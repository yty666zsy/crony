//go:build integration

package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 真实 MySQL 集成测试：工具审计落库（阶段 4）。
//
// 为什么必须在真库上验证（内存结构证明不了的事）：
//   - args_json 是 text 列，参数里可能有中文/emoji，截断点必须回退到字符边界，
//     否则 utf8mb4 列会以 Error 1366 直接拒绝插入（阶段 2 的 prompt_preview 真实事故）；
//   - reason 的长度是**列宽**约束：观察者若把超长 reason 原样写入，
//     严格模式下整条审计插入失败——而审计失败是静默的（只记日志），
//     于是"审计看起来一直在，其实早就不写了"；
//   - 观察者依赖**全局连接**（dbclient.GetMysqlDB），单测里没有这条链路。
//
// 运行方式同 mysql_integration_test.go（CRONY_TEST_MYSQL_DSN 指向临时库，未设置即跳过）。

// auditStubTool 审计测试用的只读替身。
//
// 刻意**不**用真实 shell 工具：本测试要验证的是"观察者 → 数据库"这条链路，
// 用真实命令会让结果依赖宿主机上有没有 tail/grep（Windows 上没有），
// 于是测试变成"看平台脸色"，而不是验证被测对象。
type auditStubTool struct{ name string }

func (s *auditStubTool) Name() string           { return s.name }
func (s *auditStubTool) Description() string    { return "审计链路测试用只读工具" }
func (s *auditStubTool) ReadOnly() bool         { return true }
func (s *auditStubTool) Risk() tools.RiskLevel  { return tools.RiskRead }
func (s *auditStubTool) Timeout() time.Duration { return time.Second }
func (s *auditStubTool) Schema() *jsonschema.Schema {
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"query": jsonschema.String("查询关键字"),
	}, "query")
}
func (s *auditStubTool) Execute(ctx context.Context, args map[string]interface{}) (tools.Result, error) {
	return tools.Result{OK: true, Summary: "替身工具已执行", Output: "ok", Meta: map[string]interface{}{"echo": args["query"]}}, nil
}

// TestIntegrationToolAuditLandsInDB 覆盖 allowed / denied 两类落库事实、脱敏与列宽。
func TestIntegrationToolAuditLandsInDB(t *testing.T) {
	db := integrationDB(t)
	tag := uniqueTag("itaudit")
	sessionID := tag + "-sess"

	policy := tools.DefaultPolicy()
	policy.MaxCallsPerSession = 4
	reg := tools.NewRegistry(policy, tools.WithObserver(ToolAuditObserver()))
	if err := reg.Register(&auditStubTool{name: "audit_stub"}); err != nil {
		t.Fatalf("注册替身工具失败: %v", err)
	}
	defer func() {
		_ = db.Exec("delete from "+models.CronyAgentToolAuditTableName+" where session_id = ?", sessionID).Error
	}()

	ctx := tools.WithRunMeta(context.Background(), tools.RunMeta{
		AgentKind: models.AgentKindDiagnose, RunID: tag + "-run",
	})

	// 1) 正常调用：参数里带中文与 emoji（utf8mb4 列要真的装得下）
	if _, err := reg.Call(ctx, sessionID, "audit_stub", map[string]interface{}{
		"query": "连接被拒：容器 日志🙂 /var/log/crony/app.log",
	}); err != nil {
		t.Fatalf("替身工具调用不应失败: %v", err)
	}
	// 2) 参数里藏凭据：审计参数必须脱敏
	if _, err := reg.Call(ctx, sessionID, "audit_stub", map[string]interface{}{
		"query": "Authorization: Bearer sk-abcdefghijklmnopqrstuvwxyz012345",
	}); err != nil {
		t.Fatalf("第二次调用不应失败: %v", err)
	}
	// 3) 不存在的工具：必须留下 denied 行（模型试探同样要能被复盘）
	if _, err := reg.Call(ctx, sessionID, "no_such_tool", map[string]interface{}{"x": 1}); err == nil {
		t.Fatal("未知工具必须返回错误")
	}

	var rows []models.AgentToolAudit
	if err := db.Table(models.CronyAgentToolAuditTableName).
		Where("session_id = ?", sessionID).Order("id asc").Find(&rows).Error; err != nil {
		t.Fatalf("查询审计行失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("应恰好 3 条审计行（2 allowed + 1 denied），实际 %d", len(rows))
	}

	var deniedSeen, allowedSeen bool
	for _, r := range rows {
		if r.AuditID == "" || len(r.AuditID) > 64 {
			t.Fatalf("audit_id 缺失或超长: %q", r.AuditID)
		}
		if r.AgentKind != models.AgentKindDiagnose || r.RunID != tag+"-run" {
			t.Fatalf("调用归属必须从 ctx 带进审计行: kind=%q run=%q", r.AgentKind, r.RunID)
		}
		if r.Risk == "" || r.Decision == "" {
			t.Fatalf("risk/decision 不得为空: %+v", r)
		}
		if r.Created == 0 || r.CreatedMS == 0 {
			t.Fatalf("审计行必须带双口径时间: %+v", r)
		}
		if r.ArgsJSON != "" {
			var probe interface{}
			if err := json.Unmarshal([]byte(r.ArgsJSON), &probe); err != nil {
				t.Fatalf("args_json 必须是合法 JSON（截断后也不能坏）: %v, raw=%q", err, r.ArgsJSON)
			}
			if strings.Contains(r.ArgsJSON, "sk-abcdefghijklmnopqrstuvwxyz012345") {
				t.Fatalf("审计参数必须脱敏，凭据不得落库: %s", r.ArgsJSON)
			}
		}
		switch r.Decision {
		case models.AuditDecisionAllowed:
			allowedSeen = true
			if !r.OK {
				t.Fatalf("allowed 行应为成功执行: %+v", r)
			}
		case models.AuditDecisionDenied:
			deniedSeen = true
			if r.Tool != "no_such_tool" || r.Reason == "" {
				t.Fatalf("denied 行必须写明工具名与原因: %+v", r)
			}
		default:
			t.Fatalf("只读链路不应产生 %s 决策", r.Decision)
		}
	}
	if !allowedSeen || !deniedSeen {
		t.Fatalf("allowed/denied 两类事实都要留痕: allowed=%v denied=%v", allowedSeen, deniedSeen)
	}

	// 4) 被拒调用不消耗会话预算（阶段 4 不变式在真库链路上同样成立）
	if stats := reg.SessionStats(sessionID); stats.Calls != 2 {
		t.Fatalf("会话调用数应只统计被受理的 2 次，实际 %d（denied 不得计入预算）", stats.Calls)
	}

	// 5) 超长 reason 不能把审计插入搞崩（观察者的 clip 必须生效）
	long := strings.Repeat("很长的原因", 300)
	if err := models.AppendAgentToolAudit(db, &models.AgentToolAudit{
		AuditID: models.NewToolAuditID(), SessionID: sessionID, Tool: "audit_stub", Risk: "read",
		Decision: models.AuditDecisionDenied, Reason: clip(long, 512),
		Created: time.Now().Unix(), CreatedMS: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("超长 reason 落库失败: %v", err)
	}
}
