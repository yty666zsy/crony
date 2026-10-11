package service

import (
	"fmt"
	"strings"
	"sync"

	"github.com/tmnhs/crony/common/pkg/agentconfig"
	"github.com/tmnhs/crony/common/pkg/agentruntime"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/probechannel"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// admin 侧的 Agent 运行时（控制面单例）。
//
// 为什么交互式 Agent 能力放在 admin 而不是节点：
//   - 模型凭据属于控制面，不应下发到每个节点；
//   - 自然语言建任务是"改配置"，属于控制面职责（节点只按 etcd 指令执行）；
//   - 失败诊断的证据（job_run / job_run_step / job_log）都在 MySQL，admin 直接可读。
//
// 诚实边界：admin **读不到节点本机文件日志**，因此 admin 侧诊断的证据来源只有
// 数据库（+etcd 过程记录）。节点本机日志仅在"节点侧 Agent 型任务"里可用。
// 这是阶段 2 的已知限制，已写入 PHASE2 文档 §7；阶段 3 通过 admin→node 内部证据通道消除。
var (
	agentRuntimeMu sync.RWMutex
	agentRuntime   *agentruntime.Runtime
)

// InitAgentRuntime 按配置装配 Agent 运行时（幂等；未配置/未启用时返回 nil 且不报错）。
//
// 返回 (nil, nil) 表示"Agent 能力未开启"，这是合法状态：接口层据此返回 1007/1011，
// 而不是让整个 admin 启动失败。
func InitAgentRuntime() (*agentruntime.Runtime, error) {
	cfg := config.GetConfigModels()
	if cfg == nil {
		return nil, fmt.Errorf("配置未初始化")
	}
	if !agentconfig.Configured(cfg.LLM) {
		reason := "llm 段缺失或 provider 未知"
		if strings.TrimSpace(cfg.LLM.Provider) != "" && !cfg.LLM.Enabled {
			reason = "llm.enabled=false（Agent 能力已显式关闭）"
		}
		logger.GetLogger().Warn(fmt.Sprintf("[agent] 未启用 Agent 能力：%s。相关接口将返回 1007/1011。", reason))
		return nil, nil
	}

	// 阶段 4：工具策略来自配置（`tools` 段），不再用硬编码默认值。
	// 配置非法/自相矛盾时**显式失败**：静默退回默认值会让"我明明配了"变成查不出的谜。
	toolsPolicy, perr := agentconfig.ToolsPolicyFromConfig(cfg.Tools)
	if perr != nil {
		return nil, fmt.Errorf("tools 配置非法: %w", perr)
	}

	// 阶段 3 第二段：节点只读探测通道的数据源。
	// 拿不到数据库连接时**明确降级**（不注册 node_probe），而不是注册一个
	// "调用永远失败"的工具——后者会让模型以为"探测过了，什么都没发现"。
	probeSource, probeErr := probechannel.DefaultSource()
	if probeErr != nil {
		logger.GetLogger().Warn("[agent] 节点只读探测通道未启用（node_probe 工具不注册）: " + probeErr.Error())
		probeSource = nil
	} else {
		// 兜底再校验用同一份策略（否则"收窄了可读路径"的部署仍会被默认策略放行）。
		probeSource.SetPolicy(toolsPolicy)
	}

	rt, err := agentruntime.New(agentruntime.Options{
		Config: cfg.LLM,
		Policy: toolsPolicy,
		Sources: tools.Sources{
			Logs: &agentruntime.DBLogSource{},
			// MaxRows 显式取自同一个策略：此前这里用零值（源内部默认 200），
			// 与网关的 100 行上限并存，形成"两个都说自己是上限"的漂移。
			SQL: &agentruntime.ReadonlySQLSource{MaxRows: toolsPolicy.SQLMaxRows},
			// 阶段 3 第一段：admin→node 证据通道。
			// 节点存活/心跳/主机名/在跑任务数读共享库即有；CPU/内存/磁盘/load 由
			// node 侧写入 node_metric 后才有（未上报时工具显式输出"未上报"，不编造 0%）。
			NodeStatus: &NodeStatusSource{},
			// 阶段 3 第二段：节点**本机事实**只读探测通道（端口/进程/服务/网络配置）。
			// 没有它，模型只能猜"端口未监听"（实测它就是这么猜的）；
			// 有了它，"未观测就断言"才可能被真正消除，而不是只被标记。
			Probe: probeSource,
		},
		// 阶段 4：每次工具调用（含被拒的）都落 agent_tool_audit。
		// 观察者内部对"数据库未就绪/落库失败"只记日志，不影响调用。
		Observer: agentruntime.ToolAuditObserver(),
	})
	if err != nil {
		return nil, err
	}
	SetAgentRuntime(rt)
	logger.GetLogger().Info(fmt.Sprintf(
		"[agent] Agent 运行时已就绪：provider=%s offline=%v model=%s 工具=%s（诊断证据来源：数据库 + 节点水位 + 节点本机只读探测）",
		rt.ProviderName(), rt.Offline(), rt.Model(), strings.Join(rt.ToolNames(), ",")))
	// 生效策略必须打日志：安全边界的"可读性"和正确性一样重要——
	// 出事后第一件事就是确认"当时到底放开了什么"。
	logger.GetLogger().Info(fmt.Sprintf(
		"[agent] 工具策略：自治级别=%s 允许写=%v 允许不可逆=%v 会话调用上限=%d 每分钟上限=%d 并发上限=%d 写动作上限=%d 影响主机上限=%d 免审批白名单=%v",
		toolsPolicy.Autonomy, toolsPolicy.AllowWrite, toolsPolicy.AllowDestructive,
		toolsPolicy.MaxCallsPerSession, toolsPolicy.MaxCallsPerMinute, toolsPolicy.MaxConcurrentCalls,
		toolsPolicy.MaxWriteActionsPerSession, toolsPolicy.MaxAffectedHostsPerSession, toolsPolicy.AutoApprove))
	return rt, nil
}

// SetAgentRuntime 注入运行时（供测试与启动流程使用）。
func SetAgentRuntime(rt *agentruntime.Runtime) {
	agentRuntimeMu.Lock()
	defer agentRuntimeMu.Unlock()
	agentRuntime = rt
}

// DefaultAgentRuntime 返回当前运行时（可能为 nil，调用方必须处理）。
func DefaultAgentRuntime() *agentruntime.Runtime {
	agentRuntimeMu.RLock()
	defer agentRuntimeMu.RUnlock()
	return agentRuntime
}
