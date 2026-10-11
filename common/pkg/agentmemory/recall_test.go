package agentmemory

import (
	"strings"
	"testing"

	"github.com/tmnhs/crony/common/models"
)

// 这一组用例守的是"召回凭什么可信"这条底线：
// 相似度必须可解释、可复现，且**绝不能**把不相关的东西硬凑出来。

func TestTokenizeChineseBigramsAndASCII(t *testing.T) {
	toks := Tokenize("磁盘水位过高（/var 使用率 92%），检查 node-it-01 的 log_search")
	for _, want := range []string{"磁盘", "盘水", "水位", "使用", "用率", "node-it-01", "log_search", "92"} {
		if _, ok := toks[want]; !ok {
			t.Errorf("缺少词元 %q，实际：%v", want, TopTokens(toks, 50))
		}
	}
	// 单字权重必须低于二元组：否则"的/了"这类字会把无关文本拉近。
	if toks["磁"] != 0 && toks["磁"] >= WeightCJKBigram {
		t.Errorf("单字权重应低于二元组，实际 %v", toks["磁"])
	}
	if toks["node-it-01"] != WeightASCIIWord {
		t.Errorf("西文词权重应为 %.1f，实际 %v", WeightASCIIWord, toks["node-it-01"])
	}
}

func TestTokenizeDropsStopwordsAndTemplateWords(t *testing.T) {
	toks := Tokenize("诊断任务 3（备份任务）本次失败的原因，的")
	for _, bad := range []string{"诊断", "任务", "本次", "原因", "的"} {
		if _, ok := toks[bad]; ok {
			t.Errorf("停用词/模板词 %q 不该进入词元表", bad)
		}
	}
	// 但领域信号必须保留。
	if _, ok := toks["失败"]; !ok {
		t.Errorf("「失败」是本场景核心信号，不该被停用：%v", TopTokens(toks, 20))
	}
}

func TestTokenizeStringIsDeterministic(t *testing.T) {
	// 同一文本必须产出同一个 token 串：否则"复盘相似度"无从谈起（map 迭代顺序随机）。
	a := TokenizeString("磁盘水位过高，检查 node-it-01")
	b := TokenizeString("磁盘水位过高，检查 node-it-01")
	if a != b {
		t.Fatalf("token 串不稳定：%q vs %q", a, b)
	}
	if !strings.Contains(a, "磁盘") {
		t.Fatalf("token 串应含二元组：%q", a)
	}
}

func memRow(id, symptom string, kind, target, outcome string, success, fail int) *models.AgentMemory {
	m := &models.AgentMemory{
		MemoryID:        id,
		Symptom:         symptom,
		AgentKind:       kind,
		TargetNode:      target,
		Outcome:         outcome,
		SuccessCount:    success,
		FailCount:       fail,
		OccurrenceCount: success + fail,
		LastSeen:        1000,
	}
	return m
}

func TestRecallRanksRelatedAboveUnrelatedAndExplainsWhy(t *testing.T) {
	cands := []*models.AgentMemory{
		memRow("mem-1", "备份任务失败：磁盘水位过高，/var 使用率 92%", "diagnose", "", models.MemoryOutcomeSuccess, 3, 0),
		memRow("mem-2", "清理 /tmp 过期文件的任务超时", "diagnose", "", models.MemoryOutcomeFailed, 0, 1),
		memRow("mem-3", "完全无关的内容：证书到期提醒", "diagnose", "", models.MemoryOutcomeSuccess, 1, 0),
	}
	res := Recall(cands, "备份任务又失败了，磁盘使用率 92%，请查磁盘水位", RecallOptions{AgentKind: "diagnose"})
	if len(res.Hits) == 0 {
		t.Fatalf("应至少召回 1 条，实际 0 条（查询词元：%v）", res.QueryTokens)
	}
	top := res.Hits[0]
	if top.MemoryID != "mem-1" {
		t.Fatalf("最相似的应是 mem-1，实际 %s（分数 %v）", top.MemoryID, top.Score)
	}
	if top.Score <= 0 || top.Score > 1.1 {
		t.Fatalf("分数必须在 (0, 1.1]（加成上限），实际 %v", top.Score)
	}
	// 可解释性：必须给出命中词与打分明细，而不是只有一个分数。
	if len(top.Matched) == 0 || len(top.Reasons) < 2 {
		t.Fatalf("召回必须可解释（Matched/Reasons），实际 matched=%v reasons=%v", top.Matched, top.Reasons)
	}
	// 不相关的（证书到期）不得混进结果。
	for _, h := range res.Hits {
		if h.MemoryID == "mem-3" {
			t.Fatalf("不相关条目被召回：%+v", h)
		}
	}
	// 召回结果必须自述"不能当证据"。
	if !res.AdvisoryOnly || res.UsableAsEvidence {
		t.Fatalf("召回结果必须声明 advisory_only=true / usable_as_evidence=false，实际 %+v", res)
	}
}

func TestRecallReturnsEmptyRatherThanFillingQuota(t *testing.T) {
	cands := []*models.AgentMemory{
		memRow("mem-1", "证书到期提醒", "diagnose", "", models.MemoryOutcomeSuccess, 1, 0),
		memRow("mem-2", "磁盘水位过高", "diagnose", "", models.MemoryOutcomeSuccess, 1, 0),
	}
	// 查询与候选只有"任务/失败"这类模板词组重合 → 不该硬凑出结果。
	res := Recall(cands, "队列积压长度告警", RecallOptions{})
	if len(res.Hits) != 0 {
		t.Fatalf("不相关查询必须返回空列表（宁可什么都不给），实际 %+v", res.Hits)
	}
	if res.Considered != 2 {
		t.Fatalf("Considered 应记录比较过的候选数，实际 %d", res.Considered)
	}
}

func TestRecallEmptyQueryIsSafe(t *testing.T) {
	cands := []*models.AgentMemory{memRow("mem-1", "磁盘水位过高", "diagnose", "", models.MemoryOutcomeSuccess, 1, 0)}
	for _, q := range []string{"", "   ", "的", "！？。"} {
		res := Recall(cands, q, RecallOptions{})
		if len(res.Hits) != 0 {
			t.Fatalf("空/停用词查询 %q 必须返回空结果，实际 %+v", q, res.Hits)
		}
	}
}

func TestRecallNoticesSameKindAndSameTarget(t *testing.T) {
	cands := []*models.AgentMemory{
		memRow("mem-1", "磁盘水位过高 /var 92%", "nljob", "node-a", models.MemoryOutcomeSuccess, 1, 0),
		memRow("mem-2", "磁盘水位过高 /var 92%", "diagnose", "node-b", models.MemoryOutcomeSuccess, 1, 0),
	}
	res := Recall(cands, "磁盘水位过高 /var 92%", RecallOptions{AgentKind: "diagnose", TopK: 5})
	if len(res.Hits) != 2 {
		t.Fatalf("应召回 2 条，实际 %d", len(res.Hits))
	}
	if res.Hits[0].MemoryID != "mem-2" {
		t.Fatalf("同 kind 的 mem-2 应排在前面，实际 %s", res.Hits[0].MemoryID)
	}
	if !res.Hits[0].SameKind || res.Hits[0].SameTarget {
		t.Fatalf("加成标记不对：%+v", res.Hits[0])
	}
	if res.Hits[1].SameTarget {
		t.Fatalf("mem-1 的 target 不同，不该标 SameTarget")
	}
}

func TestRecallSuccessRateThreeStates(t *testing.T) {
	cands := []*models.AgentMemory{
		memRow("mem-none", "磁盘水位过高", "diagnose", "", models.MemoryOutcomeBlocked, 0, 0),
		memRow("mem-ok", "磁盘水位过高", "diagnose", "", models.MemoryOutcomeSuccess, 2, 1),
	}
	res := Recall(cands, "磁盘水位过高", RecallOptions{TopK: 5})
	byID := map[string]Hit{}
	for _, h := range res.Hits {
		byID[h.MemoryID] = h
	}
	none := byID["mem-none"]
	if none.SuccessRateKnown {
		t.Fatalf("无执行样本时 SuccessRateKnown 必须为 false（不能显示成 0%% 成功率）")
	}
	ok := byID["mem-ok"]
	if !ok.SuccessRateKnown || ok.SuccessRate < 0.66 || ok.SuccessRate > 0.67 {
		t.Fatalf("成功率应为 2/3，实际 %v（known=%v）", ok.SuccessRate, ok.SuccessRateKnown)
	}
}

func TestRecallStableOrderingTies(t *testing.T) {
	// 同分、同出现次数、同 last_seen：必须按 memory_id 升序稳定输出，
	// 否则同一次查询在两次调用间可能换一批经验，无法复盘。
	cands := []*models.AgentMemory{
		memRow("mem-b", "磁盘水位过高", "diagnose", "", models.MemoryOutcomeSuccess, 1, 0),
		memRow("mem-a", "磁盘水位过高", "diagnose", "", models.MemoryOutcomeSuccess, 1, 0),
	}
	first := Recall(cands, "磁盘水位过高", RecallOptions{TopK: 5})
	second := Recall([]*models.AgentMemory{cands[1], cands[0]}, "磁盘水位过高", RecallOptions{TopK: 5})
	if first.Hits[0].MemoryID != second.Hits[0].MemoryID || first.Hits[0].MemoryID != "mem-a" {
		t.Fatalf("同分排序必须稳定：%s vs %s", first.Hits[0].MemoryID, second.Hits[0].MemoryID)
	}
}

func TestRenderHintsDeclaresAdvisoryOnly(t *testing.T) {
	cands := []*models.AgentMemory{
		memRow("mem-1", "备份任务失败：磁盘水位过高", "diagnose", "node-it-01", models.MemoryOutcomeSuccess, 2, 0),
	}
	res := Recall(cands, "备份任务失败 磁盘水位过高", RecallOptions{AgentKind: "diagnose", TargetNode: "node-it-01"})
	text := RenderHints(res)
	if text == "" {
		t.Fatal("有命中时必须渲染出内容")
	}
	for _, must := range []string{"历史相似经验", "不是证据", "未观测", "mem-1", "历史成功率"} {
		if !strings.Contains(text, must) {
			t.Errorf("注入块缺少必要声明 %q：\n%s", must, text)
		}
	}
	// 没有命中时不得渲染出一个空标题（会让模型以为"有历史但没内容"）。
	empty := Recall(nil, "随便什么", RecallOptions{})
	if RenderHints(empty) != "" {
		t.Fatalf("无命中时必须返回空字符串，实际：%q", RenderHints(empty))
	}
}

func TestParseTokensFallsBackToShapeWeights(t *testing.T) {
	toks := ParseTokens("磁盘 node-it-01 磁")
	if toks["node-it-01"] != WeightASCIIWord {
		t.Fatalf("西文词元权重还原错误：%v", toks)
	}
	if toks["磁盘"] != WeightCJKBigram {
		t.Fatalf("二元组权重还原错误：%v", toks)
	}
	if toks["磁"] != WeightCJKSingle {
		t.Fatalf("单字权重还原错误：%v", toks)
	}
}
