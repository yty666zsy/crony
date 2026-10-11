package agentruntime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agent"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// fakeLogSource 确定性日志源：整条"工具 → 证据 → 结论 → grounding 校验"链路
// 必须能脱离数据库与真实模型验证。
type fakeLogSource struct {
	hits []tools.LogHit
	// err 非空时模拟数据源故障（必须让整体检索失败，而不是返回"没命中"）。
	err  error
	seen []tools.LogQuery
}

func (f *fakeLogSource) SearchLogs(_ context.Context, q tools.LogQuery) ([]tools.LogHit, error) {
	f.seen = append(f.seen, q)
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

const validInspection = `{"summary":"未发现异常，日志无 error 级记录",` +
	`"findings":[],"suggested_actions":[],"assumptions":[],` +
	`"evidence_sufficient":true,"next_data_to_collect":[]}`

func offlineConfig(toolCall, response string) models.LLM {
	return models.LLM{
		Enabled:         true,
		Provider:        "offline",
		OfflineToolCall: toolCall,
		OfflineResponse: response,
	}
}

func TestNewRejectsUnconfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  models.LLM
	}{
		{"完全未配置", models.LLM{}},
		{"enabled=false", models.LLM{Enabled: false, Provider: "openai"}},
		{"provider 未知", models.LLM{Enabled: true, Provider: "magic-llm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := New(Options{Config: tc.cfg})
			if err == nil {
				t.Fatal("未配置模型时必须返回错误，而不是给一个跑得起来的空运行时")
			}
			if !errors.Is(err, llm.ErrNotConfigured) {
				t.Fatalf("错误应包装 llm.ErrNotConfigured，实际 %v", err)
			}
			if rt != nil {
				t.Error("失败时不应返回运行时")
			}
		})
	}
}

// TestRunInspectOfflineNoDB：离线 provider 下整条链路可跑通；
// 没有数据库时会话落库失败，但**必须**在结果里显式暴露，而不是假装已回放。
func TestRunInspectOfflineNoDB(t *testing.T) {
	rt, err := New(Options{Config: offlineConfig("", validInspection)})
	if err != nil {
		t.Fatalf("离线配置应能装配: %v", err)
	}
	if !rt.Offline() {
		t.Error("provider=offline 必须如实标记为离线模式")
	}
	if got := rt.ProviderName(); got != "offline" {
		t.Errorf("provider 名 = %q, want offline", got)
	}

	out, err := rt.RunInspect(context.Background(), InspectRequest{
		JobID: 7, Goal: "巡检备份任务是否正常",
		Context: []string{"任务：backup（id=7）"},
	})
	if err != nil {
		t.Fatalf("离线巡检不应报错: %v", err)
	}
	if out.Inspection == nil || !strings.Contains(out.Inspection.Summary, "未发现异常") {
		t.Fatalf("应解析出结构化结论，实际 %+v", out.Inspection)
	}
	if out.PersistError == "" {
		t.Fatal("无数据库时必须报告落库失败（否则前端会以为结论可回放）")
	}
	if !strings.Contains(out.PersistError, "数据库未初始化") {
		t.Errorf("落库失败原因应可读，实际 %q", out.PersistError)
	}
	if !strings.Contains(out.Explain, "只读 Agent 生成") {
		t.Errorf("报告应带只读免责说明，实际 %q", out.Explain[:60])
	}
}

// TestRunInspectGroundedEvidence：模型给出的证据定位只要来自工具观测，就不算幻觉。
func TestRunInspectGroundedEvidence(t *testing.T) {
	src := &fakeLogSource{hits: []tools.LogHit{{
		Source: "job_log", Ref: "job_log:812",
		Line: "backup failed: connection refused", Timestamp: time.Now(),
	}}}
	response := `{"summary":"备份失败因为连接被拒",` +
		`"findings":[{"title":"连接被拒","detail":"日志显示 connection refused","risk":"high",` +
		`"evidence":[{"ref":"job_log:812","quote":"connection refused","why":"说明目标端不可达"}]}],` +
		`"suggested_actions":[{"title":"检查目标端","detail":"确认端口开放","risk":"low","rollback":"无"}],` +
		`"assumptions":[],"evidence_sufficient":true,"next_data_to_collect":[]}`

	rt, err := New(Options{
		Config:  offlineConfig(`{"name":"log_search","arguments":{"keyword":"refused","job_id":7}}`, response),
		Sources: tools.Sources{Logs: src},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}

	out, err := rt.RunInspect(context.Background(), InspectRequest{JobID: 7, Goal: "备份为什么失败"})
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	if len(src.seen) != 1 {
		t.Fatalf("log_search 应被调用一次，实际 %d 次", len(src.seen))
	}
	if src.seen[0].Keyword != "refused" || src.seen[0].JobID != 7 {
		t.Errorf("工具参数未正确透传: %+v", src.seen[0])
	}
	ins := out.Inspection
	if len(ins.Findings) != 1 || len(ins.Findings[0].Evidence) != 1 {
		t.Fatalf("结论应含 1 条带证据的发现，实际 %+v", ins.Findings)
	}
	if len(ins.UnverifiedRefs) != 0 {
		t.Errorf("来自工具观测的定位不应被判为幻觉: %v", ins.UnverifiedRefs)
	}
	if !ins.EvidenceSufficient {
		t.Errorf("证据充分性应保持 true: %+v", ins.Warnings)
	}
	if !strings.Contains(out.Explain, "job_log:812") {
		t.Errorf("报告应展示证据定位与原文，实际 %q", out.Explain)
	}
}

// TestRunInspectHallucinatedEvidence：引用了没观测到的定位 → 降级 + 告警。
func TestRunInspectHallucinatedEvidence(t *testing.T) {
	src := &fakeLogSource{hits: []tools.LogHit{{
		Source: "job_log", Ref: "job_log:812", Line: "backup ok", Timestamp: time.Now(),
	}}}
	response := `{"summary":"磁盘写满导致备份失败",` +
		`"findings":[{"title":"磁盘写满","detail":"看起来是磁盘问题","risk":"high",` +
		`"evidence":[{"ref":"df:-1","quote":"/dev/sda1 100%","why":"空间不足"}]}],` +
		`"suggested_actions":[],"assumptions":[],"evidence_sufficient":true,"next_data_to_collect":[]}`

	rt, err := New(Options{
		Config:  offlineConfig(`{"name":"log_search","arguments":{"keyword":"backup","job_id":7}}`, response),
		Sources: tools.Sources{Logs: src},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	out, err := rt.RunInspect(context.Background(), InspectRequest{JobID: 7, Goal: "备份为什么失败"})
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	ins := out.Inspection
	if len(ins.UnverifiedRefs) != 1 || ins.UnverifiedRefs[0] != "df:-1" {
		t.Fatalf("应识别出 1 处未观测证据，实际 %v", ins.UnverifiedRefs)
	}
	if ins.EvidenceSufficient {
		t.Error("存在幻觉证据时必须把 evidence_sufficient 降级为 false")
	}
	if len(ins.Warnings) == 0 || !strings.Contains(strings.Join(ins.Warnings, " "), "并未观测到") {
		t.Errorf("必须给出告警，实际 %v", ins.Warnings)
	}
}

// TestToolSourceErrorIsNotSilent：数据源故障必须变成错误，绝不能退化成"没命中"。
func TestToolSourceErrorIsNotSilent(t *testing.T) {
	src := &fakeLogSource{err: errors.New("mysql: connection refused")}
	rt, err := New(Options{
		Config:  offlineConfig(`{"name":"log_search","arguments":{"keyword":"error","job_id":7}}`, validInspection),
		Sources: tools.Sources{Logs: src},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	out, err := rt.RunInspect(context.Background(), InspectRequest{JobID: 7, Goal: "巡检"})
	if err != nil {
		t.Fatalf("工具失败不应让整个会话报错（模型需要看到失败本身）: %v", err)
	}
	// 工具失败在 Agent 里是 observation，轨迹必须留下痕迹
	var found bool
	for _, st := range out.Inspection.Trace.StepList() {
		if st.Kind != agent.StepTool || st.ToolName != "log_search" {
			continue
		}
		found = true
		// 观测内容必须体现"检索失败"，模型才有机会据此说"证据不足"，
		// 而不是把空结果当成"日志里没有异常"。
		if !strings.Contains(st.Observation, "失败") && !strings.Contains(st.Observation, "refused") {
			t.Errorf("工具失败的观测内容应保留失败原因，实际 %q", st.Observation)
		}
	}
	if !found {
		t.Fatal("工具失败必须在轨迹里留下工具步骤")
	}
}

func TestMultiLogSourcePropagatesError(t *testing.T) {
	ok := &fakeLogSource{hits: []tools.LogHit{{Ref: "job_log:1", Line: "x"}}}
	bad := &fakeLogSource{err: errors.New("boom")}
	m := MultiLogSource{ok, bad}
	if _, err := m.SearchLogs(context.Background(), tools.LogQuery{Keyword: "x", Limit: 10}); err == nil {
		t.Fatal("任一数据源失败都必须让整体检索失败")
	}
	m2 := MultiLogSource{ok, nil}
	hits, err := m2.SearchLogs(context.Background(), tools.LogQuery{Keyword: "x", Limit: 10})
	if err != nil || len(hits) != 1 {
		t.Fatalf("单个可用源应正常返回，实际 %v, %d 条", err, len(hits))
	}
}

func TestLoadRunContextRejectsEmptyRunID(t *testing.T) {
	if _, err := LoadRunContext("  "); err == nil {
		t.Fatal("空 run_id 必须拒绝")
	}
}

// TestLoadRunContextWithoutDB：未初始化数据库时必须是可读错误（不能 panic）。
func TestLoadRunContextWithoutDB(t *testing.T) {
	_, err := LoadRunContext("j7-t100")
	if err == nil {
		t.Fatal("无数据库时应报错")
	}
	if !strings.Contains(err.Error(), "数据库") {
		t.Errorf("错误信息应说明数据库未初始化，实际 %v", err)
	}
}

// TestReadonlySQLSourceGuard：只读守卫（多条语句/写操作必须拒绝）。
func TestReadonlySQLSourceGuard(t *testing.T) {
	src := &ReadonlySQLSource{}
	cases := []struct {
		name  string
		query string
	}{
		{"写操作", "delete from job"},
		{"多语句", "select 1; drop table job"},
		{"空语句", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := src.QueryReadonly(context.Background(), tc.query); err == nil {
				t.Fatalf("应拒绝查询 %q", tc.query)
			}
		})
	}
}
