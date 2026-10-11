package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// HTTPExecutor 执行 HTTP 步骤（健康检查、WebHook、告警接口等）。
//
// 与旧 HTTPHandler 的差别：
//   - 旧实现只支持 GET/POST 且不校验返回码，2xx/4xx/5xx 一律算成功；
//   - 新实现区分"网络错误/5xx/429（可重试）"与"4xx（配置/鉴权问题，不重试）"，
//     并支持自定义期望状态码集合。
type HTTPExecutor struct {
	Client       *http.Client
	MaxBodyBytes int64
}

// NewHTTPExecutor 创建默认 HTTP 执行器。
func NewHTTPExecutor() *HTTPExecutor {
	return &HTTPExecutor{
		Client: &http.Client{
			// 不用 Client.Timeout，超时统一由 context 控制（便于取消传播）
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     60 * time.Second,
			},
		},
		MaxBodyBytes: DefaultOutputLimit,
	}
}

func (e *HTTPExecutor) Kind() Kind { return KindHTTP }

func (e *HTTPExecutor) Validate(req Request) error {
	if strings.TrimSpace(req.URL) == "" {
		return fmt.Errorf("%w: http url is required", ErrInvalidRequest)
	}
	lower := strings.ToLower(req.URL)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return fmt.Errorf("%w: only http/https is supported", ErrInvalidRequest)
	}
	return nil
}

// method 返回规范化的大写方法名。
func (e *HTTPExecutor) method(req Request) string {
	m := strings.ToUpper(strings.TrimSpace(req.Method))
	if m == "" {
		if req.Body != "" {
			return http.MethodPost
		}
		return http.MethodGet
	}
	return m
}

// expect 判断状态码是否命中期望集合（默认任意 2xx 视为成功）。
func (e *HTTPExecutor) expect(req Request, status int) bool {
	if len(req.ExpectStatus) == 0 {
		return status >= 200 && status < 300
	}
	for _, want := range req.ExpectStatus {
		if want == status {
			return true
		}
	}
	return false
}

func (e *HTTPExecutor) Execute(ctx context.Context, req Request) Result {
	res := newResult(req)
	if err := e.Validate(req); err != nil {
		return res.finish(models.RunStatusFailed, retry.Fatal(err))
	}

	timeout := req.EffectiveTimeout()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := e.method(req)
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(runCtx, method, req.URL, body)
	if err != nil {
		return res.finish(models.RunStatusFailed, retry.Fatal(fmt.Errorf("构造请求失败: %w", err)))
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if req.Body != "" && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	client := e.Client
	if client == nil {
		client = NewHTTPExecutor().Client
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		if runCtx.Err() != nil && runCtx.Err() == context.DeadlineExceeded {
			res.Artifacts["timeout"] = timeout.String()
			return res.finish(models.RunStatusTimeout, retry.Retryable(fmt.Errorf("%w: 超过 %s", ErrTimeout, timeout)))
		}
		if ctx.Err() != nil {
			return res.finish(models.RunStatusCanceled, retry.Fatal(fmt.Errorf("执行被取消: %w", ctx.Err())))
		}
		// 连接失败/DNS/重置：可重试
		return res.finish(models.RunStatusFailed, retry.Retryable(fmt.Errorf("请求失败: %w", err)))
	}
	defer resp.Body.Close()

	// 输出上限：步骤级配置优先，执行器全局上限更严格时取更小值。
	limit := int64(req.OutputLimit())
	if e.MaxBodyBytes > 0 && e.MaxBodyBytes < limit {
		limit = e.MaxBodyBytes
	}
	text, truncated, readErr := copyLimited(resp.Body, int(limit))
	res.Output = text
	res.Truncated = truncated
	res.ExitCode = resp.StatusCode
	res.Artifacts["status_code"] = fmt.Sprintf("%d", resp.StatusCode)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		res.Artifacts["content_type"] = ct
	}
	if readErr != nil {
		return res.finish(models.RunStatusFailed, retry.Retryable(fmt.Errorf("读取响应失败: %w", readErr)))
	}
	if e.expect(req, resp.StatusCode) {
		return res.finish(models.RunStatusSuccess, nil)
	}
	return res.finish(models.RunStatusFailed, classifyStatus(resp.StatusCode))
}

// classifyStatus 按状态码分类可重试性。
//
//	408 请求超时 / 425 过早 / 429 限流 / 5xx 服务端错误 → 可重试
//	其它 4xx（400/401/403/404…）→ 不可重试（改配置或修权限才会好）
func classifyStatus(status int) error {
	base := fmt.Errorf("HTTP 状态码 %d", status)
	switch {
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests,
		status == 425, status >= 500:
		return retry.Retryable(base)
	default:
		return retry.Fatal(fmt.Errorf("%w（客户端错误，不重试）", base))
	}
}

var _ Executor = (*HTTPExecutor)(nil)
