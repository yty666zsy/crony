package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// newTestProvider 起一个假的 OpenAI 兼容端点，返回捕获到的请求体。
func newTestProvider(t *testing.T, status int, response string, mutate func(*OpenAICompatConfig)) (*OpenAICompatProvider, *captureHandler) {
	t.Helper()
	handler := &captureHandler{status: status, response: response}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cfg := OpenAICompatConfig{
		Name:    "test-provider",
		BaseURL: server.URL + "/v1",
		APIKey:  "sk-test-secret-value",
		Model:   "test-model",
		Timeout: 5 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := NewOpenAICompatProvider(cfg)
	if err != nil {
		t.Fatalf("构造 provider 失败: %v", err)
	}
	return p, handler
}

type captureHandler struct {
	mu       sync.Mutex
	status   int
	response string
	requests []map[string]interface{}
	rawBody  []byte
	authHdr  string
	delay    time.Duration
}

func (h *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	var parsed map[string]interface{}
	_ = json.Unmarshal(body, &parsed)
	h.requests = append(h.requests, parsed)
	h.rawBody = body
	h.authHdr = r.Header.Get("Authorization")
	delay := h.delay
	status := h.status
	response := h.response
	h.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(response))
}

func (h *captureHandler) lastRequest(t *testing.T) map[string]interface{} {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.requests) == 0 {
		t.Fatal("端点没有收到任何请求")
	}
	return h.requests[len(h.requests)-1]
}

const okResponse = `{
  "model": "test-model-2024",
  "choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"结论：磁盘写满"}}],
  "usage": {"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150}
}`

func TestOpenAICompatHappyPath(t *testing.T) {
	p, handler := newTestProvider(t, 200, okResponse, nil)

	temp := 0.2
	comp, err := p.Complete(context.Background(), Request{
		Messages:    []Message{System("你是运维助手"), User("为什么失败？")},
		Temperature: &temp,
		MaxTokens:   512,
		Tools: []ToolSpec{{
			Name:        "node_status",
			Description: "查询节点状态",
			Parameters:  jsonschema.Object(map[string]*jsonschema.Schema{"node": jsonschema.String("节点")}, "node"),
		}},
	})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if comp.Content != "结论：磁盘写满" {
		t.Fatalf("内容不符: %q", comp.Content)
	}
	if comp.Usage.TotalTokens != 150 || comp.Usage.PromptTokens != 120 {
		t.Fatalf("usage 解析不符: %+v", comp.Usage)
	}
	if comp.Model != "test-model-2024" {
		t.Fatalf("模型名应回写端点返回值: %q", comp.Model)
	}
	if comp.Provider != "test-provider" || comp.FinishReason != "stop" {
		t.Fatalf("provider/finish_reason 不符: %+v", comp)
	}

	req := handler.lastRequest(t)
	if req["model"] != "test-model" {
		t.Fatalf("未使用默认模型: %v", req["model"])
	}
	if req["max_tokens"] != float64(512) {
		t.Fatalf("max_tokens 未透传: %v", req["max_tokens"])
	}
	if req["temperature"] != 0.2 {
		t.Fatalf("temperature 未透传: %v", req["temperature"])
	}
	msgs, _ := req["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("消息数不符: %v", req["messages"])
	}
	tools, _ := req["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("tools 未透传: %v", req["tools"])
	}
	tool := tools[0].(map[string]interface{})
	if tool["type"] != "function" {
		t.Fatalf("tool 类型应为 function: %v", tool)
	}
	if _, ok := tool["function"].(map[string]interface{})["parameters"]; !ok {
		t.Fatalf("tool 缺少 parameters: %v", tool)
	}
	if handler.authHdr != "Bearer sk-test-secret-value" {
		t.Fatalf("鉴权头不符: %q", handler.authHdr)
	}
}

// TestToolCallArgumentsNormalization 参数既可能是 JSON 字符串（标准）也可能是对象（部分自建端点）。
func TestToolCallArgumentsNormalization(t *testing.T) {
	asString := `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"node_status","arguments":"{\"node\":\"n1\"}"}}]}}],"usage":{}}`
	p, _ := newTestProvider(t, 200, asString, nil)
	comp, err := p.Complete(context.Background(), Request{Messages: []Message{User("go")}})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !comp.HasToolCalls() || len(comp.ToolCalls) != 1 {
		t.Fatalf("工具调用解析失败: %+v", comp)
	}
	args, err := comp.ToolCalls[0].DecodeArgs()
	if err != nil || args["node"] != "n1" {
		t.Fatalf("参数解码失败: %v %v", args, err)
	}

	asObject := `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[
		{"type":"function","function":{"name":"node_status","arguments":{"node":"n2"}}}]}}],"usage":{}}`
	p2, _ := newTestProvider(t, 200, asObject, nil)
	comp2, err := p2.Complete(context.Background(), Request{Messages: []Message{User("go")}})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	args2, err := comp2.ToolCalls[0].DecodeArgs()
	if err != nil || args2["node"] != "n2" {
		t.Fatalf("对象形态参数解码失败: %v %v", args2, err)
	}
	// 端点没给 id 时必须自动补一个，否则工具结果无法回灌
	if comp2.ToolCalls[0].ID == "" {
		t.Fatal("缺失的 tool_call id 应被补全")
	}
}

func TestHTTPErrorClassification(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantIs        error
	}{
		{name: "401 鉴权失败", status: 401, body: `{"error":{"message":"invalid api key"}}`, wantRetryable: false, wantIs: ErrNotConfigured},
		{name: "403 无权限", status: 403, body: `{"error":{"message":"forbidden"}}`, wantRetryable: false, wantIs: ErrNotConfigured},
		{name: "400 请求错误", status: 400, body: `{"error":{"message":"bad request"}}`, wantRetryable: false, wantIs: ErrInvalidResponse},
		{name: "429 限流", status: 429, body: `{"error":{"message":"rate limit"}}`, wantRetryable: true, wantIs: ErrRateLimited},
		{name: "500 服务端故障", status: 500, body: `{"error":{"message":"boom"}}`, wantRetryable: true, wantIs: ErrInvalidResponse},
		{name: "503 不可用", status: 503, body: `upstream unavailable`, wantRetryable: true, wantIs: ErrInvalidResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newTestProvider(t, tc.status, tc.body, nil)
			_, err := p.Complete(context.Background(), Request{Messages: []Message{User("hi")}})
			if err == nil {
				t.Fatal("期望报错")
			}
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("错误类型不符: %v（期望 %v）", err, tc.wantIs)
			}
			got := retry.Classify(err).Retryable
			if got != tc.wantRetryable {
				t.Fatalf("可重试判定不符: 实际 %v 期望 %v（err=%v）", got, tc.wantRetryable, err)
			}
		})
	}
}

// TestErrorDoesNotLeakAPIKey 错误信息里绝不能带出密钥（它会落库到 agent_step）。
func TestErrorDoesNotLeakAPIKey(t *testing.T) {
	body := `{"error":{"message":"invalid key sk-test-secret-value please check"}}`
	p, _ := newTestProvider(t, 401, body, nil)
	_, err := p.Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if err == nil {
		t.Fatal("期望报错")
	}
	if strings.Contains(err.Error(), "sk-test-secret-value") {
		t.Fatalf("错误信息泄漏了密钥: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("密钥应被替换为 ***: %v", err)
	}
}

func TestTimeoutIsRetryable(t *testing.T) {
	p, handler := newTestProvider(t, 200, okResponse, func(cfg *OpenAICompatConfig) {
		cfg.Timeout = 60 * time.Millisecond
	})
	handler.mu.Lock()
	handler.delay = 400 * time.Millisecond
	handler.mu.Unlock()

	_, err := p.Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if err == nil {
		t.Fatal("期望超时")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("应识别为超时: %v", err)
	}
	if !retry.Classify(err).Retryable {
		t.Fatalf("超时应可重试: %v", err)
	}
}

func TestCanceledContextIsNotRetryable(t *testing.T) {
	p, _ := newTestProvider(t, 200, okResponse, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Complete(ctx, Request{Messages: []Message{User("hi")}})
	if err == nil {
		t.Fatal("期望报错")
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("主动取消不应被当成超时: %v", err)
	}
	if retry.Classify(err).Retryable {
		t.Fatalf("主动取消不应重试: %v（原因: %s）", err, retry.Classify(err).Reason)
	}
}

func TestEmptyResponseAndOversizeAreRejected(t *testing.T) {
	p, _ := newTestProvider(t, 200, `{"choices":[],"message":"no model"}`, nil)
	_, err := p.Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if err == nil || !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("空 choices 应报 ErrInvalidResponse: %v", err)
	}

	big := strings.Repeat("x", 4096)
	p2, _ := newTestProvider(t, 200, `{"choices":[{"message":{"content":"`+big+`"}}]}`, func(cfg *OpenAICompatConfig) {
		cfg.MaxResponseBytes = 1024
	})
	_, err = p2.Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超长响应应被拒绝: %v", err)
	}
}

func TestBuildBodyValidation(t *testing.T) {
	p, _ := newTestProvider(t, 200, okResponse, nil)
	if _, err := p.Complete(context.Background(), Request{}); err == nil {
		t.Fatal("空 messages 应被拒绝")
	}
	p2, err := NewOpenAICompatProvider(OpenAICompatConfig{BaseURL: "http://127.0.0.1:1/v1"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := p2.Complete(context.Background(), Request{Messages: []Message{User("hi")}}); err == nil {
		t.Fatal("空模型名应被拒绝")
	}
	if _, err := NewOpenAICompatProvider(OpenAICompatConfig{}); err == nil {
		t.Fatal("空 base_url 应被拒绝")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "纯 JSON", in: `{"a":1}`, want: `{"a":1}`, ok: true},
		{name: "代码围栏", in: "```json\n{\"a\":1}\n```", want: `{"a":1}`, ok: true},
		{name: "无语言围栏", in: "```\n{\"a\":1}\n```", want: `{"a":1}`, ok: true},
		{name: "前后有解释文字", in: `好的，结果如下：{"a":{"b":"}"}} 希望有帮助`, want: `{"a":{"b":"}"}}`, ok: true},
		{name: "数组", in: `[{"a":1}]`, want: `[{"a":1}]`, ok: true},
		{name: "字符串内的括号", in: `{"cmd":"echo { ) \""}`, want: `{"cmd":"echo { ) \""}`, ok: true},
		{name: "转义引号", in: `{"q":"a\"b","n":1}`, want: `{"q":"a\"b","n":1}`, ok: true},
		{name: "配不上括号", in: `{"a":1`, ok: false},
		{name: "没有 JSON", in: `抱歉，我无法回答`, ok: false},
		{name: "空", in: `  `, ok: false},
		{name: "语法错误不改写", in: `{'a':1}`, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ExtractJSON(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok 不符: 实际 %v 期望 %v（got=%q）", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Fatalf("提取结果不符: %q vs %q", got, tc.want)
			}
		})
	}
}

func TestValidateStructured(t *testing.T) {
	schema := jsonschema.Object(map[string]*jsonschema.Schema{
		"cause":      jsonschema.String("原因").WithLength(1, 200),
		"confidence": &jsonschema.Schema{Type: jsonschema.TypeNumber},
	}, "cause", "confidence")

	obj, err := ValidateStructured(schema, "```json\n{\"cause\":\"磁盘写满\",\"confidence\":0.7}\n```")
	if err != nil {
		t.Fatalf("期望通过: %v", err)
	}
	if obj["cause"] != "磁盘写满" {
		t.Fatalf("字段不符: %#v", obj)
	}
	if _, err := ValidateStructured(schema, `{"reason":"写了错字段"}`); err == nil {
		t.Fatal("缺必填字段应失败")
	}
	if _, err := ValidateStructured(schema, `完全不是 JSON`); err == nil {
		t.Fatal("非 JSON 应失败")
	}
	if _, err := ValidateStructured(nil, `{}`); err == nil {
		t.Fatal("nil schema 应失败")
	}
}

func TestBudgetTracker(t *testing.T) {
	t.Run("调用次数上限", func(t *testing.T) {
		tr := NewTracker(Budget{MaxCalls: 2})
		if err := tr.Check(); err != nil {
			t.Fatalf("初始应可用: %v", err)
		}
		if err := tr.Charge(Usage{TotalTokens: 10}, 0); err != nil {
			t.Fatalf("第 1 次不应超: %v", err)
		}
		if err := tr.Charge(Usage{TotalTokens: 10}, 0); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("第 2 次达到上限应报预算耗尽: %v", err)
		}
		if err := tr.Check(); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("耗尽后 Check 应拦截: %v", err)
		}
		if retry.Classify(tr.Check()).Retryable {
			t.Fatal("预算耗尽不可重试")
		}
		snap := tr.Snapshot()
		if snap.Calls != 2 || snap.RemainingCalls != 0 || !snap.Exceeded {
			t.Fatalf("快照不符: %+v", snap)
		}
	})

	t.Run("token 上限", func(t *testing.T) {
		tr := NewTracker(Budget{MaxTokens: 100})
		if err := tr.Charge(Usage{PromptTokens: 60, CompletionTokens: 45}, 0); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("token 超限应报错: %v", err)
		}
		if snap := tr.Snapshot(); snap.Tokens != 105 || snap.PromptTokens != 60 || snap.CompletionTokens != 45 {
			t.Fatalf("记账明细不符: %+v", snap)
		}
	})

	t.Run("墙钟上限（注入时钟）", func(t *testing.T) {
		now := time.Now()
		tr := NewTrackerWithClock(Budget{MaxDuration: time.Minute}, func() time.Time { return now })
		if err := tr.Check(); err != nil {
			t.Fatalf("初始应可用: %v", err)
		}
		now = now.Add(2 * time.Minute)
		if err := tr.Check(); err != nil {
			t.Fatalf("Check 只看显式标记：时长在 Charge 时才判定: %v", err)
		}
		if err := tr.Charge(Usage{TotalTokens: 1}, 0); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("超时应报预算耗尽: %v", err)
		}
	})

	t.Run("无限制", func(t *testing.T) {
		tr := NewTracker(Budget{})
		for i := 0; i < 50; i++ {
			if err := tr.Charge(Usage{TotalTokens: 1000}, time.Second); err != nil {
				t.Fatalf("无限制不应报错: %v", err)
			}
		}
		snap := tr.Snapshot()
		if snap.RemainingCalls != -1 || snap.RemainingTokens != -1 || snap.Exceeded {
			t.Fatalf("无限制快照应标记 -1: %+v", snap)
		}
	})
}

func TestGatewayRoutingAndObserver(t *testing.T) {
	var mu sync.Mutex
	var records []CallRecord
	provider, _ := newTestProvider(t, 200, okResponse, nil)
	g := NewGateway(
		WithProvider(provider),
		WithBudget(Budget{MaxCalls: 5, MaxTokens: 10000}),
		WithObserver(func(r CallRecord) {
			mu.Lock()
			records = append(records, r)
			mu.Unlock()
		}),
	)
	if g.Default() != "test-provider" {
		t.Fatalf("第一个注册的 provider 应成为默认: %q", g.Default())
	}
	if !g.HasProvider() || len(g.ProviderNames()) != 1 {
		t.Fatalf("provider 列表不符: %v", g.ProviderNames())
	}
	comp, err := g.Complete(context.Background(), Request{
		Messages:      []Message{User("hi")},
		PromptVersion: "diag-v1",
		Metadata:      map[string]string{"session_id": "s1"},
	})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if comp.Provider != "test-provider" {
		t.Fatalf("provider 未回填: %+v", comp)
	}
	if g.Budget().Calls != 1 || g.Budget().Tokens != 150 {
		t.Fatalf("网关未记账: %+v", g.Budget())
	}
	mu.Lock()
	if len(records) != 1 || records[0].PromptVersion != "diag-v1" || !records[0].OK || records[0].Metadata["session_id"] != "s1" {
		t.Fatalf("审计记录不符: %+v", records)
	}
	mu.Unlock()

	// 未注册的 provider → 不可重试
	_, err = g.Complete(context.Background(), Request{Messages: []Message{User("hi")}, Provider: "不存在"})
	if !errors.Is(err, ErrNoProvider) || retry.Classify(err).Retryable {
		t.Fatalf("未知 provider 应为不可重试错误: %v", err)
	}
	// 审计回调 panic 不能影响主流程
	g2 := NewGateway(WithProvider(provider), WithObserver(func(CallRecord) { panic("审计炸了") }))
	if _, err := g2.Complete(context.Background(), Request{Messages: []Message{User("hi")}}); err != nil {
		t.Fatalf("审计回调 panic 不应影响调用: %v", err)
	}
}

func TestGatewaySchemaViolationAndBudgetExhaustion(t *testing.T) {
	schema := jsonschema.Object(map[string]*jsonschema.Schema{"cause": jsonschema.String("原因")}, "cause")
	provider, _ := newTestProvider(t, 200, `{"choices":[{"message":{"content":"{\"wrong\":1}"}}],"usage":{"total_tokens":10}}`, nil)
	g := NewGateway(WithProvider(provider), WithBudget(Budget{MaxCalls: 2}))

	comp, err := g.Complete(context.Background(), Request{Messages: []Message{User("hi")}, ResponseSchema: schema})
	if !errors.Is(err, ErrSchemaViolation) {
		t.Fatalf("应报输出不符合 Schema: %v", err)
	}
	if retry.Classify(err).Retryable {
		t.Fatalf("Schema 不符应为不可重试（由 Agent 层做修复轮）: %v", err)
	}
	if comp.Content == "" {
		t.Fatal("失败时也必须把原始输出带回来（轨迹里要留证据）")
	}

	// 预算耗尽：第 2 次调用后超限，第 3 次被拦
	good := NewFakeProvider("fake")
	g2 := NewGateway(WithProvider(good), WithBudget(Budget{MaxCalls: 2}))
	for i := 0; i < 2; i++ {
		if _, err := g2.Complete(context.Background(), Request{Messages: []Message{User("hi")}}); err != nil {
			t.Fatalf("第 %d 次调用不应失败: %v", i+1, err)
		}
	}
	_, err = g2.Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("预算耗尽应被拦截: %v", err)
	}
	if retry.Classify(err).Retryable {
		t.Fatal("预算耗尽不可重试")
	}
	// 耗尽时不应真的打到 provider
	if good.CallCount() != 2 {
		t.Fatalf("预算耗尽后不应再调用 provider，实际 %d 次", good.CallCount())
	}
}

func TestOfflineGatewayAndFakeProvider(t *testing.T) {
	g := NewOfflineGateway(Budget{MaxCalls: 10}, FakeToolCall("node_status", map[string]interface{}{"node": "n1"}), FakeText("完毕"))
	if g.Default() != "offline" {
		t.Fatalf("离线网关默认 provider 应为 offline: %q", g.Default())
	}
	comp, err := g.Complete(context.Background(), Request{Messages: []Message{User("查一下节点")}})
	if err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if !comp.HasToolCalls() || comp.ToolCalls[0].Name != "node_status" {
		t.Fatalf("脚本化工具调用不符: %+v", comp)
	}
	comp2, err := g.Complete(context.Background(), Request{Messages: []Message{User("继续")}})
	if err != nil || comp2.Content != "完毕" {
		t.Fatalf("第二次脚本不符: %v %+v", err, comp2)
	}
	// 脚本用尽后必须给出"不再调用工具"的收尾响应，避免测试里死循环
	comp3, err := g.Complete(context.Background(), Request{Messages: []Message{User("继续")}})
	if err != nil || comp3.HasToolCalls() {
		t.Fatalf("脚本用尽后不应再返回工具调用: %v %+v", err, comp3)
	}
}

func TestFakeProviderRecordsRequestsAndErrors(t *testing.T) {
	sentinel := errors.New("模型炸了")
	fake := NewFakeProvider("fake").WithErrors(sentinel).WithResponder(nil)
	g := NewGateway(WithProvider(fake))
	if _, err := g.Complete(context.Background(), Request{Messages: []Message{User("q")}}); !errors.Is(err, sentinel) {
		t.Fatalf("错误应透传: %v", err)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 || reqs[0].Messages[0].Content != "q" {
		t.Fatalf("请求未被记录: %+v", reqs)
	}

	fake2 := NewFakeProvider("fake2").WithUsage(Usage{PromptTokens: 7, CompletionTokens: 3})
	g2 := NewGateway(WithProvider(fake2))
	if _, err := g2.Complete(context.Background(), Request{Messages: []Message{User("q")}}); err != nil {
		t.Fatalf("期望成功: %v", err)
	}
	if snap := g2.Budget(); snap.Tokens != 10 || snap.PromptTokens != 7 {
		t.Fatalf("模拟用量未记账: %+v", snap)
	}
}

func TestRedactAndTruncate(t *testing.T) {
	raw := `Authorization: Bearer sk-abcdef123456 failed, plus key sk-xyz789abcd here`
	got := Redact(raw)
	if strings.Contains(got, "sk-abcdef123456") || strings.Contains(got, "sk-xyz789abcd") {
		t.Fatalf("密钥未抹掉: %s", got)
	}
	if !strings.Contains(got, "Bearer ***") {
		t.Fatalf("Bearer 形式未处理: %s", got)
	}
	long := strings.Repeat("a", 100)
	truncated := Truncate(long, 10)
	if !strings.HasPrefix(truncated, "aaaaaaaaaa") || !strings.Contains(truncated, "已截断") {
		t.Fatalf("截断不符: %s", truncated)
	}
	if Truncate("short", 100) != "short" {
		t.Fatal("未超限不应改动")
	}
}

// TestTruncateKeepsUTF8Boundary：截断绝不能把多字节字符砍成半个。
//
// 为什么必须有这条：Truncate 的返回值会直接写进 MySQL 的 utf8mb4 列
// （agent_step.prompt_preview / content / observation、job_log.output 等）。
// 非法 UTF-8 字节序列会被 MySQL 以 `Error 1366 Incorrect string value` 整行拒绝，
// 表现是"会话落库成功、步骤全部失败"——内存单测不校验字符集，只有真库能抓出来。
func TestTruncateKeepsUTF8Boundary(t *testing.T) {
	cases := []string{
		strings.Repeat("中文提示词", 500),
		strings.Repeat("嗯", 3000),
		strings.Repeat("a中b", 999),
		strings.Repeat("🙂🙃", 800), // 4 字节 emoji
		"你",                       // 单字符
	}
	limits := []int{1, 2, 3, 4, 5, 7, 63, 64, 4000}
	for _, src := range cases {
		for _, limit := range limits {
			got := Truncate(src, limit)
			if !utf8.ValidString(got) {
				t.Fatalf("截断结果不是合法 UTF-8（limit=%d, src 前缀=%q）", limit, src[:12])
			}
			// 预算含义：保留的原文不超过 limit 字节
			if body := strings.SplitN(got, "…", 2)[0]; len(body) > limit {
				t.Fatalf("保留的原文 %d 字节超过预算 %d", len(body), limit)
			}
			if limit < len(src) && !strings.Contains(got, "已截断") {
				t.Fatalf("超限时必须标注截断，实际 %q", got)
			}
		}
	}
	// limit 连一个字符都装不下时，宁可不留原文
	if got := Truncate("中文", 1); strings.HasPrefix(got, "\xe4") && !utf8.ValidString(got) {
		t.Fatalf("预算不足时必须丢弃原文，实际 %q", got)
	} else if !utf8.ValidString(got) {
		t.Fatalf("结果必须始终是合法 UTF-8，实际 %q", got)
	}
}

// TestRedactDoesNotHangOrOverRedact 回归测试：
//  1. 手写实现曾在"保留前缀 + 反复替换"时死循环（真跑出过 600s 超时），必须锁住；
//  2. 词边界：`task-list` / `disk-space` 这类正常运维文本**不能**被涂掉，
//     否则轨迹里全是 `task-***`，等于把证据毁掉。
func TestRedactDoesNotHangOrOverRedact(t *testing.T) {
	manyKeys := strings.Repeat("sk-abcdefghijklmnop ", 200)
	done := make(chan string, 1)
	go func() { done <- Redact(manyKeys) }()
	select {
	case got := <-done:
		if strings.Contains(got, "sk-abcdefghijklmnop") {
			t.Fatalf("多密钥场景未全部抹除: %.80s", got)
		}
		if strings.Count(got, "***") != 200 {
			t.Fatalf("应抹除 200 处，实际 %d", strings.Count(got, "***"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Redact 超时未返回（疑似死循环回归）")
	}

	for _, text := range []string{
		"task-list rebuild finished",
		"disk-space low on /var",
		"mask-empty check ok",
		"risk-analysis report ready",
	} {
		if got := Redact(text); got != text {
			t.Fatalf("正常英文被误伤: %q -> %q", text, got)
		}
	}

	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.signature"
	if got := Redact("Authorization: Bearer " + jwt); strings.Contains(got, "payload.signature") {
		t.Fatalf("JWT 未抹除: %s", got)
	} else if !strings.Contains(got, "Bearer ***") {
		t.Fatalf("Bearer 前缀应保留: %s", got)
	}
	if got := Redact("sk-short"); got != "sk-short" {
		t.Fatalf("短于阈值不应改写（避免误伤）: %s", got)
	}
	if Redact("") != "" {
		t.Fatal("空串应原样返回")
	}
}

func TestUsageHelpers(t *testing.T) {
	u := Usage{PromptTokens: 1, CompletionTokens: 2}.Normalize()
	if u.TotalTokens != 3 {
		t.Fatalf("Normalize 未补齐 total: %+v", u)
	}
	sum := u.Add(Usage{TotalTokens: 5})
	if sum.TotalTokens != 8 || sum.PromptTokens != 1 {
		t.Fatalf("Add 不符: %+v", sum)
	}
	if !(Usage{}).IsZero() || u.IsZero() {
		t.Fatal("IsZero 判定不符")
	}
}
