// Package executor 是阶段 1（任务模型 2.0）的执行内核：
// 把所有"真正干活"的动作收敛到一组实现统一接口的 Executor 上。
//
// 为什么必须做这件事（旧实现的问题）：
//   - 执行逻辑散落在 node/internal/handler 的 CMDHandler/HTTPHandler 里，
//     想加一种新执行方式（容器、SQL、Python）就得改调度代码；
//   - 旧 CMDHandler 直接 `exec.Command(job.Cmd[0], job.Cmd[1:]...)`，
//     Windows 上引号解析错误、无输出上限（一个死循环命令能把内存打满）、
//     失败后不区分"可重试/不可重试"。
//
// 新模型：Executor 接口 + Registry，执行器只负责"给我一个步骤，返回结构化结果"；
// 重试、并发、编排由上层（Runner / 调度器）负责。
package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tmnhs/crony/common/models"
)

// Kind 执行器类型标识（落库到 job_run_step.kind）。
type Kind string

const (
	KindShell Kind = "shell"
	KindHTTP  Kind = "http"
	// KindAgent 阶段 2 新增：一次 Agent 会话（有界 ReAct 循环 + 只读工具）。
	// 由 node 侧注入的 AgentRunner 真正执行，executor 只负责编排/重试/落库。
	KindAgent Kind = "agent"
	// 预留（阶段 3 之后逐个落地）：
	// KindContainer Kind = "container"
	// KindSQL       Kind = "sql"
	// KindPython    Kind = "python"
)

var (
	// ErrInvalidRequest 请求不合法（缺少命令/URL 等），重试无意义。
	ErrInvalidRequest = errors.New("executor: invalid request")
	// ErrTimeout 步骤执行超时。
	ErrTimeout = errors.New("executor: step timeout")
	// ErrNotFound 请求的执行器未注册。
	ErrNotFound = errors.New("executor: kind not registered")
)

// DefaultOutputLimit 单步输出落库上限（64 KiB）：
// 防止一条 `yes` 或者死循环打印把数据库和内存打满。
const DefaultOutputLimit = 64 * 1024

// DefaultTimeout 未显式指定超时时的兜底上限。
const DefaultTimeout = 30 * time.Minute

// Request 一次步骤执行请求。字段按执行器类型分组，未使用的字段被忽略。
type Request struct {
	RunID     string
	JobID     int
	StepIndex int
	Name      string
	Kind      Kind
	TraceID   string

	// shell：三选一。
	//   - RawArgv = true → 以 argv 方式直接执行 Command+Args（不经 shell 解析），
	//     这是旧任务（type=1）的原始语义，升级后必须逐字保留；
	//   - Args 非空     → 同上（argv 方式）；
	//   - 其它          → Command 作为 shell 脚本字符串执行（sh -c / cmd /C）
	Command string
	Args    []string
	RawArgv bool
	WorkDir string
	Env     map[string]string

	// http
	Method       string
	URL          string
	Headers      map[string]string
	Body         string
	ExpectStatus []int

	// 公共
	Timeout        time.Duration
	MaxOutputBytes int

	// agent（阶段 2）
	// AgentKind 会话类型：diagnose（失败诊断）/ inspect（巡检总结）/ nljob（草案）。
	AgentKind string
	// AgentGoal 目标（自然语言）。Agent 任务里它来自 job.command。
	AgentGoal string
	// AgentContext 附加上下文材料（每行一条），例如"本次运行的历史情况"。
	AgentContext []string
	// AgentSystem 场景提示词版本/系统提示（由调用方决定，执行器不解释）。
	AgentPromptVersion string
}

// EffectiveTimeout 返回实际使用的超时（<=0 时用兜底值）。
func (r Request) EffectiveTimeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultTimeout
}

// OutputLimit 返回输出上限字节数。
func (r Request) OutputLimit() int {
	if r.MaxOutputBytes > 0 {
		return r.MaxOutputBytes
	}
	return DefaultOutputLimit
}

// Result 结构化执行结果（阶段 1 的"结构化产物"）。
type Result struct {
	StepIndex  int
	Name       string
	Kind       Kind
	Status     models.RunStatus
	ExitCode   int
	Output     string
	Truncated  bool
	RetryCount int
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration
	Err        error
	// Condition / Join / DependsOn 是这一步的编排属性（阶段 4-B）。
	//
	// 为什么结果里要带它们：落库后的步骤行必须能**独立回答**"它为什么跑/没跑"。
	// 只看 status=skipped 而不看 join 与 depends_on，运维无法区分
	// "上游失败被 all_success 挡住"和"这条分支本来就不该在这个条件下跑"。
	Condition StepCondition
	Join      JoinPolicy
	DependsOn []int
	// Artifacts 结构化产物：HTTP 状态码、响应头、容器 ID 等，落库为 JSON。
	Artifacts map[string]string
}

// Error 返回错误的文本形式（无错误时为空串），便于直接落库。
func (r Result) Error() string {
	if r.Err == nil {
		return ""
	}
	return r.Err.Error()
}

// Executor 执行器接口。
type Executor interface {
	// Kind 执行器类型。
	Kind() Kind
	// Validate 在执行前校验请求，尽早失败（而不是把错误留给运行时）。
	Validate(req Request) error
	// Execute 执行步骤。约定：不 panic；把执行失败表达在 Result 里（Err 非空）。
	Execute(ctx context.Context, req Request) Result
}

// newResult 初始化结果骨架。
func newResult(req Request) Result {
	return Result{
		StepIndex: req.StepIndex,
		Name:      req.Name,
		Kind:      req.Kind,
		Status:    models.RunStatusRunning,
		ExitCode:  -1,
		StartedAt: time.Now(),
		Artifacts: make(map[string]string),
	}
}

// finish 收尾结果：写结束时间、耗时、状态。
func (r *Result) finish(status models.RunStatus, err error) Result {
	r.FinishedAt = time.Now()
	r.Duration = r.FinishedAt.Sub(r.StartedAt)
	r.Status = status
	r.Err = err
	return *r
}

// Summarize 把一次执行的多个步骤结果归纳为整体状态。
//
// 优先级：取消 > 超时 > 失败 > 全部跳过 > 成功。
// 只要有一个步骤失败就整体失败——"部分成功"在运维语义里依然是失败，
// 但 success_steps/failed_steps 会一并落库，便于诊断到底哪一步坏了。
func Summarize(results []Result) (status models.RunStatus, exitCode, successSteps, failedSteps int) {
	if len(results) == 0 {
		return models.RunStatusSkipped, -1, 0, 0
	}
	ran := 0
	canceled := false
	timeout := false
	failed := false
	exitCode = -1 // 没有任何步骤真正执行时，退出码无意义，用 -1 表示
	for _, res := range results {
		if res.Status == models.RunStatusCanceled {
			canceled = true
		}
		if res.Status == models.RunStatusTimeout {
			timeout = true
		}
		if res.Status == models.RunStatusFailed {
			failed = true
		}
		if res.Status == models.RunStatusSuccess || res.Status == models.RunStatusFailed ||
			res.Status == models.RunStatusTimeout || res.Status == models.RunStatusCanceled {
			ran++
			exitCode = res.ExitCode
		}
		switch res.Status {
		case models.RunStatusSuccess:
			successSteps++
		case models.RunStatusFailed, models.RunStatusTimeout, models.RunStatusCanceled:
			failedSteps++
		}
	}
	switch {
	case canceled:
		// 取消不算"成功"，也不该被当成"跳过"：它是一次真实发生并被中断的执行
		return models.RunStatusCanceled, exitCode, successSteps, failedSteps
	case timeout:
		return models.RunStatusTimeout, exitCode, successSteps, failedSteps
	case failed:
		return models.RunStatusFailed, exitCode, successSteps, failedSteps
	case ran == 0:
		return models.RunStatusSkipped, exitCode, successSteps, failedSteps
	default:
		return models.RunStatusSuccess, exitCode, successSteps, failedSteps
	}
}

// DescribeRequest 生成可读的步骤描述（用于日志与 console 展示），
// 注意：不打印 headers/body，避免把凭据写进日志。
func DescribeRequest(req Request) string {
	switch req.Kind {
	case KindShell:
		if len(req.Args) > 0 {
			return fmt.Sprintf("shell argv: %v", append([]string{req.Command}, req.Args...))
		}
		return "shell: " + req.Command
	case KindHTTP:
		return fmt.Sprintf("http: %s %s", req.Method, req.URL)
	default:
		return string(req.Kind)
	}
}
