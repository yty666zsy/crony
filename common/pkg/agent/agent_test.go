package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/retry"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeTool 只读工具替身。
type fakeTool struct {
	name     string
	schema   *jsonschema.Schema
	readOnly bool
	fn       func(ctx context.Context, args map[string]interface{}) (tools.Result, error)
}

func (f *fakeTool) Name() string               { return f.name }
func (f *fakeTool) Description() string        { return "测试工具" }
func (f *fakeTool) Schema() *jsonschema.Schema { return f.schema }
func (f *fakeTool) ReadOnly() bool             { return f.readOnly }
func (f *fakeTool) Risk() tools.RiskLevel {
	if f.readOnly {
		return tools.RiskRead
	}
	return tools.RiskWrite
}
func (f *fakeTool) Timeout() time.Duration { return 5 * time.Second }
func (f *fakeTool) Execute(ctx context.Context, args map[string]interface{}) (tools.Result, error) {
	return f.fn(ctx, args)
}

// echoSchema 工具参数 Schema。
func echoSchema() *jsonschema.Schema {
	return jsonschema.Object(map[string]*jsonschema.Schema{"q": jsonschema.String("查询词")}, "q")
}

// newEchoRegistry 构造只包含一个 echo 工具的注册表。
func newEchoRegistry(t *testing.T, fn func(ctx context.Context, args map[string]interface{}) (tools.Result, error)) *tools.Registry {
	t.Helper()
	r := tools.NewRegistry(tools.DefaultPolicy())
	r.MustRegister(&fakeTool{name: "echo", schema: echoSchema(), readOnly: true, fn: fn})
	return r
}

// okEcho 返回成功观测并附带证据（ref 固定为 obs:1）。
func okEcho(_ context.Context, _ map[string]interface{}) (tools.Result, error) {
	return tools.Result{
		OK:       true,
		Summary:  "查询完成",
		Output:   "命中 1 条",
		Evidence: []tools.Evidence{{Source: "log", Ref: "node.log:12", Quote: "ERROR dial timeout"}},
	}, nil
}

// simpleSchema 最小输出 Schema。
func simpleSchema() *jsonschema.Schema {
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"answer": jsonschema.String("答案").WithLength(1, 100),
	}, "answer")
}

// okAnswer 最小合规输出。
func okAnswer() map[string]interface{} { return map[string]interface{}{"answer": "ok"} }

// newGateway 构造带 fake provider 的网关。
func newGateway(opts ...llm.Option) (*llm.Gateway, *llm.FakeProvider) {
	fp := llm.NewFakeProvider("fake")
	all := append([]llm.Option{llm.WithProvider(fp)}, opts...)
	return llm.NewGateway(all...), fp
}

func testInput() Input {
	return Input{
		SessionID:     "s1",
		Goal:          "测试目标",
		System:        "你是测试助手",
		Context:       []string{"材料：任务 1 每天失败"},
		PromptVersion: "test-v1",
	}
}

// ---------------------------------------------------------------------------
// 主循环
// ---------------------------------------------------------------------------

func TestRunCompletesWithStructuredOutput(t *testing.T) {
	gateway, fp := newGateway()
	fp.WithResponder(func(llm.Request) (llm.Completion, error) { return llm.FakeJSON(okAnswer()), nil })

	a := New(gateway, nil, DefaultConfig())
	out, err := a.Run(context.Background(), testInput(), simpleSchema())
	if err != nil {
		t.Fatalf("应拿到结论: %v", err)
	}
	if out.StopReason != StopCompleted || out.Structured["answer"] != "ok" {
		t.Fatalf("结论不符: %+v", out)
	}
	if out.Trace == nil || out.Trace.ModelCalls != 1 || len(out.Trace.StepList()) != 1 {
		t.Fatalf("轨迹不符: %+v", out.Trace)
	}
	if out.Trace.Usage.TotalTokens == 0 {
		t.Fatal("应记录 token 用量")
	}
	if out.Trace.PromptVersion != "test-v1" || out.Trace.Goal != "测试目标" {
		t.Fatalf("轨迹元信息缺失: %+v", out.Trace)
	}
	// 没有注册工具时必须给出明确告警（模型只能凭材料推理）
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "没有可用工具") {
			found = true
		}
	}
	if !found {
		t.Fatalf("无工具时应有告警: %v", out.Warnings)
	}
	raw, err := out.Trace.JSON()
	if err != nil {
		t.Fatalf("轨迹应可序列化: %v", err)
	}
	for _, want := range []string{"prompt_version", "stop_reason", "model_calls"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("轨迹 JSON 缺少 %q", want)
		}
	}
	if !strings.Contains(out.Trace.Rendering(), "[模型]") {
		t.Fatalf("时间线渲染不符:\n%s", out.Trace.Rendering())
	}
}

func TestRunExecutesToolThenAnswers(t *testing.T) {
	gateway, fp := newGateway()
	fp.WithResponder(func(req llm.Request) (llm.Completion, error) {
		// 第二次请求里必须带上工具观测结果
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && strings.Contains(m.Content, "命中 1 条") {
				return llm.FakeJSON(okAnswer()), nil
			}
		}
		return llm.FakeToolCall("echo", map[string]interface{}{"q": "timeout"}), nil
	})
	registry := newEchoRegistry(t, okEcho)

	a := New(gateway, registry, DefaultConfig())
	out, err := a.Run(context.Background(), testInput(), simpleSchema())
	if err != nil {
		t.Fatalf("应拿到结论: %v", err)
	}
	if out.StopReason != StopCompleted {
		t.Fatalf("终止原因不符: %s", out.StopReason)
	}
	steps := out.Trace.StepList()
	if len(steps) != 3 || steps[0].Kind != StepModel || steps[1].Kind != StepTool || steps[2].Kind != StepModel {
		t.Fatalf("步骤序列不符: %+v", steps)
	}
	if steps[1].ToolName != "echo" || steps[1].ToolArgs["q"] != "timeout" {
		t.Fatalf("工具步骤记录不符: %+v", steps[1])
	}
	if len(steps[1].Evidence) != 1 || steps[1].Evidence[0].Ref != "node.log:12" {
		t.Fatalf("证据未记录: %+v", steps[1])
	}
	if out.Trace.ToolCalls != 1 || len(out.Trace.EvidenceList()) != 1 {
		t.Fatalf("轨迹统计不符: tool_calls=%d evidence=%d", out.Trace.ToolCalls, len(out.Trace.EvidenceList()))
	}
	if out.Trace.ToolStats.Calls != 1 {
		t.Fatalf("工具统计未快照: %+v", out.Trace.ToolStats)
	}
	if !strings.Contains(out.Trace.Rendering(), "[工具] echo") {
		t.Fatalf("时间线缺少工具步骤:\n%s", out.Trace.Rendering())
	}
	// 工具 Schema 必须真的传给模型（否则模型不知道能调什么）
	requests := fp.Requests()
	if len(requests) == 0 || len(requests[0].Tools) != 1 || requests[0].Tools[0].Name != "echo" {
		t.Fatalf("工具声明未下发: %+v", requests)
	}
}

func TestRunBoundedByMaxSteps(t *testing.T) {
	gateway, fp := newGateway()
	fp.WithResponder(func(llm.Request) (llm.Completion, error) {
		return llm.FakeToolCall("echo", map[string]interface{}{"q": "loop"}), nil
	})
	registry := newEchoRegistry(t, okEcho)

	cfg := DefaultConfig()
	cfg.MaxSteps = 3
	cfg.MaxToolCalls = 50
	a := New(gateway, registry, cfg)
	out, err := a.Run(context.Background(), testInput(), simpleSchema())
	if !errors.Is(err, ErrMaxSteps) || out.StopReason != StopMaxSteps {
		t.Fatalf("应按步数上限终止: %v %s", err, out.StopReason)
	}
	if fp.CallCount() != 3 {
		t.Fatalf("模型调用次数应等于步数上限，实际 %d", fp.CallCount())
	}
	if out.Trace.ToolCalls != 3 {
		t.Fatalf("工具调用数不符: %d", out.Trace.ToolCalls)
	}
	if !strings.Contains(out.StopReason.Explain(), "最大步数") {
		t.Fatalf("终止原因解释不符: %s", out.StopReason.Explain())
	}
	// 失败也必须留下轨迹（否则"为什么没结论"无从查起）
	if out.Trace == nil || out.Trace.StopReason != StopMaxSteps {
		t.Fatalf("失败时轨迹缺失: %+v", out.Trace)
	}
}

func TestRunBoundedByToolBudget(t *testing.T) {
	gateway, _ := newGateway()
	gateway2, fp2 := newGateway()
	_ = gateway
	fp2.WithResponder(func(llm.Request) (llm.Completion, error) {
		return llm.Completion{
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "echo", Arguments: `{"q":"1"}`},
				{ID: "c2", Name: "echo", Arguments: `{"q":"2"}`},
				{ID: "c3", Name: "echo", Arguments: `{"q":"3"}`},
			},
			FinishReason: "tool_calls",
		}, nil
	})
	executed := 0
	registry := newEchoRegistry(t, func(_ context.Context, _ map[string]interface{}) (tools.Result, error) {
		executed++
		return tools.Result{OK: true, Summary: "ok"}, nil
	})

	cfg := DefaultConfig()
	cfg.MaxToolCalls = 2
	a := New(gateway2, registry, cfg)
	out, err := a.Run(context.Background(), testInput(), simpleSchema())
	if !errors.Is(err, ErrToolBudget) || out.StopReason != StopToolBudget {
		t.Fatalf("应按工具预算终止: %v %s", err, out.StopReason)
	}
	if executed != 2 {
		t.Fatalf("超预算的工具不应执行，实际执行 %d 次", executed)
	}
}

func TestRunLLMBudgetExhaustion(t *testing.T) {
	fp := llm.NewFakeProvider("fake")
	fp.WithResponder(func(llm.Request) (llm.Completion, error) {
		return llm.FakeToolCall("echo", map[string]interface{}{"q": "x"}), nil
	})
	gateway := llm.NewGateway(llm.WithProvider(fp), llm.WithBudget(llm.Budget{MaxCalls: 1}))
	registry := newEchoRegistry(t, okEcho)

	a := New(gateway, registry, DefaultConfig())
	out, err := a.Run(context.Background(), testInput(), simpleSchema())
	if !errors.Is(err, llm.ErrBudgetExceeded) || out.StopReason != StopLLMBudget {
		t.Fatalf("应按模型预算终止: %v %s", err, out.StopReason)
	}
	if fp.CallCount() != 1 {
		t.Fatalf("预算耗尽后不应再调用 provider，实际 %d 次", fp.CallCount())
	}
	if out.Trace.Budget.Calls != 1 {
		t.Fatalf("轨迹里应快照预算用量: %+v", out.Trace.Budget)
	}
}

func TestRunSchemaRepairRound(t *testing.T) {
	gateway, fp := newGateway()
	calls := 0
	fp.WithResponder(func(llm.Request) (llm.Completion, error) {
		calls++
		if calls == 1 {
			return llm.FakeText("我先说说思路，不打算给 JSON。"), nil
		}
		return llm.FakeJSON(okAnswer()), nil
	})

	a := New(gateway, nil, DefaultConfig())
	out, err := a.Run(context.Background(), testInput(), simpleSchema())
	if err != nil {
		t.Fatalf("修复后应成功: %v", err)
	}
	if out.Repairs != 1 || out.Structured["answer"] != "ok" {
		t.Fatalf("修复轮计数不符: repairs=%d out=%+v", out.Repairs, out.Structured)
	}
	kinds := map[StepKind]int{}
	for _, s := range out.Trace.StepList() {
		kinds[s.Kind]++
	}
	if kinds[StepNotice] != 1 {
		t.Fatalf("应有 1 条修复提示: %+v", kinds)
	}
	// 修复提示里要带上具体校验错误，模型才知道改什么
	requests := fp.Requests()
	last := requests[len(requests)-1]
	foundHint := false
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "校验错误") {
			foundHint = true
		}
	}
	if !foundHint {
		t.Fatalf("修复请求缺少校验错误说明: %+v", last.Messages)
	}
}

func TestRunSchemaInvalidAfterRepair(t *testing.T) {
	gateway, _ := newGateway()
	a := New(gateway, nil, DefaultConfig())
	out, err := a.Run(context.Background(), testInput(), simpleSchema())
	// 脚本为空 → FakeProvider 返回"已无脚本，结束"的非 JSON 文本 → 修复轮后仍不合规
	if !errors.Is(err, ErrSchemaInvalid) || out.StopReason != StopSchemaInvalid {
		t.Fatalf("应报结构不合规: %v %s", err, out.StopReason)
	}
	if out.Trace == nil || out.Trace.ModelCalls != 2 {
		t.Fatalf("应为 1 次尝试 + 1 次修复: %+v", out.Trace)
	}
	if out.Raw == "" {
		t.Fatal("应保留模型原文便于排查")
	}
}

func TestRunCanceled(t *testing.T) {
	gateway, _ := newGateway()
	a := New(gateway, nil, DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := a.Run(ctx, testInput(), simpleSchema())
	if !errors.Is(err, retry.ErrCanceled) || out.StopReason != StopCanceled {
		t.Fatalf("应报取消: %v %s", err, out.StopReason)
	}
	if out.Trace == nil {
		t.Fatal("取消也要留轨迹")
	}
}

// TestRunCanceledBetweenSteps 工具执行期间被取消，循环必须在下一次迭代前停下。
func TestRunCanceledBetweenSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gateway, _ := newGateway()
	gateway2, fp2 := newGateway()
	_ = gateway
	fp2.WithResponder(func(llm.Request) (llm.Completion, error) {
		return llm.FakeToolCall("echo", map[string]interface{}{"q": "x"}), nil
	})
	registry := newEchoRegistry(t, func(_ context.Context, _ map[string]interface{}) (tools.Result, error) {
		cancel() // 模拟"运维在诊断过程中点了取消"
		return tools.Result{OK: true, Summary: "ok"}, nil
	})
	a := New(gateway2, registry, DefaultConfig())
	out, err := a.Run(ctx, testInput(), simpleSchema())
	if !errors.Is(err, retry.ErrCanceled) || out.StopReason != StopCanceled {
		t.Fatalf("应在取消后停下: %v %s", err, out.StopReason)
	}
	if fp2.CallCount() != 1 {
		t.Fatalf("取消后不应再调用模型，实际 %d 次", fp2.CallCount())
	}
}

func TestRunNoProvider(t *testing.T) {
	a := New(llm.NewGateway(), nil, DefaultConfig())
	out, err := a.Run(context.Background(), testInput(), simpleSchema())
	if !errors.Is(err, ErrNoProvider) || out.StopReason != StopNoProvider {
		t.Fatalf("应报无 provider: %v %s", err, out.StopReason)
	}
	if out.Trace == nil {
		t.Fatal("失败也要留轨迹")
	}
}

// TestToolFailureIsObservation 工具被拒/失败不能中断诊断，要变成观测继续推理。
func TestToolFailureIsObservation(t *testing.T) {
	gateway, _ := newGateway()
	gateway2, fp2 := newGateway()
	_ = gateway
	calls := 0
	fp2.WithResponder(func(req llm.Request) (llm.Completion, error) {
		calls++
		if calls == 1 {
			// 第一次故意用错参数（Schema 要求 q 必填）
			return llm.FakeToolCall("echo", map[string]interface{}{"wrong": 1}), nil
		}
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && strings.Contains(m.Content, "参数") {
				return llm.FakeJSON(okAnswer()), nil
			}
		}
		return llm.FakeText("still stuck"), nil
	})
	registry := newEchoRegistry(t, func(_ context.Context, _ map[string]interface{}) (tools.Result, error) {
		t.Fatal("参数不合法的调用不应真正执行")
		return tools.Result{}, nil
	})
	a := New(gateway2, registry, DefaultConfig())
	out, err := a.Run(context.Background(), testInput(), simpleSchema())
	if err != nil {
		t.Fatalf("应在拿到观测后继续完成: %v", err)
	}
	steps := out.Trace.StepList()
	if len(steps) < 2 || steps[1].Kind != StepTool || steps[1].Err == "" {
		t.Fatalf("工具失败步骤应留痕: %+v", steps)
	}
	if !strings.Contains(steps[1].Observation, "参数") {
		t.Fatalf("观测文本应说明参数问题: %q", steps[1].Observation)
	}
}

func TestStepObserverReceivesStepsAndIsPanicSafe(t *testing.T) {
	gateway, fp := newGateway()
	fp.WithResponder(func(llm.Request) (llm.Completion, error) { return llm.FakeJSON(okAnswer()), nil })

	var seen []StepKind
	a := New(gateway, nil, DefaultConfig(), WithStepObserver(func(s Step) {
		seen = append(seen, s.Kind)
		if s.Kind == StepModel {
			panic("观测回调自己炸了")
		}
	}))
	_, err := a.Run(context.Background(), testInput(), simpleSchema())
	if err != nil {
		t.Fatalf("回调 panic 不应影响主流程: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("回调应收到步骤")
	}
}

func TestRunRejectsMissingSchema(t *testing.T) {
	gateway, _ := newGateway()
	a := New(gateway, nil, DefaultConfig())
	out, err := a.Run(context.Background(), testInput(), nil)
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("缺 Schema 应报错: %v", err)
	}
	if out.StopReason != StopSchemaInvalid {
		t.Fatalf("终止原因不符: %s", out.StopReason)
	}
	// 非法 Schema 同样要拦住（防止场景代码写错导致校验静默失效）
	bad := jsonschema.Object(map[string]*jsonschema.Schema{"a": jsonschema.String("")}, "b")
	out2, err2 := a.Run(context.Background(), testInput(), bad)
	if !errors.Is(err2, ErrSchemaInvalid) {
		t.Fatalf("非法 Schema 应报错: %v", err2)
	}
	if !strings.Contains(out2.StopReason.Explain(), "结构") {
		t.Fatalf("终止原因解释不符: %s", out2.StopReason.Explain())
	}
}

func TestEvidenceRefsNormalization(t *testing.T) {
	tr := newTrace("s", "g", "v")
	tr.addEvidence(
		tools.Evidence{Source: "log", Ref: "node.log:12", Quote: "a"},
		tools.Evidence{Source: "log", Ref: "node.log:12", Quote: "a"}, // 重复应去重
		tools.Evidence{Source: "node", Ref: "n1", Quote: "b"},
	)
	if len(tr.EvidenceList()) != 2 {
		t.Fatalf("证据应去重: %+v", tr.EvidenceList())
	}
	refs := tr.EvidenceRefs()
	for _, want := range []string{"node.log:12", "log:node.log:12", "n1", "node:n1"} {
		if !refs[want] {
			t.Fatalf("缺少可引用键 %q: %v", want, refs)
		}
	}
	// 只有来源名（"log"/"node"）**不能**作为可引用键：否则任何含 "log" 的字符串
	// （例如编造的 syslog:99）都会被判为"已观测"，grounding 校验就形同虚设。
	for _, unwanted := range []string{"log", "node"} {
		if refs[unwanted] {
			t.Fatalf("来源名不应作为独立引用键 %q: %v", unwanted, refs)
		}
	}
	if refObserved(refs, "node.log:12") != true {
		t.Fatal("精确匹配应通过")
	}
	if refObserved(refs, "node.log:12-15") != true {
		t.Fatal("更具体的范围写法应通过")
	}
	if refObserved(refs, "syslog:99") != false {
		t.Fatal("编造的定位必须判为未观测")
	}
	if refObserved(refs, "") != false {
		t.Fatal("空引用必须判为未观测")
	}
}
