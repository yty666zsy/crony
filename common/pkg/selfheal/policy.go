package selfheal

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
)

// Policy 自愈策略（安全边界）。
//
// 每一个字段都是一道闸门，而不是"配置项"。默认值的原则只有一条：**默认关上**。
// 升级一套已经在跑的系统时，新能力不应该是"自动生效"，而应该是"运维显式打开"。
type Policy struct {
	// Enabled 自愈开关。false 时扫描照常（能把失败分类并记录），但任何动作都不执行。
	Enabled bool `json:"enabled"`
	// MaxAttemptsPerJobWindow 同一任务在 Window 内允许的自愈尝试次数上限。
	MaxAttemptsPerJobWindow int `json:"max_attempts_per_job_window"`
	// Window 尝试次数统计窗口。
	WindowSeconds int64 `json:"window_seconds"`
	// Cooldown 同一任务两次自愈之间的最小间隔（防止"失败→重跑→再失败"打成死循环）。
	CooldownSeconds int64 `json:"cooldown_seconds"`
	// ExtendTimeoutFactor 超时自愈时超时上限的放大倍数（在 MaxExtendTimeoutSeconds 内生效）。
	ExtendTimeoutFactor float64 `json:"extend_timeout_factor"`
	// MaxExtendTimeoutSeconds 单次自愈允许把超时上限调到多大（绝对上限，秒）。
	MaxExtendTimeoutSeconds int64 `json:"max_extend_timeout_seconds"`
	// MaxConcurrent 同时在"待观测"状态的自愈数量上限（防止一次大面积故障把所有任务各重跑一遍）。
	MaxConcurrent int `json:"max_concurrent"`
	// ObserveWindowSeconds 自愈执行后等待"后续执行"出现的观测窗口。
	ObserveWindowSeconds int64 `json:"observe_window_seconds"`
	// ScanLimit 单次扫描最多处理多少条失败执行（防止一次扫全表把控制面拖住）。
	ScanLimit int `json:"scan_limit"`
	// LookbackSeconds 只扫描这个时间窗内结束的失败执行。
	LookbackSeconds int64 `json:"lookback_seconds"`
}

// DefaultPolicy 返回保守的默认策略：**关着**，并且即使打开也限得很紧。
//
// 为什么默认关：自愈会**写**（重跑任务、改超时）。默认开等于"升级即自动动手"，
// 这在真实生产里是不可接受的——没有人希望改完一个版本回家发现系统自己重跑了一夜任务。
func DefaultPolicy() Policy {
	return Policy{
		Enabled:                 false,
		MaxAttemptsPerJobWindow: 3,
		WindowSeconds:           1800,
		CooldownSeconds:         300,
		ExtendTimeoutFactor:     2,
		MaxExtendTimeoutSeconds: 3600,
		MaxConcurrent:           2,
		ObserveWindowSeconds:    900,
		ScanLimit:               50,
		LookbackSeconds:         1800,
	}
}

// Window 返回统计窗口时长。
func (p Policy) Window() time.Duration { return time.Duration(p.WindowSeconds) * time.Second }

// Cooldown 返回冷却时长。
func (p Policy) Cooldown() time.Duration { return time.Duration(p.CooldownSeconds) * time.Second }

// ObserveWindow 返回观测窗口时长。
func (p Policy) ObserveWindow() time.Duration {
	return time.Duration(p.ObserveWindowSeconds) * time.Second
}

// Lookback 返回回看时长。
func (p Policy) Lookback() time.Duration { return time.Duration(p.LookbackSeconds) * time.Second }

// Normalize 修正非法/缺失的策略取值（配置写错时**用默认值兜住**，而不是让上限变成 0）。
//
// 为什么不让 0 生效：0 在语义上会被读成"一次都不许"或"窗口为 0"，两种都不是运维想要的。
// 用默认值兜住并且不影响 Enabled（保持显式开关语义），是这里唯一的正确取舍。
func (p Policy) Normalize() Policy {
	d := DefaultPolicy()
	if p.MaxAttemptsPerJobWindow <= 0 {
		p.MaxAttemptsPerJobWindow = d.MaxAttemptsPerJobWindow
	}
	if p.WindowSeconds <= 0 {
		p.WindowSeconds = d.WindowSeconds
	}
	if p.CooldownSeconds < 0 {
		p.CooldownSeconds = d.CooldownSeconds
	}
	if p.ExtendTimeoutFactor <= 1 {
		p.ExtendTimeoutFactor = d.ExtendTimeoutFactor
	}
	if p.MaxExtendTimeoutSeconds <= 0 {
		p.MaxExtendTimeoutSeconds = d.MaxExtendTimeoutSeconds
	}
	if p.MaxConcurrent <= 0 {
		p.MaxConcurrent = d.MaxConcurrent
	}
	if p.ObserveWindowSeconds <= 0 {
		p.ObserveWindowSeconds = d.ObserveWindowSeconds
	}
	if p.ScanLimit <= 0 {
		p.ScanLimit = d.ScanLimit
	}
	if p.LookbackSeconds <= 0 {
		p.LookbackSeconds = d.LookbackSeconds
	}
	return p
}

// CapRule 命中的闸门名（落进 job_heal.policy_json，供事后复盘"当时被哪条挡住"）。
type CapRule string

const (
	// CapDisabled 自愈总开关关闭。
	CapDisabled CapRule = "self_heal_disabled"
	// CapJobOptIn 该任务没有显式开启 self_heal。
	CapJobOptIn CapRule = "job_not_opted_in"
	// CapWriteOff 写能力未开启（allow_write / 自治级别不允许）：不允许自动动手。
	CapWriteOff CapRule = "write_capability_off"
	// CapAttempts 触达尝试次数上限。
	CapAttempts CapRule = "attempt_cap"
	// CapCooldown 处于冷却期。
	CapCooldown CapRule = "cooldown"
	// CapConcurrency 并发自愈数已达上限。
	CapConcurrency CapRule = "concurrency_cap"
	// CapUnsafeAction 动作本身不安全（例如没有超时可调）。
	CapUnsafeAction CapRule = "unsafe_action"
	// CapAllowed 允许执行。
	CapAllowed CapRule = "allowed"
)

// CapInput 闸门判定输入。
type CapInput struct {
	// JobSelfHeal 任务自身的自愈开关（models.Job.SelfHeal）。
	JobSelfHeal bool
	// WritesEnabled 写能力是否已开启（与工具网关同一判据：写工具是否真的注册上了）。
	WritesEnabled bool
	// History 该任务在窗口内的历史自愈记录（含被拒的：被拒也是"尝试过"）。
	History []*models.JobHeal
	// ActiveCount 当前处于"待观测"状态的自愈数量。
	ActiveCount int
	// Now 判定时刻。
	Now time.Time
}

// CapDecision 闸门判定结果。
type CapDecision struct {
	Allowed bool    `json:"allowed"`
	Rule    CapRule `json:"rule"`
	Reason  string  `json:"reason"`
	// Attempts 本次是第几次尝试（1 起）。
	Attempts int `json:"attempts"`
	// WindowCount 窗口内已发生的尝试次数。
	WindowCount int `json:"window_count"`
}

// JSON 把判定结果序列化（落库用）。
func (d CapDecision) JSON() string {
	b, err := json.Marshal(d)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// EvalCaps 逐条判定安全闸门。
//
// 顺序即优先级，且**先判总开关**：一个被总开关关掉的自愈，不该因为"次数没用满"而放行。
// 所有判定都是纯函数——同样的输入永远得到同样的结论，这是"事后能复盘"的前提。
//
// 次数口径（**这里踩过一次坑，写清楚**）：
//   - 只有**真正尝试过**的记录消耗预算（planned/executed/succeeded/failed）；
//   - `denied` 与 `escalated` **不消耗**预算。
//
// 为什么 denied 不能算：如果拒绝也计数，一台"写能力没开"或"正好在冷却"的机器
// 会在几轮扫描后把窗口次数耗满，把**从未真正试过一次**的任务转成人工——
// 那是把"系统暂时没空"伪装成"已经试过三次了"。而"拒绝会不会导致每轮重复判定刷屏"
// 由另一套机制解决（服务层：同一次失败一旦被拒就不再重复判定），
// 两件事分开：**预算是预算，幂等是幂等**。
func EvalCaps(p Policy, in CapInput) CapDecision {
	p = p.Normalize()
	hist := in.History
	windowStart := in.Now.Add(-p.Window()).Unix()
	windowCount, lastAt := 0, int64(0)
	for _, h := range hist {
		if h == nil || !IsRealAttempt(h.Status) {
			continue
		}
		if h.Created >= windowStart {
			windowCount++
		}
		if h.Created > lastAt {
			lastAt = h.Created
		}
	}
	next := windowCount + 1

	if !p.Enabled {
		return CapDecision{Allowed: false, Rule: CapDisabled,
			Reason: "自愈总开关未开启（self_heal.enabled=false）：只记录分类，不动手", Attempts: next, WindowCount: windowCount}
	}
	if !in.JobSelfHeal {
		return CapDecision{Allowed: false, Rule: CapJobOptIn,
			Reason: "该任务未开启自愈（job.self_heal=0）：不自动处置，仅记录", Attempts: next, WindowCount: windowCount}
	}
	if !in.WritesEnabled {
		return CapDecision{Allowed: false, Rule: CapWriteOff,
			Reason:   "写能力未开启（allow_write=false 或自治级别不允许）：自愈不会自动动手，需人工处置",
			Attempts: next, WindowCount: windowCount}
	}
	if windowCount >= p.MaxAttemptsPerJobWindow {
		return CapDecision{Allowed: false, Rule: CapAttempts,
			Reason:   "该任务在窗口内已自愈 " + itoa(windowCount) + " 次（上限 " + itoa(p.MaxAttemptsPerJobWindow) + "）：停止自动尝试，转人工",
			Attempts: next, WindowCount: windowCount}
	}
	if lastAt > 0 && p.Cooldown() > 0 && in.Now.Unix()-lastAt < int64(p.Cooldown().Seconds()) {
		return CapDecision{Allowed: false, Rule: CapCooldown,
			Reason:   "距上次自愈不足冷却时间（" + itoa(int(p.Cooldown().Seconds())) + "s）：避免失败—重跑—再失败的死循环",
			Attempts: next, WindowCount: windowCount}
	}
	if in.ActiveCount >= p.MaxConcurrent {
		return CapDecision{Allowed: false, Rule: CapConcurrency,
			Reason:   "同时在观测的自愈已达上限（" + itoa(p.MaxConcurrent) + "）：本轮先不新增",
			Attempts: next, WindowCount: windowCount}
	}
	return CapDecision{Allowed: true, Rule: CapAllowed,
		Reason:   "策略允许：任务已开启自愈、写能力已开、窗口内第 " + itoa(next) + " 次（上限 " + itoa(p.MaxAttemptsPerJobWindow) + "）",
		Attempts: next, WindowCount: windowCount}
}

// IsRealAttempt 该状态是否算"真正尝试过"（消耗窗口预算、且是冷却锚点）。
//
// planned/executed/succeeded/failed 都意味着系统确实决定了要做（或已经做了）动作；
// denied 什么都没做，escalated 是"交给人"的结论，二者都不该占用预算。
func IsRealAttempt(status models.HealStatus) bool {
	switch status {
	case models.HealStatusPlanned, models.HealStatusExecuted, models.HealStatusSucceeded, models.HealStatusFailed:
		return true
	}
	return false
}

// NextTimeout 计算"调大超时"后的目标值（有绝对上限，不允许无限放大）。
// 为什么不做"模型给个数字"：超时是一个会直接改变执行行为的参数，
// 放大倍数由策略给定、绝对上限由策略兜住，才能保证"自愈不会顺手把超时改成 24 小时"。
func (p Policy) NextTimeout(current int64) (int64, bool) {
	p = p.Normalize()
	if current <= 0 {
		// 当前没有超时上限：无从"调大"，此时该动作不适用（返回 false，由决策层退化为重跑）。
		return 0, false
	}
	next := int64(float64(current) * p.ExtendTimeoutFactor)
	if next <= current {
		next = current + 60
	}
	if next > p.MaxExtendTimeoutSeconds {
		next = p.MaxExtendTimeoutSeconds
	}
	if next <= current {
		return 0, false
	}
	return next, true
}

// ActionJSON 动作参数（落 job_heal.action_json）。
type ActionJSON struct {
	JobID int `json:"job_id"`
	// FromTimeout / ToTimeout 仅 retry_extend_timeout 使用。
	FromTimeout int64 `json:"from_timeout,omitempty"`
	ToTimeout   int64 `json:"to_timeout,omitempty"`
	// NodeUUID 目标节点。
	NodeUUID string `json:"node_uuid,omitempty"`
	// MaxAttempts 期望的最大尝试次数（重跑通道参数）。
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// JSON 序列化动作参数。
func (a ActionJSON) JSON() string {
	b, err := json.Marshal(a)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ParseActionJSON 解析动作参数。
//
// 与 JSON() 配对：写进去的（job_heal.action_json）必须能被读回来。
// 解析失败返回 ok=false，调用方按"没有参数"处理（**不猜**）。
func ParseActionJSON(s string) (ActionJSON, bool) {
	var a ActionJSON
	s = strings.TrimSpace(s)
	if s == "" {
		return a, false
	}
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return ActionJSON{}, false
	}
	return a, true
}

// ParseCapDecision 解析策略判定快照（job_heal.policy_json）。
//
// 为什么需要它：幂等判定要区分"配置性拒绝"（总开关没开，重复判定只会刷屏）
// 与"临时性拒绝"（冷却/并发，过一会儿可以再试）。这个区分只能从快照里读。
func ParseCapDecision(s string) (CapDecision, bool) {
	var d CapDecision
	s = strings.TrimSpace(s)
	if s == "" {
		return d, false
	}
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return CapDecision{}, false
	}
	return d, true
}

// 经验库里自愈经验的动作文本前缀（唯一契约：写与读都用它）。
const memoryActionPrefix = "self_heal:"

// MemoryActionText 生成经验库里的动作文本：`self_heal:<action> <人可读说明>`。
//
// 为什么要带机器可读前缀：自愈决策要按动作聚合历史成功率（RecallStats），
// 前缀是"从一行经验反解出它属于哪个白名单动作"的唯一依据。
func MemoryActionText(a models.HealAction, jobID int, args ActionJSON) string {
	desc := string(a)
	switch a {
	case models.HealActionRetrySameNode:
		desc = "原节点重跑"
	case models.HealActionRetryExtendTimeout:
		desc = "调大超时后重跑"
	case models.HealActionEscalate:
		desc = "转人工"
	}
	out := memoryActionPrefix + string(a) + " 任务 " + itoa(jobID) + "（" + desc + "）"
	if a == models.HealActionRetryExtendTimeout && args.ToTimeout > 0 {
		out += " 超时 " + itoa64(args.FromTimeout) + "→" + itoa64(args.ToTimeout)
	}
	return out
}

// ActionFromMemoryText 从经验文本反解动作（不认识的文本返回 ok=false）。
//
// 只认前缀 + 白名单取值：文本可以是任何历史数据，绝不能让一段乱文本变出一个动作。
func ActionFromMemoryText(text string) (models.HealAction, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, memoryActionPrefix) {
		return "", false
	}
	rest := text[len(memoryActionPrefix):]
	end := strings.IndexAny(rest, " \t")
	if end >= 0 {
		rest = rest[:end]
	}
	switch models.HealAction(rest) {
	case models.HealActionRetrySameNode, models.HealActionRetryExtendTimeout, models.HealActionEscalate:
		return models.HealAction(rest), true
	}
	return "", false
}

func itoa64(v int64) string {
	return itoa(int(v))
}
