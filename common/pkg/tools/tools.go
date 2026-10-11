// Package tools 是**工具网关**：Agent 能做的所有副作用都必须经过这里。
//
// 核心立场（对应方案 §4「工具即权限边界」，阶段 4 升级为风险感知网关）：
//   - **只读优先、写动作显式打开**：阶段 2 只注册只读工具；阶段 4 起允许注册写工具，
//     但默认策略（`AllowWrite=false`）下写工具**根本注册不进来**，且即使注册进来，
//     默认自治级别 L1 下调用也只会生成审批单、不会执行。两道默认关着；
//   - **不经过 shell**：`exec.CommandContext(binary, args...)` 直接执行，
//     从机制上消灭命令注入（而不是靠"过滤分号"）；
//   - **工具失败是观测，不是异常**：模型需要看到"这个工具失败了/没权限"，
//     才能改变策略。所以失败也会返回 Result（带 Err 字段），只有"根本不该调用"
//     （未知工具、越权、参数不合法、超预算）才返回 error；
//   - **每一次调用都要留痕**：CallRecord 交给上层落库（append-only 审计）。
//     **被拒的调用同样留痕**——"没被执行的尝试"是需要复盘的事实
//     （模型在反复试探什么？是哪道闸在拦它？）；
//   - **默认值必须安全**：写动作、不可逆动作、自治级别，全部默认关到最保守一档。
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// 错误语义。
var (
	// ErrUnknownTool 工具不存在（模型幻觉工具名）→ 不可重试，应把可用工具列表回灌给模型。
	ErrUnknownTool = errors.New("tools: 未知工具")
	// ErrToolDenied 被策略禁止（白名单/只读约束/目标不允许）→ 不可重试。
	ErrToolDenied = errors.New("tools: 工具被策略禁止")
	// ErrInvalidArgs 参数不满足工具 Schema → 不可重试（应把校验错误回灌给模型去改）。
	ErrInvalidArgs = errors.New("tools: 参数不合法")
	// ErrSessionBudgetExceeded 会话/单工具调用次数超限 → 不可重试。
	ErrSessionBudgetExceeded = errors.New("tools: 工具调用预算耗尽")
	// ErrToolPanic 工具实现 panic 被兜住（工具实现有 bug，但绝不能把 Agent 一起带崩）。
	ErrToolPanic = errors.New("tools: 工具实现 panic")
	// ErrRiskNotAllowed 风险级别未被策略允许（写工具未开启 / 不可逆工具未开启 / 自治级别 L0）。
	ErrRiskNotAllowed = errors.New("tools: 风险级别未被策略允许")
	// ErrRateLimited 进程级速率超限（每分钟调用次数）→ 可稍后重试，但不是"工具不存在"。
	ErrRateLimited = errors.New("tools: 调用速率超限")
	// ErrConcurrencyLimited 进程级并发超限 → 可稍后重试。
	//
	// 这里**刻意不排队等待**：排队会把"并发已经打满"这个事实藏起来，
	// 还会让 Agent 的墙钟预算被静默吃掉（进而在别处以"超时"的形式暴露，
	// 让人误判成节点慢）。直接拒绝 + 写清原因是可复盘的。
	ErrConcurrencyLimited = errors.New("tools: 并发调用超限")
	// ErrBlastRadiusExceeded 爆炸半径预算超限（单会话可影响的写动作数/主机数）。
	ErrBlastRadiusExceeded = errors.New("tools: 爆炸半径超限")
	// ErrApprovalUnavailable 需要人工审批但审批通道未接入（或生成审批单失败）。
	//
	// **安全默认是拒绝而不是放行**：拿不到审批单就执行，等于把"审批"变成
	// "只要审批组件挂了我就能随便动手"。
	ErrApprovalUnavailable = errors.New("tools: 审批通道不可用")
)

// DefaultMaxOutputBytes 单次工具输出上限（64 KiB，与阶段 1 执行器的输出上限一致）。
const DefaultMaxOutputBytes = 64 * 1024

// Evidence 一条证据。诊断报告里的每条结论都必须挂证据，否则就是"编的"。
type Evidence struct {
	// Source 证据类型：log / step / node / sql / http / shell / approval。
	Source string `json:"source"`
	// Ref 定位：文件:行、run_id、URL、SQL 摘要、approval:<审批单号>。
	Ref string `json:"ref"`
	// Quote 原文片段（落库前会截断）。
	Quote string `json:"quote"`
}

// Result 工具执行结果。
type Result struct {
	// OK 工具是否成功完成（"执行了但发现异常"仍然是 OK，例如 df 显示磁盘 95%）。
	OK bool
	// Summary 一句话结论（给模型看的第一手信息）。
	Summary string
	// Output 原始输出（已按 MaxOutputBytes 截断）。
	Output string
	// Truncated 输出是否被截断（模型需要知道"你看到的不是全部"）。
	Truncated bool
	// Evidence 本次调用产生的证据。
	Evidence []Evidence
	// Err 执行失败原因（OK=false 时非空）。
	Err string
	// DurationMS 耗时。
	DurationMS int64
	// Meta 结构化附加信息（状态码、行数、命中数、审批单号等）。
	Meta map[string]interface{}
}

// Failed 是否失败。
func (r Result) Failed() bool { return !r.OK }

// Text 渲染成回灌给模型的文本。
//
// 为什么要单独做个渲染函数而不是直接 json.Marshal(Result)：
// 模型对结构化 JSON 的"阅读"效果不稳定，而"状态 + 摘要 + 证据 + 输出"这种
// 紧凑文本格式在实测中更不容易被忽略；同时截断标记必须显式写出来。
func (r Result) Text() string {
	var b strings.Builder
	if r.OK {
		b.WriteString("状态: 成功\n")
	} else {
		b.WriteString("状态: 失败\n")
	}
	if r.Summary != "" {
		b.WriteString("摘要: " + r.Summary + "\n")
	}
	if r.Err != "" {
		b.WriteString("错误: " + r.Err + "\n")
	}
	if len(r.Evidence) > 0 {
		b.WriteString("证据:\n")
		for _, e := range r.Evidence {
			b.WriteString(fmt.Sprintf("- [%s] %s 「%s」\n", e.Source, e.Ref, e.Quote))
		}
	}
	if r.Output != "" {
		b.WriteString("输出:\n" + r.Output + "\n")
	}
	if r.Truncated {
		b.WriteString("（输出已截断，只保留了前面部分）\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Tool 工具抽象。
type Tool interface {
	// Name 唯一名（模型看到的就是它，必须稳定，否则历史轨迹会失真）。
	Name() string
	// Description 给模型看的用途说明。
	Description() string
	// Schema 参数 Schema（一律 additionalProperties=false）。
	Schema() *jsonschema.Schema
	// ReadOnly 是否只读。与 Risk() 必须自洽：只读 ⇔ RiskRead。
	ReadOnly() bool
	// Risk 风险级别（read/write/destructive）。
	//
	// 为什么是接口方法而不是可选接口（"实现了就用、没实现就当只读"）：
	// 新增一个工具时忘记声明风险，应该是**编译错误**，而不是静默按只读放行。
	// 一个静默默认值在安全边界上就是一条隐藏的生路。
	Risk() RiskLevel
	// Timeout 单次执行超时；0 表示用策略默认值。
	Timeout() time.Duration
	// Execute 执行。约定：不得 panic（注册表也会兜），
	// 返回 (Result, nil) 表示"拿到了观测"（哪怕业务上是失败），
	// 返回 error 表示"工具本身不可用"。
	Execute(ctx context.Context, args map[string]interface{}) (Result, error)
}

// CallRecord 一次工具调用的审计记录（**每次 Call 恰好产生一条**）。
type CallRecord struct {
	SessionID string `json:"session_id"`
	// AgentKind/RunID 调用归属（由 WithRunMeta 从 context 带入，可能为空）。
	AgentKind string `json:"agent_kind,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Tool      string `json:"tool"`
	// Risk 本次调用的风险级别（即使工具未知也会填 read 兜底值）。
	Risk RiskLevel              `json:"risk,omitempty"`
	Args map[string]interface{} `json:"args,omitempty"`
	// Decision 见 Decision* 常量：allowed / denied / approval_required / approval_executed。
	Decision string `json:"decision"`
	// Reason 人类可读原因：被拒时必须写明**是哪一道闸**。
	Reason string `json:"reason,omitempty"`
	// ApprovalID 关联审批单（转审批时有值）。
	ApprovalID string `json:"approval_id,omitempty"`
	// Targets 本次调用影响/观测的目标（写动作必填）。
	Targets []string `json:"targets,omitempty"`
	OK      bool     `json:"ok"`
	Err     string   `json:"err,omitempty"`
	// Denied 兼容旧口径：等价于 Decision == DecisionDenied。
	Denied      bool  `json:"denied,omitempty"`
	DurationMS  int64 `json:"duration_ms"`
	OutputBytes int   `json:"output_bytes"`
	Evidence    int   `json:"evidence"`
}

// Observer 审计回调（不允许阻塞、不允许 panic 影响主流程）。
type Observer func(CallRecord)

// Policy 工具网关策略。
type Policy struct {
	// Allow 允许的工具名（空 = 所有已注册工具中策略允许的那些）。
	Allow []string
	// Deny 显式禁止（优先级高于 Allow）。
	Deny []string
	// MaxCallsPerSession 单会话工具调用总次数上限（默认 12）。
	MaxCallsPerSession int
	// MaxCallsPerTool 单会话对同一个工具的调用次数上限（默认 4）。
	// 用来打断"同一个查询反复试"的模型行为——这类循环不产生新证据，只烧钱。
	MaxCallsPerTool int
	// DefaultTimeout 单次工具默认超时（默认 15s）。
	DefaultTimeout time.Duration
	// MaxOutputBytes 输出上限（默认 64 KiB）。
	MaxOutputBytes int
	// AllowHosts http_get 允许访问的 host（支持 `*.example.com`）。
	// **默认为空 = 完全禁止外部请求**：默认不发外网，是刻意的。
	AllowHosts []string
	// ShellReadPaths tail 等可读文件路径的前缀白名单（默认 /var/log、/var/log/crony）。
	ShellReadPaths []string
	// SQLMaxRows 只读 SQL 允许的最大行数（默认 100）。
	SQLMaxRows int
	// SQLAllowTables 预留：按表进一步收窄（暂未生效，保留字段以免配置漂移）。
	SQLAllowTables []string

	// ---- 阶段 4：风险与自治 ----

	// AllowWrite 是否允许注册"可逆写"工具（默认 false）。
	AllowWrite bool
	// AllowDestructive 是否允许注册"不可逆/高爆"工具（默认 false，且**永远需要审批**）。
	AllowDestructive bool
	// Autonomy 自治级别（默认 L1：写动作一律人工审批）。
	Autonomy AutonomyLevel
	// MaxCallsPerMinute 进程级速率上限（**跨会话**，默认 60）。
	//
	// 为什么速率闸与会话配额都要有：会话配额防"同一个会话无限试"，
	// 速率闸防"多个会话同时把节点打爆/把模型预算烧穿"。
	// 默认值取 60 是算出来的：单次诊断 ≤8 次工具调用、耗时 30~60s，
	// 60/分钟足以支撑数个并发会话；再低就会开始误伤正常并发（拒绝对真实用户可见）。
	// 诚实边界：它是**进程内**滑窗，多实例部署时是"每实例 N 次/分钟"，
	// 不是集群级限额（集群级需要共享存储，属于后续项）。
	MaxCallsPerMinute int
	// MaxConcurrentCalls 进程级并发上限（默认 4）。
	MaxConcurrentCalls int
	// MaxWriteActionsPerSession 单会话写动作数上限（默认 2）。
	MaxWriteActionsPerSession int
	// MaxAffectedHostsPerSession 单会话可影响的**去重**主机数上限（默认 1）。
	MaxAffectedHostsPerSession int
	// ApprovalTTL 审批单有效期（默认 15 分钟）：写动作的合理性依赖当时的现场。
	ApprovalTTL time.Duration
	// AutoApprove L2/L3 的自动放行白名单，条目形如 `job_retry` 或 `job_retry@node-uuid`。
	// **默认空 = 所有写动作都要审批**；destructive 永不自动放行。
	AutoApprove []string
}

// DefaultPolicy 默认策略（保守：写动作全关、自治级别 L1）。
func DefaultPolicy() Policy {
	return Policy{
		MaxCallsPerSession: 12,
		MaxCallsPerTool:    4,
		DefaultTimeout:     15 * time.Second,
		MaxOutputBytes:     DefaultMaxOutputBytes,
		ShellReadPaths:     []string{"/var/log/", "/var/log/crony/"},
		SQLMaxRows:         100,

		AllowWrite:                 false,
		AllowDestructive:           false,
		Autonomy:                   AutonomyL1,
		MaxCallsPerMinute:          60,
		MaxConcurrentCalls:         4,
		MaxWriteActionsPerSession:  2,
		MaxAffectedHostsPerSession: 1,
		ApprovalTTL:                15 * time.Minute,
	}
}

// Normalize 补齐零值。
//
// 注意：布尔字段无法用"零值即未设置"判断（false 是合法取值），
// 所以 AllowWrite/AllowDestructive 的默认值只体现在 DefaultPolicy 里，
// 不从配置构造策略时若忘了显式赋值，结果就是"关着"——这正是我们要的方向。
func (p Policy) Normalize() Policy {
	if p.MaxCallsPerSession <= 0 {
		p.MaxCallsPerSession = 12
	}
	if p.MaxCallsPerTool <= 0 {
		p.MaxCallsPerTool = 4
	}
	if p.DefaultTimeout <= 0 {
		p.DefaultTimeout = 15 * time.Second
	}
	if p.MaxOutputBytes <= 0 {
		p.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if p.SQLMaxRows <= 0 {
		p.SQLMaxRows = 100
	}
	if len(p.ShellReadPaths) == 0 {
		p.ShellReadPaths = []string{"/var/log/", "/var/log/crony/"}
	}
	if !p.Autonomy.Valid() {
		p.Autonomy = AutonomyL1
	}
	if p.MaxCallsPerMinute <= 0 {
		p.MaxCallsPerMinute = 60
	}
	if p.MaxConcurrentCalls <= 0 {
		p.MaxConcurrentCalls = 4
	}
	if p.MaxWriteActionsPerSession <= 0 {
		p.MaxWriteActionsPerSession = 2
	}
	if p.MaxAffectedHostsPerSession <= 0 {
		p.MaxAffectedHostsPerSession = 1
	}
	if p.ApprovalTTL <= 0 {
		p.ApprovalTTL = 15 * time.Minute
	}
	// destructive 隐含要求 write 开关：只开 destructive 不开 write 是配置错误，
	// 这里补齐而不是报错——因为"两个开关里只开了一个"本身不会放宽权限，
	// 补齐后的语义是"按更严格的那一档理解"，即都得显式打开。
	if p.AllowDestructive {
		p.AllowWrite = true
	}
	return p
}

// allows 判断工具是否被策略允许。
func (p Policy) allows(name string) bool {
	for _, d := range p.Deny {
		if d == name {
			return false
		}
	}
	if len(p.Allow) == 0 {
		return true
	}
	for _, a := range p.Allow {
		if a == name {
			return true
		}
	}
	return false
}

// SessionStats 会话内工具使用统计（预算展示 + 轨迹落库）。
type SessionStats struct {
	SessionID   string         `json:"session_id"`
	Calls       int            `json:"calls"`
	PerTool     map[string]int `json:"per_tool"`
	Evidence    int            `json:"evidence"`
	Denied      int            `json:"denied"`
	StartedAt   time.Time      `json:"started_at"`
	LastCallAt  time.Time      `json:"last_call_at"`
	TotalMillis int64          `json:"total_millis"`
	// WriteActions 已受理的写动作数（含"已转人工审批"的那些——它们是**意图**）。
	WriteActions int `json:"write_actions"`
	// Hosts 已受理的写动作影响的目标（去重）。
	Hosts []string `json:"hosts"`
	// ApprovalRequired 已生成审批单的次数。
	ApprovalRequired int `json:"approval_required"`
	// DeniedBy 被拒原因分类计数（"被拒"必须能细分到哪道闸，否则复盘无法定位）。
	DeniedBy map[string]int `json:"denied_by"`
}

type sessionState struct {
	calls       int
	perTool     map[string]int
	evidence    int
	denied      int
	startedAt   time.Time
	lastCallAt  time.Time
	totalMillis int64

	writeActions     int
	hosts            map[string]struct{}
	approvalRequired int
	deniedBy         map[string]int
}

// rateWindow 进程级滑窗限速（每分钟）。
type rateWindow struct {
	mu   sync.Mutex
	hits []time.Time
}

// allow 判定并记录一次调用（now 时刻）。
func (w *rateWindow) allow(now time.Time, max int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cut := now.Add(-time.Minute)
	kept := w.hits[:0]
	for _, h := range w.hits {
		if h.After(cut) {
			kept = append(kept, h)
		}
	}
	w.hits = kept
	if max > 0 && len(w.hits) >= max {
		return false
	}
	w.hits = append(w.hits, now)
	return true
}

// retract 撤销刚才记录的一次调用（审批单生成失败时把额度还回去）。
func (w *rateWindow) retract() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := len(w.hits); n > 0 {
		w.hits = w.hits[:n-1]
	}
}

// Registry 工具注册表 + 调用网关。
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]Tool
	order    []string
	policy   Policy
	observer Observer
	approver Approver
	sessions map[string]*sessionState
	// sem 进程级并发信号量（容量 = policy.MaxConcurrentCalls）。
	sem chan struct{}
	// rate 进程级速率滑窗。
	rate rateWindow
}

// Option 注册表选项。
type Option func(*Registry)

// WithObserver 设置审计回调。
func WithObserver(o Observer) Option {
	return func(r *Registry) { r.observer = o }
}

// WithPolicy 覆盖策略。
func WithPolicy(p Policy) Option {
	return func(r *Registry) { r.policy = p.Normalize() }
}

// WithApprover 设置审批单生成方（不设置时，需要审批的写动作会被**拒绝**）。
func WithApprover(a Approver) Option {
	return func(r *Registry) { r.approver = a }
}

// NewRegistry 创建注册表。
func NewRegistry(policy Policy, opts ...Option) *Registry {
	r := &Registry{
		tools:    make(map[string]Tool),
		policy:   policy.Normalize(),
		sessions: make(map[string]*sessionState),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	// 信号量按**最终**策略建（WithPolicy 在选项里生效，必须排在后面）。
	r.sem = make(chan struct{}, r.policy.MaxConcurrentCalls)
	return r
}

// Register 注册工具。
//
// 阶段 4 的闸门（默认全部关着）：
//   - read：总是允许（只读是阶段 2 起的既有能力面）；
//   - write：需要 `AllowWrite=true`，且工具**必须**实现 TargetProvider；
//   - destructive：需要 `AllowDestructive=true`（隐含 AllowWrite）且实现 TargetProvider。
//
// 另外做「ReadOnly() 与 Risk() 自洽」检查：一个工具同时声称"只读"和"写"
// 是自相矛盾的实现，与其在运行期猜，不如在注册期拒绝。
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tools: 工具为 nil")
	}
	name := strings.TrimSpace(t.Name())
	if name == "" || strings.ContainsAny(name, " \t") {
		return fmt.Errorf("tools: 工具名 %q 非法（不能为空或含空白）", name)
	}
	policy := r.policy
	risk := t.Risk()
	if !risk.Valid() {
		return fmt.Errorf("%w: %s 未声明合法的风险级别（read/write/destructive），实际 %q",
			ErrRiskNotAllowed, name, string(risk))
	}
	if t.ReadOnly() != (risk == RiskRead) {
		return fmt.Errorf("%w: %s 的 ReadOnly()=%v 与 Risk()=%s 自相矛盾（只读 ⇔ read）",
			ErrRiskNotAllowed, name, t.ReadOnly(), risk)
	}
	switch risk {
	case RiskWrite:
		if !policy.AllowWrite {
			return fmt.Errorf("%w: %s 是可逆写工具，但策略未开启（allow_write=false）", ErrToolDenied, name)
		}
	case RiskDestructive:
		if !policy.AllowDestructive {
			return fmt.Errorf("%w: %s 是不可逆写工具，但策略未开启（allow_destructive=false）", ErrToolDenied, name)
		}
	}
	if risk.IsWrite() {
		if _, ok := t.(TargetProvider); !ok {
			return fmt.Errorf("%w: 写工具 %s 必须实现 TargetProvider（声明影响目标），否则爆炸半径预算无法生效",
				ErrRiskNotAllowed, name)
		}
	}
	if s := t.Schema(); s != nil {
		if err := s.Valid(); err != nil {
			return fmt.Errorf("tools: 工具 %s 的参数 Schema 非法: %w", name, err)
		}
	} else {
		return fmt.Errorf("tools: 工具 %s 缺少参数 Schema", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tools: 工具 %s 重复注册", name)
	}
	r.tools[name] = t
	r.order = append(r.order, name)
	return nil
}

// MustRegister 注册并在失败时 panic（用于内置工具：名字写错要在启动时炸）。
func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Get 按名取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回所有已注册工具名（字典序，保证 prompt 稳定）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// AllowedNames 返回策略允许的工具名。
func (r *Registry) AllowedNames() []string {
	r.mu.RLock()
	policy := r.policy
	r.mu.RUnlock()
	out := make([]string, 0)
	for _, name := range r.Names() {
		if policy.allows(name) {
			out = append(out, name)
		}
	}
	return out
}

// Specs 返回提供给模型的工具声明（只包含策略允许的）。
//
// 为什么不把被禁工具也告诉模型：模型看到就会去调，调一次就被拒一次，
// 白烧 token 还会污染轨迹。**只暴露能用的**。
func (r *Registry) Specs() []llm.ToolSpec {
	specs := make([]llm.ToolSpec, 0)
	for _, name := range r.AllowedNames() {
		t, ok := r.Get(name)
		if !ok {
			continue
		}
		specs = append(specs, llm.ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		})
	}
	return specs
}

// SetApprover 后置装配审批通道（见 agentruntime.ConfigureWrites 的"环"说明）。
//
// 允许后置装配是因为装配顺序上审批服务依赖运行时；不允许**换**审批通道，
// 因为"审批人是谁"必须是进程生命周期内唯一的事实。
func (r *Registry) SetApprover(a Approver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.approver != nil && a != nil && r.approver != a {
		// 换通道是配置错误：保留原通道（宁可拒绝，也不要静默改判）。
		return
	}
	r.approver = a
}

// HasWriteTools 当前能力面里是否**有**写工具（含被策略拒绝但已注册的）。
func (r *Registry) HasWriteTools() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tools {
		if t.Risk().IsWrite() {
			return true
		}
	}
	return false
}

// Policy 返回当前策略的**深拷贝**。
//
// 必须深拷贝：调用方（HTTP 视图层）会读切片字段，若返回共享底层数组，
// 调用方一次意外的 append/改写就会改到注册表正在使用的策略——
// "读了策略却改了策略"是最难查的一类越权。
func (r *Registry) Policy() Policy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p := r.policy
	p.Allow = append([]string(nil), p.Allow...)
	p.Deny = append([]string(nil), p.Deny...)
	p.AllowHosts = append([]string(nil), p.AllowHosts...)
	p.ShellReadPaths = append([]string(nil), p.ShellReadPaths...)
	p.SQLAllowTables = append([]string(nil), p.SQLAllowTables...)
	p.AutoApprove = append([]string(nil), p.AutoApprove...)
	return p
}

// Call 调用一个工具（Agent 循环的唯一入口）。
//
// 闸门顺序（顺序本身就是设计，不可随意调整）：
//
//	工具存在 → 风险级别合法/自洽 → 策略白名单 → 风险与自治级别
//	→ 参数 Schema → 进程级速率 → 进程级并发 → 爆炸半径与会话预算
//	→ 审批闸门（L1 或未命中白名单）→ 执行
//
// 为什么把"最便宜且最确定的检查"放前面：模型幻觉工具名与参数错误是最常见的两种失败，
// 让它们在碰到任何预算/限速账本之前就被拒，账本才不会被噪声污染。
func (r *Registry) Call(ctx context.Context, sessionID, name string, args map[string]interface{}) (Result, error) {
	return r.call(ctx, sessionID, name, args, "")
}

// ExecuteApproved 执行一个**已获人工批准**的写动作（`approvalID` 非空）。
//
// 它与 Call 的唯一差别是"审批那道闸"：Call 在需要审批时生成审批单并返回，
// ExecuteApproved 则不生成审批单、直接进入执行。其余每一道闸**都要再走一遍**
// （工具存在、风险自洽、策略允许、自治级别、Schema、速率、并发），
// 因为批准发生在过去，而"现在还能不能执行"必须重新判断——这是 TOCTOU 的第二道防线
// （第一道是控制面里的参数哈希比对，见 admin 的 ApprovalService）。
//
// 为什么必须做成注册表的方法，而不是"绕开网关直接调 Tool.Execute"：
// 绕开网关意味着审计里没有 approval_executed 这条事实，事后无法回答
// "这次执行是谁批准的、走的哪张单子"。
func (r *Registry) ExecuteApproved(ctx context.Context, sessionID, name string,
	args map[string]interface{}, approvalID string) (Result, error) {
	if strings.TrimSpace(approvalID) == "" {
		return Result{}, retry.Fatal(fmt.Errorf("%w: 缺少审批单号，拒绝执行已批准动作", ErrInvalidArgs))
	}
	return r.call(ctx, sessionID, name, args, strings.TrimSpace(approvalID))
}

func (r *Registry) call(ctx context.Context, sessionID, name string, args map[string]interface{}, approvalID string) (Result, error) {
	approved := approvalID != ""
	if r == nil {
		return Result{}, retry.Fatal(fmt.Errorf("tools: 注册表未初始化"))
	}
	meta := RunMetaFrom(ctx)
	r.mu.RLock()
	tool, ok := r.tools[name]
	policy := r.policy
	approver := r.approver
	r.mu.RUnlock()

	if !ok {
		// 把可用工具列表带上：模型下一轮可以自我纠正
		r.recordDenied(sessionID, name, args, RiskRead, meta, ReasonUnknownTool)
		return Result{}, retry.Fatal(fmt.Errorf("%w: %q（可用: %s）", ErrUnknownTool, name, strings.Join(r.AllowedNames(), ", ")))
	}

	risk := tool.Risk()
	if !risk.Valid() || tool.ReadOnly() != (risk == RiskRead) {
		r.recordDenied(sessionID, name, args, risk, meta, ReasonRiskNotAllowed)
		return Result{}, retry.Fatal(fmt.Errorf("%w: 工具 %s 的风险级别声明有问题（risk=%q readonly=%v）",
			ErrRiskNotAllowed, name, string(risk), tool.ReadOnly()))
	}
	if !policy.allows(name) {
		r.recordDenied(sessionID, name, args, risk, meta, ReasonPolicyDenied)
		return Result{}, retry.Fatal(fmt.Errorf("%w: %s", ErrToolDenied, name))
	}
	if risk.IsWrite() {
		if !policy.AllowWrite || (risk == RiskDestructive && !policy.AllowDestructive) {
			r.recordDenied(sessionID, name, args, risk, meta, ReasonRiskNotAllowed)
			return Result{}, retry.Fatal(fmt.Errorf("%w: 工具 %s 是%s工具，但当前策略未开启该类动作",
				ErrRiskNotAllowed, name, map[bool]string{true: "不可逆写", false: "可逆写"}[risk == RiskDestructive]))
		}
		if !policy.Autonomy.AllowsWriteTools() {
			r.recordDenied(sessionID, name, args, risk, meta, ReasonAutonomyL0)
			return Result{}, retry.Fatal(fmt.Errorf("%w: 当前自治级别 %s 只提供建议，不提供写动作",
				ErrRiskNotAllowed, policy.Autonomy))
		}
	}
	// 审批单只对**写动作**有意义：拿一张单子来执行只读工具，说明调用方搞错了
	// （或者参数被换过），一律拒绝并留下证据。
	if approved && !risk.IsWrite() {
		r.recordDenied(sessionID, name, args, risk, meta, ReasonApprovalMissed)
		return Result{}, retry.Fatal(fmt.Errorf("%w: 审批单 %s 对应写动作，但 %s 是只读工具",
			ErrApprovalUnavailable, approvalID, name))
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	if err := tool.Schema().Validate(args); err != nil {
		r.recordDenied(sessionID, name, args, risk, meta, ReasonInvalidArgs)
		return Result{}, retry.Fatal(fmt.Errorf("%w: 工具 %s: %v", ErrInvalidArgs, name, err))
	}

	targets := TargetsOf(tool, args)

	// 速率闸（进程级，跨会话）。
	if !r.rate.allow(time.Now(), policy.MaxCallsPerMinute) {
		r.recordDenied(sessionID, name, args, risk, meta, ReasonRateLimited)
		return Result{}, retry.Fatal(fmt.Errorf("%w: 最近一分钟调用已达 %d 次上限", ErrRateLimited, policy.MaxCallsPerMinute))
	}
	// 并发闸（进程级）：满则直接拒绝，不排队（见 ErrConcurrencyLimited 注释）。
	if !r.acquire() {
		r.rate.retract()
		r.recordDenied(sessionID, name, args, risk, meta, ReasonConcurrencyLimit)
		return Result{}, retry.Fatal(fmt.Errorf("%w: 同时在执行的工具调用已达 %d 个", ErrConcurrencyLimited, policy.MaxConcurrentCalls))
	}
	defer r.release()

	needApproval := risk.IsWrite() && !approved && !policy.autoApproveMatch(name, targets, risk)

	// 爆炸半径 + 会话预算：直接拒绝的两种情形都不消耗额度。
	//
	// 已批准的执行**不重复扣预算**：这笔写动作在"转审批"时就扣过一次
	// （那是刻意的——它保证模型不能靠狂发审批单绕过爆炸半径）。若执行时再扣一次，
	// 一张单子会吃掉两个名额；在"单会话最多影响 1 个主机"的默认策略下，
	// 结果就是**任何写动作都永远批不出来**——安全策略变成了功能禁用。
	// 之所以不担心"跳过预算导致狂执行"：每张审批单最多执行一次（控制面的 CAS），
	// 而单子数量本身已经被扣过预算。
	if !approved {
		if reason, err := r.admit(sessionID, name, risk, targets); err != nil {
			r.recordDenied(sessionID, name, args, risk, meta, reason)
			return Result{}, err
		}
	}

	if needApproval {
		// 审批通道缺失或生成失败 → **拒绝**（安全默认），并把刚占的额度还回去。
		if approver == nil {
			r.refund(sessionID, name, risk, targets)
			r.recordDenied(sessionID, name, args, risk, meta, ReasonApprovalNoChannel)
			return Result{}, retry.Fatal(fmt.Errorf("%w: 写工具 %s 需要人工审批，但未装配审批通道（按安全默认拒绝，不执行）",
				ErrApprovalUnavailable, name))
		}
		in := ApprovalInput{
			SessionID:   sessionID,
			RunID:       meta.RunID,
			AgentKind:   meta.AgentKind,
			Tool:        name,
			Risk:        risk,
			Args:        args,
			ArgsHash:    ArgsHash(args),
			Reason:      llm.StringOr(args, "reason", ""),
			Targets:     targets,
			Impact:      impactSummary(risk, targets, args),
			RequestedBy: meta.AgentKind,
			TTL:         policy.ApprovalTTL,
		}
		out, aerr := approver.RequestApproval(ctx, in)
		if aerr != nil {
			r.refund(sessionID, name, risk, targets)
			r.recordDenied(sessionID, name, args, risk, meta, ReasonApprovalNoChannel)
			return Result{}, retry.Fatal(fmt.Errorf("%w: 生成审批单失败: %v", ErrApprovalUnavailable, aerr))
		}
		res := approvalObservation(in, out)
		rec := r.baseRecord(sessionID, name, args, risk, meta, targets, DecisionApprovalRequired,
			"写动作已转人工审批（本次调用**没有副作用**）")
		rec.ApprovalID = out.ApprovalID
		rec.OK = true
		rec.Evidence = len(res.Evidence)
		r.observe(rec)
		return res, nil
	}

	timeout := tool.Timeout()
	if timeout <= 0 {
		timeout = policy.DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	res, err := r.execute(callCtx, tool, args)
	res.DurationMS = time.Since(started).Milliseconds()
	// 写动作的观测必须**显式**说明"这次真的执行了"：审批路径的观测写的是
	// executed=false（尚未执行），如果执行路径什么都不写，模型和前端就只能靠
	// "没有 approval_id"去猜——而"猜"在写动作上是不可接受的。
	stampWriteMeta(&res, risk)

	if err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("工具 %s 执行超时（%s）: %w", name, timeout, err)
		}
		res.OK = false
		if res.Err == "" {
			res.Err = llm.Redact(err.Error())
		}
		rec := r.baseRecord(sessionID, name, args, risk, meta, targets, decisionOf(approved), ReasonTimeout)
		if !errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			rec.Reason = ReasonExecuteFailed
		}
		rec.ApprovalID = approvalID
		rec.OK = false
		rec.Err = res.Err
		rec.DurationMS = res.DurationMS
		r.observe(rec)
		return res, nil
	}

	// 输出截断（保留前 N 字节，但**绝不切断多字节字符**：切一半的 UTF-8
	// 落库后会变成坏字节，让整份报告打不开——阶段 3 的验收报告就踩过这个坑）。
	if cut, truncated := truncateUTF8(res.Output, policy.MaxOutputBytes); truncated {
		res.Output = cut
		res.Truncated = true
	}
	// 证据原文也收口（避免一条证据就塞满 1 MiB）
	for i := range res.Evidence {
		res.Evidence[i].Quote = llm.Truncate(res.Evidence[i].Quote, 500)
	}
	if res.Err != "" {
		res.Err = llm.Redact(res.Err)
	}
	if res.Summary == "" && res.OK {
		res.Summary = "已执行"
	}

	rec := r.baseRecord(sessionID, name, args, risk, meta, targets, decisionOf(approved), "")
	rec.ApprovalID = approvalID
	rec.OK = res.OK
	rec.Err = res.Err
	rec.DurationMS = res.DurationMS
	rec.OutputBytes = len(res.Output)
	rec.Evidence = len(res.Evidence)
	r.observe(rec)
	return res, nil
}

// decisionOf 审计决策取值：走审批单执行的记为 approval_executed，
// 其余正常执行记为 allowed。两者都不代表"成功"——成败看 CallRecord.OK。
func decisionOf(approved bool) string {
	if approved {
		return DecisionApprovalExecuted
	}
	return DecisionAllowed
}

// stampWriteMeta 给写动作的执行结果打上"已执行"标记（只读工具不动）。
//
// 语义约定（前端与模型都按这个读）：
//   - `executed=false`：本次调用**没有副作用**，只生成了审批单；
//   - `executed=true`：网关真的调用了执行器；成功与否看 OK/Err，**不要**用 executed 判成败。
//
// 执行失败也标 true：动作确实跑了（可能产生了部分副作用），把它标成 false
// 会让人误以为"什么都没发生"，这是审计上更危险的谎言。
func stampWriteMeta(res *Result, risk RiskLevel) {
	if res == nil || !risk.IsWrite() {
		return
	}
	if res.Meta == nil {
		res.Meta = map[string]interface{}{}
	}
	res.Meta["executed"] = true
	res.Meta["risk"] = risk.String()
}

// baseRecord 组装审计记录的公共部分（每次 Call 恰好一条）。
func (r *Registry) baseRecord(sessionID, name string, args map[string]interface{}, risk RiskLevel,
	meta RunMeta, targets []string, decision, reason string) CallRecord {
	return CallRecord{
		SessionID: sessionID,
		AgentKind: meta.AgentKind,
		RunID:     meta.RunID,
		Tool:      name,
		Risk:      risk,
		Args:      args,
		Decision:  decision,
		Reason:    reason,
		Targets:   targets,
		Denied:    decision == DecisionDenied,
	}
}

// acquire 尝试占用一个并发名额（非阻塞）。
func (r *Registry) acquire() bool {
	if r.sem == nil {
		return true
	}
	select {
	case r.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// release 归还并发名额。
func (r *Registry) release() {
	if r.sem == nil {
		return
	}
	select {
	case <-r.sem:
	default:
	}
}

// execute 执行工具，并兜住 panic（工具实现有 bug 不能把 Agent 带崩）。
func (r *Registry) execute(ctx context.Context, tool Tool, args map[string]interface{}) (res Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%w: 工具 %s: %v", ErrToolPanic, tool.Name(), rec)
			res = Result{OK: false, Summary: "工具实现异常"}
		}
	}()
	return tool.Execute(ctx, args)
}

// admit 受理一次调用：判定会话预算与爆炸半径，通过则记账。
//
// 返回的 reason 是**具体哪道闸**，调用方据此写审计；这正是"被拒也要留痕且能定位"的实现点。
func (r *Registry) admit(sessionID, name string, risk RiskLevel, targets []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.sessionLocked(sessionID)
	if st.calls >= r.policy.MaxCallsPerSession {
		return ReasonSessionBudget, retry.Fatal(fmt.Errorf("%w: 会话 %s 已达 %d 次上限",
			ErrSessionBudgetExceeded, sessionID, r.policy.MaxCallsPerSession))
	}
	if st.perTool[name] >= r.policy.MaxCallsPerTool {
		return ReasonSessionBudget, retry.Fatal(fmt.Errorf("%w: 工具 %s 已达 %d 次上限（同一工具反复调用不产生新证据）",
			ErrSessionBudgetExceeded, name, r.policy.MaxCallsPerTool))
	}
	if risk.IsWrite() {
		if st.writeActions >= r.policy.MaxWriteActionsPerSession {
			return ReasonBlastRadius, retry.Fatal(fmt.Errorf("%w: 本会话已提交 %d 个写动作（上限 %d），需人工介入",
				ErrBlastRadiusExceeded, st.writeActions, r.policy.MaxWriteActionsPerSession))
		}
		if n := newHostCount(st.hosts, targets); n > r.policy.MaxAffectedHostsPerSession {
			return ReasonBlastRadius, retry.Fatal(fmt.Errorf("%w: 本会话写动作将影响 %d 台主机（上限 %d），需人工介入",
				ErrBlastRadiusExceeded, n, r.policy.MaxAffectedHostsPerSession))
		}
	}
	st.calls++
	st.perTool[name]++
	if risk.IsWrite() {
		st.writeActions++
		for _, h := range targets {
			st.hosts[h] = struct{}{}
		}
	}
	return "", nil
}

// refund 归还一次受理（审批单生成失败时用：不能让"没发生的动作"占用预算）。
func (r *Registry) refund(sessionID, name string, risk RiskLevel, targets []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		return
	}
	if st.calls > 0 {
		st.calls--
	}
	if st.perTool[name] > 0 {
		st.perTool[name]--
	}
	if risk.IsWrite() && st.writeActions > 0 {
		st.writeActions--
		// 目标集合无法精确回滚（多目标时可能与他人共享），因此**保守地保留**：
		// 宁可少放行一次，也不多放行一次。
		_ = targets
	}
}

// sessionLocked 取/建会话状态（调用方必须已持锁）。
func (r *Registry) sessionLocked(sessionID string) *sessionState {
	if r.sessions == nil {
		r.sessions = make(map[string]*sessionState)
	}
	st, ok := r.sessions[sessionID]
	if !ok {
		st = &sessionState{
			perTool:   make(map[string]int),
			hosts:     make(map[string]struct{}),
			deniedBy:  make(map[string]int),
			startedAt: time.Now(),
		}
		r.sessions[sessionID] = st
	}
	if st.perTool == nil {
		st.perTool = make(map[string]int)
	}
	if st.hosts == nil {
		st.hosts = make(map[string]struct{})
	}
	if st.deniedBy == nil {
		st.deniedBy = make(map[string]int)
	}
	return st
}

// recordDenied 记录一次被拒调用（被拒也算"模型想做的事"，审计必须留痕）。
func (r *Registry) recordDenied(sessionID, name string, args map[string]interface{}, risk RiskLevel, meta RunMeta, reason string) {
	r.mu.Lock()
	st := r.sessionLocked(sessionID)
	st.denied++
	if reason != "" {
		st.deniedBy[reason]++
	}
	r.mu.Unlock()

	rec := r.baseRecord(sessionID, name, args, risk, meta, nil, DecisionDenied, reason)
	rec.OK = false
	rec.Err = reason
	r.observe(rec)
}

// observe 安全触发审计回调，并累计统计。
func (r *Registry) observe(rec CallRecord) {
	// 统计与回调分离：统计必须有，回调可能被禁用
	r.mu.Lock()
	st, ok := r.sessions[rec.SessionID]
	if ok {
		st.evidence += rec.Evidence
		st.totalMillis += rec.DurationMS
		st.lastCallAt = time.Now()
		if rec.Decision == DecisionApprovalRequired {
			st.approvalRequired++
		}
	}
	observer := r.observer
	r.mu.Unlock()

	if observer == nil {
		return
	}
	defer func() { _ = recover() }()
	observer(rec)
}

// SessionStats 返回会话统计。
func (r *Registry) SessionStats(sessionID string) SessionStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		return SessionStats{SessionID: sessionID, PerTool: map[string]int{}, DeniedBy: map[string]int{}, Hosts: []string{}}
	}
	per := make(map[string]int, len(st.perTool))
	for k, v := range st.perTool {
		per[k] = v
	}
	deniedBy := make(map[string]int, len(st.deniedBy))
	for k, v := range st.deniedBy {
		deniedBy[k] = v
	}
	hosts := make([]string, 0, len(st.hosts))
	for h := range st.hosts {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return SessionStats{
		SessionID:        sessionID,
		Calls:            st.calls,
		PerTool:          per,
		Evidence:         st.evidence,
		Denied:           st.denied,
		StartedAt:        st.startedAt,
		LastCallAt:       st.lastCallAt,
		TotalMillis:      st.totalMillis,
		WriteActions:     st.writeActions,
		Hosts:            hosts,
		ApprovalRequired: st.approvalRequired,
		DeniedBy:         deniedBy,
	}
}

// ResetSession 清理会话统计（会话结束/复用 sessionID 时调用，避免配额被历史占用）。
func (r *Registry) ResetSession(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, sessionID)
}

// RemainingCalls 返回会话剩余工具调用次数（Agent 循环据此决定何时收尾）。
func (r *Registry) RemainingCalls(sessionID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		return r.policy.MaxCallsPerSession
	}
	if left := r.policy.MaxCallsPerSession - st.calls; left > 0 {
		return left
	}
	return 0
}

// truncateUTF8 按字节上限截断，但**不切断多字节字符**。
//
// 返回 (截断后的字符串, 是否发生了截断)。
func truncateUTF8(s string, max int) (string, bool) {
	if max <= 0 {
		return "", len(s) > 0
	}
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// newHostCount 计算"加上本次目标后"的去重主机数。
func newHostCount(existing map[string]struct{}, targets []string) int {
	seen := make(map[string]struct{}, len(existing)+len(targets))
	for h := range existing {
		seen[h] = struct{}{}
	}
	for _, t := range targets {
		if t == "" {
			continue
		}
		seen[t] = struct{}{}
	}
	return len(seen)
}

// impactSummary 人类可读的影响面摘要（审批台展示的第一行）。
//
// 宁可保守地写"影响面未声明"也不编造：审批人看到的具体数字必须来自事实，
// 一个写错的"影响 1 个任务"会直接导致误批。
func impactSummary(risk RiskLevel, targets []string, args map[string]interface{}) string {
	parts := make([]string, 0, 3)
	if n := len(targets); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 个目标（%s）", n, strings.Join(targets, "、")))
	} else {
		parts = append(parts, "影响面未声明（工具未返回具体目标）")
	}
	if risk == RiskDestructive {
		parts = append(parts, "不可逆动作")
	} else {
		parts = append(parts, "可逆动作")
	}
	if v := llm.StringOr(args, "reason", ""); v != "" {
		parts = append(parts, "理由："+v)
	}
	return strings.Join(parts, "；")
}
