package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
)

// 本文件是阶段 4 的**写工具**（方案 §6 阶段 4）。
//
// 与只读工具集的三条硬性差异（不是风格选择，是安全边界）：
//
//  1. `Risk()` 必须声明 write/destructive，且 `ReadOnly()` 必须为 false
//     （注册期就校验二者自洽，见 Registry.Register）；
//  2. 必须实现 `TargetProvider`——爆炸半径预算要有"影响谁"这个输入，
//     没有目标的写动作根本无法被限制；
//  3. 默认**不会执行**：注册表在 `AllowWrite=true` 且未被 `auto-approve`
//     命中时，先把调用转成审批单，等人工批准后由控制面执行（见 Registry.Call）。
//
// 另外，写工具的执行实现**不在本包**：本包不依赖数据库与调度器，只定义接口。
// 这样网关的全部闸门都能用纯内存单测覆盖，而"审批单/重跑真的落库了吗"
// 由 admin 侧用真库回答（这是阶段 3 就定下的分层）。

// RetryRequest 一次"重跑任务"的请求。
//
// 为什么把 Reason 一路带下来：审批人批的是"为什么重跑"，不是"重跑 job 21"。
// 这条理由在审批单里给人看，也在执行后的审计行里留痕。
type RetryRequest struct {
	// JobID 目标任务 ID（crony 的 job.id）。
	JobID int
	// RunID 触发本次重跑的场景标识（诊断会话通常指向某次失败的 run）。
	RunID string
	// Reason 模型自述的重跑理由（透传到审批单与审计）。
	Reason string
	// MaxAttempts 期望的最大尝试次数（<=0 时由执行方按默认值处理）。
	MaxAttempts int
}

// RetryExecutor 写动作的执行方。
//
// 契约（执行方必须自己保证，tools 包无法替它保证）：
//   - 幂等：同一个审批单最多执行一次，重复调用不得产生第二次副作用；
//   - 可回滚语义：本工具是"可逆写"（重跑不会删除数据），执行失败必须
//     返回 `Result{OK:false, Err:...}` 而不是 error（错误会让模型反复重试）；
//   - 不得自行扩权：执行方不能再调用其它写工具（审计里会出现"嵌套写"）。
type RetryExecutor interface {
	RetryJob(ctx context.Context, req RetryRequest) (Result, error)
}

// JobRetryTool `job_retry`：重跑一个失败的任务（可逆写）。
type JobRetryTool struct {
	exec    RetryExecutor
	timeout time.Duration
}

// NewJobRetryTool 创建写工具。exec 为 nil 时工具仍可注册，但调用会被拒绝
// （"注册了却没人能执行"必须是显式失败，而不是静默成功）。
func NewJobRetryTool(exec RetryExecutor) *JobRetryTool {
	return &JobRetryTool{exec: exec, timeout: 30 * time.Second}
}

// Name 实现 Tool。
func (t *JobRetryTool) Name() string { return "job_retry" }

// Description 实现 Tool。
func (t *JobRetryTool) Description() string {
	return "重跑一个失败的任务（写动作，可逆）。" +
		"这是写动作：默认不会立即执行，会先生成一张审批单，人工批准后才会真正触发重跑；" +
		"调用成功后要如实写\"已提交人工审批\"，不要写成已执行。"
}

// Schema 实现 Tool。
func (t *JobRetryTool) Schema() *jsonschema.Schema {
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"job_id":       jsonschema.Integer("要重跑的任务 ID（必填）").WithRange(1, 1e9),
		"reason":       jsonschema.String("为什么要重跑（必填，写进审批单给人看）").WithLength(4, 500),
		"run_id":       jsonschema.String("触发场景的运行标识（可选，便于审计追溯）").WithLength(0, 128),
		"max_attempts": jsonschema.Integer("期望的最大尝试次数（可选，1~5）").WithRange(1, 5),
	}, "job_id", "reason")
}

// ReadOnly 实现 Tool：写工具必须为 false（与 Risk()=write 自洽）。
func (t *JobRetryTool) ReadOnly() bool { return false }

// Risk 实现 Tool：重跑是可逆写（不删除数据、不改配置），因此不是 destructive。
// 一旦将来出现"改调度表达式/删任务"这类动作，必须声明 destructive 走 allow_destructive 开关。
func (t *JobRetryTool) Risk() RiskLevel { return RiskWrite }

// Timeout 实现 Tool：重跑要走一次调度投递，给 30s。
func (t *JobRetryTool) Timeout() time.Duration { return t.timeout }

// Targets 实现 TargetProvider：本次动作影响的目标。
// 输出形如 `job:21`，与 impactSummary 共用同一口径（爆炸半径按主机/任务两类目标计数）。
func (t *JobRetryTool) Targets(args map[string]interface{}) []string {
	return []string{fmt.Sprintf("job:%s", numericText(args["job_id"]))}
}

// Execute 实现 Tool。
//
// 本方法被两类调用方复用：
//   - 网关在"自动放行"（L2/L3 白名单）或"审批已通过"时代为执行；
//   - admin 侧的审批执行器在 CAS 抢到"待执行"后调用。
func (t *JobRetryTool) Execute(ctx context.Context, args map[string]interface{}) (Result, error) {
	if t.exec == nil {
		return Result{}, fmt.Errorf("%w: job_retry 没有装配执行器（admin 侧未接入重跑实现）", ErrToolDenied)
	}
	jobID, err := intArg(args, "job_id")
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidArgs, err)
	}
	reason := strings.TrimSpace(llm.StringOr(args, "reason", ""))
	if reason == "" {
		return Result{}, fmt.Errorf("%w: reason 必填（没有理由的写动作不允许执行）", ErrInvalidArgs)
	}
	req := RetryRequest{
		JobID:       jobID,
		RunID:       strings.TrimSpace(llm.StringOr(args, "run_id", "")),
		Reason:      reason,
		MaxAttempts: intOrZero(args["max_attempts"]),
	}
	res, err := t.exec.RetryJob(ctx, req)
	if err != nil {
		// 执行方返回 error 视为"基础设施故障"：转成失败观测，避免上层把它当崩溃。
		return Result{OK: false, Err: llm.Redact(err.Error())}, nil
	}
	if res.Summary == "" && res.OK {
		res.Summary = fmt.Sprintf("已触发任务 %d 的重跑", jobID)
	}
	return res, nil
}

// intArg 取必填整数参数（JSON 解码后是 float64，这里统一口径）。
//
// 为什么不用 llm.FloatOr：它会把字符串 "21" 也解析成 21，而写动作的参数
// 类型必须是**声明即所得**——模型传字符串说明它没按 Schema 来，宁可拒绝，
// 也不要"宽容地猜它的意思"（宽容解析是写动作里最危险的一类便利）。
func intArg(args map[string]interface{}, key string) (int, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("%s 必填", key)
	}
	f, ok := argNumber(v)
	if !ok {
		return 0, fmt.Errorf("%s 必须是整数", key)
	}
	if f != float64(int64(f)) {
		return 0, fmt.Errorf("%s 必须是整数（收到 %v）", key, v)
	}
	return int(f), nil
}

// intOrZero 取可选整数参数，缺失或非法一律按 0（由执行方兜底默认值）。
func intOrZero(v interface{}) int {
	f, ok := argNumber(v)
	if !ok {
		return 0
	}
	return int(f)
}

// argNumber 严格取数值（只接受 JSON 能产生的数值类型）。
func argNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// numericText 把参数渲染成稳定的文本（用于 Targets/Impact 的展示，避免 float64 的 `21` 变成 `21`）。
func numericText(v interface{}) string {
	if f, ok := argNumber(v); ok {
		if f == float64(int64(f)) {
			return fmt.Sprintf("%d", int64(f))
		}
		return fmt.Sprintf("%g", f)
	}
	return fmt.Sprintf("%v", v)
}

// ---------------------------------------------------------------------------
// 装配
// ---------------------------------------------------------------------------

// WriteSources 写工具的执行实现。
//
// 与只读源（Sources）分开是刻意的：只读能力面是阶段 2 的既有承诺，
// 写能力的装配必须是一次**显式**决定；把两者塞进同一个结构体，
// 很容易在"只读部署"里因为一个字段被填上就悄悄长出写工具。
type WriteSources struct {
	// Retry 重跑执行器。为 nil 时不注册 job_retry。
	Retry RetryExecutor
	// Rollout 集群滚动重启执行器（阶段 K3）。为 nil 时不注册 k8s_rollout_restart。
	Rollout K8sRolloutExecutor
}

// RegisterWriteTools 按策略注册写工具，返回实际注册的工具名（供启动日志展示）。
//
// 三种"不注册"的情形都是**静默跳过**而不是报错，理由各不相同：
//   - `!policy.AllowWrite`：部署方明确要求只读，这不是错误；
//   - 对应执行器为 nil：该部署没有接入写能力（例如 node 侧）；
//   - `policy.Autonomy` 不允许写：策略层已经拒绝，注册了也只会被网关拒。
//
// 与之相对，**注册失败**（Schema 非法、名字重复、缺 TargetProvider）一律返回错误，
// 因为那是代码缺陷，必须在启动时炸出来。
func RegisterWriteTools(r *Registry, src WriteSources) ([]string, error) {
	if r == nil {
		return nil, fmt.Errorf("tools: 注册表未初始化")
	}
	policy := r.Policy()
	if !policy.AllowWrite || !policy.Autonomy.AllowsWriteTools() {
		return nil, nil
	}
	registered := make([]string, 0, 2)
	if src.Retry != nil {
		tool := NewJobRetryTool(src.Retry)
		if err := r.Register(tool); err != nil {
			return registered, err
		}
		registered = append(registered, tool.Name())
	}
	// 阶段 K3：集群写动作。只有在"接入集群"的部署里 src.Rollout 才非 nil，
	// 因此没配集群的部署**看不到**这个工具（不是"看得到但调用失败"）。
	if src.Rollout != nil {
		tool := NewK8sRolloutTool(src.Rollout)
		if err := r.Register(tool); err != nil {
			return registered, err
		}
		registered = append(registered, tool.Name())
	}
	return registered, nil
}
