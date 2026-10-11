package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agent"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// ---------- 视图层（契约字段翻译）----------

func TestSessionViewMapsContractFields(t *testing.T) {
	created := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC).Unix()
	s := &models.AgentSession{
		SessionID: "diag-1", Kind: models.AgentKindDiagnose, JobID: 900002, RunID: "j900002-t1791708060",
		NodeUUID: "node-1", Goal: "诊断失败原因", Status: models.AgentStatusCompleted,
		StopReason: string(agent.StopCompleted), Provider: "offline", Model: "offline-model",
		PromptVersion: "diagnose-v1", ModelCalls: 2, ToolCalls: 3, TotalTokens: 1234,
		DurationMs: 4321, WarnCount: 1, EvidenceCount: 2, Created: created,
	}
	v := sessionView(s)
	if v.StopReasonText != "正常完成" {
		t.Fatalf("stop_reason_text = %q", v.StopReasonText)
	}
	if v.StatusText != "正常完成" {
		t.Fatalf("status_text = %q", v.StatusText)
	}
	if v.Created != "2026-03-02T10:00:00Z" || v.CreatedMS != created*1000 {
		t.Fatalf("created/created_ms = %q/%d", v.Created, v.CreatedMS)
	}
	// 未知取值必须如实显示，不能伪造中文解释
	unknown := sessionView(&models.AgentSession{StopReason: "weird", Status: "weird"})
	if unknown.StopReasonText != "weird" || unknown.StatusText != "weird" {
		t.Fatalf("未知取值应原样展示，实际 %q/%q", unknown.StopReasonText, unknown.StatusText)
	}
	empty := sessionView(nil)
	if empty.SessionID != "" {
		t.Fatal("nil 会话应返回空视图而不是 panic")
	}
}

func TestStepViewDecodesToolCallsAndEvidence(t *testing.T) {
	toolCalls := `[{"ID":"call_1","Name":"log_search","Arguments":"{\"keyword\":\"1045\"}"}]`
	evidence := `[{"source":"log","ref":"job_log:77","quote":"Access denied for user"}]`
	s := &models.AgentStep{
		Seq: 2, Kind: string(agent.StepModel), Provider: "openai", Model: "gpt-x",
		PromptPreview: "…", PromptBytes: 100, Content: "结论", ToolCallsJSON: toolCalls,
		TotalTokens: 55, LatencyMs: 900, FinishReason: "tool_calls", Created: 1000,
	}
	v := stepView(s, 1024)
	if v.KindText != "模型调用" {
		t.Fatalf("kind_text = %q", v.KindText)
	}
	if len(v.ToolCalls) != 1 || v.ToolCalls[0].Name != "log_search" || !strings.Contains(v.ToolCalls[0].Arguments, "1045") {
		t.Fatalf("tool_calls 解析错误: %#v", v.ToolCalls)
	}

	tool := &models.AgentStep{
		Seq: 3, Kind: string(agent.StepTool), ToolName: "log_search",
		ToolArgsJSON: `{"keyword":"1045"}`, Observation: "命中 2 行", EvidenceJSON: evidence,
		DurationMs: 12, Created: 1001,
	}
	tv := stepView(tool, 1024)
	if tv.KindText != "工具调用" || tv.ToolArgs["keyword"] != "1045" {
		t.Fatalf("工具步骤解析错误: %#v", tv)
	}
	if len(tv.Evidence) != 1 || tv.Evidence[0].Ref != "job_log:77" {
		t.Fatalf("evidence 解析错误: %#v", tv.Evidence)
	}

	notice := stepView(&models.AgentStep{Kind: string(agent.StepNotice), Notice: "工具预算已用尽"}, 1024)
	if notice.KindText != "系统提示" || notice.Notice == "" {
		t.Fatalf("notice 步骤解析错误: %#v", notice)
	}

	// 观测超长必须按**字节**截断（契约上限 8KiB），且不能切出乱码
	big := stepView(&models.AgentStep{Kind: string(agent.StepTool), Observation: strings.Repeat("字", 5000)}, 1024)
	if len(big.Observation) > 1024+64 {
		t.Fatalf("观测应按字节上限截断，实际 %d 字节", len(big.Observation))
	}
	if !utf8.ValidString(big.Observation) {
		t.Fatal("截断后必须是合法 UTF-8（不能把中文切成乱码）")
	}
}

func TestDiagnosisViewIsAlwaysAdvisoryAndHonest(t *testing.T) {
	d := agent.Diagnosis{
		RunID:      "j1-t2",
		Summary:    "疑似数据库拒绝连接",
		Category:   "resource",
		Cause:      "连接数打满",
		Confidence: 0.72,
		Evidence:   []agent.EvidenceRef{{Ref: "job_log:77", Quote: "Too many connections", Why: "报错原文"}},
		SuggestedActions: []agent.SuggestedAction{
			{Title: "提高 max_connections", Detail: "…", Risk: "medium", Rollback: "改回原值"},
		},
		Warnings: []string{"证据不足：仅 1 条日志"},
		Provider: "offline", Model: "offline-model", PromptVersion: "diagnose-v1",
		DurationMS: 812, ToolCalls: 3, TotalTokens: 999,
	}
	v := diagnosisView(d, &models.AgentSession{SessionID: "diag-1", Created: 1700000000})
	if !v.AdvisoryOnly {
		t.Fatal("诊断必须是 advisory_only（阶段 2 不允许任何自动处置）")
	}
	if v.Confidence != 72 {
		t.Fatalf("置信度应换算为 0-100，实际 %d", v.Confidence)
	}
	if !strings.Contains(v.ConfidenceNote, "不代表准确率") {
		t.Fatalf("置信度必须标注为模型自评，实际 %q", v.ConfidenceNote)
	}
	if v.CategoryText == "" || v.CategoryText == "resource" {
		t.Fatalf("分类应翻译为中文，实际 %q", v.CategoryText)
	}
	if len(v.SuggestedActions) != 1 || v.SuggestedActions[0].RiskText != "中等风险" {
		t.Fatalf("风险等级应翻译，实际 %#v", v.SuggestedActions)
	}
	if len(v.Evidence) != 1 || v.Evidence[0].Why == "" {
		t.Fatalf("证据链必须带上 why，实际 %#v", v.Evidence)
	}
	if v.SessionID != "diag-1" || v.Created == "" {
		t.Fatalf("会话信息应回填，实际 %q/%q", v.SessionID, v.Created)
	}
	if v.Explain == "" {
		t.Fatal("应给出可读回放文本")
	}
}

func TestDiagnosisViewFallsBackToSessionWarnings(t *testing.T) {
	session := &models.AgentSession{
		SessionID:    "diag-2",
		WarningsJSON: `["模型未给出 next_data_to_collect","证据链不足"]`,
	}
	v := diagnosisView(agent.Diagnosis{Summary: "s"}, session)
	if len(v.Warnings) != 2 {
		t.Fatalf("应回落到会话级告警，实际 %#v", v.Warnings)
	}
}

func TestDraftViewComputesTextFields(t *testing.T) {
	now := time.Date(2026, 3, 2, 1, 0, 0, 0, time.UTC)
	d := agent.JobDraft{
		Name: "每日备份", JobType: "cmd", Spec: "0 0 2 * * *", Command: []string{"backup.sh"},
		RawArgv: true, TimeoutSeconds: 1800, RetryTimes: 2, RetryInterval: 60,
		ConcurrencyPolicy: "buffer_one", ConcurrencyValue: 1, Timezone: "Asia/Shanghai",
		NextTriggers: []time.Time{now}, NextTriggerText: []string{"03-02 09:00"},
		Provider: "offline", Model: "m", PromptVersion: "nljob-v1", StopReason: agent.StopCompleted,
		Warnings: nil,
	}
	v := draftView(d, "nljob-1")
	if v.ConcurrencyPolicyText == "" || v.ConcurrencyPolicyText == "buffer_one" {
		t.Fatalf("并发策略应翻译为中文，实际 %q", v.ConcurrencyPolicyText)
	}
	if !v.RequiresConfirmation {
		t.Fatal("草案必须要求人工确认")
	}
	if v.NextTriggers[0] != now.Format(time.RFC3339) {
		t.Fatalf("next_triggers 应为 RFC3339，实际 %#v", v.NextTriggers)
	}
	if v.Warnings == nil {
		t.Fatal("warnings 必须是空数组而不是 null（前端直接渲染）")
	}
	if v.StopReasonText != "正常完成" {
		t.Fatalf("stop_reason_text = %q", v.StopReasonText)
	}
}

func TestToolViewsAndPolicy(t *testing.T) {
	reg, err := tools.NewReadonlyRegistry(tools.DefaultPolicy(), tools.Sources{})
	if err != nil {
		t.Fatalf("注册只读工具失败: %v", err)
	}
	views := toolViews(reg, reg.Policy())
	if len(views) == 0 {
		t.Fatal("应至少返回 shell 工具")
	}
	for _, v := range views {
		if !v.Readonly || v.Risk != string(tools.RiskRead) {
			t.Fatalf("阶段 4 的只读工具集里，%s 的风险声明不符: readonly=%v risk=%s", v.Name, v.Readonly, v.Risk)
		}
		if v.TimeoutMS <= 0 {
			t.Fatalf("%s 应带超时上限", v.Name)
		}
	}
	// 超时必须来自工具自身（而不是策略默认值）：两者不同才能区分出"取错了来源"
	if v := views[0]; v.Name == "node_probe" {
		t.Fatal("不接入探测数据源时不应出现 node_probe")
	}
	p := toolPolicyView(reg.Policy())
	if p.MaxCallsPerSession <= 0 || p.AllowHosts == nil || p.ShellReadPaths == nil {
		t.Fatalf("策略视图不应给出 null 数组/零预算: %#v", p)
	}
	// 阶段 4 契约：默认策略必须**明确表达"写动作关着、且一律需要审批"**
	if p.AllowWrite || p.AllowDestructive {
		t.Fatalf("默认策略不得放开写动作: %#v", p)
	}
	if p.Autonomy != string(tools.AutonomyL1) {
		t.Fatalf("默认自治级别应为 L1（写动作一律审批），实际 %q", p.Autonomy)
	}
	if p.MaxCallsPerMinute <= 0 || p.MaxConcurrentCalls <= 0 || p.ApprovalTTLSeconds <= 0 {
		t.Fatalf("阶段 4 新增策略字段必须有非零默认值: %#v", p)
	}
	if p.AutoApprove == nil || p.Deny == nil {
		t.Fatalf("白名单/黑名单必须是空数组而不是 null: %#v", p)
	}
}

// ---------- 草案 → 任务（纯函数转换）----------

func TestApplyDraftOverridesWhitelist(t *testing.T) {
	base := agent.JobDraft{
		Name: "原名称", Spec: "0 0 2 * * *", TimeoutSeconds: 1800, RetryTimes: 2,
		Command: []string{"a"}, JobType: "cmd",
	}
	// nil = 未修改；0 = 显式改成 0（合法值，不能被当成"未传"）
	got := applyDraftOverrides(base, request.AgentNLJobOverrides{})
	if got.Name != "原名称" || got.RetryTimes != 2 || got.TimeoutSeconds != 1800 {
		t.Fatalf("空覆盖不应改动任何字段: %#v", got)
	}
	zero := 0
	got = applyDraftOverrides(base, request.AgentNLJobOverrides{RetryTimes: &zero})
	if got.RetryTimes != 0 {
		t.Fatalf("显式 0 必须生效，实际 %d", got.RetryTimes)
	}
	// 负数 = "不改"（契约 §5.2；与 timeout_seconds 的 <= 0 一致）。
	// 真实联调前这里会把 -1 写进草案，随后校验必然 1009。
	neg := -1
	got = applyDraftOverrides(base, request.AgentNLJobOverrides{RetryTimes: &neg})
	if got.RetryTimes != 2 {
		t.Fatalf("retry_times=-1 应表示不改（保持 2），实际 %d", got.RetryTimes)
	}
	got = applyDraftOverrides(base, request.AgentNLJobOverrides{TimeoutSeconds: -1})
	if got.TimeoutSeconds != 1800 {
		t.Fatalf("timeout_seconds=-1 应表示不改（保持 1800），实际 %d", got.TimeoutSeconds)
	}
	name, spec, timeout := "新名称", "0 30 3 * * *", int64(600)
	got = applyDraftOverrides(base, request.AgentNLJobOverrides{Name: name, Spec: spec, TimeoutSeconds: timeout})
	if got.Name != name || got.Spec != spec || got.TimeoutSeconds != timeout {
		t.Fatalf("白名单内字段应生效: %#v", got)
	}
	if len(got.Command) != 1 || got.JobType != "cmd" {
		t.Fatal("覆盖不得改动命令与类型（白名单外字段必须原样保留）")
	}
}

func TestJobFromDraftArgvRegistrable(t *testing.T) {
	d := agent.JobDraft{
		Name: "备份", JobType: "cmd", Command: []string{"tar", "-czf", "/backup/my file.tar.gz", "/data"},
		RawArgv: true, TimeoutSeconds: 600, RetryTimes: 1, RetryInterval: 30, ConcurrencyValue: 0,
	}
	job, warnings := jobFromDraft(d)
	if job.Type != models.JobTypeCmd {
		t.Fatalf("类型应为 cmd，实际 %d", job.Type)
	}
	if job.Status != models.JobStatusNotAssigned {
		t.Fatalf("确认创建的任务必须是未分配（status=0），实际 %d", job.Status)
	}
	if len(warnings) != 0 {
		t.Fatalf("argv 模式不需要告警，实际 %v", warnings)
	}
	if err := job.Check(); err != nil {
		t.Fatalf("Check 不应失败: %v", err)
	}
	want := []string{"tar", "-czf", "/backup/my file.tar.gz", "/data"}
	if len(job.Cmd) != len(want) {
		t.Fatalf("argv 还原长度不符: %#v", job.Cmd)
	}
	for i := range want {
		if job.Cmd[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, job.Cmd[i], want[i])
		}
	}
}

func TestJobFromDraftWrapsShellSyntax(t *testing.T) {
	d := agent.JobDraft{
		Name: "清理", JobType: "cmd", Command: []string{"find /tmp -mtime +7 | xargs rm -f"},
		RawArgv: false, TimeoutSeconds: 600, RetryInterval: 30,
	}
	job, warnings := jobFromDraft(d)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "sh -c") {
		t.Fatalf("含 shell 语法必须包装并告警，实际 %v", warnings)
	}
	if err := job.Check(); err != nil {
		t.Fatalf("Check 不应失败: %v", err)
	}
	if len(job.Cmd) != 3 || job.Cmd[0] != "sh" || job.Cmd[1] != "-c" {
		t.Fatalf("应包装为 sh -c，实际 %#v", job.Cmd)
	}
	if !strings.Contains(job.Cmd[2], "xargs rm -f") {
		t.Fatalf("脚本原文必须保留，实际 %q", job.Cmd[2])
	}
}

func TestJobFromDraftHTTP(t *testing.T) {
	d := agent.JobDraft{
		Name: "健康检查", JobType: "http", Spec: "0 * * * * *",
		HTTP:           &agent.HTTPDraft{Method: "POST", URL: "https://example.com/health?full=1"},
		TimeoutSeconds: 60, RetryInterval: 10,
	}
	job, warnings := jobFromDraft(d)
	if len(warnings) != 0 {
		t.Fatalf("http 不需要告警，实际 %v", warnings)
	}
	if job.Type != models.JobTypeHttp || job.Command != "https://example.com/health?full=1" {
		t.Fatalf("http 任务映射错误: type=%d command=%q", job.Type, job.Command)
	}
	if job.HttpMethod != models.HTTPMethodPost {
		t.Fatalf("方法应为 POST，实际 %v", job.HttpMethod)
	}
	if job.Status != models.JobStatusNotAssigned {
		t.Fatal("确认创建的任务必须是未分配")
	}
}

// ---------- HTTP 契约（无需数据库即可验证的拒绝路径）----------

func newAgentTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	a := AgentRouterInstance()
	r.POST("/agent/session/find", a.SessionFind)
	r.POST("/agent/session/search", a.SessionSearch)
	r.POST("/agent/diagnosis/find", a.DiagnosisFind)
	r.POST("/agent/diagnosis/run", a.DiagnosisRun)
	r.POST("/agent/tool/list", a.ToolList)
	r.POST("/agent/nljob/draft", a.NLJobDraft)
	r.POST("/agent/nljob/confirm", a.NLJobConfirm)
	return r
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func doAgentPost(t *testing.T, r *gin.Engine, path, body string) envelope {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s HTTP 状态应为 200（本系统统一 200+code），实际 %d", path, w.Code)
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s 响应不是合法 JSON: %v (%s)", path, err, w.Body.String())
	}
	return env
}

func TestAgentEndpointsValidateInput(t *testing.T) {
	// 未配置运行时（测试环境没有 llm 配置）→ 必须给出 1007/1011 而不是 500
	service.SetAgentRuntime(nil)
	r := newAgentTestRouter()

	cases := []struct {
		path     string
		body     string
		wantCode int
	}{
		{"/agent/session/find", `{}`, resp.ErrorAgentSessionNotFound},
		{"/agent/session/find", `{"session_id":"  "}`, resp.ErrorAgentSessionNotFound},
		{"/agent/diagnosis/find", `{}`, resp.ErrorAgentRunNotFound},
		{"/agent/diagnosis/run", `{"run_id":""}`, resp.ErrorAgentRunNotFound},
		{"/agent/nljob/draft", `{"text":"   "}`, resp.ErrorRequestParameter},
		// 确认闸门：没有 confirmed=true 时**绝不能**走到创建任务
		{"/agent/nljob/confirm", `{"session_id":"nljob-1"}`, resp.ErrorAgentDraftInvalid},
		{"/agent/nljob/confirm", `{"session_id":"nljob-1","confirmed":false}`, resp.ErrorAgentDraftInvalid},
		// 运行时未配置：起草/诊断/工具清单都要给出可解释的业务码
		{"/agent/nljob/draft", `{"text":"每天两点备份 /data"}`, resp.ErrorAgentNotConfigured},
		{"/agent/diagnosis/run", `{"run_id":"j1-t2"}`, resp.ErrorAgentNotConfigured},
		{"/agent/tool/list", `{}`, resp.ErrorAgentNotConfigured},
	}
	for _, tc := range cases {
		env := doAgentPost(t, r, tc.path, tc.body)
		if env.Code != tc.wantCode {
			t.Fatalf("%s %s → code=%d(%s), want %d", tc.path, tc.body, env.Code, env.Msg, tc.wantCode)
		}
		if strings.TrimSpace(env.Msg) == "" {
			t.Fatalf("%s 失败时必须给出可读 msg", tc.path)
		}
	}
}

func TestAgentDisabledCodeDistinguishesOffAndMissing(t *testing.T) {
	// 测试环境通常没有 llm 配置 → 1007（未配置）
	code, msg := agentDisabledCode()
	if code != resp.ErrorAgentNotConfigured && code != resp.ErrorAgentDisabled {
		t.Fatalf("未配置时应返回 1007/1011，实际 %d", code)
	}
	if msg == "" {
		t.Fatal("必须给出可读原因")
	}
}

func TestMapRuntimeError(t *testing.T) {
	a := AgentRouterInstance()
	cases := []struct {
		err  error
		want int
	}{
		{agent.ErrDraftInvalid, resp.ErrorAgentDraftInvalid},
		{agent.ErrNoProvider, resp.ErrorAgentNotConfigured},
	}
	for _, tc := range cases {
		code, msg := a.mapRuntimeError("t", tc.err)
		if code != tc.want {
			t.Fatalf("%v → code=%d, want %d", tc.err, code, tc.want)
		}
		if !strings.Contains(msg, "t") {
			t.Fatalf("msg 应带接口标识，实际 %q", msg)
		}
	}
	if code, _ := a.mapRuntimeError("t", nil); code != resp.SUCCESS {
		t.Fatalf("nil 错误应返回 SUCCESS，实际 %d", code)
	}
}
