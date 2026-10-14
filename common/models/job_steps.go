package models

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 阶段 4-B：多步骤任务定义（编排）。
//
// 存放位置：`job.steps_json`（单个 text 列，一个 JSON 对象）。
// 为什么不建 `job_step` 表：编排是**任务的属性**而不是独立实体——它随任务一起被创建、
// 复制、删除、下发到节点；拆表会带来"任务删了步骤还在""下发时忘了带步骤"这类
// 只会在生产上暴露的一致性问题。代价是这条 JSON 必须自带版本与校验（见 ParseJobPlan）。
//
// JSON 形态（steps_json 列的内容）：
//
//	{
//	  "max_parallel": 2,
//	  "fail_fast": false,
//	  "steps": [
//	    {"index":0, "name":"extract",  "kind":"shell", "command":"/opt/extract.sh"},
//	    {"index":1, "name":"transform","kind":"shell", "command":"/opt/transform.sh"},
//	    {"index":2, "name":"enrich",   "kind":"shell", "command":"/opt/enrich.sh"},
//	    {"index":3, "name":"load",     "kind":"shell", "command":"/opt/load.sh",
//	     "depends_on":"1,2", "join":"all_success"}
//	  ]
//	}
//
// `depends_on` 三种写法（**这是最容易写错的地方，所以规则要少**）：
//   - 不写：默认依赖"索引顺序上的前一步"（索引 0 则没有上游）——线性编排不必写依赖；
//   - `"0,2"`：显式依赖这两个索引；
//   - `"root"`：显式声明"没有上游"（分支的并排起点）。
//
// 一个步骤都没有的 steps_json 等价于"没有编排"，任务按老的单步语义执行（见 node 的 BuildPlan）。
// 这条规则是向后兼容的关键：老任务的 steps_json 为空列，行为逐字节不变。
type JobStepSpec struct {
	// Index 步骤索引，必须唯一且非负；结果与落库都按它对齐。
	Index int `json:"index"`
	// Name 展示名（控制台/审计用）。
	Name string `json:"name,omitempty"`
	// Kind 执行器类型：shell / http / agent / k8s。为空时继承任务的 job_type。
	Kind string `json:"kind,omitempty"`
	// Command：shell=程序或脚本、http=URL、agent=自然语言目标、k8s=容器镜像。
	Command string `json:"command"`
	// Args：argv 模式的参数（给 shell 步骤用）。写了它就是 argv 模式，按原样传参。
	// k8s 步骤下它是**容器内命令**（argv），与 shell 的 argv 语义一致。
	Args []string `json:"args,omitempty"`
	// K8sNamespace 仅 k8s 步骤：目标命名空间（空 = 运行时的默认命名空间）。
	K8sNamespace string `json:"k8s_namespace,omitempty"`
	// K8sEnv 仅 k8s 步骤：注入容器的环境变量（在运行时基线上叠加）。
	// 刻意用 map 而不是 "K=V,K=V" 字符串：逗号与等号都在真实值里出现过。
	K8sEnv map[string]string `json:"k8s_env,omitempty"`
	// DependsOn 见文件头说明。
	DependsOn string `json:"depends_on,omitempty"`
	// Join 汇聚策略：all_success / any_success / all_done（空=all_success）。
	Join string `json:"join,omitempty"`
	// Condition 条件：on_success / on_failure / always（空=on_success）。
	Condition string `json:"condition,omitempty"`
	// ContinueOnFailure 仅在线性（无任何依赖）编排里生效。
	ContinueOnFailure bool `json:"continue_on_failure,omitempty"`
	// TimeoutSeconds 步骤级超时；<=0 用任务级 timeout，再兜底执行器默认值。
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
	// RetryTimes / RetryInterval 步骤级重试（<=0 表示不重试）。
	RetryTimes    int   `json:"retry_times,omitempty"`
	RetryInterval int64 `json:"retry_interval,omitempty"`
	// HTTPMethod 仅 http 步骤：GET/POST（空=GET）。
	HTTPMethod string `json:"http_method,omitempty"`
}

// JobPlanSpec 一份完整编排定义。
type JobPlanSpec struct {
	// MaxParallel 并发上限；<=1 表示串行（默认串行，理由见 executor 的 Plan 注释）。
	MaxParallel int `json:"max_parallel,omitempty"`
	// FailFast 任一步骤失败后是否急停其它待启动步骤。
	FailFast bool `json:"fail_fast,omitempty"`
	// Steps 步骤列表。
	Steps []JobStepSpec `json:"steps"`
}

// MaxPlanSteps 单个任务的步骤数上限。
//
// 上限不是为了省资源，而是为了"编排必须是人能看懂的东西"：一张几十个节点的图，
// 出问题时没人能在一小时内定位，而且它往往说明该拆成多个任务（用依赖/触发串联）。
const MaxPlanSteps = 32

// MaxFanIn 单个步骤的上游数量上限（同上：超过这个数就该引入中间汇总步骤）。
const MaxFanIn = 8

// ParseJobPlan 解析 steps_json。空串/纯空白返回 (nil, nil)：**没有编排就是合法状态**。
func ParseJobPlan(raw string) (*JobPlanSpec, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	var spec JobPlanSpec
	dec := json.NewDecoder(strings.NewReader(trimmed))
	// 禁止未知字段：拼错的 "depends_ons" / "join_policy" 必须当场报错，
	// 否则它会静默退化成"没有依赖的并排步骤"，而并排执行正是最危险的形态。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("编排定义不是合法 JSON 对象：%v", err)
	}
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// Normalize 做**结构层面**的校验与补全（不动图语义：环/汇聚的可执行性由 executor 校验）。
func (p *JobPlanSpec) Normalize() error {
	if len(p.Steps) == 0 {
		return fmt.Errorf("编排定义里没有任何步骤：要么删掉 steps_json，要么给它一个步骤")
	}
	if len(p.Steps) > MaxPlanSteps {
		return fmt.Errorf("步骤数 %d 超过上限 %d：请拆成多个任务", len(p.Steps), MaxPlanSteps)
	}
	if p.MaxParallel < 0 {
		return fmt.Errorf("max_parallel 不能为负：%d", p.MaxParallel)
	}
	seen := make(map[int]struct{}, len(p.Steps))
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.Index < 0 {
			return fmt.Errorf("第 %d 个步骤的 index 非法：%d（不允许负数）", i+1, s.Index)
		}
		if _, dup := seen[s.Index]; dup {
			return fmt.Errorf("步骤 index 重复：%d（每步的 index 必须唯一）", s.Index)
		}
		seen[s.Index] = struct{}{}
		if len(strings.TrimSpace(s.Command)) == 0 {
			return fmt.Errorf("步骤 %d 缺少 command", s.Index)
		}
		if s.Kind != "" {
			if _, ok := ParseStepKind(s.Kind); !ok {
				return fmt.Errorf("步骤 %d 的 kind 非法：%q（可选 shell/http/agent/k8s）", s.Index, s.Kind)
			}
		}
		if _, ok := ParseJoinName(s.Join); !ok {
			return fmt.Errorf("步骤 %d 的 join 非法：%q（可选 all_success/any_success/all_done）", s.Index, s.Join)
		}
		if _, ok := ParseConditionName(s.Condition); !ok {
			return fmt.Errorf("步骤 %d 的 condition 非法：%q（可选 on_success/on_failure/always）", s.Index, s.Condition)
		}
		if strings.TrimSpace(s.HTTPMethod) != "" {
			m := strings.ToUpper(strings.TrimSpace(s.HTTPMethod))
			if m != "GET" && m != "POST" {
				return fmt.Errorf("步骤 %d 的 http_method 非法：%q（可选 GET/POST）", s.Index, s.HTTPMethod)
			}
			s.HTTPMethod = m
		}
		if s.TimeoutSeconds < 0 {
			return fmt.Errorf("步骤 %d 的 timeout_seconds 不能为负", s.Index)
		}
		if s.RetryTimes < 0 {
			return fmt.Errorf("步骤 %d 的 retry_times 不能为负", s.Index)
		}
		if s.RetryTimes > MaxRetryTimes {
			return fmt.Errorf("步骤 %d 的 retry_times %d 超过上限 %d", s.Index, s.RetryTimes, MaxRetryTimes)
		}
		if s.RetryInterval < 0 {
			return fmt.Errorf("步骤 %d 的 retry_interval 不能为负", s.Index)
		}
		// 依赖语法在这里就报错，而不是留给执行期。
		if _, err := s.ResolveDependsOn(p.Steps, i); err != nil {
			return err
		}
	}
	return nil
}

// ResolveDependsOn 把 `depends_on` 的三种写法解析成索引列表。
//
// prevFallback 为 true 时（字段没写），返回"索引顺序上的前一步"：
// 这是线性编排的默认，也是"不写依赖 = 按书写顺序串起来"这一直觉的落点。
func (s JobStepSpec) ResolveDependsOn(all []JobStepSpec, selfPos int) ([]int, error) {
	raw := strings.TrimSpace(s.DependsOn)
	switch strings.ToLower(raw) {
	case "":
		if selfPos == 0 {
			return nil, nil // 第一个步骤没有上游
		}
		return []int{all[selfPos-1].Index}, nil
	case "root", "none", "-":
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > MaxFanIn {
		return nil, fmt.Errorf("步骤 %d 的上游数量 %d 超过上限 %d", s.Index, len(parts), MaxFanIn)
	}
	out := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		val := strings.TrimSpace(part)
		if val == "" {
			return nil, fmt.Errorf("步骤 %d 的 depends_on 含有空的依赖项：%q", s.Index, raw)
		}
		idx, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("步骤 %d 的 depends_on 含非数字项 %q（只支持 \"0,2\" 或 \"root\"）", s.Index, val)
		}
		if idx == s.Index {
			return nil, fmt.Errorf("步骤 %d 依赖了自身", s.Index)
		}
		if _, dup := seen[idx]; dup {
			continue // 重复写同一个依赖没有语义差别，去重而不是报错
		}
		seen[idx] = struct{}{}
		out = append(out, idx)
	}
	sort.Ints(out) // 排序只为了让"同样的图"产生同样的计划（diff 友好），不影响语义
	return out, nil
}

// StepKind 步骤类型（与 executor.Kind 的字符串形式一致，但这里是**模型层**，
// 不能 import executor——executor 反过来 import models）。
type StepKind string

const (
	StepKindShell StepKind = "shell"
	StepKindHTTP  StepKind = "http"
	StepKindAgent StepKind = "agent"
	StepKindK8s   StepKind = "k8s"
)

// ParseStepKind 解析步骤类型（大小写不敏感，空串交给调用方继承任务类型）。
func ParseStepKind(raw string) (StepKind, bool) {
	switch StepKind(strings.ToLower(strings.TrimSpace(raw))) {
	case StepKindShell:
		return StepKindShell, true
	case StepKindHTTP:
		return StepKindHTTP, true
	case StepKindAgent:
		return StepKindAgent, true
	case StepKindK8s:
		return StepKindK8s, true
	default:
		return "", false
	}
}

// ParseJoinName 校验汇聚策略名（真正的语义解析在 executor.ParseJoin）。
func ParseJoinName(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "all_success", true
	case "all_success", "any_success", "all_done":
		return strings.ToLower(strings.TrimSpace(raw)), true
	default:
		return "", false
	}
}

// ParseConditionName 校验条件名（真正的语义解析在 executor.ParseCondition）。
func ParseConditionName(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "on_success", true
	case "on_success", "on_failure", "always":
		return strings.ToLower(strings.TrimSpace(raw)), true
	default:
		return "", false
	}
}

// PlanSummary 是给控制台/接口用的编排摘要（步骤 + 边），不涉及执行语义。
type PlanSummary struct {
	MaxParallel int            `json:"max_parallel"`
	FailFast    bool           `json:"fail_fast"`
	Steps       []PlanStepView `json:"steps"`
	Edges       []PlanEdgeView `json:"edges"`
	// Depth 每个步骤的层深（最长路径），控制台据此分层渲染；同层可并行。
	// 这里刻意给"最坏情况下的层深"而不是"最早可开始层"：运维看到的是
	// "这一步最晚要等到哪一层"，用它解释 SLO 延迟比乐观值更诚实。
	MaxDepth int `json:"max_depth"`
}

// PlanStepView 单个步骤的展示视图。
type PlanStepView struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Command   string `json:"command"`
	DependsOn []int  `json:"depends_on"`
	Join      string `json:"join"`
	Condition string `json:"condition"`
	Depth     int    `json:"depth"`
	Retries   int    `json:"retry_times"`
	Timeout   int64  `json:"timeout_seconds"`
	// Warnings 刻意**不加 omitempty**：控制台对 warnings/depends_on 直接取 .length，
	// 字段缺失（而不是空数组）会让整页渲染时抛错白屏。数组一律给 []。
	Warnings []string `json:"warnings"`
}

// PlanEdgeView 一条依赖边（from → to 表示 to 依赖 from）。
type PlanEdgeView struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// Summarize 生成编排摘要（供只读接口与控制台）。
//
// 它不校验环（校验由 executor 负责），但对"孤立步骤（既不是根也没有下游）"给出 warning：
// 一个谁也不依赖、也没人依赖它的步骤往往是删改图时留下的手尾，值得提醒而不是静默执行。
func (p *JobPlanSpec) Summarize() PlanSummary {
	summary := PlanSummary{
		MaxParallel: p.MaxParallel,
		FailFast:    p.FailFast,
		// 数组一律初始化为空切片（而不是 nil）：控制台直接对 .length 取值，
		// null 会让"只读编排"整页渲染时抛 TypeError 白屏。
		Steps: make([]PlanStepView, 0, len(p.Steps)),
		Edges: make([]PlanEdgeView, 0),
	}
	if p.MaxParallel <= 0 {
		summary.MaxParallel = 1
	}
	depsByPos := make([][]int, len(p.Steps))
	for i := range p.Steps {
		deps, err := p.Steps[i].ResolveDependsOn(p.Steps, i)
		if err != nil {
			deps = nil
		}
		if deps == nil {
			// 根步骤/显式 root 解析出的就是 nil；这里统一成空切片，接口签名稳定为 []。
			deps = []int{}
		}
		depsByPos[i] = deps
	}

	hasDownstream := make(map[int]bool, len(p.Steps))
	for i, s := range p.Steps {
		deps := depsByPos[i]
		kind := strings.ToLower(strings.TrimSpace(s.Kind))
		if kind == "" {
			kind = string(StepKindShell)
		}
		condition := "on_success"
		if c, ok := ParseConditionName(s.Condition); ok {
			condition = c
		}
		join := "all_success"
		if j, ok := ParseJoinName(s.Join); ok {
			join = j
		}
		view := PlanStepView{
			Index:     s.Index,
			Name:      s.Name,
			Kind:      kind,
			Command:   s.Command,
			DependsOn: deps,
			Join:      join,
			Condition: condition,
			Retries:   s.RetryTimes,
			Timeout:   s.TimeoutSeconds,
			Warnings:  []string{},
		}
		for _, d := range deps {
			summary.Edges = append(summary.Edges, PlanEdgeView{From: d, To: s.Index})
			hasDownstream[d] = true
		}
		if len(deps) == 0 && i != 0 && !isExplicitRoot(s) {
			view.Warnings = append(view.Warnings, "该步骤没有上游，会被当作并排分支的起点执行")
		}
		summary.Steps = append(summary.Steps, view)
	}

	// 层深 = 依赖链上最长的一条（迭代松弛，图有环时会停在上限，由 validate 负责报错）
	depth := make(map[int]int, len(p.Steps))
	posByIndex := make(map[int]int, len(p.Steps))
	for i, s := range p.Steps {
		posByIndex[s.Index] = i
		depth[s.Index] = 0
	}
	for round := 0; round < len(p.Steps); round++ {
		changed := false
		for i, s := range p.Steps {
			best := 0
			for _, d := range depsByPos[i] {
				if dv := depth[d] + 1; dv > best {
					best = dv
				}
			}
			if best > depth[s.Index] {
				depth[s.Index] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	for i := range summary.Steps {
		idx := summary.Steps[i].Index
		summary.Steps[i].Depth = depth[idx]
		if depth[idx] > summary.MaxDepth {
			summary.MaxDepth = depth[idx]
		}
		if len(summary.Steps[i].DependsOn) == 0 && !hasDownstream[idx] && len(p.Steps) > 1 {
			summary.Steps[i].Warnings = append(summary.Steps[i].Warnings,
				"该步骤既没有上游也没有下游：它是一座孤岛（确认这是有意为之）")
		}
	}
	return summary
}

func isExplicitRoot(s JobStepSpec) bool {
	switch strings.ToLower(strings.TrimSpace(s.DependsOn)) {
	case "root", "none", "-":
		return true
	default:
		return false
	}
}
