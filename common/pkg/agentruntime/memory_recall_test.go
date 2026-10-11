package agentruntime

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agent"
	"github.com/tmnhs/crony/common/pkg/agentmemory"
)

// 这一组用例守的是"历史经验进入诊断"这条链路的**接口纪律**：
// 召回只允许写 MemoryHints / MemoryHintIDs，绝不能碰 Given（本次可用证据的登记表）。
//
// 为什么值得单独测：如果哪天有人图省事把经验塞进 Given，模型引用它就会被
// grounding 校验当成"观测过"，而所有既有测试都仍然是绿的——
// 那意味着"历史提示 ≠ 本次证据"这条不变量在实现层已经破了，只是没人发现。

// fakeRecaller 记录被问到的参数，并返回预设结果。
type fakeRecaller struct {
	res      agentmemory.RecallResult
	gotQuery string
	gotKind  string
	gotNode  string
	gotTopK  int
	calls    int
}

func (f *fakeRecaller) RecallForDiagnose(symptom, agentKind, targetNode string, topK int) agentmemory.RecallResult {
	f.calls++
	f.gotQuery, f.gotKind, f.gotNode, f.gotTopK = symptom, agentKind, targetNode, topK
	return f.res
}

func hintHit(id string) agentmemory.Hit {
	return agentmemory.Hit{
		MemoryID: id, Symptom: "备份任务失败 磁盘水位 92%", ActionText: "log_search keyword=no space",
		Outcome: models.MemoryOutcomeFailed, Score: 0.7, Matched: []string{"磁盘", "水位"},
		Reasons: []string{"命中查询词元 2 个（覆盖率 0.70）"}, Occurrences: 2,
	}
}

func TestAttachMemoryHintsOnlyTouchesHintsNotGiven(t *testing.T) {
	rec := &fakeRecaller{res: agentmemory.RecallResult{
		Query: "q", Hits: []agentmemory.Hit{hintHit("mem-1")}, Considered: 5,
		AdvisoryOnly: true, UsableAsEvidence: false, MinScore: 0.34,
	}}
	rt := &Runtime{recaller: rec}

	rc := &agent.RunContext{
		JobID: 11, JobName: "备份任务", Status: "failed",
		NodeUUID: "node-it-01", StderrTail: "mysqldump: Got error: 1045 Access denied",
		Extra: []string{"运维补充：这台机器刚做过凭据轮换"},
	}
	before := *rc

	rt.attachMemoryHints(rc)

	if len(rc.MemoryHints) != 1 || len(rc.MemoryHintIDs) != 1 || rc.MemoryHintIDs[0] != "mem-1" {
		t.Fatalf("必须把命中写进 MemoryHints/MemoryHintIDs: %+v / %+v", rc.MemoryHints, rc.MemoryHintIDs)
	}
	// 关键不变量：除了这两个字段，RunContext 里**任何东西都不许变**。
	//
	// 为什么不逐个字段断言：Given（本次可用证据）不是 RunContext 的字段，
	// 而是 agent 包用 JobID/RunID/StdoutTail/RecentRuns… 现算出来的
	// （见 givenEvidence）。也就是说**任何**被召回顺手改掉的字段都可能间接扩张证据集，
	// 所以正确的守门方式是"只允许这两个字段变化"，而不是"检查某个字段没变"。
	allowed := before
	allowed.MemoryHints = rc.MemoryHints
	allowed.MemoryHintIDs = rc.MemoryHintIDs
	if !reflect.DeepEqual(allowed, *rc) {
		t.Fatalf("召回只允许改 MemoryHints/MemoryHintIDs，其它字段被改动了:\n改前=%+v\n改后=%+v", before, *rc)
	}
	// 召回的查询必须由"当前症状"构成：任务目标 + 状态 + stderr 尾部。
	for _, want := range []string{"诊断任务 11", "备份任务", "failed", "1045"} {
		if !strings.Contains(rec.gotQuery, want) {
			t.Fatalf("召回查询缺少 %q：%s", want, rec.gotQuery)
		}
	}
	if rec.gotKind != models.AgentKindDiagnose || rec.gotNode != "node-it-01" {
		t.Fatalf("召回应带上会话类型与目标节点: kind=%q node=%q", rec.gotKind, rec.gotNode)
	}
	// topK 传 0 时由 agentmemory 兜默认值（3）：运行时不该自己发明一个数。
	if rec.gotTopK != 0 {
		t.Fatalf("未显式配置时应传 0 由下层兜默认值，实际 %d", rec.gotTopK)
	}
}

func TestAttachMemoryHintsSkipsWhenNothingToInject(t *testing.T) {
	cases := []struct {
		name string
		rt   *Runtime
	}{
		{"未接线召回", &Runtime{}},
		{"召回为空", &Runtime{recaller: &fakeRecaller{res: agentmemory.RecallResult{}}}},
		{
			"命中但渲染不出行",
			&Runtime{recaller: &fakeRecaller{res: agentmemory.RecallResult{
				// 只有空 id 的命中：渲染层会给出空行，此时**不该**注入一个空块。
				Hits: []agentmemory.Hit{{MemoryID: ""}},
			}}},
		},
	}
	for _, c := range cases {
		rc := &agent.RunContext{JobID: 1, JobName: "x"}
		c.rt.attachMemoryHints(rc)
		if len(rc.MemoryHints) != 0 || len(rc.MemoryHintIDs) != 0 {
			t.Fatalf("%s：不应注入任何东西，实际 %+v / %+v", c.name, rc.MemoryHints, rc.MemoryHintIDs)
		}
	}
	// nil 安全（诊断路径上多种装配状态都可能走到这里）。
	(&Runtime{}).attachMemoryHints(nil)
	var nilRT *Runtime
	nilRT.attachMemoryHints(&agent.RunContext{})
}

func TestAttachMemoryHintsRedactsStderrTail(t *testing.T) {
	rec := &fakeRecaller{}
	rt := &Runtime{recaller: rec}
	rc := &agent.RunContext{
		JobID: 11, JobName: "备份任务", Status: "failed",
		// 真实场景里 stderr 经常带口令/连接串：进过提示词的文本必须先脱敏。
		StderrTail: "mysql -u backup -pSup3rSecret --password=Sup3rSecret connection refused",
	}
	rt.attachMemoryHints(rc)
	if strings.Contains(rec.gotQuery, "Sup3rSecret") {
		t.Fatalf("召回查询必须脱敏（否则口令会进经验库的检索词/日志）: %s", rec.gotQuery)
	}
}
