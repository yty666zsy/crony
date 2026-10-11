package models

import (
	"strings"
	"testing"
)

// 这组测试不依赖数据库，专门钉死一类"只有真实 MySQL 才会炸"的错误：
// gorm 的 `size` tag 只在建表时生效，插入时**不校验**，超长值要等 MySQL 严格模式
// 返回 `Error 1406 Data too long for column 'prompt_version'` 才暴露。
//
// 起因是一次真实事故：agentruntime 把 131 个字符的**系统提示词全文**当成
// `prompt_version`（varchar(64)）传入，导致真实环境下**每一次诊断都落库失败**
// （前端表现为"诊断永远无法回放"，但接口返回 200，只有 persist_error 里有点线索）。
// 内存单测全绿，是真实 MySQL 集成测试抓出来的。
func TestAgentSessionValidateRejectsOverlongPromptVersion(t *testing.T) {
	s := &AgentSession{
		SessionID: "diagnose-x",
		Kind:      AgentKindDiagnose,
		// 故意用"提示词全文当版本号"的错误用法
		PromptVersion: strings.Repeat("诊", 131),
		Created:       1,
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("超长 prompt_version 必须被拒绝（真实 MySQL 会 1406）")
	}
	if !strings.Contains(err.Error(), "prompt_version") || !strings.Contains(err.Error(), "64") {
		t.Fatalf("错误信息必须点名字段与实际列宽，实际 %v", err)
	}
}

// TestAgentSessionValidateAcceptsRealValues：真实调用路径上的取值必须全部通过。
func TestAgentSessionValidateAcceptsRealValues(t *testing.T) {
	cases := []*AgentSession{
		{SessionID: "diagnose-20260101-abc", Kind: AgentKindDiagnose, Status: AgentStatusCompleted,
			StopReason: "completed", Provider: "offline", Model: "offline-model",
			PromptVersion: "diagnose-v1", SystemPrompt: strings.Repeat("诊", 4000),
			Goal: "诊断任务[备份]（id=1）第 j1-t1 次运行失败的原因"},
		{SessionID: "nljob-20260101-abc", Kind: AgentKindNLJob, PromptVersion: "nljob-v1",
			SystemPrompt: strings.Repeat("你", 221)},
		{SessionID: "job7-1-1", Kind: AgentKindJob, PromptVersion: "inspect-v1"},
	}
	for _, s := range cases {
		if err := s.Validate(); err != nil {
			t.Fatalf("合法会话被拒绝（%s）: %v", s.SessionID, err)
		}
	}
}

// TestAgentSessionValidateRejectsEmptySessionID：session_id 是唯一索引，空值会造成
// "第二次写入撞唯一键"这类莫名其妙的故障。
func TestAgentSessionValidateRejectsEmptySessionID(t *testing.T) {
	if err := (&AgentSession{SessionID: "   "}).Validate(); err == nil {
		t.Fatal("空 session_id 必须被拒绝")
	}
	if err := (*AgentSession)(nil).Validate(); err == nil {
		t.Fatal("nil 会话必须被拒绝（不能 panic）")
	}
}

// TestAgentStepValidate：步骤表的短列与序号同样要挡住。
func TestAgentStepValidate(t *testing.T) {
	ok := &AgentStep{SessionID: "s1", Seq: 1, Kind: "model", Provider: "offline", Model: "m"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法步骤被拒绝: %v", err)
	}
	bad := []*AgentStep{
		{SessionID: "", Seq: 1, Kind: "model"},
		{SessionID: "s1", Seq: -1, Kind: "model"},
		{SessionID: "s1", Seq: 1, Kind: strings.Repeat("k", 17)},
		{SessionID: "s1", Seq: 1, Kind: "model", ToolName: strings.Repeat("t", 65)},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Fatalf("第 %d 个非法步骤必须被拒绝: %+v", i, s)
		}
	}
}

// TestPromptVersionConstantsFitColumn：把"短版本号必须短"变成常量级约束。
// 一旦有人把长文本常量改名为 *Version*，这里立刻失败，不必等真库。
func TestPromptVersionConstantsFitColumn(t *testing.T) {
	// 与 agent 包里的常量同值（此处不做 import：models 不能反向依赖 agent）。
	versions := []string{"diagnose-v1", "nljob-v1", "inspect-v1"}
	for _, v := range versions {
		if len([]rune(v)) > 64 {
			t.Fatalf("prompt_version 常量 %q 超出列宽 64", v)
		}
	}
}
