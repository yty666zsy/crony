package llm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tmnhs/crony/common/pkg/retry"
)

// CallRecord 一次调用的审计记录（由 Gateway 产出，交给调用方落库）。
//
// 为什么把"审计"做成回调而不是直接写库：
//   - common 包不能依赖 admin/node 的数据层，否则测试必须起数据库；
//   - 阶段 3 要把同样的记录写进 append-only 审计表，回调式改起来只动一处。
type CallRecord struct {
	Provider      string            `json:"provider"`
	Model         string            `json:"model"`
	PromptVersion string            `json:"prompt_version,omitempty"`
	Usage         Usage             `json:"usage"`
	LatencyMS     int64             `json:"latency_ms"`
	OK            bool              `json:"ok"`
	SchemaOK      bool              `json:"schema_ok"`
	Err           string            `json:"err,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// Observer 审计回调。约定：**不允许阻塞**（由调用方决定是否异步落库），
// 且实现内部不能 panic 影响主流程。
type Observer func(CallRecord)

// Gateway LLM 网关。
type Gateway struct {
	mu        sync.RWMutex
	providers map[string]Provider
	order     []string
	def       string
	tracker   *Tracker
	observer  Observer
}

// Option 构造函数选项。
type Option func(*Gateway)

// WithProvider 注册 provider（第一个注册的自动成为默认）。
func WithProvider(p Provider) Option {
	return func(g *Gateway) { g.AddProvider(p) }
}

// WithDefaultProvider 指定默认 provider。
func WithDefaultProvider(name string) Option {
	return func(g *Gateway) {
		if _, ok := g.providers[name]; ok {
			g.def = name
		}
	}
}

// WithBudget 设置会话预算。
func WithBudget(b Budget) Option {
	return func(g *Gateway) { g.tracker = NewTracker(b) }
}

// WithTracker 注入记账器（多会话共享/测试注入时钟）。
func WithTracker(t *Tracker) Option {
	return func(g *Gateway) { g.tracker = t }
}

// WithObserver 设置审计回调。
func WithObserver(o Observer) Option {
	return func(g *Gateway) { g.observer = o }
}

// NewGateway 创建网关。
func NewGateway(opts ...Option) *Gateway {
	g := &Gateway{
		providers: make(map[string]Provider),
		tracker:   NewTracker(DefaultBudget()),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	return g
}

// NewOfflineGateway 创建一个"离线可用"的网关：没有真实模型时也能跑通 Agent 全链路。
//
// 这是刻意的默认值：**宁可默认不可用（离线）也不要默认悄悄连外网**。
func NewOfflineGateway(budget Budget, script ...Completion) *Gateway {
	return NewGateway(
		WithProvider(NewFakeProvider("offline", script...)),
		WithDefaultProvider("offline"),
		WithBudget(budget),
	)
}

// AddProvider 注册/覆盖 provider。
func (g *Gateway) AddProvider(p Provider) {
	if g == nil || p == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.providers == nil {
		g.providers = make(map[string]Provider, 1)
	}
	if _, exists := g.providers[p.Name()]; !exists {
		g.order = append(g.order, p.Name())
	}
	g.providers[p.Name()] = p
	if g.def == "" {
		g.def = p.Name()
	}
}

// ProviderNames 返回已注册 provider（按注册顺序，便于界面稳定展示）。
func (g *Gateway) ProviderNames() []string {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, len(g.order))
	copy(out, g.order)
	return out
}

// Default 返回默认 provider 名。
func (g *Gateway) Default() string {
	if g == nil {
		return ""
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.def
}

// HasProvider 是否至少有一个可用 provider。
func (g *Gateway) HasProvider() bool { return len(g.ProviderNames()) > 0 }

// Tracker 返回记账器。
func (g *Gateway) Tracker() *Tracker {
	if g == nil {
		return nil
	}
	return g.tracker
}

// Budget 返回预算快照。
func (g *Gateway) Budget() BudgetSnapshot { return g.Tracker().Snapshot() }

// providerFor 解析本次请求使用哪个 provider。
func (g *Gateway) providerFor(name string) (Provider, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if name == "" {
		name = g.def
	}
	if name == "" {
		return nil, fmt.Errorf("%w: 未配置任何 LLM provider（阶段 2 默认离线，请在配置中注入）", ErrNotConfigured)
	}
	p, ok := g.providers[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s（已注册: %v）", ErrNoProvider, name, g.order)
	}
	return p, nil
}

// Complete 执行一次调用：预算检查 → provider 调用 → 记账 → 输出校验 → 审计。
//
// 语义要点：
//   - **预算耗尽 → 不可重试错误**（ErrBudgetExceeded）。继续重试只会烧更多钱；
//   - **输出不符合 Schema → 不可重试错误**（ErrSchemaViolation）。重发同样的提示词
//     大概率得到同样的跑偏结果；正确做法是 Agent 层发起一次"修复轮"（见 agent 包），
//     或者直接把"证据不足"作为结论输出；
//   - 本次调用超预算（Charge 报错）但已经成功时，**仍然返回成功结果**，
//     由下一次 Check 拦截，避免"明明拿到了答案却丢掉"。
//   - **预算按 ctx 绑定**：优先用 `ContextWithTracker` 注入的记账器（一次 Agent 运行一个），
//     没有绑定时才回退到网关自身的 tracker（测试/单次调用场景）。
//     这条纠正了"进程级预算"的真实缺陷：见 budget.go 中 Tracker 的说明。
func (g *Gateway) Complete(ctx context.Context, req Request) (Completion, error) {
	if g == nil {
		return Completion{}, retry.Fatal(fmt.Errorf("%w: gateway 未初始化", ErrNotConfigured))
	}
	tr := TrackerFromContext(ctx)
	if tr == nil {
		tr = g.tracker
	}
	if err := tr.Check(); err != nil {
		g.observe(CallRecord{PromptVersion: req.PromptVersion, OK: false, Err: err.Error(), Metadata: req.Metadata})
		return Completion{}, err
	}
	provider, err := g.providerFor(req.Provider)
	if err != nil {
		g.observe(CallRecord{PromptVersion: req.PromptVersion, OK: false, Err: err.Error(), Metadata: req.Metadata})
		return Completion{}, retry.Fatal(err)
	}

	started := time.Now()
	comp, callErr := provider.Complete(ctx, req)
	latency := time.Since(started)
	if comp.Latency <= 0 {
		comp.Latency = latency
	}
	if comp.Provider == "" {
		comp.Provider = provider.Name()
	}

	record := CallRecord{
		Provider:      comp.Provider,
		Model:         comp.Model,
		PromptVersion: req.PromptVersion,
		Usage:         comp.Usage,
		LatencyMS:     latency.Milliseconds(),
		Metadata:      req.Metadata,
	}
	if callErr != nil {
		record.OK = false
		record.Err = Redact(callErr.Error())
		// 失败的调用同样消耗了配额（429/超时都算），必须记账
		_ = tr.Charge(comp.Usage, latency)
		g.observe(record)
		return comp, callErr
	}

	chargeErr := tr.Charge(comp.Usage, latency)

	if req.ResponseSchema != nil {
		structured, verr := ValidateStructured(req.ResponseSchema, comp.Content)
		if verr != nil {
			record.OK = true
			record.SchemaOK = false
			record.Err = verr.Error()
			g.observe(record)
			return comp, retry.Fatal(verr)
		}
		comp.Structured = structured
	}
	record.OK = true
	record.SchemaOK = req.ResponseSchema != nil
	if chargeErr != nil {
		// 记在记录里但不影响本次结果
		record.Err = "本次调用后已超预算: " + chargeErr.Error()
	}
	g.observe(record)
	return comp, nil
}

// CompleteStructured 便捷方法：要求结构化输出并直接返回对象。
func (g *Gateway) CompleteStructured(ctx context.Context, req Request) (map[string]interface{}, Completion, error) {
	if req.ResponseSchema == nil {
		return nil, Completion{}, retry.Fatal(fmt.Errorf("%w: ResponseSchema 不能为空", ErrSchemaViolation))
	}
	comp, err := g.Complete(ctx, req)
	if err != nil {
		return nil, comp, err
	}
	if comp.Structured == nil {
		return nil, comp, retry.Fatal(fmt.Errorf("%w: 结构化结果为空", ErrSchemaViolation))
	}
	return comp.Structured, comp, nil
}

// observe 安全地触发审计回调。
func (g *Gateway) observe(record CallRecord) {
	if g == nil || g.observer == nil {
		return
	}
	defer func() {
		// 审计回调不应该影响主流程（回调里可能写库失败）
		_ = recover()
	}()
	g.observer(record)
}
