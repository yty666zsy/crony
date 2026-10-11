package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 场景三：Agent 型定时任务（阶段 2 的 JobTypeAgent）。
//
// 与失败诊断的区别：诊断是"事后归因"（有明确的失败事实要解释），
// 巡检是"定期体检"（没事也要给结论，且必须能说"没发现异常"）。
// 因此这里刻意要求模型：
//  1. 允许 findings 为空数组（不能为了凑内容编问题）；
//  2. 每条发现都要带证据引用，没证据的只能进 "无法确认" 清单；
//  3. 与诊断一致的硬约束：只读、只建议、不执行。
const (
	// PromptVersionInspectV1 巡检提示词版本。
	PromptVersionInspectV1 = "inspect-v1"
)

// PromptInspectV1 巡检系统提示词。
const PromptInspectV1 = `你是 crony 的只读运维巡检助手。你会收到一个巡检目标与若干材料（任务定义、最近运行记录、服务状态）。

## 硬规则
1. 你**只能读**：所有工具都是只读的。任何"执行/重启/清理/改配置"的动作都不能做，只能写进建议。
2. 每条 finding 必须带 evidence：ref 必须逐字来自工具返回的定位（如 "node.log:12"）。
   没有证据的判断不要写进 findings，改写到 next_data_to_collect 里说明需要补什么数据。
3. **没发现问题就写空 findings 数组**，并在 summary 里明确说明"未发现异常、依据是什么"。
   不要为了显得有价值而编造或夸大问题。
4. 不确定就承认：evidence_sufficient=false，并在 next_data_to_collect 说明要采集什么。
5. 只输出一个 JSON 对象，不要 Markdown 围栏、不要额外解释。

## 输出 JSON 结构
{
  "summary": "一句话结论（必须能独立读懂）",
  "findings": [
    {
      "title": "发现标题",
      "detail": "具体说明（现象、范围、影响）",
      "risk": "low|medium|high",
      "evidence": [{"ref": "证据定位", "quote": "证据原文片段", "why": "这条证据说明了什么"}]
    }
  ],
  "suggested_actions": [
    {"title": "建议动作", "detail": "怎么做", "risk": "low|medium|high", "rollback": "如何回退；无需回退则写 无"}
  ],
  "evidence_sufficient": true,
  "next_data_to_collect": [],
  "assumptions": []
}`

// InspectionFinding 一条巡检发现。
type InspectionFinding struct {
	Title    string        `json:"title"`
	Detail   string        `json:"detail"`
	Risk     string        `json:"risk"`
	Evidence []EvidenceRef `json:"evidence"`
}

// Inspection 巡检结论（只读、只建议）。
type Inspection struct {
	Summary            string              `json:"summary"`
	Findings           []InspectionFinding `json:"findings"`
	SuggestedActions   []SuggestedAction   `json:"suggested_actions"`
	Assumptions        []string            `json:"assumptions"`
	EvidenceSufficient bool                `json:"evidence_sufficient"`
	NextDataToCollect  []string            `json:"next_data_to_collect"`
	UnverifiedRefs     []string            `json:"unverified_refs"`
	Warnings           []string            `json:"warnings"`
	// AdvisoryOnly 恒为 true：阶段 2 的 Agent 不具备执行权。
	AdvisoryOnly  bool   `json:"advisory_only"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	PromptVersion string `json:"prompt_version"`
	StopReason    string `json:"stop_reason"`
	DurationMS    int64  `json:"duration_ms"`
	ToolCalls     int    `json:"tool_calls"`
	TotalTokens   int    `json:"total_tokens"`
	Trace         *Trace `json:"-"`
}

// InspectionSchema 巡检输出 Schema（全部字段必填，逼模型把话说完整）。
func InspectionSchema() *jsonschema.Schema {
	evidence := jsonschema.Array(jsonschema.Object(map[string]*jsonschema.Schema{
		"ref":   jsonschema.String("证据定位（必须逐字来自工具返回）").WithLength(1, 200),
		"quote": jsonschema.String("证据原文片段").WithLength(1, 400),
		"why":   jsonschema.String("这条证据说明了什么").WithLength(1, 400),
	}, "ref", "quote", "why"), "证据列表")

	finding := jsonschema.Object(map[string]*jsonschema.Schema{
		"title":    jsonschema.String("发现标题").WithLength(1, 200),
		"detail":   jsonschema.String("具体说明").WithLength(1, 1200),
		"risk":     jsonschema.String("风险等级").WithEnum("low", "medium", "high"),
		"evidence": evidence,
	}, "title", "detail", "risk", "evidence")

	action := jsonschema.Object(map[string]*jsonschema.Schema{
		"title":    jsonschema.String("建议动作").WithLength(1, 200),
		"detail":   jsonschema.String("怎么做").WithLength(1, 1200),
		"risk":     jsonschema.String("风险等级").WithEnum("low", "medium", "high"),
		"rollback": jsonschema.String("回退方式；无需回退写 无").WithLength(1, 400),
	}, "title", "detail", "risk", "rollback")

	return jsonschema.Object(map[string]*jsonschema.Schema{
		"summary":              jsonschema.String("一句话结论").WithLength(1, 1200),
		"findings":             jsonschema.Array(finding, "发现列表（允许为空数组）"),
		"suggested_actions":    jsonschema.Array(action, "建议动作（可为空数组）"),
		"assumptions":          jsonschema.Array(jsonschema.String("无法验证的假设").WithLength(1, 400), "假设清单"),
		"evidence_sufficient":  jsonschema.Bool("证据是否充分"),
		"next_data_to_collect": jsonschema.Array(jsonschema.String("还需要采集什么数据").WithLength(1, 400), "待采集清单"),
	}, "summary", "findings", "suggested_actions", "assumptions", "evidence_sufficient", "next_data_to_collect")
}

// InspectRequest 一次巡检的输入。
type InspectRequest struct {
	// SessionID 会话 ID（为空则自动生成）。
	SessionID string
	// Goal 巡检目标（自然语言），来自 Agent 型任务的 command 字段。
	Goal string
	// Context 材料（每行一条）。
	Context []string
	// Now 材料里的"当前时间"，便于测试确定性。
	Now time.Time
}

// Inspect 执行一次巡检并返回结论。
//
// 失败语义与 Diagnose 一致：能拿到结论就返回结论（附带 warnings），
// 拿不到结论时返回 error，但**仍然返回带 Trace 的 Inspection**，
// 这样"为什么没结论"永远可查。
func Inspect(ctx context.Context, a *Agent, req InspectRequest) (*Inspection, error) {
	out := &Inspection{AdvisoryOnly: true}

	goal := strings.TrimSpace(req.Goal)
	if goal == "" {
		return out, fmt.Errorf("%w: 巡检目标为空", ErrSchemaInvalid)
	}
	if a == nil {
		return out, fmt.Errorf("%w: Agent 未初始化", ErrNoProvider)
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = sessionIDFor("inspect", 0, "")
	}

	userCtx := make([]string, 0, len(req.Context)+2)
	userCtx = append(userCtx, req.Context...)
	if len(userCtx) == 0 {
		userCtx = append(userCtx, "（本次没有提供额外材料，请仅依据工具观测下结论；无观测时如实说明）")
	}
	userCtx = append(userCtx, "巡检要求："+goal)

	start := time.Now()
	result, err := a.Run(ctx, Input{
		SessionID:     sessionID,
		Goal:          goal,
		System:        PromptInspectV1,
		Context:       userCtx,
		PromptVersion: PromptVersionInspectV1,
	}, InspectionSchema())

	out.StopReason = string(result.StopReason)
	out.DurationMS = time.Since(start).Milliseconds()
	out.PromptVersion = PromptVersionInspectV1
	if result.Trace != nil {
		out.Trace = result.Trace
		out.Provider = providerOf(result.Trace)
		out.Model = modelOf(result.Trace)
		out.ToolCalls = result.Trace.ToolCalls
		out.TotalTokens = result.Trace.Usage.TotalTokens
	}
	if len(result.Warnings) > 0 {
		out.Warnings = append(out.Warnings, result.Warnings...)
	}

	// 没有结构化结论时，说明原因而不是给一个空报告。
	if err != nil || result.Structured == nil {
		reason := "模型未返回结构化结论"
		if err != nil {
			reason = err.Error()
		}
		out.Summary = "本次巡检未能给出结论（" + reason + "），请查看轨迹与告警后重试"
		out.Warnings = append(out.Warnings, fmt.Sprintf("巡检未完成：%s", reason))
		if err == nil {
			err = fmt.Errorf("%w: %s", ErrSchemaInvalid, reason)
		}
		return out, err
	}

	obj := result.Structured
	out.Summary = llm.StringOr(obj, "summary", "")
	out.Assumptions = stringList(obj, "assumptions")
	out.NextDataToCollect = stringList(obj, "next_data_to_collect")
	out.EvidenceSufficient = boolOr(obj["evidence_sufficient"], false)
	out.Findings = parseFindings(obj["findings"])
	out.SuggestedActions = parseActions(obj["suggested_actions"])

	// grounding 校验：巡检里"编造发现"比诊断里更危险（会引发无谓的变更）。
	if result.Trace != nil {
		observed := result.Trace.EvidenceRefs()
		seen := map[string]bool{}
		for _, f := range out.Findings {
			for _, ev := range f.Evidence {
				ref := strings.TrimSpace(ev.Ref)
				if ref == "" || observed[ref] || refObserved(observed, ref) {
					continue
				}
				if !seen[ref] {
					seen[ref] = true
					out.UnverifiedRefs = append(out.UnverifiedRefs, ref)
				}
			}
		}
	}
	if len(out.UnverifiedRefs) > 0 {
		out.EvidenceSufficient = false
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"有 %d 处证据定位本次并未观测到（可能来自模型想象或历史记忆）：%s。这些发现不可直接采信。",
			len(out.UnverifiedRefs), strings.Join(dedupe(out.UnverifiedRefs), "、")))
	}
	if len(out.Findings) > 0 && !hasAnyEvidence(out.Findings) {
		out.EvidenceSufficient = false
		out.Warnings = append(out.Warnings, "所有发现都没有附带证据，已降级为不可采信。")
	}
	if !out.EvidenceSufficient && len(out.NextDataToCollect) == 0 {
		out.Warnings = append(out.Warnings, "报告承认证据不足，但未给出还需要采集什么数据，人工无法推进。")
	}
	if a.Registry() == nil {
		out.Warnings = append(out.Warnings, "本次巡检没有可用工具，结论仅基于任务定义与历史记录，可信度较低。")
	}

	if strings.TrimSpace(out.Summary) == "" {
		out.Summary = "未给出结论"
	}
	return out, nil
}

// parseFindings 解析发现列表。
func parseFindings(raw interface{}) []InspectionFinding {
	items := asList(raw)
	out := make([]InspectionFinding, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		title := strings.TrimSpace(llm.StringOr(m, "title", ""))
		detail := strings.TrimSpace(llm.StringOr(m, "detail", ""))
		if title == "" && detail == "" {
			continue
		}
		out = append(out, InspectionFinding{
			Title:    title,
			Detail:   detail,
			Risk:     normalizeRisk(llm.StringOr(m, "risk", "medium")),
			Evidence: parseEvidence(m["evidence"]),
		})
	}
	return out
}

// hasAnyEvidence 是否至少有一条发现带了证据。
func hasAnyEvidence(findings []InspectionFinding) bool {
	for _, f := range findings {
		for _, ev := range f.Evidence {
			if strings.TrimSpace(ev.Ref) != "" {
				return true
			}
			_ = ev
		}
	}
	return false
}

// normalizeRisk 归一化风险等级（Schema 已限定枚举，这里兜住解析层）。
func normalizeRisk(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low":
		return "low"
	case "high":
		return "high"
	default:
		return "medium"
	}
}

// Explain 人话版巡检报告。
func (i *Inspection) Explain() string {
	if i == nil {
		return "（无巡检结论）"
	}
	var b strings.Builder
	provider := i.Provider
	if provider == "" {
		provider = "未知"
	}
	fmt.Fprintf(&b, "【巡检总结】%s\n", i.Summary)
	fmt.Fprintf(&b, "执行信息：provider=%s model=%s 提示词=%s 终止原因=%s 工具调用=%d 用时=%dms\n",
		provider, orUnknown(i.Model), orUnknown(i.PromptVersion), orUnknown(i.StopReason),
		i.ToolCalls, i.DurationMS)

	if len(i.Findings) == 0 {
		b.WriteString("\n【发现】无（若证据充分，这是合法且有价值的结论）\n")
	} else {
		b.WriteString("\n【发现】\n")
		for idx, f := range i.Findings {
			fmt.Fprintf(&b, "  %d. [%s] %s\n     %s\n", idx+1, f.Risk, f.Title, f.Detail)
			for _, ev := range f.Evidence {
				fmt.Fprintf(&b, "     证据 %s：%s（%s）\n", ev.Ref, ev.Quote, ev.Why)
			}
		}
	}
	if len(i.SuggestedActions) > 0 {
		b.WriteString("\n【建议动作（需人工执行，Agent 不会执行）】\n")
		for idx, a := range i.SuggestedActions {
			fmt.Fprintf(&b, "  %d. [%s] %s\n     %s\n", idx+1, a.Risk, a.Title, a.Detail)
			if strings.TrimSpace(a.Rollback) != "" {
				fmt.Fprintf(&b, "     回退：%s\n", a.Rollback)
			}
		}
	}
	if len(i.Assumptions) > 0 {
		b.WriteString("\n【未验证的假设】\n")
		for _, s := range i.Assumptions {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
	}
	if !i.EvidenceSufficient {
		b.WriteString("\n⚠️ 证据不足：以下结论不可直接采信\n")
		if len(i.NextDataToCollect) > 0 {
			b.WriteString("  还需要采集：\n")
			for _, s := range i.NextDataToCollect {
				fmt.Fprintf(&b, "  - %s\n", s)
			}
		}
	}
	if len(i.UnverifiedRefs) > 0 {
		fmt.Fprintf(&b, "\n⚠️ 引用了未观测到的证据定位：%s\n", strings.Join(dedupe(i.UnverifiedRefs), "、"))
	}
	if len(i.Warnings) > 0 {
		b.WriteString("\n【告警】\n")
		for _, w := range i.Warnings {
			fmt.Fprintf(&b, "  - %s\n", w)
		}
	}
	b.WriteString("\n说明：本报告由只读 Agent 生成，仅供人工决策；阶段 2 不具备任何执行权限。\n")
	return b.String()
}

// 确保 tools 包被用于证据类型（InspectionFinding.Evidence 与工具证据同源）。
var _ = tools.Evidence{}

// 确保 errors 包被使用（保留给未来把巡检失败细分成错误类型）。
var _ = errors.Is
