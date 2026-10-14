package k8sclient

import (
	"time"

	"github.com/tmnhs/crony/common/models"
)

// FromConfig 按配置建立集群客户端。
//
// 返回 (nil, nil) 表示"未接入集群"，这是**合法状态**：
// 调用方据此不注册 k8s 能力，而不是注册一个"调用必失败"的东西。
//
// 为什么把这段转换放在本包而不是各调用方各写一遍：
// node 要跑 k8s 任务、admin 要给 Agent 挂只读集群工具，两边的边界配置
// 必须是**同一份**。曾经 SQL 行数上限就在两处漂移（100 vs 200），
// 结果"我明明收窄了"变成查不出的谜——这里不再重犯。
func FromConfig(cfg models.K8s) (*Client, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	poll := time.Duration(cfg.PollIntervalSeconds) * time.Second
	if poll <= 0 {
		poll = 2 * time.Second
	}
	return New(Config{
		Kubeconfig:               cfg.Kubeconfig,
		Context:                  cfg.Context,
		Namespace:                cfg.Namespace,
		AllowNamespaces:          cfg.AllowNamespaces,
		AllowImages:              cfg.AllowImages,
		DefaultCPU:               cfg.DefaultCPU,
		DefaultMemory:            cfg.DefaultMemory,
		ServiceAccountName:       cfg.ServiceAccountName,
		TTLSecondsAfterFinished:  cfg.TTLSecondsAfterFinished,
		PollInterval:             poll,
		AllowPrivilegeEscalation: cfg.AllowPrivilegeEscalation,
	})
}
