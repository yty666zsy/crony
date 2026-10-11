package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// AgentRunner 由 node 侧注入的 Agent 运行时。
//
// 为什么用接口注入而不是让 executor 直接依赖 agent/llm：
//   - executor 是"步骤执行"的通用抽象，不该知道 ReAct/提示词/证据这些概念；
//   - 注入后，编排、重试、取消、落库这些既有能力对 Agent 步骤天然生效，
//     不需要为 Agent 单独写一套调度；
//   - 测试里可以注入确定性假运行时，编排逻辑照旧可测（不依赖模型）。
type AgentRunner interface {
	// RunAgent 执行一次 Agent 会话。约定：不 panic；失败表达在 AgentOutcome.Err 里。
	RunAgent(ctx context.Context, req Request) AgentOutcome
}

// AgentOutcome 一次 Agent 会话对上层可见的结果。
type AgentOutcome struct {
	// SessionID 会话 ID（轨迹在 agent_session/agent_step 表里，前端按它回放）。
	SessionID string
	// StopReason Agent 循环的终止原因（completed/max_steps/tool_budget/…）。
	StopReason string
	// Output 人话结果，落 job_run_step.output（调用方负责截断）。
	Output string
	// Artifacts 结构化产物（模型/provider/用量/工具次数等），落 job_run_step.artifacts。
	Artifacts map[string]string
	Err       error
}

// AgentExecutor 把 Agent 会话包装成普通步骤执行器。
type AgentExecutor struct {
	runner AgentRunner
}

func NewAgentExecutor(runner AgentRunner) *AgentExecutor { return &AgentExecutor{runner: runner} }

func (e *AgentExecutor) Kind() Kind { return KindAgent }

// Validate 在执行前拒绝脏请求：目标为空、运行器未注入都要尽早失败。
func (e *AgentExecutor) Validate(req Request) error {
	if strings.TrimSpace(req.AgentGoal) == "" {
		return retry.Fatal(fmt.Errorf("%w: agent 目标（goal）不能为空", ErrInvalidRequest))
	}
	if e.runner == nil {
		return retry.Fatal(fmt.Errorf("%w: 本节点未注入 Agent 运行时（检查 llm 配置）", ErrInvalidRequest))
	}
	return nil
}

// Execute 执行一次 Agent 会话。
//
// ⚠️ 重试语义与 shell/http 步骤**刻意不同**：
// Agent 步骤只有在"模型调用本身是可重试故障"（限流、超时）时才允许自动重试；
// 其余失败（结构不合规、步数/预算耗尽、无 provider、目标写错）一律 Fatal。
// 理由：一次 Agent 会话要消耗真实 token，把"提示词没写好"这类确定性失败
// 按任务重试策略重跑 N 次，等于按次烧钱且不会变好；这类失败应当由人来修配置。
func (e *AgentExecutor) Execute(ctx context.Context, req Request) Result {
	res := newResult(req)
	out := e.runner.RunAgent(ctx, req)
	// 结构化产物先落，成功失败都保留（排查"为什么没结论"靠它）
	if out.Artifacts == nil {
		out.Artifacts = map[string]string{}
	}
	if out.SessionID != "" {
		out.Artifacts["agent_session_id"] = out.SessionID
	}
	if out.StopReason != "" {
		out.Artifacts["stop_reason"] = out.StopReason
	}
	res.Artifacts = out.Artifacts
	// Agent 结论可能很长（巡检报告），与 shell/http 一样按 OutputLimit 截断并标记，
	// 保证 job_run_step.output 不会因为一次 Agent 会话被撑爆。
	res.Output, res.Truncated = truncateOutput(out.Output, req.OutputLimit())
	if out.Err == nil {
		res.ExitCode = 0
		return res.finish(models.RunStatusSuccess, nil)
	}
	res.ExitCode = 1
	return res.finish(models.RunStatusFailed, classifyAgentError(out.Err))
}

// classifyAgentError 决定 Agent 失败能不能自动重试。
func classifyAgentError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	// 取消：绝不重试（与阶段 1 的取消语义一致）。
	case errors.Is(err, retry.ErrCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return retry.Fatal(err)
	// 模型侧瞬时故障：允许按步骤重试策略重试（有次数上限）。
	case errors.Is(err, llm.ErrRateLimited), errors.Is(err, llm.ErrTimeout):
		return err
	// 其余一律 Fatal。
	default:
		return retry.Fatal(err)
	}
}

var _ Executor = (*AgentExecutor)(nil)

// truncateOutput 按字节上限截断输出。
//
// 刻意在字节层截断而不是按 rune：上限的目的是限制**存储与传输量**，
// 按 rune 截断会让中文输出占三倍字节。截断点可能切到多字节字符中间，
// 这里向前回退到合法边界，避免落库出现非法 UTF-8。
func truncateOutput(s string, limit int) (string, bool) {
	if limit <= 0 {
		limit = DefaultOutputLimit
	}
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…（输出已截断）", true
}
