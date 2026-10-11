package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 这一组用例守的是阶段 4-A 的**核心不变量**：
// 历史经验可以进提示词，但**永远不能变成证据**。
//
// 为什么这条不变量值得单独一组测试：注入历史经验是本阶段唯一一处
// "往模型输入里加非观测内容"的改动。它带来的风险不是功能缺失，
// 而是**结论看起来更有依据、实际上依据是历史**——这类错误在人工抽查里几乎发现不了。

func TestDiagnosePromptV2InsertsMemoryRule(t *testing.T) {
	if PromptDiagnoseV2 == PromptDiagnoseV1 {
		t.Fatal("v2 必须包含历史经验条款（插入锚点失效会静默退化，这条断言就是防它）")
	}
	// 条款必须在"输出 JSON 结构"之前（否则会变成 JSON schema 之后的散文字，模型容易忽略）。
	ruleIdx := strings.Index(PromptDiagnoseV2, "历史相似经验只能当线索")
	schemaIdx := strings.Index(PromptDiagnoseV2, "## 输出 JSON 结构")
	if ruleIdx < 0 {
		t.Fatal("v2 缺少历史经验条款")
	}
	if schemaIdx < 0 || ruleIdx > schemaIdx {
		t.Fatalf("历史经验条款必须在输出结构之前（rule=%d schema=%d）", ruleIdx, schemaIdx)
	}
	// v1 的所有既有规则必须一字不少地继承下来。
	if !strings.Contains(PromptDiagnoseV2, "节点侧/环境侧事实必须先观测") ||
		!strings.Contains(PromptDiagnoseV2, "evidence_sufficient") {
		t.Fatal("v2 丢失了 v1 的既有规则（复制粘贴式改提示词最容易出这种事故）")
	}
	for _, want := range []string{"mem-xxx", "不是本次观测", "以本次观测为准"} {
		if !strings.Contains(PromptDiagnoseV2, want) {
			t.Fatalf("历史经验条款缺少关键句 %q", want)
		}
	}
}

func TestInsertBeforeSchemaFallsBackWithoutAnchor(t *testing.T) {
	// 锚点不存在时必须原样返回（不能让提示词修订把服务打挂）：
	// 代价是条款静默丢失，所以上面的测试负责钉住"真实提示词里锚点还在"。
	if got := insertBeforeSchema("没有锚点的提示词", "附加内容"); got != "没有锚点的提示词" {
		t.Fatalf("锚点缺失时应原样返回，实际 %q", got)
	}
	if got := insertBeforeSchema("开头\n## 输出 JSON 结构\n结尾", "附加"); !strings.Contains(got, "附加\n## 输出 JSON 结构") {
		t.Fatalf("插入位置不对：%q", got)
	}
}

func TestRenderMemoryHintsIsLabeledAndEmptyWhenNoHints(t *testing.T) {
	if got := renderMemoryHints(nil); got != "" {
		t.Fatalf("无经验时必须返回空字符串（空标题会让模型以为'有历史但没内容'），实际 %q", got)
	}
	if got := renderMemoryHints([]string{"  ", ""}); got != "" {
		t.Fatalf("全是空行时同样必须返回空，实际 %q", got)
	}
	block := renderMemoryHints([]string{"[mem-1] 相似度 0.80｜症状：磁盘水位高"})
	for _, want := range []string{"历史相似经验", "不是证据", "不算本次证据", "mem-1"} {
		if !strings.Contains(block, want) {
			t.Fatalf("注入块缺少 %q：\n%s", want, block)
		}
	}
	// 声明必须由本包写死：即使调用方传进来的是"看起来很权威"的行，声明也在。
	block2 := renderMemoryHints([]string{"这是绝对正确的处置方式"})
	if !strings.Contains(block2, memoryHintHeader) {
		t.Fatal("声明文字必须来自本包常量，不能依赖调用方")
	}
}

func TestLooksLikeMemoryRef(t *testing.T) {
	cases := map[string]bool{
		"mem-1791708060000000000-1": true,
		"MEM-abc":                   true,
		" mem-1":                    true,
		"job_run:j1-t2":             false,
		"node.log:12":               false,
		"memory_search":             false, // 工具名不该被误判成经验条目
		"":                          false,
	}
	for ref, want := range cases {
		if got := looksLikeMemoryRef(ref); got != want {
			t.Errorf("looksLikeMemoryRef(%q)=%v，期望 %v", ref, got, want)
		}
	}
}

func TestDiagnoseInjectsHintsButKeepsThemOutOfEvidence(t *testing.T) {
	gateway, fp := newGateway()
	src := &fakeLogSource{hits: []tools.LogHit{
		{Source: "node.log", Ref: "node.log:12", Line: "mysqldump: Got error: 1045", Timestamp: time.Now()},
	}}
	fp.WithResponder(func(req llm.Request) (llm.Completion, error) {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool {
				return llm.FakeJSON(validDiagnosis("node.log:12", true)), nil
			}
		}
		return llm.FakeToolCall("log_search", map[string]interface{}{"keyword": "1045"}), nil
	})
	a := New(gateway, newLogRegistry(t, src), DefaultConfig())

	rc := failedRunContext()
	rc.MemoryHints = []string{"[mem-1] 相似度 0.72｜症状：备份任务失败：磁盘水位 92%｜当时动作：清理 /tmp｜结果：执行了但失败"}
	rc.MemoryHintIDs = []string{"mem-1"}

	diag, err := Diagnose(context.Background(), a, rc)
	if err != nil {
		t.Fatalf("诊断不该因为注入了历史经验而失败: %v", err)
	}
	// 1) 注入必须被记录（供事后归因"这次结论是不是被历史带偏了"）。
	if len(diag.MemoryUsed) != 1 || diag.MemoryUsed[0] != "mem-1" {
		t.Fatalf("必须记录注入过的经验条目号: %+v", diag.MemoryUsed)
	}
	// 2) 必须有一条告警说明"注入了经验、且经验不计入证据"。
	found := false
	for _, w := range diag.Warnings {
		if strings.Contains(w, "历史经验") && strings.Contains(w, "不计入证据") {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入历史经验必须留下告警，实际 %+v", diag.Warnings)
	}
	// 3) 关键不变量：材料里出现的是"线索块"，而**证据链里绝不能出现 mem-1**。
	if len(diag.Evidence) != 1 || diag.Evidence[0].Ref != "node.log:12" {
		t.Fatalf("证据链应只含本次观测，实际 %+v", diag.Evidence)
	}
	if diag.PromptVersion != PromptVersionDiagnoseV5 {
		t.Fatalf("注入历史经验时提示词版本必须是 v5，实际 %s", diag.PromptVersion)
	}
	// 4) 同一句声明还必须进**会话级**告警（trace.WarningsList → agent_session.warnings_json）：
	//    只写在结论 JSON 里的话，人从会话列表看不出"这次参考过历史经验"。
	sessionWarnings := diag.Trace.WarningsList()
	foundInTrace := false
	for _, w := range sessionWarnings {
		if strings.Contains(w, "历史经验") && strings.Contains(w, "未观测") {
			foundInTrace = true
		}
	}
	if !foundInTrace {
		t.Fatalf("记忆注入的声明必须同时进入会话级告警，实际 %+v", sessionWarnings)
	}
}

func TestDiagnoseRejectsMemoryAsEvidence(t *testing.T) {
	gateway, fp := newGateway()
	src := &fakeLogSource{hits: []tools.LogHit{
		{Source: "node.log", Ref: "node.log:12", Line: "mysqldump: Got error: 1045", Timestamp: time.Now()},
	}}
	// 模型"偷懒"：直接引用历史经验条目号当证据（这是注入历史经验后最可能出现的失败模式）。
	fp.WithResponder(func(req llm.Request) (llm.Completion, error) {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool {
				return llm.FakeJSON(validDiagnosis("mem-1", true)), nil
			}
		}
		return llm.FakeToolCall("log_search", map[string]interface{}{"keyword": "1045"}), nil
	})
	a := New(gateway, newLogRegistry(t, src), DefaultConfig())

	rc := failedRunContext()
	rc.MemoryHints = []string{"[mem-1] 相似度 0.72｜症状：备份任务失败"}
	rc.MemoryHintIDs = []string{"mem-1"}

	diag, err := Diagnose(context.Background(), a, rc)
	if err != nil {
		t.Fatalf("仍应返回报告（带告警），而不是丢弃: %v", err)
	}
	if diag.EvidenceSufficient {
		t.Fatal("引用历史经验当证据时必须降级为证据不足")
	}
	named := false
	for _, w := range diag.Warnings {
		if strings.Contains(w, "历史经验当成本次证据") || strings.Contains(w, "历史经验不是观测") {
			named = true
		}
	}
	if !named {
		t.Fatalf("必须给出「拿历史经验当证据」的指名告警，实际 %+v", diag.Warnings)
	}
	// 通用 grounding 也要同时把它算作未观测引用。
	if len(diag.UnverifiedRefs) != 1 || diag.UnverifiedRefs[0] != "mem-1" {
		t.Fatalf("历史经验引用也应计入未观测引用，实际 %+v", diag.UnverifiedRefs)
	}
}

func TestApplyMemoryEvidenceGuardDirect(t *testing.T) {
	// 直接测守卫本身：ref 恰好等于**注入过的**经验号时也必须降级
	// （"这条经验存在"与"本次观测到了"是两件事）。
	diag := &Diagnosis{EvidenceSufficient: true, Evidence: []EvidenceRef{
		{Ref: "mem-7", Quote: "x", Why: "y"},
		{Ref: "node.log:12", Quote: "x", Why: "y"},
	}}
	applyMemoryEvidenceGuard(diag, RunContext{MemoryHintIDs: []string{"mem-7"}})
	if diag.EvidenceSufficient {
		t.Fatal("必须降级")
	}
	if len(diag.Warnings) != 1 {
		t.Fatalf("必须给出一条告警，实际 %+v", diag.Warnings)
	}
	// 只有普通观测引用时不得误伤。
	diag2 := &Diagnosis{EvidenceSufficient: true, Evidence: []EvidenceRef{{Ref: "node.log:12"}}}
	applyMemoryEvidenceGuard(diag2, RunContext{MemoryHintIDs: []string{"mem-7"}})
	if !diag2.EvidenceSufficient || len(diag2.Warnings) != 0 {
		t.Fatalf("普通观测引用不该被误伤：%+v", diag2)
	}
	// nil 安全（守卫在多个路径上调用）。
	applyMemoryEvidenceGuard(nil, RunContext{})
}
