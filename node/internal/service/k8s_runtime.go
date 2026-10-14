package service

import (
	"fmt"

	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/k8sclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/node/internal/handler"
)

// initK8sRuntime 装配节点侧 K8s 集群执行器并注入步骤执行器。
//
// 与 Agent 运行时同一个模式：未配置就不注入，于是 k8s 步骤会以
// **明确错误**失败（"本节点未接入 Kubernetes"），而不是静默跳过。
//
// 为什么执行器要注入到"步骤"这一层而不是另开一条调度路径：
// 定时、并发策略、重试、编排、失败传播、取消、落库（job_run/job_run_step）
// 在阶段 1~4 已经做完且被测试覆盖，k8s 只改变"这一步在哪里跑"。
// 这样 k8s 任务天然拥有重试与并发语义，也天然出现在执行记录页里。
func (srv *NodeServer) initK8sRuntime() {
	cfg := config.GetConfigModels()
	if cfg == nil {
		return
	}
	client, err := k8sclient.FromConfig(cfg.K8s)
	if err != nil {
		// 配置了但连不上/没找到 kubeconfig：必须显式告警（静默降级会让
		// "我明明配了 k8s"变成一个查不出的谜），但**不拖垮节点**——
		// shell/http 任务照常跑，这正是执行器分层的好处。
		logger.GetLogger().Error(fmt.Sprintf("[k8s] 集群客户端初始化失败，k8s 任务不可用（其它任务不受影响）：%s", err.Error()))
		return
	}
	if client == nil {
		logger.GetLogger().Warn("[k8s] 未启用集群接入（k8s.enabled=false）：k8s 任务将以明确错误失败，不会静默降级成别的类型。")
		return
	}
	handler.SetK8sRunner(client)
	// 生效边界必须打日志：出事后第一件事就是确认"当时到底放开了什么"。
	logger.GetLogger().Info(fmt.Sprintf(
		"[k8s] 集群执行器已就绪：namespace=%s 允许命名空间=%v 允许镜像=%v（空白名单=不允许任何镜像）",
		client.DefaultNamespace(), cfg.K8s.AllowNamespaces, cfg.K8s.AllowImages))
	if len(cfg.K8s.AllowImages) == 0 {
		logger.GetLogger().Warn("[k8s] allow-images 为空：**任何** k8s 任务都会被拒绝。需要跑镜像时必须显式列出（或写 [\"*\"] 承担风险）。")
	}
}
