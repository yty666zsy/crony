package service

import (
	"strings"
	"testing"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/eval"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 轨迹映射：风险级别靠工具注册表补上，延迟以会话端到端耗时为准。
func TestEvalActualFromStepsFillsRiskAndNormalizesLatency(t *testing.T) {
	steps := []models.AgentStep{
		{Kind: eval.KindModel, LatencyMs: 1200},
		{Kind: eval.KindTool, ToolName: "log_search", ToolArgsJSON: `{"keyword":"x"}`},
		{Kind: eval.KindTool, ToolName: "job_retry"},
		{Kind: eval.KindTool, ToolName: "mcp_unknown"},
	}
	a := evalActualFromSteps(steps, 9000)
	if len(a.Steps) != 4 {
		t.Fatalf("步数映射错误：%d", len(a.Steps))
	}
	if a.Steps[0].Risk != "" {
		t.Fatalf("模型步不该有风险级别，实际 %q", a.Steps[0].Risk)
	}
	if a.Steps[1].Risk != tools.RiskRead || a.Steps[2].Risk != tools.RiskWrite {
		t.Fatalf("风险级别映射错误：log_search=%q job_retry=%q", a.Steps[1].Risk, a.Steps[2].Risk)
	}
	if a.Steps[3].Risk != "" {
		t.Fatalf("非内置工具必须留空=未分类（不许猜），实际 %q", a.Steps[3].Risk)
	}
	if a.Steps[1].ArgsHash == "" {
		t.Fatal("有参数的步骤应算出 ArgsHash")
	}
	if a.LatencyMS != 9000 {
		t.Fatalf("会话有端到端耗时时应以它为准，实际 %d", a.LatencyMS)
	}
	if got := evalActualFromSteps(steps, 0).LatencyMS; got != 1200 {
		t.Fatalf("无会话耗时时应退回步耗时之和，实际 %d", got)
	}
}

func TestRiskOfToolDoesNotGuess(t *testing.T) {
	if got := riskOfTool("job_retry"); got != tools.RiskWrite {
		t.Fatalf("job_retry 应为写动作，实际 %q", got)
	}
	if got := riskOfTool("node_probe"); got != tools.RiskRead {
		t.Fatalf("node_probe 应为只读，实际 %q", got)
	}
	if got := riskOfTool("mcp_some"); got != "" {
		t.Fatalf("未知工具必须返回空（未分类），实际 %q", got)
	}
}

func TestCaseFromRowUsesCaseKeyAndRejectsBrokenJSON(t *testing.T) {
	row := models.EvalCase{CaseKey: "seed-x", ExpectJSON: `{"id":"stale","forbid_write":true,"max_steps":7}`}
	c, err := caseFromRow(&row)
	if err != nil {
		t.Fatalf("合法期望值不该报错：%v", err)
	}
	if c.ID != "seed-x" {
		t.Fatalf("身份应以 case_key 为准（防止种子改键名后身份漂移），实际 %q", c.ID)
	}
	if !c.ForbidWrite || c.MaxSteps != 7 {
		t.Fatalf("断言未还原：%+v", c)
	}
	if _, err := caseFromRow(&models.EvalCase{CaseKey: "x", ExpectJSON: `{`}); err == nil {
		t.Fatal("损坏的期望值必须报错（调用方据此判 failed，不能静默当成无断言样本）")
	}
}

// 样本级判定：任一会话不通过，样本就不通过；明细要指明来源会话。
func TestAggregateCaseChecksFailsIfAnySessionFails(t *testing.T) {
	per := []EvalSessionResult{
		{SessionID: "s1", Checks: []eval.Check{
			{Name: "no-write", Passed: true, Detail: "未出现任何写/破坏性工具"},
			{Name: "min-tool-calls", Passed: false, Detail: "取证不足：0 次 / 至少 1 次"},
		}},
		{SessionID: "s2", Checks: []eval.Check{
			{Name: "no-write", Passed: false, Detail: "出现了写/破坏性工具：[job_retry(write)]"},
			{Name: "min-tool-calls", Passed: true, Detail: "工具调用 3 次 / 至少 1 次"},
		}},
	}
	got := aggregateCaseChecks(per)
	if len(got) != 2 {
		t.Fatalf("断言条数应为 2（按首次出现顺序），实际 %d", len(got))
	}
	if got[0].Name != "no-write" || got[0].Passed {
		t.Fatalf("no-write 有会话不过，样本级必须不过：%+v", got[0])
	}
	if !strings.Contains(got[0].Detail, "s2") {
		t.Fatalf("明细应指明来源会话，实际 %q", got[0].Detail)
	}
	if got[1].Name != "min-tool-calls" || got[1].Passed {
		t.Fatalf("min-tool-calls 有会话不过，样本级必须不过：%+v", got[1])
	}
}

func TestFirstFailureReasonNamesSession(t *testing.T) {
	per := []EvalSessionResult{
		{SessionID: "s1", Checks: []eval.Check{{Name: "no-write", Passed: false, Detail: "x"}}},
	}
	got := firstFailureReason(per)
	if !strings.Contains(got, "s1") || !strings.Contains(got, "no-write") {
		t.Fatalf("失败原因应包含会话与断言名，实际 %q", got)
	}
}

// 样本前缀里的 LIKE 通配符必须转义：否则一条手写样本会意外匹配到别的会话类型。
func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"diagnose:": "diagnose:",
		"a%b":       `a\%b`,
		"a_b":       `a\_b`,
		`a\b`:       `a\\b`,
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Fatalf("escapeLike(%q) = %q，期望 %q", in, got, want)
		}
	}
}
