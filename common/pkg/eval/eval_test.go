package eval

import "testing"

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
