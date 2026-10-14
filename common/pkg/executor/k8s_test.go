package executor

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// fakeK8sRunner 记录收到的 Spec，并返回固定结果。
// 有了它，K8s 执行器的**全部**语义（校验/超时/取消/截断/错误分类）都能纯内存测试，
// 不需要真集群，也不需要 client-go 的假对象去"假装"执行成功。
type fakeK8sRunner struct {
	got     K8sJobSpec
	calls   int
	outcome K8sJobOutcome
}

func (f *fakeK8sRunner) RunK8sJob(_ context.Context, spec K8sJobSpec) K8sJobOutcome {
	f.calls++
	f.got = spec
	return f.outcome
}

func baseK8sRequest() Request {
	return Request{
		RunID:     "run-1",
		JobID:     7,
		StepIndex: 2,
		Name:      "deploy",
		Kind:      KindK8s,
		Command:   "busybox:1.36",
		Args:      []string{"sh", "-c", "echo hi"},
		Env:       map[string]string{"APP_ENV": "prod"},
	}
}

func TestK8sExecutorSuccess(t *testing.T) {
	runner := &fakeK8sRunner{outcome: K8sJobOutcome{
		Namespace: "prod",
		JobName:   "crony-7-run-1",
		PodName:   "crony-7-run-1-abc12",
		Phase:     "succeeded",
		ExitCode:  0,
		Output:    "hi\n",
	}}
	exec := NewK8sExecutor(runner)

	res := exec.Execute(context.Background(), baseK8sRequest())
	if res.Status != models.RunStatusSuccess {
		t.Fatalf("期望成功，实际 %v（err=%v）", res.Status, res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("成功时退出码应为 0，实际 %d", res.ExitCode)
	}
	if res.Output != "hi\n" {
		t.Fatalf("输出未回传：%q", res.Output)
	}
	// 结构化产物必须能把人带到集群里那个 Pod——这是"集群执行"相对
	// "拼一条 kubectl 命令"的核心价值。
	for k, want := range map[string]string{
		"k8s_namespace": "prod",
		"k8s_job":       "crony-7-run-1",
		"k8s_pod":       "crony-7-run-1-abc12",
		"k8s_phase":     "succeeded",
		"k8s_image":     "busybox:1.36",
	} {
		if res.Artifacts[k] != want {
			t.Errorf("artifacts[%s] = %q，期望 %q", k, res.Artifacts[k], want)
		}
	}
	// 请求必须被完整翻译成 Spec（镜像/参数/环境变量/身份一个都不能丢）。
	if runner.got.Image != "busybox:1.36" || runner.got.RunID != "run-1" || runner.got.JobID != 7 {
		t.Fatalf("Spec 身份字段翻译错误：%+v", runner.got)
	}
	if len(runner.got.Command) != 3 || runner.got.Command[2] != "echo hi" {
		t.Fatalf("容器命令翻译错误：%v", runner.got.Command)
	}
	if runner.got.Env["APP_ENV"] != "prod" {
		t.Fatalf("环境变量未透传：%v", runner.got.Env)
	}
}

func TestK8sExecutorRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Request)
		runner K8sRunner
		want   string
	}{
		{
			name:   "镜像为空",
			mutate: func(r *Request) { r.Command = "" },
			runner: &fakeK8sRunner{},
			want:   "镜像",
		},
		{
			name:   "镜像有首尾空格",
			mutate: func(r *Request) { r.Command = "busybox:1.36 " },
			runner: &fakeK8sRunner{},
			want:   "首尾空格",
		},
		{
			name:   "未接入集群",
			mutate: func(r *Request) {},
			runner: nil,
			want:   "未接入 Kubernetes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseK8sRequest()
			tc.mutate(&req)
			res := NewK8sExecutor(tc.runner).Execute(context.Background(), req)
			if res.Status != models.RunStatusFailed {
				t.Fatalf("期望失败，实际 %v", res.Status)
			}
			if !strings.Contains(res.Error(), tc.want) {
				t.Fatalf("错误 %q 未包含 %q", res.Error(), tc.want)
			}
			// 配置类错误重试没有意义，必须标记 Fatal（否则会把配置错误放大成重试风暴）。
			if retry.Classify(res.Err).Retryable {
				t.Fatalf("配置错误不应可重试：%v", res.Err)
			}
		})
	}
}

func TestK8sExecutorTimeoutIsRetryable(t *testing.T) {
	runner := &fakeK8sRunner{outcome: K8sJobOutcome{
		Namespace: "prod",
		JobName:   "crony-1-run-1",
		Phase:     "timeout",
		ExitCode:  -1,
		Err:       context.DeadlineExceeded,
	}}
	req := baseK8sRequest()
	req.Timeout = 50 * time.Millisecond

	res := NewK8sExecutor(runner).Execute(context.Background(), req)
	if res.Status != models.RunStatusTimeout {
		t.Fatalf("期望超时状态，实际 %v（err=%v）", res.Status, res.Err)
	}
	if !retry.Classify(res.Err).Retryable {
		t.Fatalf("超时算瞬时故障，应可重试：%v", res.Err)
	}
	if res.Artifacts["timeout"] == "" {
		t.Fatalf("超时必须在 artifacts 里留证（实际用的超时值）")
	}
}

func TestK8sExecutorCancelIsFatal(t *testing.T) {
	runner := &fakeK8sRunner{outcome: K8sJobOutcome{Phase: "canceled", ExitCode: -1, Err: retry.ErrCanceled}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := NewK8sExecutor(runner).Execute(ctx, baseK8sRequest())
	if res.Status != models.RunStatusCanceled {
		t.Fatalf("期望取消状态，实际 %v", res.Status)
	}
	if retry.Classify(res.Err).Retryable {
		t.Fatalf("取消是确定性中断，绝不能重试：%v", res.Err)
	}
}

func TestK8sExecutorFailureKeepsEvidence(t *testing.T) {
	runner := &fakeK8sRunner{outcome: K8sJobOutcome{
		Namespace: "prod",
		JobName:   "crony-1-run-1",
		PodName:   "crony-1-run-1-x9",
		Phase:     "failed",
		ExitCode:  3,
		Output:    "boom",
		// 未显式分类 → 按 retry.Classify 缺省语义视为可重试（集群抖动居多）。
		Err: errString("pod 失败"),
	}}
	res := NewK8sExecutor(runner).Execute(context.Background(), baseK8sRequest())
	if res.Status != models.RunStatusFailed {
		t.Fatalf("期望失败，实际 %v", res.Status)
	}
	if res.ExitCode != 3 {
		t.Fatalf("容器退出码必须原样回传，实际 %d", res.ExitCode)
	}
	if res.Output != "boom" {
		t.Fatalf("失败时的容器日志是排查主证据，必须回传：%q", res.Output)
	}
	if res.Artifacts["k8s_pod"] == "" {
		t.Fatalf("失败也必须留下 Pod 名（否则无法去集群里查）")
	}
}

// 有错误但退出码为 0（例如创建被拒、日志取不到）时，不能落库成"退出码 0 却失败"。
func TestK8sExecutorFailureWithoutExitCode(t *testing.T) {
	runner := &fakeK8sRunner{outcome: K8sJobOutcome{Phase: "error", ExitCode: 0, Err: errString("log unavailable")}}
	res := NewK8sExecutor(runner).Execute(context.Background(), baseK8sRequest())
	if res.Status != models.RunStatusFailed {
		t.Fatalf("期望失败，实际 %v", res.Status)
	}
	if res.ExitCode != -1 {
		t.Fatalf("没有真实退出码时必须是 -1，实际 %d", res.ExitCode)
	}
}

func TestK8sExecutorTruncatesOutput(t *testing.T) {
	runner := &fakeK8sRunner{outcome: K8sJobOutcome{Phase: "succeeded", Output: strings.Repeat("x", 5000)}}
	req := baseK8sRequest()
	req.MaxOutputBytes = 1000

	res := NewK8sExecutor(runner).Execute(context.Background(), req)
	if !res.Truncated {
		t.Fatalf("超过上限必须标记截断")
	}
	if len([]byte(res.Output)) > 1200 {
		t.Fatalf("输出未被限制在 1000 字节附近，实际 %d", len([]byte(res.Output)))
	}
}

// 容器日志里的非法 UTF-8 不能把整条步骤结果变成脏数据（与 shell/http 同口径）。
func TestK8sExecutorTruncationKeepsUTF8Valid(t *testing.T) {
	// 中文按字节切会切到多字节字符中间，执行器必须回退到合法边界。
	runner := &fakeK8sRunner{outcome: K8sJobOutcome{Output: strings.Repeat("中文", 200)}}
	req := baseK8sRequest()
	req.MaxOutputBytes = 101

	res := NewK8sExecutor(runner).Execute(context.Background(), req)
	if !res.Truncated {
		t.Fatalf("期望截断")
	}
	if !utf8.ValidString(res.Output) {
		t.Fatalf("截断后不是合法 UTF-8")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// 注册表层面：k8s 步骤在"未注入运行器"时必须给出可区分的明确错误。
func TestRegistryRunK8sWithoutRunner(t *testing.T) {
	req := baseK8sRequest()
	res := DefaultRegistry().Run(context.Background(), req)
	if res.Status != models.RunStatusFailed {
		t.Fatalf("期望失败，实际 %v", res.Status)
	}
	if !strings.Contains(res.Error(), "未接入 Kubernetes") {
		t.Fatalf("错误信息未区分'集群未接入'：%q", res.Error())
	}
}
