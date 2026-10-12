// Package eval 实现阶段 5 的「轨迹即评测资产」打分内核（方案 §8-19 Trace → Eval）。
//
// 思路：把一次真实运行/诊断的**轨迹**变成一条可回归的评测样本——
// 记录它的工具调用序列、步数、成本、延迟，再用**确定性断言**去卡它。
//
// 一条刻意的边界：本包**不做 LLM 判官**。方案 §8-19 要求"LLM 判官必须交换选项位置 +
// 多模型陪审投票，禁止单模型单次打分"。在没有落实那套约束之前，任何"让模型给个 0~1 分"
// 的实现都会把评测变成噪声。因此这里只做**机械可复核**的断言：
// 工具序列、步数上限、成本上限、延迟上限。语义正确性判断留给人工标注（阶段 5 的种子集）。
package eval

// Step 轨迹里的一步（从 agent_step / 工具审计行归一而来）。
type Step struct {
	Kind string `json:"kind"` // think / tool_call / observation / final
	Tool string `json:"tool"`
	// ArgsHash 工具入参的哈希（用于 tool-args-match 断言）。
	ArgsHash string `json:"args_hash,omitempty"`
	// Cost / LatencyMS 该步的成本与耗时（token 成本已在网关记账）。
	Cost      float64 `json:"cost,omitempty"`
	LatencyMS int64   `json:"latency_ms,omitempty"`
}

// Actual 一次运行的实际轨迹汇总。
type Actual struct {
	Steps []Step
	// Cost 总成本；<=0 时用量不参与断言。
	Cost float64
	// LatencyMS 端到端耗时。
	LatencyMS int64
}

// Case 一条评测样本（期望）。
type Case struct {
	ID string `json:"id"`
	// ExpectedToolSequence 期望的工具调用序列；空表示不断言序列。
	ExpectedToolSequence []string `json:"expected_tool_sequence,omitempty"`
	// MaxSteps 轨迹步数上限；<=0 不断言。
	MaxSteps int `json:"max_steps,omitempty"`
	// MaxCost / MaxLatencyMS 成本与延迟上限；<=0 不断言。
	MaxCost      float64 `json:"max_cost,omitempty"`
	MaxLatencyMS int64   `json:"max_latency_ms,omitempty"`
}

// Check 单项断言结果。
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// Result 一条样本的打分结果。
type Result struct {
	CaseID string  `json:"case_id"`
	Passed bool    `json:"passed"`
	Checks []Check `json:"checks"`
	// Score 通过率（0~1），供看板汇总。
	Score float64 `json:"score"`
}

// ToolSequence 从轨迹里抽出工具调用序列（只取 kind=tool_call 的步骤）。
func ToolSequence(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.Kind == "tool_call" && s.Tool != "" {
			out = append(out, s.Tool)
		}
	}
	return out
}

// Score 对一条样本打分：跑所有适用断言，全过才算通过。
func Score(c Case, a Actual) Result {
	res := Result{CaseID: c.ID}
	// 1) 工具序列（逐项相等）。
	if len(c.ExpectedToolSequence) > 0 {
		got := ToolSequence(a.Steps)
		pass := equalStrings(got, c.ExpectedToolSequence)
		res.Checks = append(res.Checks, Check{
			Name:   "tool-sequence",
			Passed: pass,
			Detail: detailSeq(got, c.ExpectedToolSequence, pass),
		})
	} else {
		res.Checks = append(res.Checks, Check{Name: "tool-sequence", Passed: true, Detail: "未声明期望序列：跳过"})
	}
	// 2) 步数上限。
	if c.MaxSteps > 0 {
		n := len(a.Steps)
		res.Checks = append(res.Checks, Check{
			Name:   "step-count",
			Passed: n <= c.MaxSteps,
			Detail: "步数 " + itoa(n) + " / 上限 " + itoa(c.MaxSteps),
		})
	}
	// 3) 成本上限（<=0 视为未计量：跳过而不是判失败）。
	if c.MaxCost > 0 {
		pass := a.Cost <= c.MaxCost
		detail := "成本 " + ftoa(a.Cost) + " / 上限 " + ftoa(c.MaxCost)
		if a.Cost <= 0 {
			pass, detail = true, "未计量成本：跳过（不计入通过率分母的惩罚）"
		}
		res.Checks = append(res.Checks, Check{Name: "cost", Passed: pass, Detail: detail})
	}
	// 4) 延迟上限。
	if c.MaxLatencyMS > 0 {
		pass := a.LatencyMS <= c.MaxLatencyMS
		detail := "延迟 " + itoa64(a.LatencyMS) + "ms / 上限 " + itoa64(c.MaxLatencyMS) + "ms"
		if a.LatencyMS <= 0 {
			pass, detail = true, "未计量延迟：跳过"
		}
		res.Checks = append(res.Checks, Check{Name: "latency", Passed: pass, Detail: detail})
	}
	if len(res.Checks) == 0 {
		res.Checks = append(res.Checks, Check{Name: "no-assertions", Passed: true, Detail: "该样本没有任何断言"})
	}
	passed, total := 0, 0
	for _, ck := range res.Checks {
		total++
		if ck.Passed {
			passed++
		}
	}
	res.Score = float64(passed) / float64(total)
	res.Passed = passed == total
	return res
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func detailSeq(got, want []string, pass bool) string {
	if pass {
		return "序列匹配：" + join(got)
	}
	return "实际 " + join(got) + " ≠ 期望 " + join(want)
}

func join(s []string) string {
	if len(s) == 0 {
		return "[]"
	}
	out := "["
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out + "]"
}

func itoa(v int) string { return itoa64(int64(v)) }

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func ftoa(v float64) string {
	// 保留 2 位小数，避免浮点噪声影响报告可读性。
	neg := v < 0
	if neg {
		v = -v
	}
	whole := int64(v)
	frac := int64((v-float64(whole))*100 + 0.5)
	if frac >= 100 {
		whole++
		frac -= 100
	}
	s := itoa64(whole)
	if frac > 0 {
		s += "." + pad2(frac)
	}
	if neg {
		s = "-" + s
	}
	return s
}

func pad2(v int64) string {
	if v < 10 {
		return "0" + itoa64(v)
	}
	return itoa64(v)
}
