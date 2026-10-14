package service

import (
	"fmt"
	"strings"
	"sync"

	"github.com/tmnhs/crony/common/pkg/agentconfig"
	"github.com/tmnhs/crony/common/pkg/agentruntime"
	"github.com/tmnhs/crony/common/pkg/autonomy"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/k8sclient"
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
	// approvalService 审批控制面单例（未启用 Agent 能力时为 nil）。
	approvalMu      sync.RWMutex
	approvalService *ApprovalService
	// memoryService 经验库控制面单例（未启用 Agent 能力时为 nil）。
	memoryMu      sync.RWMutex
	memoryService *MemoryService
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

	// 阶段 K2：集群只读证据通道。
	//
	// 未配置集群（k8s.enabled=false）或 kubeconfig 不可用时**不注册** k8s_readonly：
	// 注册一个"调用必失败"的工具会让模型以为"查过集群了，什么都没发现"，
	// 那比"我知道我没有这个能力"更危险。
	k8sClient, k8sErr := k8sclient.FromConfig(cfg.K8s)
	if k8sErr != nil {
		logger.GetLogger().Warn("[agent] 集群证据通道未启用（k8s_readonly 工具不注册）: " + k8sErr.Error())
		k8sClient = nil
	}
	k8sSource := NewK8sReadSource(k8sClient)

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
			// 阶段 K2：集群证据（Pod/事件/容器日志/Deployment/节点）。
			K8s: k8sSource,
		},
		// 阶段 4：每次工具调用（含被拒的）都落 agent_tool_audit。
		// 观察者内部对"数据库未就绪/落库失败"只记日志，不影响调用。
		Observer: agentruntime.ToolAuditObserver(),
	})
	if err != nil {
		return nil, err
	}
	SetAgentRuntime(rt)

	// 阶段 4.2：把写动作能力面接上去（审批通道 + 重跑执行器）。
	//
	// 顺序说明（这是一个环，必须分两步装配）：
	//  1. 先建运行时（此时只有只读能力面，写工具**不存在**，模型看不到）；
	//  2. 用"已建好的运行时"当执行方建审批服务（审批执行必须经它走工具网关）；
	//  3. 把审批服务当审批通道接回运行时，此时才按策略决定是否注册写工具。
	// 这样"写工具在接入前不存在"与"策略没开就不注册"两条都成立。
	approvalStore := NewGormApprovalStore()
	approvalService := NewApprovalService(approvalStore, rt, toolsPolicy)
	SetApprovalService(approvalService)
	writeNames, werr := rt.ConfigureWrites(approvalService, tools.WriteSources{
		Retry: NewJobRetryExecutor(),
		// 阶段 K3：集群写动作（滚动重启）。集群未接入时为 nil → 工具不注册。
		Rollout: NewK8sRolloutExecutor(k8sClient),
	})
	if werr != nil {
		// 写工具装配失败不拖垮整个 Agent 能力（只读诊断仍然可用），但必须显式告警：
		// 静默降级会让"我明明开了 allow_write"变成一个查不出的谜。
		logger.GetLogger().Error("[agent] 写工具装配失败（写动作不可用，只读能力不受影响）: " + werr.Error())
		writeNames = nil
	}

	// 阶段 5：把「自治信用」与「放量档位」接进放行判定（两个开关都默认关闭）。
	//
	// 开启后：写动作的实际生效级别 = min(配置自治级别, 信用级别, 放量档位)。
	// 信用与实际都只能**收紧**，不能越权升过配置上限；两者都不表态时沿用配置。
	// 默认关闭 = 与阶段 4 行为完全一致（零回归）。
	creditOn, rolloutOn := cfg.Tools.AutonomyCredit, cfg.Tools.RolloutGate
	if creditOn || rolloutOn {
		rt.SetAutonomyResolver(RollingGateResolver(cfg.Tools.AutonomyEnv, creditOn, rolloutOn))
		if creditOn {
			// 阶段 5 补全：把**结果回写**也接上，让信用自己会动（而不是等人喂数据）。
			//
			// 只在真正执行过的写动作后回写：成功累积、失败降级冻结（判定在信用内核里）。
			// 与解析器共用同一个环境段（ConfiguredAutonomyEnv），避免"放行按 acc、回写按 prod"两套口径。
			rt.SetAutonomyOutcomeRecorder(func(tool string, success bool) {
				scope := AutonomyScope(tool, ConfiguredAutonomyEnv())
				row, err := DefaultAutonomyService.ObserveAutonomy(scope, success)
				if err != nil {
					logger.GetLogger().Warn(fmt.Sprintf("[agent] 自治信用回写失败 scope=%s success=%v: %v", scope, success, err))
					return
				}
				logger.GetLogger().Info(fmt.Sprintf(
					"[agent] 自治信用回写 scope=%s success=%v -> 等级=%s 连成=%d 冻结=%v",
					scope, success, autonomy.Level(row.Level).Name(), row.Streak, row.Frozen))
			})
		}
		logger.GetLogger().Info(fmt.Sprintf(
			"[agent] 放行判定已接入：环境段=%q；配置上限=%s；自治信用=%v；放量档位=%v（三者取最严，均不可越权升过配置；无表态则沿用配置）",
			ConfiguredAutonomyEnv(), toolsPolicy.Autonomy, creditOn, rolloutOn))
	} else {
		logger.GetLogger().Info("[agent] 放行判定按配置的自治级别（tools.autonomy-credit=false 且 tools.rollout-gate=false）")
	}
	logger.GetLogger().Info(fmt.Sprintf(
		"[agent] Agent 运行时已就绪：provider=%s offline=%v model=%s 工具=%s（诊断证据来源：数据库 + 节点水位 + 节点本机只读探测）",
		rt.ProviderName(), rt.Offline(), rt.Model(), strings.Join(rt.ToolNames(), ",")))
	if len(writeNames) > 0 {
		logger.GetLogger().Info(fmt.Sprintf("[agent] 写动作能力已开启：%s（每次写动作都会生成审批单，人批准后才执行）",
			strings.Join(writeNames, ",")))
	} else {
		logger.GetLogger().Info("[agent] 写动作能力未开启（allow_write=false 或自治级别不允许或缺少执行器）：模型只能建议，不能动手")
	}
	// 生效策略必须打日志：安全边界的"可读性"和正确性一样重要——
	// 出事后第一件事就是确认"当时到底放开了什么"。
	logger.GetLogger().Info(fmt.Sprintf(
		"[agent] 工具策略：自治级别=%s 允许写=%v 允许不可逆=%v 会话调用上限=%d 每分钟上限=%d 并发上限=%d 写动作上限=%d 影响主机上限=%d 免审批白名单=%v",
		toolsPolicy.Autonomy, toolsPolicy.AllowWrite, toolsPolicy.AllowDestructive,
		toolsPolicy.MaxCallsPerSession, toolsPolicy.MaxCallsPerMinute, toolsPolicy.MaxConcurrentCalls,
		toolsPolicy.MaxWriteActionsPerSession, toolsPolicy.MaxAffectedHostsPerSession, toolsPolicy.AutoApprove))
	// 阶段 4-A：经验库（沉淀 + 召回）接线。
	//
	// 为什么在同一处接线：沉淀要读会话与审计（运行时产生），召回要写进 RunContext（运行时消费），
	// 而两者都要访问 MySQL——只有装配层同时具备"运行时实例"与"数据库"。
	// 经验库接线失败**不影响** Agent 主能力：诊断照常，只是不带历史经验。
	memoryService := NewMemoryService(NewGormMemoryStore())
	SetMemoryService(memoryService)
	rt.SetMemoryRecorder(memoryService)
	rt.SetMemoryRecaller(memoryService, 0)
	logger.GetLogger().Info(fmt.Sprintf(
		"[agent] 经验库已接线：沉淀=%v 召回=%v（召回只是线索，注入提示词时会显式标注「不是证据」）",
		rt.MemoryEnabled(), rt.RecallEnabled()))
	return rt, nil
}

// SetMemoryService 注入经验库控制面（启动流程与测试用）。
func SetMemoryService(s *MemoryService) {
	memoryMu.Lock()
	defer memoryMu.Unlock()
	memoryService = s
}

// DefaultMemoryService 返回经验库控制面（可能为 nil，调用方必须处理）。
func DefaultMemoryService() *MemoryService {
	memoryMu.RLock()
	defer memoryMu.RUnlock()
	return memoryService
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

// SetApprovalService 注入审批控制面（启动流程与测试用）。
func SetApprovalService(s *ApprovalService) {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	approvalService = s
}

// DefaultApprovalService 返回审批控制面（可能为 nil，调用方必须处理）。
func DefaultApprovalService() *ApprovalService {
	approvalMu.RLock()
	defer approvalMu.RUnlock()
	return approvalService
}
