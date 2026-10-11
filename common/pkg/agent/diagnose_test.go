package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// ---------------------------------------------------------------------------
// 假日志源
// ---------------------------------------------------------------------------

// fakeLogSource 只读日志源替身。
type fakeLogSource struct {
	hits  []tools.LogHit
	calls int
}

func (f *fakeLogSource) SearchLogs(_ context.Context, _ tools.LogQuery) ([]tools.LogHit, error) {
	f.calls++
	return f.hits, nil
}

// newLogRegistry 构造只带 log_search 的注册表。
func newLogRegistry(t *testing.T, src *fakeLogSource) *tools.Registry {
	t.Helper()
	r := tools.NewRegistry(tools.DefaultPolicy())
	r.MustRegister(tools.NewLogSearchTool(src))
	return r
}

// failedRunContext 一次失败运行的客观事实。
func failedRunContext() RunContext {
	base := time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC)
	return RunContext{
		JobID:       42,
		JobName:     "每日备份",
		JobType:     "cmd",
		Spec:        "0 0 2 * * *",
		NodeUUID:    "node-1",
		RunID:       "j42-t1",
		Status:      "failed",
		ExitCode:    1,
		ScheduledAt: base,
		StartedAt:   base.Add(time.Second),
		FinishedAt:  base.Add(3 * time.Second),
		DurationMS:  2000,
		Attempts:    2,
		Command:     "/usr/local/bin/backup --target /data",
		StderrTail:  "mysqldump: Got error: 1045: Access denied for user 'backup'",
		StepSummaries: []string{
			"step1 shell 退出码 0（用时 500ms）",
			"step2 shell 退出码 1（用时 1500ms）",
		},
		RecentRuns: []RunSummary{
			{RunID: "j42-t0", Status: "failed", ScheduledAt: base.Add(-24 * time.Hour), DurationMS: 2100, ExitCode: 1, Note: "同样的报错"},
			{RunID: "j42-t-1", Status: "success", ScheduledAt: base.Add(-48 * time.Hour), DurationMS: 1900, ExitCode: 0},
		},
	}
}

// validDiagnosis 构造合规的诊断 JSON。
func validDiagnosis(ref string, sufficient bool) map[string]interface{} {
	return map[string]interface{}{
		"summary":    "备份账号密码过期导致 mysqldump 鉴权失败",
		"category":   "permission",
		"cause":      "mysqldump 使用的 backup 账号权限/密码已变更（错误码 1045 指向鉴权而非网络）",
		"confidence": 72,
		"evidence": []interface{}{
			map[string]interface{}{"ref": ref, "quote": "Got error: 1045: Access denied", "why": "1045 是鉴权失败而非连接失败"},
		},
		"assumptions": []interface{}{"数据库侧密码最近被轮换（本节点无法验证，需要 DBA 确认）"},
		"ruled_out": []interface{}{
			map[string]interface{}{"hypothesis": "网络不通", "reason": "连接已建立并返回业务错误码，说明 TCP 可达"},
			map[string]interface{}{"hypothesis": "任务本身未触发", "reason": "本次 run 记录存在且状态为 failed"},
		},
		"suggested_actions": []interface{}{
			map[string]interface{}{"title": "更新备份账号凭据", "detail": "在凭据管理里更新 backup 账号密码后重跑任务", "risk": "medium", "rollback": "恢复旧凭据"},
		},
		"evidence_sufficient":  sufficient,
		"next_data_to_collect": []interface{}{},
	}
}

// ---------------------------------------------------------------------------
// 诊断场景
// ---------------------------------------------------------------------------

// TestDiagnoseHappyPathWithGroundedEvidence 走完"调工具 → 引用观测到的证据 → 出结论"。
func TestDiagnoseHappyPathWithGroundedEvidence(t *testing.T) {
	gateway, fp := newGateway()
	src := &fakeLogSource{hits: []tools.LogHit{
		{Source: "node.log", Ref: "node.log:12", Line: "mysqldump: Got error: 1045: Access denied", Timestamp: time.Now()},
	}}
	fp.WithResponder(func(req llm.Request) (llm.Completion, error) {
		// 第一次：让模型去查日志
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool {
				return llm.FakeJSON(validDiagnosis("node.log:12", true)), nil
			}
		}
		return llm.FakeToolCall("log_search", map[string]interface{}{"keyword": "1045", "job_id": 42}), nil
	})
	a := New(gateway, newLogRegistry(t, src), DefaultConfig())

	diag, err := Diagnose(context.Background(), a, failedRunContext())
	if err != nil {
		t.Fatalf("应有结论: %v", err)
	}
	if !diag.AdvisoryOnly {
		t.Fatal("诊断报告必须永远是「只建议」性质（AdvisoryOnly=true）")
	}
	if diag.Category != "permission" || diag.Confidence < 0.7 || diag.Confidence > 0.73 {
		t.Fatalf("结论字段不符: %+v", diag)
	}
	if !diag.EvidenceSufficient {
		t.Fatalf("证据充分标记不符: %+v", diag.Warnings)
	}
	if len(diag.UnverifiedRefs) != 0 {
		t.Fatalf("引用的就是观测到的 ref，不应被判为编造: %v", diag.UnverifiedRefs)
	}
	if len(diag.Evidence) != 1 || len(diag.RuledOut) != 2 || len(diag.SuggestedActions) != 1 {
		t.Fatalf("报告结构不完整: %+v", diag)
	}
	if src.calls != 1 {
		t.Fatalf("应调用一次日志查询: %d", src.calls)
	}
	if diag.ToolCalls != 1 || diag.Provider != "fake" || diag.PromptVersion != PromptVersionDiagnoseV5 {
		t.Fatalf("运行元信息不符: %+v", diag)
	}
	if diag.Trace == nil || diag.Trace.ModelCalls != 2 {
		t.Fatalf("轨迹不符: %+v", diag.Trace)
	}
	text := diag.Explain()
	for _, want := range []string{"【诊断】", "证据链", "已排除", "建议动作", "需人工执行", "1045"} {
		if !strings.Contains(text, want) {
			t.Fatalf("人话报告缺少 %q:\n%s", want, text)
		}
	}
}

// TestDiagnosePromptRequiresObservingNodeSideFacts 锁定 v3/v4 的新规则：
// "节点侧/环境侧事实必须先观测，再断言"，且**结论句本身**也要受约束。
//
// 为什么要有这条测试：这条规则来自一次真实联调——node_status 工具已接入，
// 同一个播种用例里模型**一次工具都没调**，却在总结里断言"13307 未在节点上监听"。
// 提示词是这条纪律的唯一执行点（grounding 校验只能查 ref 真假，查不出
// "这个结论本应观测却靠推测"），所以必须把规则文本本身钉住。
//
// v3 的实测：模型学会了把"需在节点确认"写进 next_data_to_collect，
// 但 summary 里依然是断言句 → v4 增加"summary/cause 措辞同样受约束"，
// 本测试同时钉住这两条。
func TestDiagnosePromptRequiresObservingNodeSideFacts(t *testing.T) {
	if PromptVersionDiagnoseV4 == PromptVersionDiagnoseV2 || PromptVersionDiagnoseV4 == PromptVersionDiagnoseV3 {
		t.Fatal("v4 必须是新版本号，否则历史会话无法归因")
	}
	if PromptVersionDiagnoseV5 == PromptVersionDiagnoseV4 {
		t.Fatal("v5（注入历史经验）必须是新版本号，否则无法区分'结论是否受了历史提示影响'")
	}
	for _, want := range []string{
		"节点侧/环境侧事实必须先观测",
		"连接被拒绝",
		"不等于",
		"next_data_to_collect",
		// v4 新增：结论句措辞约束（实测 v3 只约束了 next_data_to_collect）
		"同样约束 summary 与 cause 的措辞",
		"疑似目标实例未在该节点监听（需在节点确认）",
		"不可以说",
	} {
		if !strings.Contains(PromptDiagnoseV1, want) {
			t.Fatalf("提示词缺少节点侧事实规则的关键句 %q", want)
		}
	}
}

// TestDiagnoseFlagsHallucinatedEvidence 模型编造证据定位——这是最危险的情况，必须被抓住。
func TestDiagnoseFlagsHallucinatedEvidence(t *testing.T) {
	gateway, fp := newGateway()
	src := &fakeLogSource{hits: []tools.LogHit{
		{Source: "node.log", Ref: "node.log:12", Line: "mysqldump: Got error: 1045", Timestamp: time.Now()},
	}}
	fp.WithResponder(func(req llm.Request) (llm.Completion, error) {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool {
				// 引用一个本次从未观测到的定位（编造）
				return llm.FakeJSON(validDiagnosis("syslog:999", true)), nil
			}
		}
		return llm.FakeToolCall("log_search", map[string]interface{}{"keyword": "1045"}), nil
	})
	a := New(gateway, newLogRegistry(t, src), DefaultConfig())

	diag, err := Diagnose(context.Background(), a, failedRunContext())
	if err != nil {
		t.Fatalf("仍应返回报告（附带告警），而不是丢弃: %v", err)
	}
	if len(diag.UnverifiedRefs) != 1 || diag.UnverifiedRefs[0] != "syslog:999" {
		t.Fatalf("必须标记未观测引用: %+v", diag.UnverifiedRefs)
	}
	if diag.EvidenceSufficient {
		t.Fatal("存在编造引用时不得声称证据充分")
	}
	joined := strings.Join(diag.Warnings, " | ")
	if !strings.Contains(joined, "并未观测到") {
		t.Fatalf("应有明确告警: %v", diag.Warnings)
	}
	if !strings.Contains(diag.Explain(), "未观测到的引用") {
		t.Fatalf("人话报告也要警告:\n%s", diag.Explain())
	}
}

// TestDiagnoseDowngradesEmptyEvidence 声称证据充分但没有证据条目 → 降级。
func TestDiagnoseDowngradesEmptyEvidence(t *testing.T) {
	gateway, fp := newGateway()
	payload := validDiagnosis("x", true)
	payload["evidence"] = []interface{}{}
	fp.WithResponder(func(llm.Request) (llm.Completion, error) { return llm.FakeJSON(payload), nil })

	a := New(gateway, nil, DefaultConfig())
	diag, err := Diagnose(context.Background(), a, failedRunContext())
	if err != nil {
		t.Fatalf("应返回报告: %v", err)
	}
	if diag.EvidenceSufficient {
		t.Fatal("无证据条目不得声称证据充分")
	}
	found := false
	for _, w := range diag.Warnings {
		if strings.Contains(w, "未给出任何证据") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应有降级告警: %v", diag.Warnings)
	}
	// 证据不足但没给"还需要什么" → 必须再告警（否则这条结论无法推进）
	found = false
	for _, w := range diag.Warnings {
		if strings.Contains(w, "未给出待收集数据") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应提示缺少待收集清单: %v", diag.Warnings)
	}
}

// TestDiagnoseWithoutToolsWarns 没有工具时必须诚实提示"可信度会下降"。
func TestDiagnoseWithoutToolsWarns(t *testing.T) {
	gateway, fp := newGateway()
	fp.WithResponder(func(llm.Request) (llm.Completion, error) {
		return llm.FakeJSON(validDiagnosis("n/a", true)), nil
	})
	a := New(gateway, nil, DefaultConfig())
	diag, err := Diagnose(context.Background(), a, failedRunContext())
	if err != nil {
		t.Fatalf("应返回报告: %v", err)
	}
	joined := strings.Join(diag.Warnings, " | ")
	if !strings.Contains(joined, "没有可用工具") {
		t.Fatalf("应提示无工具: %v", diag.Warnings)
	}
	// 没有工具就不可能"证据充分"
	if diag.EvidenceSufficient {
		t.Fatal("无工具时不应声称证据充分")
	}
	// 提示词里必须要求模型在无工具时如实说明
	if !strings.Contains(PromptDiagnoseV1, "evidence_sufficient") {
		t.Fatal("提示词应包含 evidence_sufficient 约束")
	}
}

// TestDiagnosePartialResultOnFailure 拿不到结论时，也要留下轨迹与终止原因。
func TestDiagnosePartialResultOnFailure(t *testing.T) {
	a := New(llm.NewGateway(), nil, DefaultConfig())
	diag, err := Diagnose(context.Background(), a, failedRunContext())
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("应报无 provider: %v", err)
	}
	if diag.StopReason != StopNoProvider || diag.Trace == nil {
		t.Fatalf("失败也应带终止原因与轨迹: %+v", diag)
	}
	if !strings.Contains(diag.Summary, "未能给出诊断结论") {
		t.Fatalf("摘要应说明未给出结论: %q", diag.Summary)
	}
	if !strings.Contains(diag.Explain(), "终止原因") {
		t.Fatalf("报告应说明为何没有结论:\n%s", diag.Explain())
	}
}

func TestDiagnosisSchemaIsStrictEnough(t *testing.T) {
	var schema *jsonschema.Schema = DiagnosisSchema()
	if err := schema.Valid(); err != nil {
		t.Fatalf("Schema 自身不合法: %v", err)
	}
	// 必填字段缺失必须被拦（否则模型可以只回一句 summary 就算"诊断"）
	if _, err := llm.ValidateStructured(schema, `{"summary":"x","category":"config","cause":"y","confidence":50}`); err == nil {
		t.Fatal("缺少 evidence/evidence_sufficient 等字段应被拒绝")
	}
	// 分类枚举
	if _, err := llm.ValidateStructured(schema, jsonOf(validDiagnosis("r", true), "category", "乱写")); err == nil {
		t.Fatal("非法分类应被拒绝")
	}
	// 置信度范围（模型爱写 0.9 或 150）
	if _, err := llm.ValidateStructured(schema, jsonOf(validDiagnosis("r", true), "confidence", 150)); err == nil {
		t.Fatal("置信度越界应被拒绝")
	}
	if _, err := llm.ValidateStructured(schema, jsonOf(validDiagnosis("r", true), "confidence", 0.9)); err == nil {
		t.Fatal("置信度必须是 0-100 的整数（0.9 应被拒绝，避免歧义）")
	}
	// 风险等级枚举
	bad := validDiagnosis("r", true)
	bad["suggested_actions"] = []interface{}{map[string]interface{}{"title": "t", "detail": "d", "risk": "catastrophic", "rollback": ""}}
	if _, err := llm.ValidateStructured(schema, mustJSON(bad)); err == nil {
		t.Fatal("非法风险等级应被拒绝")
	}
	// 合规样本
	if _, err := llm.ValidateStructured(schema, mustJSON(validDiagnosis("r", true))); err != nil {
		t.Fatalf("合规样本应通过: %v", err)
	}
}

func TestRenderRunContextMarksUnknowns(t *testing.T) {
	text := renderRunContext(RunContext{JobID: 7})
	if !strings.Contains(text, "未知") {
		t.Fatalf("缺失字段应标注未知: %s", text)
	}
	if !strings.Contains(text, "不要臆测") {
		t.Fatalf("应提醒模型不要臆测未提供的信息: %s", text)
	}
	// 完整上下文应包含历史运行与"一直失败 vs 最近开始失败"的提示
	full := renderRunContext(failedRunContext())
	for _, want := range []string{"最近运行历史", "一直失败", "stderr 尾部", "1045", "步骤"} {
		if !strings.Contains(full, want) {
			t.Fatalf("材料缺少 %q:\n%s", want, full)
		}
	}
}

// TestDiagnosePromptAndSchemaAgreeOnConfidence 锁定"提示词与 Schema 对 confidence 的说法一致"。
//
// 真实联调事故：Schema 要求 0-100 整数，提示词却写 0.0-1.0，
// 模型按提示词回了 0.95 → 结构校验失败 → 白烧一轮修复（实测多花 17.8s / 3885 tokens）。
func TestDiagnosePromptAndSchemaAgreeOnConfidence(t *testing.T) {
	if strings.Contains(PromptDiagnoseV1, "0.0-1.0") {
		t.Fatalf("提示词仍在教模型输出 0.0-1.0，与 Schema 的整数百分比冲突")
	}
	if !strings.Contains(PromptDiagnoseV1, "0-100 的整数") {
		t.Fatalf("提示词必须明确要求 0-100 整数百分比")
	}
}

// TestRunContextFactsAreCitable 锁定"材料里的事实可以逐字引用，且能通过 grounding 校验"。
//
// 真实联调事故：材料从未显示过任何 ref，模型只能自己写"步骤0 的输出"这种描述，
// grounding 判为捏造证据 → 正确的诊断结论被降级为"证据不足"。
func TestRunContextFactsAreCitable(t *testing.T) {
	rc := failedRunContext()
	rendered := renderRunContext(rc)
	given := givenEvidence(rc)
	if len(given) == 0 {
		t.Fatal("材料自带的事实必须登记为可引用证据")
	}

	// 材料里必须能看到 ref 字符串，模型才可能逐字复制
	for _, e := range given {
		if !strings.Contains(rendered, "[ref: "+e.Ref+"]") {
			t.Fatalf("材料里没有显示 ref %q，模型无从引用:\n%s", e.Ref, rendered)
		}
		if strings.TrimSpace(e.Quote) == "" {
			t.Fatalf("证据 %q 缺少原文片段", e.Ref)
		}
	}

	// 登记之后，这些 ref 必须能通过 grounding 校验（否则等于没登记）
	trace := newTrace("s", "g", "v")
	trace.addEvidence(given...)
	observed := trace.EvidenceRefs()
	for _, e := range given {
		if !refObserved(observed, e.Ref) {
			t.Fatalf("材料事实 ref %q 未进入可引用集合", e.Ref)
		}
	}
	// 没观测到的 ref 仍然必须被拦下（grounding 不能被放宽）
	if refObserved(observed, "job_run_step:不存在#9") {
		t.Fatal("不存在的 ref 不应通过 grounding 校验")
	}
}

// jsonOf / mustJSON 测试辅助。
func mustJSON(v map[string]interface{}) string {
	comp := llm.FakeJSON(v)
	return comp.Content
}

func jsonOf(base map[string]interface{}, key string, value interface{}) string {
	clone := map[string]interface{}{}
	for k, v := range base {
		clone[k] = v
	}
	clone[key] = value
	return mustJSON(clone)
}
