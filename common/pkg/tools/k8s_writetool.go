package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
)

// 本文件是阶段 K3 的**写工具**：`k8s_rollout_restart`（滚动重启 Deployment）。
//
// 为什么第一个集群写动作选它：
//   - 它是运维最常用的"救命动作"（进程僵住、连接池泄漏、需要清缓存），
//     而配置本身没错——所以它不该被归到"重跑任务"那一类；
//   - 它是**可逆**的：不改变期望状态，只是把 Pod 换一批，出问题再重启一次即可，
//     与"删数据/改网络/改镜像"有本质区别（那些属于 destructive，本期一律不实现）；
//   - 它有**明确的目标**（一个 deployment），所以爆炸半径能被写清楚，
//     而不是"影响一大片但说不清影响谁"。
//
// 与只读的 k8s_readonly 的关系：两者是**两个工具**，不是同一个工具的参数。
// 理由是安全边界必须挂在工具粒度上——如果把"重启"做成 k8s_readonly 的一个 op，
// 那么"只读工具箱"这个标签就骗人了，审批与自治级别的判定也会跟着错。

// K8sRolloutRequest 一次滚动重启请求。
//
// 为什么要带 Reason：审批人批的是"为什么重启 web"，不是"重启 web"。
// 这条理由在审批单里给人看，也在执行后的审计行里留痕。
type K8sRolloutRequest struct {
	Namespace  string
	Deployment string
	Reason     string
}

// K8sRolloutExecutor 集群写动作的执行实现（由 admin 侧装配）。
type K8sRolloutExecutor interface {
	// RolloutRestart 执行滚动重启。约定：不 panic；失败表达在 Result.Err 里。
	RolloutRestart(ctx context.Context, req K8sRolloutRequest) (Result, error)
}

// K8sRolloutTool 滚动重启工具。
type K8sRolloutTool struct {
	exec    K8sRolloutExecutor
	timeout time.Duration
}

// NewK8sRolloutTool 创建写工具。exec 为 nil 时工具仍可注册，但调用会被拒绝
// （"注册了却没人能执行"必须是显式失败，而不是静默成功）。
func NewK8sRolloutTool(exec K8sRolloutExecutor) *K8sRolloutTool {
	return &K8sRolloutTool{exec: exec, timeout: 60 * time.Second}
}

// Name 实现 Tool。
func (t *K8sRolloutTool) Name() string { return "k8s_rollout_restart" }

// Description 实现 Tool。
func (t *K8sRolloutTool) Description() string {
	return "滚动重启一个 Kubernetes Deployment（写动作，可逆：不改变期望状态，只把 Pod 换一批）。" +
		"这是写动作：默认不会立即执行，会先生成一张审批单，人工批准后才会真正重启；" +
		"调用成功后要如实写\"已提交人工审批\"，不要写成已执行。" +
		"它**不能**改镜像、改副本数、删资源——那些是更高风险的动作，本工具不提供。"
}

// Schema 实现 Tool。
func (t *K8sRolloutTool) Schema() *jsonschema.Schema {
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"deployment": jsonschema.String("要重启的 Deployment 名（必填；只支持单个目标，避免一次审批影响一大片）").WithLength(1, 253),
		"namespace":  jsonschema.String("命名空间（可选；省略 = 默认命名空间）").WithLength(0, 63),
		"reason":     jsonschema.String("为什么要重启（必填，写进审批单给人看）").WithLength(4, 500),
	}, "deployment", "reason")
}

// ReadOnly 实现 Tool：写工具必须为 false（与 Risk()=write 自洽）。
func (t *K8sRolloutTool) ReadOnly() bool { return false }

// Risk 实现 Tool：可逆写。
//
// 为什么不是 destructive：重启不删除任何东西、不改期望状态，最坏结果是短暂不可用，
// 而且"再重启一次/回滚"都能恢复。真正 destructive 的是"改镜像到错误版本""改副本数为 0"
// "删 Deployment"——本工具从设计上就不提供这些能力。
func (t *K8sRolloutTool) Risk() RiskLevel { return RiskWrite }

// Timeout 实现 Tool：一次 patch + 集群响应。
func (t *K8sRolloutTool) Timeout() time.Duration { return t.timeout }

// Targets 实现 TargetProvider：本次动作影响的目标（爆炸半径按它计数）。
//
// 口径与只读证据的 ref 保持一致（`deployment/<ns>/<name>`）：
// 审批人看到的"影响谁"和执行后审计里的"影响了谁"必须是同一个字符串。
func (t *K8sRolloutTool) Targets(args map[string]interface{}) []string {
	ns := strings.TrimSpace(llm.StringOr(args, "namespace", ""))
	if ns == "" {
		ns = "default"
	}
	name := strings.TrimSpace(llm.StringOr(args, "deployment", ""))
	if name == "" {
		return nil
	}
	return []string{"deployment/" + ns + "/" + name}
}

// Execute 实现 Tool。
//
// 与 job_retry 同样被两类调用方复用：网关自动放行（L2/L3 白名单）与
// 审批执行器在 CAS 抢到"待执行"后调用。
func (t *K8sRolloutTool) Execute(ctx context.Context, args map[string]interface{}) (Result, error) {
	if t.exec == nil {
		return Result{}, fmt.Errorf("%w: k8s_rollout_restart 没有装配执行器（admin 侧未接入集群写能力）", ErrToolDenied)
	}
	deployment := strings.TrimSpace(llm.StringOr(args, "deployment", ""))
	if deployment == "" {
		return Result{}, fmt.Errorf("%w: deployment 必填", ErrInvalidArgs)
	}
	reason := strings.TrimSpace(llm.StringOr(args, "reason", ""))
	if reason == "" {
		// 与 job_retry 同一立场：没有理由的写动作不允许执行。
		return Result{}, fmt.Errorf("%w: reason 必填（没有理由的写动作不允许执行）", ErrInvalidArgs)
	}
	req := K8sRolloutRequest{
		Namespace:  strings.TrimSpace(llm.StringOr(args, "namespace", "")),
		Deployment: deployment,
		Reason:     reason,
	}
	res, err := t.exec.RolloutRestart(ctx, req)
	if err != nil {
		return Result{OK: false, Err: llm.Redact(err.Error())}, nil
	}
	if res.Summary == "" && res.OK {
		res.Summary = fmt.Sprintf("已触发 Deployment %s/%s 的滚动重启", req.Namespace, req.Deployment)
	}
	return res, nil
}

var _ Tool = (*K8sRolloutTool)(nil)
var _ TargetProvider = (*K8sRolloutTool)(nil)
