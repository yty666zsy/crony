package agentmemory

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tmnhs/crony/common/models"
)

// Hit 一条召回命中。
//
// 每个字段都有存在理由：**召回结果的每一部分都要能被追问**。
// 只给一个分数，等于要求使用者相信一个黑盒；而这个场景里被影响的是
// "模型接下来怎么判断"，所以必须把"凭什么像"和"历史上它管用吗"一起给出来。
type Hit struct {
	MemoryID  string `json:"memory_id"`
	SessionID string `json:"session_id"`

	Symptom    string `json:"symptom"`
	ActionKind string `json:"action_kind"`
	ActionText string `json:"action_text"`
	Tool       string `json:"tool"`
	TargetNode string `json:"target_node"`
	Risk       string `json:"risk"`

	Outcome    string `json:"outcome"`
	ResultText string `json:"result_text"`

	// Score 相似度（0~1，含加成），Matched 是命中的查询词元（按权重降序）。
	Score   float64  `json:"score"`
	Matched []string `json:"matched"`
	// Reasons 打分明细：基础覆盖率来自哪些词、有没有加/减成。
	// 这是"为什么这条被召回"的**唯一**依据，不允许只有分数。
	Reasons []string `json:"reasons"`

	// SuccessRate 只在有可执行样本（成功+失败 > 0）时有效；
	// SuccessRateKnown 为 false 表示"没有执行样本"，此时 SuccessRate 恒为 0。
	//
	// 这个三态（0% / 100% / 无数据）必须分开：把无数据当成 0% 成功率
	// 会让人误以为"这个动作从来不好使"。
	SuccessRate      float64 `json:"success_rate"`
	SuccessRateKnown bool    `json:"success_rate_known"`
	Occurrences      int     `json:"occurrence_count"`
	LastSeen         int64   `json:"last_seen"`

	// 结果构成（只读展示用）。
	//
	// 为什么要带这三个计数：`Outcome` 只是**最近一次**的结果，而一条经验往往被
	// 观测过多次、每次结果都可能不同（例如同一个动作有时被拦下、有时被拒）。
	// 只把"最近一次"写进提示词，模型会把局部当成全部——"被拦下过"读成"这个动作
	// 一直跑不通"。带上构成，读的人（模型/人）才能看到全貌。
	SuccessCount int `json:"success_count"`
	FailCount    int `json:"fail_count"`
	BlockedCount int `json:"blocked_count"`
	DeniedCount  int `json:"denied_count"`
	AdvisedCount int `json:"advised_count"`

	// SameKind 命中的经验与会话类型相同（打分有小幅加成）。
	SameKind bool `json:"same_kind"`
	// SameTarget 命中的经验作用在同一台节点上。
	SameTarget bool `json:"same_target"`
}

// RecallOptions 召回参数。
type RecallOptions struct {
	// TopK 返回条数上限（默认 3）。**默认为什么是 3**：注入提示词的上下文很贵，
	// 而且历史经验越多，模型越容易拿"上次是这么修的"替代"这次去观测"——
	// 因此宁可少给，由人来决定要不要看更多（接口支持 top_k 调大）。
	TopK int
	// MinScore 最低相似度（默认 0.34）。低于它就返回空列表——
	// "硬凑 3 条不相关的经验"比"什么都不给"更有害：模型会认真对待它。
	MinScore float64
	// AgentKind 当前会话类型，用于同类型加成与展示。
	AgentKind string
	// TargetNode 当前目标节点，命中同节点的经验给加成。
	TargetNode string
}

// DefaultTopK / DefaultMinScore 默认召回参数。
const (
	DefaultTopK     = 3
	DefaultMinScore = 0.34
	// KindBonus 同会话类型的加成（同一类场景的经验更可能适用）。
	KindBonus = 0.05
	// TargetBonus 同目标节点的加成。
	TargetBonus = 0.05
)

func (o RecallOptions) withDefaults() RecallOptions {
	if o.TopK <= 0 {
		o.TopK = DefaultTopK
	}
	if o.TopK > 20 {
		o.TopK = 20
	}
	if o.MinScore <= 0 {
		o.MinScore = DefaultMinScore
	}
	return o
}

// RecallResult 一次召回的完整结果（含"看了多少条候选"这类可审计信息）。
type RecallResult struct {
	Query string `json:"query"`
	// QueryTokens 查询被切出的词元（可解释性：先把"我理解你说的是什么"摊开）。
	QueryTokens []string `json:"query_tokens"`
	Considered  int      `json:"considered"`
	Hits        []Hit    `json:"hits"`
	TopK        int      `json:"top_k"`
	MinScore    float64  `json:"min_score"`
	// AdvisoryOnly 恒为 true：记忆**只能当线索**，不能作为本次结论的证据。
	//
	// 为什么把它写进返回值而不是只写在文档里：接口的任何消费方（控制台、模型调用编排）
	// 都应该能直接看到这个约束，而不是靠读注释才知道"这东西不能当证据"。
	AdvisoryOnly bool `json:"advisory_only"`
	// UsableAsEvidence 恒为 false，明确回答"能不能拿它当证据"。
	UsableAsEvidence bool `json:"usable_as_evidence"`
}

// candidateTokens 取候选文本的词元。
//
// 优先**从症状原文现算**，而不是用落库的 symptom_tokens：
// 落库的是"权重降序 + 字典序"的字符串，权重已经丢了，用它打分只能靠形态反推权重，
// 那会让"分数为什么是这个数"变得说不清。落库的 token 串保留下来是为了**人工复盘**，
// 只有在症状原文缺失（历史数据）时才回退用它。
func candidateTokens(m *models.AgentMemory) map[string]float64 {
	if strings.TrimSpace(m.Symptom) != "" {
		return Tokenize(m.Symptom)
	}
	return ParseTokens(m.SymptomTokens)
}

// Recall 从候选集里挑出与 query 最相似的经验（纯函数，无副作用）。
//
// 打分口径（全部可解释）：
//
//	基础分 = 被命中的查询词元权重之和 / 查询词元总权重      （0~1 的非对称覆盖率）
//	加成   = 同 agent_kind +0.05；同 target_node +0.05
//	基础分 = min(1, 被命中的查询词元权重之和 / 查询词元总权重)   （0~1 的非对称覆盖率）
//	加成   = 同 agent_kind +0.05；同 target_node +0.05
//	最终分 = 基础分 + 加成（最高 1.10）
//
// 为什么最终分允许**略大于 1**：最初把总分截断到 1.0，结果"同类型的经验"
// 在基础分已经满分时无法再体现优先级（两条都被压成 1.0，排序退化成按 id）——
// 单测 TestRecallNoticesSameKindAndSameTarget 直接抓出了这一点。
// 现在只截断基础分：分数是**排序信号**，不是概率，包装成"0~1 的置信度"才是误导。
//
// 为什么用**非对称覆盖率**而不是 Jaccard：查询短、经验长。
// Jaccard 会让"经验里多写了两句结果描述"直接压低了相似度，
// 而真正该问的问题是"查询里说的这些特征，这条经验里有没有"。
func Recall(candidates []*models.AgentMemory, query string, opts RecallOptions) RecallResult {
	opts = opts.withDefaults()
	res := RecallResult{
		Query:            query,
		QueryTokens:      []string{},
		Hits:             []Hit{},
		TopK:             opts.TopK,
		MinScore:         opts.MinScore,
		AdvisoryOnly:     true,
		UsableAsEvidence: false,
	}
	qTokens := Tokenize(query)
	res.QueryTokens = TopTokens(qTokens, 40)
	total := 0.0
	for _, w := range qTokens {
		total += w
	}
	res.Considered = len(candidates)
	if total == 0 || len(candidates) == 0 {
		// 查询没有可用词元（比如只有一个"的"）：返回空，而不是"随便给几条"。
		// 空结果在这里是**正确的答案**，调用方不需要特殊处理。
		return res
	}

	hits := make([]Hit, 0, len(candidates))
	for _, m := range candidates {
		if m == nil {
			continue
		}
		cTokens := candidateTokens(m)
		matchedWeight := 0.0
		matched := make([]string, 0, 8)
		for tok, w := range qTokens {
			if _, ok := cTokens[tok]; ok {
				matchedWeight += w
				matched = append(matched, tok)
			}
		}
		if len(matched) == 0 {
			continue
		}
		sortTokens(matched, qTokens)

		base := matchedWeight / total
		if base > 1 {
			base = 1
		}
		bonus := 0.0
		sameKind := opts.AgentKind != "" && m.AgentKind == opts.AgentKind
		if sameKind {
			bonus += KindBonus
		}
		sameTarget := opts.TargetNode != "" && m.TargetNode == opts.TargetNode
		if sameTarget {
			bonus += TargetBonus
		}
		score := base + bonus
		if score < opts.MinScore {
			continue
		}

		reasons := []string{fmt.Sprintf("命中查询词元 %d 个（覆盖率 %.2f）", len(matched), base)}
		if len(matched) > 0 {
			top := matched
			if len(top) > 8 {
				top = top[:8]
			}
			reasons = append(reasons, "命中词："+strings.Join(top, "、"))
		}
		if sameKind {
			reasons = append(reasons, fmt.Sprintf("同一会话类型（%s）+%.2f", m.AgentKind, KindBonus))
		}
		if sameTarget {
			reasons = append(reasons, fmt.Sprintf("同一目标节点（%s）+%.2f", m.TargetNode, TargetBonus))
		}
		rate, known := m.MemorySuccessRate()
		if !known {
			reasons = append(reasons, "无执行样本：不要把它当成成功率 0%")
		}

		hits = append(hits, Hit{
			MemoryID:         m.MemoryID,
			SessionID:        m.SessionID,
			Symptom:          m.Symptom,
			ActionKind:       m.ActionKind,
			ActionText:       m.ActionText,
			Tool:             m.Tool,
			TargetNode:       m.TargetNode,
			Risk:             m.Risk,
			Outcome:          m.Outcome,
			ResultText:       m.ResultText,
			Score:            score,
			Matched:          matched,
			Reasons:          reasons,
			SuccessRate:      rate,
			SuccessRateKnown: known,
			Occurrences:      m.OccurrenceCount,
			LastSeen:         m.LastSeen,
			SameKind:         sameKind,
			SameTarget:       sameTarget,
			SuccessCount:     m.SuccessCount,
			FailCount:        m.FailCount,
			BlockedCount:     m.BlockedCount,
			DeniedCount:      m.DeniedCount,
			AdvisedCount:     m.AdvisedCount,
		})
	}

	// 排序：分数降序 → 出现次数降序 → 最近出现时间降序 → memory_id 升序。
	// 多级排序的意义是**结果稳定**：同分时不能因为数据库返回顺序变了就换一批经验。
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Occurrences != hits[j].Occurrences {
			return hits[i].Occurrences > hits[j].Occurrences
		}
		if hits[i].LastSeen != hits[j].LastSeen {
			return hits[i].LastSeen > hits[j].LastSeen
		}
		return hits[i].MemoryID < hits[j].MemoryID
	})
	if len(hits) > opts.TopK {
		hits = hits[:opts.TopK]
	}
	res.Hits = hits
	return res
}

// RenderHintLines 把召回结果渲染成**逐条一行**的提示行（供诊断提示词注入）。
//
// 为什么单独给"行"而不是复用 RenderHints 的整块：提示词里的每一行都要付费（token），
// 而且模型对"一行一条"的清单比多行嵌套更不容易读串行。整块版本留给控制台展示
// （人看得越清楚越好，机器输入越省越好——两者的最优格式不同）。
//
// 返回的每一行都已经带上"条目号 + 相似度 + 命中理由 + 结果与样本量"，
// 这样模型即使只看单行，也能判断"这条经验值不值得参考"。
func RenderHintLines(res RecallResult) []string {
	if len(res.Hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		// 没有条目号的命中**不渲染**：提示行里的 `[mem-x]` 是这副线索唯一可回溯的把手，
		// 没有它人无法去控制台核对"这到底是哪条经验"（也解释了为什么会写出 `[] 相似度 0.00`）。
		if strings.TrimSpace(h.MemoryID) == "" {
			continue
		}
		action := strings.TrimSpace(h.ActionText)
		if action == "" {
			action = h.ActionKind
		}
		line := fmt.Sprintf("[%s] 相似度 %.2f（%s）｜症状：%s｜当时动作：%s%s｜结果：%s",
			h.MemoryID, h.Score, strings.Join(h.Reasons, "；"),
			clipRunes(h.Symptom, 120), clipRunes(action, 120),
			targetText(h.TargetNode), outcomeText(h))
		out = append(out, line)
	}
	return out
}

// targetText 目标节点的展示前缀（为空时不显示，避免出现"目标节点：—"这种噪声）。
func targetText(node string) string {
	if strings.TrimSpace(node) == "" {
		return ""
	}
	return "（节点 " + node + "）"
}

// RenderHints 把召回结果渲染成注入提示词的材料块。
//
// 这一段文本是**模型输入**，因此三条硬约束必须写在文本里（而不是只在代码注释里）：
//  1. 历史经验只是线索，不是本次事实；
//  2. 引用它不会构成证据（证据必须来自本次工具观测）；
//  3. 经验可能过时/不适用，写明了"历史成功率"与"样本量"，让模型自己掂量。
//
// 返回空字符串表示没有可注入的内容——此时调用方不应往上下文里塞一个空标题，
// 那会让模型误以为"有历史但没内容"。
func RenderHints(res RecallResult) string {
	if len(res.Hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 历史相似经验（线索，不是证据）\n")
	b.WriteString("以下条目由系统从历史会话自动沉淀，**可能与当前情况不同、可能已经过时**。\n")
	b.WriteString("它们只能用来提示排查方向；**引用它们不算本次证据**（证据必须来自本次工具观测，")
	b.WriteString("引用历史条目会被标记为「未观测」并触发人工复核）。\n")
	b.WriteString(fmt.Sprintf("（相似度阈值 %.2f，本次共比较 %d 条启用经验，返回 %d 条）\n",
		res.MinScore, res.Considered, len(res.Hits)))
	for i, h := range res.Hits {
		b.WriteString(fmt.Sprintf("\n%d. [%s] 相似度 %.2f（%s）\n", i+1, h.MemoryID, h.Score, strings.Join(h.Reasons, "；")))
		b.WriteString("   症状：" + clipRunes(h.Symptom, 160) + "\n")
		action := h.ActionText
		if strings.TrimSpace(action) == "" {
			action = h.ActionKind
		}
		b.WriteString("   当时动作：" + clipRunes(action, 160) + "\n")
		if h.TargetNode != "" {
			b.WriteString("   目标节点：" + h.TargetNode + "\n")
		}
		b.WriteString("   结果：" + outcomeText(h) + "\n")
		if strings.TrimSpace(h.ResultText) != "" {
			b.WriteString("   结果摘要：" + clipRunes(h.ResultText, 200) + "\n")
		}
	}
	return b.String()
}

// outcomeText 把结果写成一句人话（含成功率三态）。
//
// 措辞纪律：`Outcome` 是**最近一次**的结果，所以标签前必须加"最近一次"；
// 出现多次时补一句构成，避免"最近一次被拦下"被读成"这个动作一直跑不通"。
func outcomeText(h Hit) string {
	label := map[string]string{
		models.MemoryOutcomeSuccess: "成功",
		models.MemoryOutcomeFailed:  "执行了但失败",
		models.MemoryOutcomeBlocked: "未执行（被拦下/转人工）",
		models.MemoryOutcomeDenied:  "被闸门拒绝",
		models.MemoryOutcomeAdvised: "只给了建议，没有动手",
	}[h.Outcome]
	if label == "" {
		label = h.Outcome
	}
	rate := "无执行样本"
	if h.SuccessRateKnown {
		rate = fmt.Sprintf("历史成功率 %.0f%%", h.SuccessRate*100)
	}
	if h.Occurrences > 1 {
		return fmt.Sprintf("最近一次：%s；累计 %d 次（成功 %d / 失败 %d / 未执行 %d / 被拒 %d / 只建议 %d），%s",
			label, h.Occurrences, h.SuccessCount, h.FailCount, h.BlockedCount, h.DeniedCount, h.AdvisedCount, rate)
	}
	return fmt.Sprintf("最近一次：%s；%s", label, rate)
}

// clipRunes 按字符截断（不能按字节，否则中文会被切成半个字）。
func clipRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
