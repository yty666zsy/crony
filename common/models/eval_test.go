package models

import "testing"

// 合法样本：字段齐、期望值是 JSON 对象。
func TestEvalCaseValidateAcceptsWellFormed(t *testing.T) {
	c := EvalCase{
		CaseKey:       "seed-diagnose-readonly",
		SetName:       "seed",
		SetVersion:    "seed-v1",
		Scenario:      "故障诊断：只取证、不动手",
		SessionPrefix: "diagnose:",
		ExpectJSON:    `{"id":"seed-diagnose-readonly","forbid_write":true}`,
		Enabled:       true,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("合法样本不该被拒：%v", err)
	}
}

// 写坏的样本必须在**入库时**就被挡住：
// 若等到跑批时 JSON 解析失败，那条样本会被当成"没通过"，掩盖真正的坏数据。
func TestEvalCaseValidateRejectsBroken(t *testing.T) {
	base := func() EvalCase {
		return EvalCase{
			CaseKey: "k", SetName: "seed", SetVersion: "seed-v1",
			ExpectJSON: `{"id":"k"}`, Enabled: true,
		}
	}
	cases := []struct {
		name string
		mut  func(*EvalCase)
	}{
		{"缺 case_key", func(c *EvalCase) { c.CaseKey = "" }},
		{"缺版本", func(c *EvalCase) { c.SetVersion = "" }},
		{"缺期望值", func(c *EvalCase) { c.ExpectJSON = "" }},
		{"期望值不是 JSON", func(c *EvalCase) { c.ExpectJSON = "not-json" }},
		{"期望值是数组不是对象", func(c *EvalCase) { c.ExpectJSON = `[1,2]` }},
		{"场景说明超长", func(c *EvalCase) { c.Scenario = string(make([]rune, 513)) }},
	}
	for _, tc := range cases {
		c := base()
		tc.mut(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("%s：应被拒绝", tc.name)
		}
	}
}

// 跑批计数自洽是硬要求：一份自相矛盾的报告会让人以为"评测过了"。
func TestEvalRunValidateRequiresConsistentCounters(t *testing.T) {
	ok := EvalRun{SetName: "seed", SetVersion: "seed-v1", Mode: EvalModeReplay,
		Cases: 4, Passed: 2, Failed: 1, Skipped: 1}
	if err := ok.Validate(); err != nil {
		t.Fatalf("自洽的跑批不该被拒：%v", err)
	}
	bad := ok
	bad.Skipped = 0 // 2+1+0 ≠ 4
	if err := bad.Validate(); err == nil {
		t.Fatal("计数不自洽必须被拒绝")
	}
	if err := (&EvalRun{Cases: 0, Passed: 0, Failed: 0, Skipped: 0}).Validate(); err == nil {
		t.Fatal("缺版本必须被拒绝")
	}
}
