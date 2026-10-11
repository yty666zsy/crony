// Package agentmemory 实现阶段 4 的「Agent 记忆 / 经验库」：
// 症状→动作→结果 的沉淀（Extract）、相似历史召回（Recall）、统计口径（见 models.QueryMemoryStats）。
//
// 为什么不用向量库（本阶段的明确取舍）：
//   - 方案里写的是"MySQL 优先，向量库后置"，而真正需要先回答的问题是
//     "召回出来的东西到底像不像、凭什么是它"——词元重合度是**可解释**的，
//     向量相似度在没有评测集的情况下只能给出一个无法复盘的分数；
//   - 现阶段的语料是几十到几千条运维经验，词元重合 + 去重计数已经够用；
//   - 换成向量库的代价（模型、存储、一致性、成本）与当前收益不成比例。
//
// 这个包**纯内存、无数据库依赖**：召回打分可以在单测里逐条验证，
// 不需要起 MySQL；数据库只负责"把候选集取出来"和"把沉淀写回去"。
package agentmemory

import (
	"strings"
	"unicode"
)

// TokenWeight 词元权重。
//
// 权重设计的唯一目的是"让打出来的分数能解释"：
//   - 英文/数字词（如 error、1406、node-it-01）通常是**最具体**的线索，权重最高；
//   - 中文二元组（如"磁盘"）是运维语料里信息量最实在的单位；
//   - 单个汉字（如"慢"）歧义太大，只给很低权重，避免"两个句子都含'的'就算相似"。
const (
	WeightASCIIWord = 1.5
	WeightCJKBigram = 1.0
	WeightCJKSingle = 0.3
)

// MaxTokens 单个文本最多保留的词元数。
//
// 上限的作用是**防止长文本靠体量刷分**：覆盖率是"查询词元被命中多少"，
// 但候选文本越长越容易命中，所以生成候选词元时也要有边界（与库列宽 2000 字符对齐）。
const MaxTokens = 400

// stopwords 停用词（中文单字与常见虚词，以及本场景里不携带故障信息的通用词）。
//
// 刻意**不**停用"失败/超时/磁盘/端口"这类领域词：它们是本场景的核心信号。
// 若哪天要扩充这张表，判断标准只有一个：去掉它之后，
// "两条本该相似的运维记录"是否还相似、两条无关的是否被拉近。
var stopwords = map[string]bool{
	"的": true, "了": true, "是": true, "在": true, "和": true, "与": true, "或": true,
	"不": true, "我": true, "你": true, "他": true, "这": true, "那": true, "有": true,
	"会": true, "要": true, "请": true, "帮": true, "把": true, "被": true, "为": true,
	"就": true, "也": true, "都": true, "还": true, "个": true, "次": true, "上": true,
	"下": true, "中": true, "以": true, "及": true, "而": true, "但": true, "很": true,
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"from": true, "into": true, "was": true, "are": true, "not": true, "you": true,
	// 会话模板里的通用词：每次诊断的 goal 都带着它们，留着会让所有症状互相"相似"。
	"诊断": true, "任务": true, "本次": true, "原因": true, "原因。": true, "什么": true,
}

// isCJK 判断是否为中日韩统一表意文字。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r)
}

// isWordRune 判断是否为"西文词"字符（字母、数字、下划线、连字符）。
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) && !isCJK(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == ':'
}

// Tokenize 把一段文本切成带权重的词元（重复词元只保留一次，权重取最大）。
//
// 规则（逐条可复现，便于复盘"为什么这两条相似"）：
//  1. 转小写；标点与空白当分隔符；
//  2. 连续的西文字符/数字/_-.: 视为一个词（如 node-it-01、err_1406），权重 1.5；
//  3. 连续汉字按**二元组**切（"磁盘水位" → 磁盘/盘水/水位），权重 1.0；
//  4. 只有 1 个汉字的片段保留单字，权重 0.3；
//  5. 停用词丢弃。
func Tokenize(text string) map[string]float64 {
	out := map[string]float64{}
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return out
	}
	add := func(tok string, weight float64) {
		if tok == "" || stopwords[tok] || len([]rune(tok)) == 0 {
			return
		}
		if w, ok := out[tok]; !ok || weight > w {
			out[tok] = weight
		}
	}

	i := 0
	runes := []rune(text)
	for i < len(runes) {
		r := runes[i]
		switch {
		case isCJK(r):
			j := i
			for j < len(runes) && isCJK(runes[j]) {
				j++
			}
			run := runes[i:j]
			if len(run) == 1 {
				add(string(run), WeightCJKSingle)
			}
			for k := 0; k+1 < len(run); k++ {
				add(string(run[k:k+2]), WeightCJKBigram)
			}
			i = j
		case isWordRune(r):
			j := i
			for j < len(runes) && isWordRune(runes[j]) {
				j++
			}
			add(string(runes[i:j]), WeightASCIIWord)
			i = j
		default:
			i++
		}
	}
	if len(out) > MaxTokens {
		// 超过上限时保留权重最高的词元，保证结果稳定（同分按字典序，不依赖 map 顺序）。
		trimmed := map[string]float64{}
		for _, tok := range TopTokens(out, MaxTokens) {
			trimmed[tok] = out[tok]
		}
		out = trimmed
	}
	return out
}

// TokenizeString 把词元表拼成可落库的字符串（空格分隔、按权重降序、同权重按字典序）。
//
// 排序是**平滑版**的 TopTokens：没有排序时 map 迭代顺序随机，
// 同样的文本会写出不同的 token 字符串，"复盘相似度"就失去了可比性。
func TokenizeString(text string) string {
	return strings.Join(TopTokens(Tokenize(text), MaxTokens), " ")
}

// TopTokens 返回按（权重降序、词元字典序升序）排序后的词元列表，最多 n 个。
func TopTokens(tokens map[string]float64, n int) []string {
	list := make([]string, 0, len(tokens))
	for tok := range tokens {
		list = append(list, tok)
	}
	sortTokens(list, tokens)
	if n > 0 && len(list) > n {
		list = list[:n]
	}
	return list
}

// sortTokens 就地排序（权重降序、字典序升序）。
func sortTokens(list []string, tokens map[string]float64) {
	// 简单的插入排序：词元数量有上限（MaxTokens），不需要引入 sort 包的比较器开销，
	// 也避免对同权重元素的稳定性产生依赖。
	for i := 1; i < len(list); i++ {
		cur := list[i]
		j := i - 1
		for j >= 0 && less(cur, list[j], tokens) {
			list[j+1] = list[j]
			j--
		}
		list[j+1] = cur
	}
}

// less 判断 a 是否应排在 b 前面。
func less(a, b string, tokens map[string]float64) bool {
	if wa, wb := tokens[a], tokens[b]; wa != wb {
		return wa > wb
	}
	return a < b
}

// ParseTokens 解析落库的 token 字符串（用于直接对库中的 symptom_tokens 打分）。
func ParseTokens(s string) map[string]float64 {
	out := map[string]float64{}
	for _, tok := range strings.Fields(s) {
		// 落库时丢掉了权重：按词元形态还原（西文 1.5、汉字二元组 1.0、单字 0.3）。
		weight := WeightCJKBigram
		runes := []rune(tok)
		switch {
		case len(runes) == 1 && isCJK(runes[0]):
			weight = WeightCJKSingle
		case !isCJK(runes[0]):
			weight = WeightASCIIWord
		}
		out[tok] = weight
	}
	return out
}
