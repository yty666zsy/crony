package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/tmnhs/crony/common/pkg/k8sclient"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// k8sRolloutExecutor 把 k8sclient 适配成 tools.K8sRolloutExecutor。
//
// 为什么审批执行链路最终一定会落回这里：
// 审批服务只负责"这张单子该不该执行、由谁执行、执行一次"，
// 真正的集群调用必须经过工具网关 → 本适配层 → k8sclient，
// 这样"命名空间白名单"这类边界**只有一处**（k8sclient 内部），
// 不会出现"审批放行了、执行时绕过白名单"的错配。
type k8sRolloutExecutor struct {
	client *k8sclient.Client
}

// NewK8sRolloutExecutor 构造集群写执行器（client 为 nil 时返回 nil，调用方据此不注册写工具）。
func NewK8sRolloutExecutor(client *k8sclient.Client) tools.K8sRolloutExecutor {
	if client == nil || !client.Enabled() {
		return nil
	}
	return &k8sRolloutExecutor{client: client}
}

// RolloutRestart 执行一次滚动重启。
//
// 约定与其它执行器一致：**不 panic、不返回 error 来表达业务失败**——
// 越权/目标不存在这类结果必须是 Result（工具失败是观测），
// 只有"执行方自身坏了"才是 error。
func (e *k8sRolloutExecutor) RolloutRestart(ctx context.Context, req tools.K8sRolloutRequest) (tools.Result, error) {
	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = e.client.DefaultNamespace()
	}
	if !e.client.NamespaceAllowed(ns) {
		return tools.Result{
			OK:      false,
			Summary: fmt.Sprintf("命名空间 %q 不在允许列表内，已拒绝重启", ns),
			Err:     fmt.Sprintf("k8s_rollout_restart 拒绝：命名空间 %q 未被 k8s.allow-namespaces 允许", ns),
		}, nil
	}
	if err := e.client.RolloutRestart(ctx, ns, req.Deployment, "crony-agent"); err != nil {
		return tools.Result{
			OK:      false,
			Summary: fmt.Sprintf("重启 %s/%s 失败", ns, req.Deployment),
			Err:     err.Error(),
		}, nil
	}
	return tools.Result{
		OK: true,
		Summary: fmt.Sprintf("已触发 Deployment %s/%s 的滚动重启（理由：%s）；Pod 会按集群节奏逐批替换",
			ns, req.Deployment, req.Reason),
		Meta: map[string]interface{}{"namespace": ns, "deployment": req.Deployment},
		// 写动作的"证据"是它自己的执行事实，落进审计即可；
		// 这里不伪造 evidence 引用——重启本身不构成"某条诊断结论的证据"。
	}, nil
}
