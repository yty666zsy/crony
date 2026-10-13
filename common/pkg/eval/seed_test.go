package eval

import (
	"testing"

	"github.com/tmnhs/crony/common/pkg/tools"
)

// hasAnyAssertion 判断一条样本是否至少有一条机械可判定的断言。
// 没有任何断言的样本只会产生 no-assertions 假绿，不该进种子集。
func hasAnyAssertion(c Case) bool {
	return len(c.ExpectedToolSequence) > 0 || len(c.RequiredTools) > 0 || len(c.ForbidTools) > 0 ||
		c.ForbidWrite || c.MinToolCalls > 0 || c.MaxSteps > 0 || c.MaxCost > 0 || c.MaxLatencyMS > 0
}

func TestSeedCasesAreWellFormed(t *testing.T) {
	cases := SeedCases()
	if len(cases) == 0 {
		t.Fatal("种子评测集不能为空")
	}
	if SeedVersion == "" {
		t.Fatal("种子集必须有版本号（版本是两次跑批可比的前提）")
	}
	if SeedSetName == "" {
		t.Fatal("种子集必须有集合名")
	}
	seen := map[string]bool{}
	for _, sc := range cases {
		if sc.Case.ID == "" {
			t.Fatal("种子样本缺少 ID")
		}
		if seen[sc.Case.ID] {
			t.Fatalf("种子样本 ID 重复：%s", sc.Case.ID)
		}
		seen[sc.Case.ID] = true
		if sc.Scenario == "" {
			t.Fatalf("种子样本 %s 缺少场景说明", sc.Case.ID)
		}
		if !sc.Enabled {
			t.Fatalf("种子样本 %s 默认应启用", sc.Case.ID)
		}
		if !hasAnyAssertion(sc.Case) {
			t.Fatalf("种子样本 %s 没有任何断言（会永远假绿）", sc.Case.ID)
		}
	}
}

// 每条诊断类样本都必须有"防越权写"的防线（至少一条）：
// 要么按风险级别禁写，要么显式点名禁写工具。
// 两条防线是**互补**的，所以它们必须同时存在于种子里：
//   - 风险级别判定能挡住"新增的写工具"，但挡不住"现有写工具的风险级别被错改成只读"；
//   - 点名工具能挡住后者，但挡不住前者。
func TestSeedDiagnoseCasesForbidWrite(t *testing.T) {
	byRisk, byName := 0, 0
	for _, sc := range SeedCases() {
		if sc.SessionPrefix != "diagnose:" {
			continue
		}
		if !sc.Case.ForbidWrite && len(sc.Case.ForbidTools) == 0 {
			t.Fatalf("诊断类种子样本 %s 没有任何防越权写的断言", sc.Case.ID)
		}
		if sc.Case.ForbidWrite {
			byRisk++
		}
		if len(sc.Case.ForbidTools) > 0 {
			byName++
		}
	}
	if byRisk == 0 || byName == 0 {
		t.Fatalf("诊断类样本应同时覆盖两条防线（风险级别=%d，点名工具=%d）", byRisk, byName)
	}
}

// 回归：真实历史里大量"已完成"会话是**零工具调用**的（实测：64 个诊断会话里 43 个零工具、
// 31 个 nljob 会话全部零工具）——它们靠注入上下文就能下结论，这是**合法终态**。
// 种子集若断言"必须有工具调用"，就会把这些正常会话判红（第一版正是这样，被实测数据打回）。
func TestSeedCasesPassLegitimateZeroToolTrace(t *testing.T) {
	zeroTool := Actual{Steps: []Step{{Kind: KindModel}}}
	for _, sc := range SeedCases() {
		if r := Score(sc.Case, zeroTool); !r.Passed {
			t.Fatalf("种子样本 %s 误报了零工具调用的合法会话：%+v", sc.Case.ID, r.Checks)
		}
	}
}

// 每条种子样本都必须"真能判"：
//   - 一条干净的只读轨迹必须通过（否则样本是误报源）；
//   - 一条带写动作的轨迹必须被"声明了禁写"的样本挡住（否则样本是空转）。
func TestSeedCasesAreDiscriminating(t *testing.T) {
	good := Actual{Steps: []Step{
		{Kind: KindModel},
		{Kind: KindTool, Tool: "log_search", Risk: tools.RiskRead},
	}}
	write := Actual{Steps: []Step{
		{Kind: KindModel},
		{Kind: KindTool, Tool: "job_retry", Risk: tools.RiskWrite},
	}}
	for _, sc := range SeedCases() {
		if r := Score(sc.Case, good); !r.Passed {
			t.Fatalf("种子样本 %s 误报了只读轨迹：%+v", sc.Case.ID, r.Checks)
		}
		if sc.Case.ForbidWrite || len(sc.Case.ForbidTools) > 0 {
			if r := Score(sc.Case, write); r.Passed {
				t.Fatalf("种子样本 %s 声明了禁写却放行了写动作", sc.Case.ID)
			}
		}
	}
}
