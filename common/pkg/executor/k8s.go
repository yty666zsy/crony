package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// 本文件是阶段 K1 的 **k8s 执行器**：把一次步骤执行变成"在集群里跑一个 Job"。
//
// 为什么它是"执行器"而不是"另一套调度器"：
//   - 任务的定时、并发策略、重试、编排（steps_json）、失败传播、取消、
//     结果落库（job_run/job_run_step）在阶段 1~4 已经做完且被测试覆盖；
//     K8s 改变的只是"这一步在哪里跑"，不该动上面任何一层；
//   - 所以它遵守 executor.Executor 契约：给一个 Request，返回一个 Result，
//     失败表达在 Result 里而不是 panic/异常。
//
// 与 AgentExecutor 相同的注入风格：本包**不 import 任何 k8s 库**，
// 真正的集群调用由 K8sRunner（节点/控制面启动时注入）完成。
// 这样做的三个理由：
//   - 执行层的全部语义（校验、超时、输出截断、错误分类）可用纯内存单测穷举，
//     不需要一个真集群，也不需要 client-go 的假对象去"假装"执行成功；
//   - 未接入集群时，k8s 步骤必须**明确失败**（"本节点未接入 Kubernetes"），
//     而不是含糊的"类型未注册"；
//   - 将来把执行后端换成 K8s 之外的容器编排（例如 Nomad/ECS）时，
//     只需再实现一个 Runner，调度与落库一行不用改。

// KindK8s 阶段 K1：在 Kubernetes 集群里以 Job（Pod）方式执行步骤（常量定义见 executor.go）。
//
// 与 KindShell 的关键差异：副作用**不在本机**，而在集群里。
// 因此"在哪跑"是请求的一部分（命名空间 + 镜像），而不再是一个本地命令字符串。

// K8sJobSpec 一次"集群内执行"的请求。
//
// 为什么单独定义 Spec 而不是直接把 Request 传下去：
// Request 是"所有执行器共用的大结构"，字段按类型分组、互相之间没有约束；
// Spec 是"k8s 执行需要的最小闭包"，交给 Runner 后它可以被独立测试与审计。
type K8sJobSpec struct {
	// 运行身份（落库与审计用，注入到 Pod 的标签与环境变量里，
	// 这样"集群里这个 Pod 是谁派出来的"在 kubectl 侧也能一眼看到）。
	RunID     string
	JobID     int
	StepIndex int
	Name      string
	TraceID   string

	// Namespace 目标命名空间；空 = 运行时默认命名空间（由 Runner 决定）。
	Namespace string
	// Image 容器镜像（必填）。
	Image string
	// Command 容器内命令（argv）；为空 = 用镜像自带的 entrypoint。
	Command []string
	// Env 注入到容器的环境变量（在 Runner 的基线上叠加）。
	Env map[string]string

	// Timeout 墙钟上限；<=0 时用执行器兜底值。
	Timeout time.Duration
	// MaxOutputBytes 日志回传上限。
	MaxOutputBytes int
}

// K8sJobOutcome 一次集群执行对上层可见的结果。
type K8sJobOutcome struct {
	// Namespace/JobName/PodName 落库到 artifacts，供控制台一键跳转排查。
	Namespace string
	JobName   string
	PodName   string
	// Phase 集群视角的最终阶段（succeeded/failed/timeout/error/…）。
	Phase string
	// ExitCode 容器退出码；拿不到时为 -1（例如 Pod 从未启动）。
	ExitCode int
	// Output 容器日志（调用方负责截断）。
	Output string
	// Artifacts 结构化产物（镜像、命名空间、Pod、退出原因等）。
	Artifacts map[string]string
	// Err 执行失败原因。约定：**不 panic**，失败表达在这里。
	//
	// 错误分类约定：Runner 应尽量用 retry.Fatal 标记"重试也不会好"的失败
	// （镜像不存在、命名空间不存在、RBAC 拒绝、资源上限被拒），
	// 其余（apiserver 瞬时不可用、节点 NotReady、Pod 被驱逐）保持未分类，
	// 让上层的重试策略去处理。
	Err error
}

// K8sRunner 由节点/控制面侧注入的集群执行器。
type K8sRunner interface {
	// RunK8sJob 在集群里执行一次 Job 并等待结束。
	// 约定：不 panic；失败表达在 K8sJobOutcome.Err 里。
	RunK8sJob(ctx context.Context, spec K8sJobSpec) K8sJobOutcome
}

// K8sExecutor 把"集群内执行"包装成普通步骤执行器。
type K8sExecutor struct {
	runner K8sRunner
}

// NewK8sExecutor 创建 k8s 执行器（runner 为 nil 表示本进程未接入集群）。
func NewK8sExecutor(runner K8sRunner) *K8sExecutor { return &K8sExecutor{runner: runner} }

func (e *K8sExecutor) Kind() Kind { return KindK8s }

// Validate 尽早拒绝脏请求。
//
// 与 shell 的"命令不能为空"对应：k8s 步骤的必填项是**镜像**——
// 没有镜像就没有任何可执行的东西，而且它无法在执行期被"补救"。
func (e *K8sExecutor) Validate(req Request) error {
	if strings.TrimSpace(req.Command) == "" {
		return retry.Fatal(fmt.Errorf("%w: k8s 步骤缺少容器镜像（image 不能为空）", ErrInvalidRequest))
	}
	if strings.TrimSpace(req.Command) != req.Command {
		// 镜像名带首尾空格几乎总是配置手误（"nginx:1.25 " 会被运行时当成不存在的镜像），
		// 但这里**不改写**用户输入：静默 trim 会让"配置错"变成"跑对了"，掩盖真实问题。
		return retry.Fatal(fmt.Errorf("%w: 镜像名不能有首尾空格：%q", ErrInvalidRequest, req.Command))
	}
	if e.runner == nil {
		return retry.Fatal(fmt.Errorf("%w: 本节点未接入 Kubernetes（检查 k8s 配置与 kubeconfig）", ErrInvalidRequest))
	}
	return nil
}

// Execute 执行一次集群任务。
//
// ⚠️ 与 Agent 步骤一致、与 shell/http 不同的是：超时**不在这里算重试语义**，
// 而是转成 RunStatusTimeout + 可重试错误；具体重试次数由步骤策略决定。
func (e *K8sExecutor) Execute(ctx context.Context, req Request) Result {
	res := newResult(req)
	if err := e.Validate(req); err != nil {
		return res.finish(models.RunStatusFailed, retry.Fatal(err))
	}

	timeout := req.EffectiveTimeout()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := e.runner.RunK8sJob(runCtx, K8sJobSpec{
		RunID:          req.RunID,
		JobID:          req.JobID,
		StepIndex:      req.StepIndex,
		Name:           req.Name,
		TraceID:        req.TraceID,
		Namespace:      req.K8sNamespace,
		Image:          req.Command,
		Command:        req.Args,
		Env:            req.Env,
		Timeout:        timeout,
		MaxOutputBytes: req.OutputLimit(),
	})

	if out.Artifacts == nil {
		out.Artifacts = map[string]string{}
	}
	// 结构化产物先落，成功失败都保留：排查"为什么没起来"靠的就是
	// 命名空间/Pod 名/退出原因，而不是一句错误文本。
	if out.Namespace != "" {
		out.Artifacts["k8s_namespace"] = out.Namespace
	}
	if out.JobName != "" {
		out.Artifacts["k8s_job"] = out.JobName
	}
	if out.PodName != "" {
		out.Artifacts["k8s_pod"] = out.PodName
	}
	if out.Phase != "" {
		out.Artifacts["k8s_phase"] = out.Phase
	}
	out.Artifacts["k8s_image"] = req.Command
	res.Artifacts = out.Artifacts
	res.Output, res.Truncated = truncateOutput(out.Output, req.OutputLimit())
	res.ExitCode = out.ExitCode

	if out.Err == nil {
		if res.ExitCode < 0 {
			res.ExitCode = 0
		}
		return res.finish(models.RunStatusSuccess, nil)
	}

	// 超时：算可重试（多数超时来自下游瞬时变慢），与 shell 执行器保持一致口径。
	if errors.Is(out.Err, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
		res.Artifacts["timeout"] = timeout.String()
		return res.finish(models.RunStatusTimeout,
			retry.Retryable(fmt.Errorf("%w: 超过 %s：%s", ErrTimeout, timeout, out.Err)))
	}
	// 被取消：绝不重试（与阶段 1 的取消语义一致）。
	if ctx.Err() != nil || errors.Is(out.Err, retry.ErrCanceled) {
		return res.finish(models.RunStatusCanceled, retry.Fatal(fmt.Errorf("执行被取消: %w", out.Err)))
	}
	if res.ExitCode == 0 {
		// 有错误但退出码是 0：说明失败发生在"执行前/执行后"（创建被拒、日志取不到）。
		// 用 -1 而不是 0，避免控制台显示"退出码 0 却失败"这种自相矛盾的行。
		res.ExitCode = -1
	}
	return res.finish(models.RunStatusFailed, classifyK8sError(out.Err))
}

// classifyK8sError 决定集群执行失败能不能自动重试。
//
// 缺省语义交给 retry.Classify（未分类错误按可重试处理）：
// 这是刻意的——"apiserver 抖动/节点 NotReady"在真实集群里占比远高于配置错误，
// 而配置错误应由 Runner 显式标记 retry.Fatal 收窄。
func classifyK8sError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return retry.Fatal(err)
	}
	return err
}

var _ Executor = (*K8sExecutor)(nil)
