// Package tools 是阶段 2 的**工具网关**：Agent 能做的所有副作用都必须经过这里。
//
// 核心立场（对应方案 §4「工具即权限边界」）：
//   - **只读优先**：阶段 2 只注册只读工具。真正的"执行动作"等到阶段 3 的
//     审批 + 爆炸半径预算落地后再放开，否则等于给模型一把没有护栏的 root shell；
//   - **不经过 shell**：`exec.CommandContext(binary, args...)` 直接执行，
//     从机制上消灭命令注入（而不是靠"过滤分号"）；
//   - **工具失败是观测，不是异常**：模型需要看到"这个工具失败了/没权限"，
//     才能改变策略。所以失败也会返回 Result（带 Err 字段），只有"根本不该调用"
//     （未知工具、越权、参数不合法、超预算）才返回 error；
//   - **每次调用都要留痕**：CallRecord 交给上层落库（阶段 3 变成 append-only 审计）。
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

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
)

// DefaultMaxOutputBytes 单次工具输出上限（64 KiB，与阶段 1 执行器的输出上限一致）。
const DefaultMaxOutputBytes = 64 * 1024

// Evidence 一条证据。诊断报告里的每条结论都必须挂证据，否则就是"编的"。
type Evidence struct {
	// Source 证据类型：log / step / node / sql / http / shell。
	Source string `json:"source"`
	// Ref 定位：文件:行、run_id、URL、SQL 摘要。
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
	// Meta 结构化附加信息（状态码、行数、命中数等）。
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
	// ReadOnly 是否只读。阶段 2 注册表**拒绝**非只读工具。
	ReadOnly() bool
	// Timeout 单次执行超时；0 表示用策略默认值。
	Timeout() time.Duration
	// Execute 执行。约定：不得 panic（注册表也会兜），
	// 返回 (Result, nil) 表示"拿到了观测"（哪怕业务上是失败），
	// 返回 error 表示"工具本身不可用"。
	Execute(ctx context.Context, args map[string]interface{}) (Result, error)
}

// CallRecord 一次工具调用的审计记录。
type CallRecord struct {
	SessionID   string                 `json:"session_id"`
	Tool        string                 `json:"tool"`
	Args        map[string]interface{} `json:"args,omitempty"`
	OK          bool                   `json:"ok"`
	Err         string                 `json:"err,omitempty"`
	DurationMS  int64                  `json:"duration_ms"`
	OutputBytes int                    `json:"output_bytes"`
	Evidence    int                    `json:"evidence"`
	Denied      bool                   `json:"denied,omitempty"`
}

// Observer 审计回调（不允许阻塞、不允许 panic 影响主流程）。
type Observer func(CallRecord)

// Policy 工具网关策略。
type Policy struct {
	// Allow 允许的工具名（空 = 所有已注册的只读工具）。
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
	// SQLAllowTables 预留：按表进一步收窄（阶段 3）。
	SQLAllowTables []string
}

// DefaultPolicy 默认策略（保守）。
func DefaultPolicy() Policy {
	return Policy{
		MaxCallsPerSession: 12,
		MaxCallsPerTool:    4,
		DefaultTimeout:     15 * time.Second,
		MaxOutputBytes:     DefaultMaxOutputBytes,
		ShellReadPaths:     []string{"/var/log/", "/var/log/crony/"},
		SQLMaxRows:         100,
	}
}

// Normalize 补齐零值。
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
}

type sessionState struct {
	calls       int
	perTool     map[string]int
	evidence    int
	denied      int
	startedAt   time.Time
	lastCallAt  time.Time
	totalMillis int64
}

// Registry 工具注册表 + 调用网关。
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]Tool
	order    []string
	policy   Policy
	observer Observer
	sessions map[string]*sessionState
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
	return r
}

// Register 注册工具。**拒绝非只读工具**（阶段 2 的硬约束）。
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tools: 工具为 nil")
	}
	name := strings.TrimSpace(t.Name())
	if name == "" || strings.ContainsAny(name, " \t") {
		return fmt.Errorf("tools: 工具名 %q 非法（不能为空或含空白）", name)
	}
	if !t.ReadOnly() {
		return fmt.Errorf("%w: %s 不是只读工具（阶段 2 只允许只读）", ErrToolDenied, name)
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
	out := make([]string, 0)
	for _, name := range r.Names() {
		if r.policy.allows(name) {
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

// Policy 返回当前策略副本。
func (r *Registry) Policy() Policy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.policy
}

// Call 调用一个工具（Agent 循环的唯一入口）。
func (r *Registry) Call(ctx context.Context, sessionID, name string, args map[string]interface{}) (Result, error) {
	if r == nil {
		return Result{}, retry.Fatal(fmt.Errorf("tools: 注册表未初始化"))
	}
	r.mu.RLock()
	tool, ok := r.tools[name]
	policy := r.policy
	r.mu.RUnlock()

	if !ok {
		// 把可用工具列表带上：模型下一轮可以自我纠正
		return Result{}, retry.Fatal(fmt.Errorf("%w: %q（可用: %s）", ErrUnknownTool, name, strings.Join(r.AllowedNames(), ", ")))
	}
	if !policy.allows(name) {
		r.recordDenied(sessionID, name, args)
		return Result{}, retry.Fatal(fmt.Errorf("%w: %s", ErrToolDenied, name))
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	if err := tool.Schema().Validate(args); err != nil {
		return Result{}, retry.Fatal(fmt.Errorf("%w: 工具 %s: %v", ErrInvalidArgs, name, err))
	}
	if err := r.reserve(sessionID, name); err != nil {
		return Result{}, err
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

	if err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("工具 %s 执行超时（%s）: %w", name, timeout, err)
		}
		res.OK = false
		if res.Err == "" {
			res.Err = llm.Redact(err.Error())
		}
		r.observe(CallRecord{SessionID: sessionID, Tool: name, Args: args, OK: false, Err: res.Err, DurationMS: res.DurationMS})
		return res, nil
	}

	// 输出截断（按字节，保留前 N 字节；截断标记必须显式）
	if len(res.Output) > policy.MaxOutputBytes {
		res.Output = res.Output[:policy.MaxOutputBytes]
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

	r.observe(CallRecord{
		SessionID:   sessionID,
		Tool:        name,
		Args:        args,
		OK:          res.OK,
		Err:         res.Err,
		DurationMS:  res.DurationMS,
		OutputBytes: len(res.Output),
		Evidence:    len(res.Evidence),
	})
	return res, nil
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

// reserve 预占会话配额。
func (r *Registry) reserve(sessionID, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = make(map[string]*sessionState)
	}
	st, ok := r.sessions[sessionID]
	if !ok {
		st = &sessionState{perTool: make(map[string]int), startedAt: time.Now()}
		r.sessions[sessionID] = st
	}
	if st.calls >= r.policy.MaxCallsPerSession {
		return retry.Fatal(fmt.Errorf("%w: 会话 %s 已达 %d 次上限", ErrSessionBudgetExceeded, sessionID, r.policy.MaxCallsPerSession))
	}
	if st.perTool[name] >= r.policy.MaxCallsPerTool {
		return retry.Fatal(fmt.Errorf("%w: 工具 %s 已达 %d 次上限（同一工具反复调用不产生新证据）", ErrSessionBudgetExceeded, name, r.policy.MaxCallsPerTool))
	}
	st.calls++
	st.perTool[name]++
	return nil
}

// recordDenied 记录一次被拒调用（被拒也算"模型想做的事"，审计必须留痕）。
func (r *Registry) recordDenied(sessionID, name string, args map[string]interface{}) {
	r.mu.Lock()
	if r.sessions == nil {
		r.sessions = make(map[string]*sessionState)
	}
	st, ok := r.sessions[sessionID]
	if !ok {
		st = &sessionState{perTool: make(map[string]int), startedAt: time.Now()}
		r.sessions[sessionID] = st
	}
	st.denied++
	r.mu.Unlock()
	r.observe(CallRecord{SessionID: sessionID, Tool: name, Args: args, OK: false, Denied: true, Err: "被策略拒绝"})
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
		return SessionStats{SessionID: sessionID, PerTool: map[string]int{}}
	}
	per := make(map[string]int, len(st.perTool))
	for k, v := range st.perTool {
		per[k] = v
	}
	return SessionStats{
		SessionID:   sessionID,
		Calls:       st.calls,
		PerTool:     per,
		Evidence:    st.evidence,
		Denied:      st.denied,
		StartedAt:   st.startedAt,
		LastCallAt:  st.lastCallAt,
		TotalMillis: st.totalMillis,
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
