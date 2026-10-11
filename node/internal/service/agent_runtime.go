package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agentconfig"
	"github.com/tmnhs/crony/common/pkg/agentruntime"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/executor"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/tools"
	"github.com/tmnhs/crony/node/internal/handler"
)

// initAgentRuntime 装配节点侧 Agent 运行时并注入步骤执行器。
//
// 与 admin 的差异只有数据源：节点额外能读**本机日志文件**（崩溃栈、调度器日志都在盘上，
// 数据库里没有）。工具策略、prompt、Schema、预算、落库全部与 admin 共用同一份实现。
//
// 未配置 llm（或 enabled=false）时不注入运行器：Agent 型任务的步骤会以
// "本节点未注入 Agent 运行时"明确失败，而不是静默跳过或假装成功。
func (srv *NodeServer) initAgentRuntime() {
	cfg := config.GetConfigModels()
	if cfg == nil {
		return
	}
	if !agentconfig.Configured(cfg.LLM) {
		logger.GetLogger().Warn("[agent] 未配置 llm（llm.enabled/llm.provider 为空）：Agent 型任务与 AI 诊断在本节点不可用。")
		return
	}

	roots := make([]string, 0, 2)
	if dir := strings.TrimSpace(cfg.Log.Director); dir != "" {
		roots = append(roots, dir)
	}

	rt, err := agentruntime.New(agentruntime.Options{
		Config: cfg.LLM,
		Policy: tools.DefaultPolicy(),
		Sources: tools.Sources{
			Logs: agentruntime.MultiLogSource{
				&agentruntime.DBLogSource{},
				&FileLogSource{Roots: roots},
			},
			SQL: &agentruntime.ReadonlySQLSource{},
		},
		NodeUUID: srv.UUID,
	})
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[agent] 装配 Agent 运行时失败，Agent 型任务不可用：%s", err.Error()))
		return
	}

	handler.SetAgentRunner(&nodeAgentRunner{rt: rt})
	logger.GetLogger().Info(fmt.Sprintf("[agent] Agent 运行时已就绪：provider=%s offline=%v 工具=%s",
		rt.ProviderName(), rt.Offline(), strings.Join(rt.ToolNames(), ",")))
}

// nodeAgentRunner 把共享运行时适配成 executor.AgentRunner。
type nodeAgentRunner struct {
	rt *agentruntime.Runtime
}

// RunAgent 执行一次 Agent 型任务（当前只有"只读巡检"场景）。
//
// 说明：Agent 型任务的定位是**只读巡检**，不是"自动干活"。因此这里只走 inspect 场景；
// 需要执行动作的场景（自愈）属于阶段 4，必须经过审批与沙箱，不会从这里偷偷放行。
func (a *nodeAgentRunner) RunAgent(ctx context.Context, req executor.Request) executor.AgentOutcome {
	res := executor.AgentOutcome{Artifacts: map[string]string{}}

	kind := strings.TrimSpace(req.AgentKind)
	if kind == "" {
		kind = models.AgentKindJob
	}
	if kind != models.AgentKindJob && kind != "inspect" {
		res.Err = fmt.Errorf("节点侧不支持 Agent 场景 %q（仅支持只读巡检 %q）", kind, models.AgentKindJob)
		res.Output = "Agent 场景不支持：" + kind
		return res
	}

	sessionID := models.NewAgentSessionID(fmt.Sprintf("job%d", req.JobID), time.Now())
	out, err := a.rt.RunInspect(ctx, agentruntime.InspectRequest{
		SessionID: sessionID,
		JobID:     req.JobID,
		JobName:   req.Name,
		Goal:      req.AgentGoal,
		Context:   req.AgentContext,
	})
	if out == nil {
		res.Err = err
		res.Output = fmt.Sprintf("巡检未执行：%v", err)
		return res
	}

	res.SessionID = out.SessionID
	res.Output = out.Explain
	if out.Inspection != nil {
		res.StopReason = out.Inspection.StopReason
		res.Artifacts["provider"] = out.Inspection.Provider
		res.Artifacts["model"] = out.Inspection.Model
		res.Artifacts["prompt_version"] = out.Inspection.PromptVersion
		res.Artifacts["tool_calls"] = fmt.Sprintf("%d", out.Inspection.ToolCalls)
		res.Artifacts["total_tokens"] = fmt.Sprintf("%d", out.Inspection.TotalTokens)
	}
	if out.PersistError != "" {
		res.Artifacts["persist_error"] = llm.Truncate(out.PersistError, 500)
	}
	res.Err = err
	return res
}

var _ executor.AgentRunner = (*nodeAgentRunner)(nil)
