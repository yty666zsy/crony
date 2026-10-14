// Package orchestration 把"任务上的编排定义"（job.steps_json）编译成可执行的
// executor.Plan，并在编译时完成**图级别**的校验。
//
// 三层职责划分（刻意拆开，避免"校验到处都写一半"）：
//
//	common/models      解析 JSON + 结构校验（索引唯一、kind/join/condition 合法、依赖语法）
//	common/pkg/orchestration  本包：编译成 executor.Step（kind/argv/http/agent 语义）+ 图校验
//	common/pkg/executor       执行语义（汇聚、并发、失败传播、重试）
//
// 这样"什么是合法编排"只有一个答案：`BuildPlan` 通过即为合法。
package orchestration

import (
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/executor"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// BuildPlan 把任务的编排定义编译成 executor.Plan。
//
// 行为约定：
//   - `job.StepsJSON` 为空 → 返回 (Plan{}, nil) 且 second=false：
//     调用方（node 的 BuildPlan）应继续走原有的单步逻辑，**不改变老任务行为**；
//   - 编译或图校验失败 → 返回错误。调用方必须**拒绝**这个任务定义（保存时拒绝、
//     执行前也不再尝试），而不是"跳过坏的那一步"。
func BuildPlan(job *models.Job, runID string) (executor.Plan, bool, error) {
	spec, err := models.ParseJobPlan(job.StepsJSON)
	if err != nil {
		return executor.Plan{}, false, err
	}
	if spec == nil {
		return executor.Plan{}, false, nil
	}

	plan := executor.Plan{
		RunID:       runID,
		JobID:       job.ID,
		MaxParallel: spec.MaxParallel,
		FailFast:    spec.FailFast,
		Steps:       make([]executor.Step, 0, len(spec.Steps)),
	}
	for i := range spec.Steps {
		step, err := compileStep(job, spec, i)
		if err != nil {
			return executor.Plan{}, false, err
		}
		// 运行上下文在这里补齐：编译产物自身自洽，便于落库/快照排错
		// （执行时 Runner.normalize 会再写一遍同样的值，不依赖调用方）。
		step.Request.RunID = plan.RunID
		step.Request.JobID = plan.JobID
		step.Request.StepIndex = step.Index
		plan.Steps = append(plan.Steps, step)
	}
	if err := plan.Validate(); err != nil {
		return executor.Plan{}, false, fmt.Errorf("编排非法：%v", err)
	}
	return plan, true, nil
}

// ValidateJobPlan 只做校验，用于保存任务时拒绝坏图。
//
// 注意它在编译阶段就把 run_id 补成占位值：`run_id` 属于"这一次运行"而不是"这份编排"，
// 保存时不该要求调用方编一个假的 run_id 出来。
func ValidateJobPlan(job *models.Job) error {
	probe := *job
	if strings.TrimSpace(probe.StepsJSON) == "" {
		return nil
	}
	if _, _, err := BuildPlan(&probe, "validate-only"); err != nil {
		return err
	}
	return nil
}

// Summarize 返回编排摘要（控制台只读视图用），顺带把图校验结果一并返回。
func Summarize(job *models.Job) (models.PlanSummary, error) {
	spec, err := models.ParseJobPlan(job.StepsJSON)
	if err != nil {
		return models.PlanSummary{}, err
	}
	if spec == nil {
		return models.PlanSummary{}, fmt.Errorf("该任务没有编排定义（steps_json 为空）")
	}
	if _, _, err := BuildPlan(job, "summarize-only"); err != nil {
		// 摘要照给，但把非法原因一起抛给调用方：控制台需要"画出图 + 标出哪一行错了"。
		summary := spec.Summarize()
		return summary, err
	}
	return spec.Summarize(), nil
}

// compileStep 把单个步骤规格编译成 executor.Step。
func compileStep(job *models.Job, spec *models.JobPlanSpec, pos int) (executor.Step, error) {
	s := spec.Steps[pos]
	deps, err := s.ResolveDependsOn(spec.Steps, pos)
	if err != nil {
		return executor.Step{}, err
	}
	kind, err := resolveKind(job, s)
	if err != nil {
		return executor.Step{}, err
	}
	condition, _ := executor.ParseCondition(s.Condition)
	join, ok := executor.ParseJoin(s.Join)
	if !ok {
		return executor.Step{}, fmt.Errorf("步骤 %d 的 join 非法：%q", s.Index, s.Join)
	}

	timeout := time.Duration(s.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(job.Timeout) * time.Second
	}

	step := executor.Step{
		Index:             s.Index,
		Name:              stepName(s),
		Kind:              kind,
		Condition:         condition,
		ContinueOnFailure: s.ContinueOnFailure,
		DependsOn:         deps,
		Join:              join,
		Policy:            stepPolicy(s),
	}

	switch kind {
	case executor.KindHTTP:
		method := resolveHTTPMethod(job, s)
		url, body := s.Command, ""
		if method == "POST" {
			// 与单步任务保持同一约定：POST 用 '?' 拆分 URL 与 body。
			if fields := strings.SplitN(s.Command, "?", 2); len(fields) == 2 {
				url, body = fields[0], fields[1]
			}
		}
		step.Request = executor.Request{
			Kind:    executor.KindHTTP,
			Method:  method,
			URL:     url,
			Body:    body,
			Timeout: timeout,
		}
	case executor.KindAgent:
		// Agent 步骤的执行材料（巡检材料）由节点侧装配，编排层不猜。
		step.Request = executor.Request{
			Kind:      executor.KindAgent,
			AgentKind: models.AgentKindJob,
			AgentGoal: s.Command,
			Timeout:   timeout,
		}
	case executor.KindK8s:
		// k8s 步骤：command = 容器镜像，args = 容器内命令（argv）。
		//
		// 这里**不做 TrimSpace**：镜像名带空格是配置手误，必须由执行器明确拒绝
		// （"镜像名不能有首尾空格"），而不是被静默修好——静默修好会掩盖真实问题，
		// 让人以为"配置没问题却跑出了别的东西"。
		step.Request = executor.Request{
			Kind:         executor.KindK8s,
			Command:      s.Command,
			Args:         s.Args,
			K8sNamespace: s.K8sNamespace,
			Env:          s.K8sEnv,
			Timeout:      timeout,
		}
	default:
		argv, err := resolveArgv(s)
		if err != nil {
			return executor.Step{}, err
		}
		var args []string
		if len(argv) > 1 {
			args = argv[1:]
		}
		step.Request = executor.Request{
			Kind:    executor.KindShell,
			Command: argv[0],
			Args:    args,
			RawArgv: true,
			Timeout: timeout,
		}
	}
	// 让编译产物**自身自洽**：run_id/job_id/step_index 一并写进请求，
	// 这样计划可以直接落库或打快照用于排错（执行时 Runner.normalize 会再写一遍同样的值）。
	step.Request.Kind = step.Kind
	step.Request.Name = step.Name
	return step, nil
}

// resolveHTTPMethod 决定 http 步骤的方法：步骤自己声明优先，否则继承任务的 http_method。
func resolveHTTPMethod(job *models.Job, s models.JobStepSpec) string {
	if strings.EqualFold(strings.TrimSpace(s.HTTPMethod), "POST") {
		return "POST"
	}
	if strings.TrimSpace(s.HTTPMethod) != "" {
		return "GET"
	}
	if job.HttpMethod == models.HTTPMethodPost {
		return "POST"
	}
	return "GET"
}

// resolveArgv 决定 shell 步骤的 argv。
//
// 两种写法，规则刻意保持"少且不猜"：
//   - 只写 command：按 models.SplitCommand 切分（与单步任务逐字一致），所以
//     `/opt/backup.sh -name "my file"` 切出 3 段，引号内的空格不会被拆开；
//   - 同时写 command + args：**command 只能是程序名**，args 是它的参数。
//     如果 command 里还带着参数，直接报错——因为"哪部分是程序、哪部分是参数"
//     一旦靠猜，就会出现"参数被当成程序名"的执行错误，而且是静默的。
func resolveArgv(s models.JobStepSpec) ([]string, error) {
	if len(s.Args) == 0 {
		argv := models.SplitCommand(s.Command)
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			return nil, fmt.Errorf("步骤 %d 的命令为空", s.Index)
		}
		return argv, nil
	}
	program := strings.TrimSpace(s.Command)
	if len(models.SplitCommand(program)) != 1 {
		return nil, fmt.Errorf("步骤 %d 同时写了 args 和带参数的命令 %q：写 args 时 command 只能是程序名", s.Index, s.Command)
	}
	return append([]string{program}, s.Args...), nil
}

// resolveKind 决定步骤用哪个执行器：步骤自己声明优先，否则继承任务类型。
func resolveKind(job *models.Job, s models.JobStepSpec) (executor.Kind, error) {
	if strings.TrimSpace(s.Kind) != "" {
		kind, ok := models.ParseStepKind(s.Kind)
		if !ok {
			return "", fmt.Errorf("步骤 %d 的 kind 非法：%q", s.Index, s.Kind)
		}
		return executor.Kind(kind), nil
	}
	switch job.Type {
	case models.JobTypeHttp:
		return executor.KindHTTP, nil
	case models.JobTypeAgent:
		return executor.KindAgent, nil
	case models.JobTypeK8s:
		return executor.KindK8s, nil
	case models.JobTypeCmd, 0:
		return executor.KindShell, nil
	default:
		return "", fmt.Errorf("步骤 %d 无法确定执行类型：任务类型 %d 未知", s.Index, job.Type)
	}
}

func stepName(s models.JobStepSpec) string {
	if strings.TrimSpace(s.Name) != "" {
		return s.Name
	}
	return fmt.Sprintf("step-%d", s.Index)
}

// stepPolicy 步骤级重试策略（<=0 表示不重试）。
func stepPolicy(s models.JobStepSpec) *retry.Policy {
	if s.RetryTimes <= 0 {
		return nil
	}
	policy := retry.FromLegacy(s.RetryTimes, s.RetryInterval)
	return &policy
}
