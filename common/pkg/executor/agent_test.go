package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// fakeAgentRunner 确定性假运行时：编排层的行为不该依赖真实模型。
type fakeAgentRunner struct {
	outcome AgentOutcome
	calls   int
	gotReq  Request
}

func (f *fakeAgentRunner) RunAgent(_ context.Context, req Request) AgentOutcome {
	f.calls++
	f.gotReq = req
	return f.outcome
}

func TestAgentExecutorSuccess(t *testing.T) {
	runner := &fakeAgentRunner{outcome: AgentOutcome{
		SessionID:  "job7-1-1",
		StopReason: "completed",
		Output:     "【巡检总结】一切正常",
		Artifacts:  map[string]string{"provider": "offline"},
	}}
	exec := NewAgentExecutor(runner)
	req := Request{RunID: "j7-t100", JobID: 7, Kind: KindAgent, AgentKind: "job", AgentGoal: "巡检备份任务"}

	if err := exec.Validate(req); err != nil {
		t.Fatalf("Validate 不应失败: %v", err)
	}
	res := exec.Execute(context.Background(), req)
	if res.Status != models.RunStatusSuccess {
		t.Fatalf("Status = %s, want success（err=%s）", res.Status, res.Error())
	}
	if runner.calls != 1 {
		t.Fatalf("runner 调用次数 = %d, want 1", runner.calls)
	}
	if res.Artifacts["agent_session_id"] != "job7-1-1" {
		t.Errorf("会话 ID 未写入产物: %v", res.Artifacts)
	}
	if res.Artifacts["stop_reason"] != "completed" || res.Artifacts["provider"] != "offline" {
		t.Errorf("产物应保留 stop_reason/provider: %v", res.Artifacts)
	}
	if !strings.Contains(res.Output, "一切正常") {
		t.Errorf("输出应保留结论，实际 %q", res.Output)
	}
}

func TestAgentExecutorRequiresGoalAndRunner(t *testing.T) {
	// 目标为空：属于请求非法，且必须 Fatal（重试也不会好）
	exec := NewAgentExecutor(&fakeAgentRunner{})
	err := exec.Validate(Request{RunID: "j1-t1", Kind: KindAgent, AgentGoal: "   "})
	if err == nil {
		t.Fatal("目标为空必须拒绝")
	}
	if retry.Classify(err).Retryable {
		t.Error("缺少目标属于确定性错误，不应重试")
	}
	// 未注入运行器：错误信息必须点明原因
	bare := NewAgentExecutor(nil)
	err = bare.Validate(Request{RunID: "j1-t1", Kind: KindAgent, AgentGoal: "巡检"})
	if err == nil || !strings.Contains(err.Error(), "Agent 运行时") {
		t.Fatalf("未注入运行器时应报运行时缺失，实际 %v", err)
	}
	if retry.Classify(err).Retryable {
		t.Error("未注入运行器不应重试")
	}
}

// TestAgentExecutorRetrySemantics 重试语义：只有模型侧瞬时故障才自动重试。
//
// 这是本项目"不烧钱"的核心约束之一：提示词写错/结构不合规这类失败重跑一百次
// 也不会变好，只会产生一百次计费。
func TestAgentExecutorRetrySemantics(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantRetryable bool
	}{
		{"限流可重试", llm.ErrRateLimited, true},
		{"模型超时可重试", llm.ErrTimeout, true},
		{"结构不合规不重试", ErrSchemaProbe, false},
		{"取消不重试", retry.ErrCanceled, false},
		{"上下文取消不重试", context.Canceled, false},
		{"未分类错误保守不重试", errors.New("模型胡说八道"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := NewAgentExecutor(&fakeAgentRunner{outcome: AgentOutcome{
				SessionID: "s1", Output: "失败", Err: tc.err,
			}})
			res := exec.Execute(context.Background(), Request{
				RunID: "j1-t1", Kind: KindAgent, AgentGoal: "巡检",
			})
			if res.Status != models.RunStatusFailed {
				t.Fatalf("Status = %s, want failed", res.Status)
			}
			if got := retry.Classify(res.Err).Retryable; got != tc.wantRetryable {
				t.Errorf("Retryable = %v, want %v（reason=%s）", got, tc.wantRetryable, retry.Classify(res.Err).Reason)
			}
		})
	}
}

// TestAgentExecutorTruncatesOutput 输出截断：一次会话的报告不能撑爆 job_run_step。
func TestAgentExecutorTruncatesOutput(t *testing.T) {
	long := strings.Repeat("诊", 3000) // 9000 字节
	exec := NewAgentExecutor(&fakeAgentRunner{outcome: AgentOutcome{
		SessionID: "s1", StopReason: "completed", Output: long,
	}})
	res := exec.Execute(context.Background(), Request{
		RunID: "j1-t1", Kind: KindAgent, AgentGoal: "巡检", MaxOutputBytes: 100,
	})
	if !res.Truncated {
		t.Fatal("超过上限必须标记 Truncated")
	}
	if len(res.Output) > 200 {
		t.Fatalf("输出应被截断到接近上限，实际 %d 字节", len(res.Output))
	}
	if !strings.HasPrefix(res.Output, "诊") {
		t.Fatalf("截断后应保留开头，实际 %q", res.Output[:min(12, len(res.Output))])
	}
	// 截断点必须落在合法 UTF-8 边界上（否则落库会出现非法字节）
	for i, r := range res.Output {
		if r == '\uFFFD' {
			t.Fatalf("截断产生了非法字符，位置 %d", i)
		}
	}
}

// ErrSchemaProbe 占位错误：代表"结构不合规"这一类确定性失败。
var ErrSchemaProbe = fmt.Errorf("agent: 输出不符合要求的结构")

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
