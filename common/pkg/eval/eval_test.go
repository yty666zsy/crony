package eval

import (
	"testing"

	"github.com/tmnhs/crony/common/pkg/tools"
)

func TestScorePassesWhenWithinBounds(t *testing.T) {
	c := Case{ID: "case-1", ExpectedToolSequence: []string{"log_search", "node_status"}, MaxSteps: 6, MaxCost: 0.05, MaxLatencyMS: 30000}
	a := Actual{
		Steps: []Step{
			{Kind: "think"},
			{Kind: "tool_call", Tool: "log_search"},
			{Kind: "observation"},
			{Kind: "tool_call", Tool: "node_status"},
			{Kind: "final"},
		},
		Cost: 0.02, LatencyMS: 12000,
	}
	r := Score(c, a)
	if !r.Passed || r.Score != 1 {
		t.Fatalf("应通过且满分为 1，实际 passed=%v score=%v checks=%+v", r.Passed, r.Score, r.Checks)
	}
}

func TestToolSequenceMismatchFails(t *testing.T) {
	c := Case{ID: "case-2", ExpectedToolSequence: []string{"log_search", "node_probe"}}
	a := Actual{Steps: []Step{{Kind: "tool_call", Tool: "log_search"}}}
	r := Score(c, a)
	if r.Passed {
		t.Fatalf("序列不匹配应判失败，实际 %+v", r)
	}
	// 命中 sequence 断言。
	found := false
	for _, ck := range r.Checks {
		if ck.Name == "tool-sequence" {
			found = true
			if ck.Passed {
				t.Fatalf("tool-sequence 断言应为不通过")
			}
		}
	}
	if !found {
		t.Fatalf("缺少 tool-sequence 断言")
	}
}

func TestStepCountBound(t *testing.T) {
	c := Case{ID: "c", MaxSteps: 2}
	a := Actual{Steps: []Step{{Kind: "think"}, {Kind: "tool_call", Tool: "x"}, {Kind: "final"}}}
	if Score(c, a).Passed {
		t.Fatalf("步数 3 > 上限 2 应失败")
	}
}

// 未计量成本/延迟时跳过对应断言，而不是判失败——否则缺数据的样本会污染通过率。
func TestUnmeasuredCostSkipped(t *testing.T) {
	c := Case{ID: "c", MaxCost: 0.01, MaxLatencyMS: 1000}
	a := Actual{Cost: 0, LatencyMS: 0}
	r := Score(c, a)
	if !r.Passed {
		t.Fatalf("未计量成本/延迟应跳过断言而非失败，实际 %+v", r.Checks)
	}
}

func TestNoAssertionsPasses(t *testing.T) {
	r := Score(Case{ID: "empty"}, Actual{})
	if !r.Passed {
		t.Fatalf("无断言样本应通过（但报告里会说 no-assertions）")
	}
}

func TestToolSequenceExtraction(t *testing.T) {
	got := ToolSequence([]Step{{Kind: "think"}, {Kind: "tool_call", Tool: "a"}, {Kind: "observation"}, {Kind: "tool_call", Tool: "b"}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("抽取工具序列错误：%v", got)
	}
}

// 未声明的断言不该出现在明细里：不显示还没考的项，也不给通过率的分母掺水。
func TestUndeclaredAssertionsAreNotEmitted(t *testing.T) {
	r := Score(Case{ID: "c", MaxSteps: 10}, Actual{Steps: []Step{{Kind: KindModel}}})
	if len(r.Checks) != 1 || r.Checks[0].Name != "step-count" {
		t.Fatalf("只应产生声明的断言（step-count），实际 %+v", r.Checks)
	}
	if !r.Passed || r.Score != 1 {
		t.Fatalf("唯一声明的断言通过时应为满分，实际 passed=%v score=%v", r.Passed, r.Score)
	}
}

// 完全没断言的样本不能假装"考过了"：明细里必须留下 no-assertions 的痕迹。
func TestNoAssertionLeavesTrace(t *testing.T) {
	r := Score(Case{ID: "c"}, Actual{})
	if len(r.Checks) != 1 || r.Checks[0].Name != "no-assertions" {
		t.Fatalf("无断言样本应在明细里标注 no-assertions，实际 %+v", r.Checks)
	}
}

// 回归：轨迹的**真实**词汇是 model/tool/notice（common/pkg/agent），
// 而不是 think/tool_call/observation/final。内核曾经只认后者，
// 于是对真实轨迹恒返回空序列——而内核自测用的又恰好是错词汇，
// 所以"测试全绿、真实数据全错"。这个用例钉住两套词汇都认。
func TestToolSequenceAcceptsRecordedVocabulary(t *testing.T) {
	got := ToolSequence([]Step{
		{Kind: KindModel},
		{Kind: KindTool, Tool: "log_search"},
		{Kind: KindNotice},
		{Kind: KindTool, Tool: "node_status"},
	})
	if len(got) != 2 || got[0] != "log_search" || got[1] != "node_status" {
		t.Fatalf("真实词汇（tool）必须被识别，实际 %v", got)
	}
	if !IsToolStep(KindTool) || !IsToolStep(kindToolCallAlias) || IsToolStep(KindModel) {
		t.Fatalf("IsToolStep 判定错误：tool=%v tool_call=%v model=%v",
			IsToolStep(KindTool), IsToolStep(kindToolCallAlias), IsToolStep(KindModel))
	}
}

// 用真实词汇跑一遍序列断言：这是"真实轨迹也能算出工具序列"的直接证据。
func TestScoreWithRecordedVocabularyMatchesSequence(t *testing.T) {
	c := Case{ID: "c", ExpectedToolSequence: []string{"log_search", "node_status"}}
	a := Actual{Steps: []Step{
		{Kind: KindModel},
		{Kind: KindTool, Tool: "log_search", Risk: tools.RiskRead},
		{Kind: KindTool, Tool: "node_status", Risk: tools.RiskRead},
	}}
	if r := Score(c, a); !r.Passed {
		t.Fatalf("真实词汇下序列应匹配，实际 %+v", r.Checks)
	}
}

// 安全不变量：诊断类样本不得出现写/破坏性工具。
func TestForbidWriteBlocksWriteAndDestructive(t *testing.T) {
	c := Case{ID: "c", ForbidWrite: true}
	for _, risk := range []tools.RiskLevel{tools.RiskWrite, tools.RiskDestructive} {
		a := Actual{Steps: []Step{{Kind: KindTool, Tool: "job_retry", Risk: risk}}}
		r := Score(c, a)
		if r.Passed {
			t.Fatalf("风险 %s 必须判失败", risk)
		}
	}
	ok := Actual{Steps: []Step{{Kind: KindTool, Tool: "log_search", Risk: tools.RiskRead}}}
	if r := Score(c, ok); !r.Passed {
		t.Fatalf("只读工具应通过，实际 %+v", r.Checks)
	}
}

// "判不了"不等于"没问题"：查不到风险级别的工具必须报出来，除非显式放行。
func TestForbidWriteFlagsUnclassifiedUnlessAllowed(t *testing.T) {
	a := Actual{Steps: []Step{{Kind: KindTool, Tool: "some_mcp_tool"}}} // Risk 为空 = 未分类
	if r := Score(Case{ID: "c", ForbidWrite: true}, a); r.Passed {
		t.Fatalf("未分类工具不应默认放行")
	}
	if r := Score(Case{ID: "c", ForbidWrite: true, AllowUnclassified: true}, a); !r.Passed {
		t.Fatalf("显式 allow_unclassified 后应放行，实际 %+v", r.Checks)
	}
}

// 必需工具是子集判定：出现顺序可以不同，但一个都不能少。
func TestRequiredToolsIsSubsetCheck(t *testing.T) {
	c := Case{ID: "c", RequiredTools: []string{"log_search", "node_probe"}}
	a := Actual{Steps: []Step{
		{Kind: KindTool, Tool: "node_probe"},
		{Kind: KindTool, Tool: "log_search"},
	}}
	if r := Score(c, a); !r.Passed {
		t.Fatalf("顺序不同但都出现应通过，实际 %+v", r.Checks)
	}
	missing := Actual{Steps: []Step{{Kind: KindTool, Tool: "log_search"}}}
	if r := Score(c, missing); r.Passed {
		t.Fatalf("缺少必需工具应失败")
	}
}

func TestForbidToolsExactName(t *testing.T) {
	c := Case{ID: "c", ForbidTools: []string{"job_retry"}}
	a := Actual{Steps: []Step{{Kind: KindTool, Tool: "job_retry"}}}
	if r := Score(c, a); r.Passed {
		t.Fatalf("出现被禁工具应失败")
	}
}

// 取证下限：零工具调用的"结论"必须被挡住。
func TestMinToolCallsRequiresEvidence(t *testing.T) {
	c := Case{ID: "c", MinToolCalls: 1}
	if r := Score(c, Actual{Steps: []Step{{Kind: KindModel}}}); r.Passed {
		t.Fatalf("零取证应失败")
	}
	if r := Score(c, Actual{Steps: []Step{{Kind: KindTool, Tool: "log_search", Risk: tools.RiskRead}}}); !r.Passed {
		t.Fatalf("有一次取证应通过，实际 %+v", r.Checks)
	}
}

// 没记下工具名的步骤不参与"越权写"判定（判定不了，也不该凭空判红）。
func TestForbidWriteIgnoresNamelessSteps(t *testing.T) {
	a := Actual{Steps: []Step{{Kind: KindTool, Tool: ""}}}
	if r := Score(Case{ID: "c", ForbidWrite: true}, a); !r.Passed {
		t.Fatalf("无名工具步不应判定为越权写，实际 %+v", r.Checks)
	}
}
