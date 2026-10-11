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

	// 阶段 3 第二段：节点只读探测通道的数据源。
	// 拿不到数据库连接时**明确降级**（不注册 node_probe），而不是注册一个
	// "调用永远失败"的工具——后者会让模型以为"探测过了，什么都没发现"。
	probeSource, probeErr := probechannel.DefaultSource()
	if probeErr != nil {
		logger.GetLogger().Warn("[agent] 节点只读探测通道未启用（node_probe 工具不注册）: " + probeErr.Error())
		probeSource = nil
	}

	rt, err := agentruntime.New(agentruntime.Options{
		Config: cfg.LLM,
		Policy: tools.DefaultPolicy(),
		Sources: tools.Sources{
			Logs: &agentruntime.DBLogSource{},
			SQL:  &agentruntime.ReadonlySQLSource{},
			// 阶段 3 第一段：admin→node 证据通道。
			// 节点存活/心跳/主机名/在跑任务数读共享库即有；CPU/内存/磁盘/load 由
			// node 侧写入 node_metric 后才有（未上报时工具显式输出"未上报"，不编造 0%）。
			NodeStatus: &NodeStatusSource{},
			// 阶段 3 第二段：节点**本机事实**只读探测通道（端口/进程/服务/网络配置）。
			// 没有它，模型只能猜"端口未监听"（实测它就是这么猜的）；
			// 有了它，"未观测就断言"才可能被真正消除，而不是只被标记。
			Probe: probeSource,
		},
	})
	if err != nil {
		return nil, err
	}
	SetAgentRuntime(rt)
	logger.GetLogger().Info(fmt.Sprintf(
		"[agent] Agent 运行时已就绪：provider=%s offline=%v model=%s 工具=%s（诊断证据来源：数据库 + 节点水位 + 节点本机只读探测）",
		rt.ProviderName(), rt.Offline(), rt.Model(), strings.Join(rt.ToolNames(), ",")))
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
