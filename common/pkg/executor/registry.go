package executor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// Registry 执行器注册表：调度侧只依赖接口，新增执行方式只需注册，
// 不改动编排与调度代码（阶段 2 的 Agent 执行器、阶段 1 后续的容器/SQL 执行器都走这里）。
type Registry struct {
	mu    sync.RWMutex
	execs map[Kind]Executor
}

// NewRegistry 创建注册表并注册给定执行器。
func NewRegistry(execs ...Executor) *Registry {
	r := &Registry{execs: make(map[Kind]Executor, len(execs))}
	for _, e := range execs {
		r.Register(e)
	}
	return r
}

// Register 注册/覆盖一个执行器。传入 nil 会被忽略（避免 nil 接口 panic）。
func (r *Registry) Register(e Executor) {
	if e == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.execs == nil {
		r.execs = make(map[Kind]Executor, 1)
	}
	r.execs[e.Kind()] = e
}

// Get 取出执行器。
func (r *Registry) Get(kind Kind) (Executor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.execs[kind]
	return e, ok
}

// Kinds 返回已注册的执行器类型（排序，便于稳定输出到接口/日志）。
func (r *Registry) Kinds() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	kinds := make([]Kind, 0, len(r.execs))
	for kind := range r.execs {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}

// Run 是调度侧的便捷入口：查表 → 校验 → 执行。
// 任何一步失败都返回结构化 Result（调用方只需看 Result.Status），
// 避免"忘记处理 error 分支导致步骤静默丢失"。
func (r *Registry) Run(ctx context.Context, req Request) Result {
	res := newResult(req)
	if err := ValidateRequest(req); err != nil {
		return res.finish(models.RunStatusFailed, retry.Fatal(err))
	}
	exec, ok := r.Get(req.Kind)
	if !ok {
		return res.finish(models.RunStatusFailed,
			retry.Fatal(fmt.Errorf("%w: %s", ErrNotFound, req.Kind)))
	}
	if err := exec.Validate(req); err != nil {
		return res.finish(models.RunStatusFailed, retry.Fatal(err))
	}
	return exec.Execute(ctx, req)
}

// DefaultRegistry 默认注册表：shell + http。
func DefaultRegistry() *Registry {
	return NewRegistry(NewShellExecutor(), NewHTTPExecutor())
}

// KindOf 从字符串解析执行器类型（控制台/接口传入）。
func KindOf(raw string) (Kind, bool) {
	switch Kind(strings.ToLower(strings.TrimSpace(raw))) {
	case KindShell:
		return KindShell, true
	case KindHTTP:
		return KindHTTP, true
	default:
		return "", false
	}
}

// ValidateRequest 校验请求的基本字段（类型必需、RunID 必需），
// 供上层在入队前快速拒绝脏数据。
func ValidateRequest(req Request) error {
	if req.RunID == "" {
		return fmt.Errorf("%w: run_id is required", ErrInvalidRequest)
	}
	if req.Kind == "" {
		return fmt.Errorf("%w: kind is required", ErrInvalidRequest)
	}
	switch req.Kind {
	case KindShell:
		if !req.RawArgv && len(req.Args) == 0 && strings.TrimSpace(req.Command) == "" {
			return fmt.Errorf("%w: shell command is required", ErrInvalidRequest)
		}
		if req.RawArgv && strings.TrimSpace(req.Command) == "" {
			return fmt.Errorf("%w: raw argv command is required", ErrInvalidRequest)
		}
	case KindHTTP:
		if strings.TrimSpace(req.URL) == "" {
			return fmt.Errorf("%w: http url is required", ErrInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: unsupported kind %q", ErrInvalidRequest, req.Kind)
	}
	return nil
}
