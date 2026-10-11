package models

import (
	"strings"
	"testing"
	"time"
)

// 这一组用例守的是经验库的两个"算错就误导决策"的地方：
// 去重键的稳定性（决定经验库会不会无限膨胀）与成功率口径（决定历史数据怎么被解读）。

func TestMemoryDedupeKeyIsStableAndCaseInsensitive(t *testing.T) {
	a := MemoryDedupeKey("诊断任务 11 本次失败的原因", "tool", "node_status {\"node_uuid\":\"n1\"}", "node-it-01")
	b := MemoryDedupeKey("  诊断任务 11   本次失败的原因 ", "TOOL", "node_status {\"node_uuid\":\"n1\"}", "node-it-01")
	if a != b {
		t.Fatalf("同一症状的大小写/空白差异必须得到同一个去重键：\n%s\n%s", a, b)
	}
	c := MemoryDedupeKey("诊断任务 11 本次失败的原因", "tool", "log_search {\"keyword\":\"x\"}", "node-it-01")
	if a == c {
		t.Fatal("不同动作不能共用去重键（否则会把两种处置合并成一条经验）")
	}
	d := MemoryDedupeKey("诊断任务 11 本次失败的原因", "tool", "node_status {\"node_uuid\":\"n1\"}", "node-it-02")
	if a == d {
		t.Fatal("不同目标节点不能共用去重键（一条经验不一定适用于另一台机器）")
	}
	if len(a) != 64 {
		t.Fatalf("去重键应是 64 位 sha256 十六进制，实际 %d", len(a))
	}
}

func TestMemorySuccessRateExcludesNonExecutedSamples(t *testing.T) {
	m := &AgentMemory{SuccessCount: 3, FailCount: 1, BlockedCount: 5, DeniedCount: 2, AdvisedCount: 9}
	rate, known := m.MemorySuccessRate()
	if !known {
		t.Fatal("有执行样本时必须 known=true")
	}
	if rate != 0.75 {
		t.Fatalf("成功率应为 3/(3+1)=0.75（不含 blocked/denied/advised），实际 %v", rate)
	}
	// 从没执行过：必须能区分"0% 成功"与"没有数据"。
	none := &AgentMemory{BlockedCount: 4, AdvisedCount: 2}
	if r, known := none.MemorySuccessRate(); known || r != 0 {
		t.Fatalf("无执行样本时应返回 (0,false)，实际 (%v,%v)", r, known)
	}
}

func TestApplyOutcomeKeepsCountsConsistent(t *testing.T) {
	m := &AgentMemory{}
	m.ApplyOutcome(MemoryOutcomeSuccess)
	m.ApplyOutcome(MemoryOutcomeFailed)
	m.ApplyOutcome(MemoryOutcomeBlocked)
	m.ApplyOutcome(MemoryOutcomeDenied)
	m.ApplyOutcome(MemoryOutcomeAdvised)
	if m.OccurrenceCount != 5 {
		t.Fatalf("出现次数应为 5，实际 %d", m.OccurrenceCount)
	}
	counts := m.MemoryOutcomeCounts()
	sum := 0
	for _, v := range counts {
		sum += v
	}
	if sum != m.OccurrenceCount {
		t.Fatalf("各结果计数之和必须等于出现次数（%d != %d）：%v", sum, m.OccurrenceCount, counts)
	}
	if m.Outcome != MemoryOutcomeAdvised {
		t.Fatalf("Outcome 应更新为最近一次的结果，实际 %s", m.Outcome)
	}
}

func TestAgentMemoryValidateRejectsOverlongColumns(t *testing.T) {
	base := MemoryObservation{
		SessionID: "s", AgentKind: AgentKindDiagnose,
		Symptom: "诊断任务 1 本次失败的原因", ActionKind: MemoryActionTool, Outcome: MemoryOutcomeSuccess,
	}
	m := base.ToAgentMemory(time.Now())
	if err := m.Validate(); err != nil {
		t.Fatalf("正常条目不该报错：%v", err)
	}
	// 症状超长（> 512 字符）必须在校验阶段被拒，而不是等 MySQL 1406。
	long := base
	long.Symptom = strings.Repeat("症", 513)
	if err := long.ToAgentMemory(time.Now()).Validate(); err == nil {
		t.Fatal("超长 symptom 必须被 Validate 拒绝")
	}
	empty := base
	empty.Symptom = "   "
	if err := empty.ToAgentMemory(time.Now()).Validate(); err == nil {
		t.Fatal("空 symptom 必须被拒绝（无症状的经验无法被召回，也不可解释）")
	}
}

func TestToAgentMemorySetsBothTimeScales(t *testing.T) {
	now := time.Unix(1800000000, 123456789)
	obs := MemoryObservation{
		SessionID: "s", AgentKind: AgentKindDiagnose, Symptom: "症状", ActionKind: MemoryActionAdvice,
		Outcome: MemoryOutcomeAdvised,
	}
	m := obs.ToAgentMemory(now)
	if m.Created != now.Unix() || m.CreatedMS != now.UnixMilli() {
		t.Fatalf("created 双口径时间不对：%d / %d", m.Created, m.CreatedMS)
	}
	if m.LastSeen != now.Unix() || m.LastSeenMS != now.UnixMilli() {
		t.Fatalf("last_seen 双口径时间不对：%d / %d", m.LastSeen, m.LastSeenMS)
	}
	if !m.Enabled {
		t.Fatal("新沉淀的经验默认必须启用")
	}
	if m.OccurrenceCount != 1 {
		t.Fatalf("新条目出现次数应为 1，实际 %d", m.OccurrenceCount)
	}
	if m.MemoryID == "" || !strings.HasPrefix(m.MemoryID, "mem-") {
		t.Fatalf("memory_id 形态不对：%q", m.MemoryID)
	}
	if m.SuccessCount+m.FailCount+m.BlockedCount+m.DeniedCount+m.AdvisedCount != 1 {
		t.Fatalf("计数列应有且只有一个为 1：%v", m.MemoryOutcomeCounts())
	}
}

func TestNewMemoryIDIsUniqueUnderBurst(t *testing.T) {
	now := time.Now()
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := NewMemoryID(now)
		if seen[id] {
			t.Fatalf("同纳秒下 id 碰撞：%s", id)
		}
		seen[id] = true
	}
}

func TestSetAgentMemoryEnabledRequiresReasonAndIsCAS(t *testing.T) {
	// 这两个断言不需要数据库：参数校验发生在 SQL 之前，
	// 而"停用必须写原因"是治理要求，不是存储细节。
	if _, err := SetAgentMemoryEnabled(nil, MemoryStateChange{MemoryID: "mem-1", Enabled: false}); err == nil {
		t.Fatal("停用必须写明原因")
	}
	if _, err := SetAgentMemoryEnabled(nil, MemoryStateChange{MemoryID: "", Enabled: true}); err == nil {
		t.Fatal("memory_id 不能为空")
	}
}

func TestMemoryQueryNormalize(t *testing.T) {
	q := MemoryQuery{}
	q.normalize()
	if q.Page != 1 || q.PageSize != 20 {
		t.Fatalf("默认分页应为 1/20，实际 %d/%d", q.Page, q.PageSize)
	}
	q2 := MemoryQuery{Page: -3, PageSize: 100000}
	q2.normalize()
	if q2.Page != 1 || q2.PageSize != 200 {
		t.Fatalf("分页必须被夹到 1/200 以内，实际 %d/%d", q2.Page, q2.PageSize)
	}
}
