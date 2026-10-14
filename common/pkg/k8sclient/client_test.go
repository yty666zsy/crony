package k8sclient

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/tmnhs/crony/common/pkg/executor"
	"github.com/tmnhs/crony/common/pkg/retry"
)

func testSpec() executor.K8sJobSpec {
	return executor.K8sJobSpec{
		RunID:     "job21-1750000000",
		JobID:     21,
		StepIndex: 0,
		Name:      "backup",
		Image:     "busybox:1.36",
		Command:   []string{"sh", "-c", "echo hi"},
		Env:       map[string]string{"B": "2", "A": "1"},
	}
}

// ---------------------------------------------------------------------------
// 边界：默认关闭
// ---------------------------------------------------------------------------

func TestImageAllowlistDefaultsToClosed(t *testing.T) {
	cs := fake.NewSimpleClientset()
	// 未配置 allow_images：必须拒绝任何镜像。
	c := NewWithInterface(cs, Config{Namespace: "default"})
	if err := c.checkImage("busybox:1.36"); err == nil {
		t.Fatalf("空白名单必须拒绝一切镜像（默认关闭）")
	}
	// 显式写 "*" 才是承担风险的选择。
	c2 := NewWithInterface(cs, Config{Namespace: "default", AllowImages: []string{"*"}})
	if err := c2.checkImage("anything:v1"); err != nil {
		t.Fatalf("\"*\" 应允许任意镜像，实际 %v", err)
	}
	// 前缀通配
	c3 := NewWithInterface(cs, Config{Namespace: "default", AllowImages: []string{"registry.internal/*"}})
	if err := c3.checkImage("registry.internal/team/backup:1.0"); err != nil {
		t.Fatalf("前缀通配应命中，实际 %v", err)
	}
	if err := c3.checkImage("docker.io/library/busybox:1.36"); err == nil {
		t.Fatalf("未列出的镜像仓库必须被拒绝（否则白名单形同虚设）")
	}
}

func TestNamespaceAllowlistDefaultsToSingleNamespace(t *testing.T) {
	c := NewWithInterface(fake.NewSimpleClientset(), Config{Namespace: "crony-jobs"})
	if !c.NamespaceAllowed("") || !c.NamespaceAllowed("crony-jobs") {
		t.Fatalf("默认命名空间必须可用")
	}
	if c.NamespaceAllowed("prod") {
		t.Fatalf("空白名单下 prod 必须不可用（默认只允许配置里的那一个命名空间）")
	}
	c2 := NewWithInterface(fake.NewSimpleClientset(), Config{Namespace: "crony-jobs", AllowNamespaces: []string{"prod", "staging"}})
	if !c2.NamespaceAllowed("prod") {
		t.Fatalf("显式列出的命名空间应可用")
	}
	if c2.NamespaceAllowed("kube-system") {
		t.Fatalf("未列出的命名空间仍不可用")
	}
}

// ---------------------------------------------------------------------------
// Job 定义：确定性、安全上下文、重试归属
// ---------------------------------------------------------------------------

func TestJobNameIsDeterministicAndDNS1123(t *testing.T) {
	spec := testSpec()
	name := JobName(spec)
	if name != JobName(spec) {
		t.Fatalf("同一个 (job,step,run) 必须得到同一个 Job 名")
	}
	spec.RunID = "别的 run_id/UPPER_case"
	other := JobName(spec)
	if other == name {
		t.Fatalf("不同 run_id 不应撞名")
	}
	if strings.ToLower(other) != other {
		t.Fatalf("Job 名必须是小写：%q", other)
	}
	if strings.ContainsAny(other, "_/.") {
		t.Fatalf("Job 名含非法字符：%q", other)
	}
	if len(other) > 63 {
		t.Fatalf("Job 名超过 63 字符：%q", other)
	}
}

func TestBuildJobCarriesBoundaries(t *testing.T) {
	c := NewWithInterface(fake.NewSimpleClientset(), Config{
		Namespace:               "crony-jobs",
		AllowImages:             []string{"busybox*"},
		DefaultCPU:              "500m",
		DefaultMemory:           "256Mi",
		TTLSecondsAfterFinished: 600,
	})
	job, err := c.buildJob(testSpec(), "crony-jobs")
	if err != nil {
		t.Fatalf("buildJob: %v", err)
	}
	// 重试归属：k8s 侧不允许重试，否则会出现 N×M 次执行且退出码说不清。
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("BackoffLimit 必须为 0（重试交给 crony 自己的策略）")
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 600 {
		t.Fatalf("必须设置 TTL，避免集群里堆积 Job")
	}
	podSpec := job.Spec.Template.Spec
	// 这个 Pod 没有任何理由访问 API Server。
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Fatalf("必须关闭 ServiceAccount token 挂载")
	}
	if podSpec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("重试由 crony 负责，Pod 自身不应重启")
	}
	sc := podSpec.Containers[0].SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatalf("默认不允许提权")
	}
	if podSpec.Containers[0].Resources.Limits.Cpu().String() != "500m" {
		t.Fatalf("CPU 上限未生效：%v", podSpec.Containers[0].Resources.Limits)
	}
	// 环境变量顺序必须稳定（同样的请求产生同样的 Job 定义，便于回放与 diff）。
	envNames := make([]string, 0, len(podSpec.Containers[0].Env))
	for _, e := range podSpec.Containers[0].Env {
		envNames = append(envNames, e.Name)
	}
	want := []string{"CRONY_RUN_ID", "CRONY_JOB_ID", "CRONY_STEP_INDEX", "A", "B"}
	if strings.Join(envNames, ",") != strings.Join(want, ",") {
		t.Fatalf("环境变量顺序/内容不符：%v", envNames)
	}
	if got := job.Labels["app.kubernetes.io/managed-by"]; got != "crony" {
		t.Fatalf("必须打 managed-by 标签（集群侧能看出是谁起的 Pod）")
	}
}

// ---------------------------------------------------------------------------
// 端到端（fake clientset）：成功 / 镜像拉取失败
// ---------------------------------------------------------------------------

// 用 fake 的 reactor 模拟"Job 创建后立即成功"，并预置一个完成态 Pod。
func fakeRunningClient(t *testing.T, spec executor.K8sJobSpec, pod *corev1.Pod, jobStatus batchv1.JobStatus) *Client {
	t.Helper()
	cs := fake.NewSimpleClientset()
	name := JobName(spec)
	cs.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "crony-jobs"},
			Status:     jobStatus,
		}, nil
	})
	if pod != nil {
		pod.Labels = map[string]string{"job-name": name}
		if _, err := cs.CoreV1().Pods("crony-jobs").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("预置 Pod: %v", err)
		}
	}
	c := NewWithInterface(cs, Config{
		Namespace:               "crony-jobs",
		AllowImages:             []string{"busybox*"},
		PollInterval:            5 * time.Millisecond,
		TTLSecondsAfterFinished: 600,
	})
	c.logFn = func(context.Context, string, string, string, int64) (string, error) { return "hi\n", nil }
	return c
}

func TestRunK8sJobSuccess(t *testing.T) {
	spec := testSpec()
	done := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "crony-21-0-abc12", Namespace: "crony-jobs"},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}},
		},
	}
	c := fakeRunningClient(t, spec, done, batchv1.JobStatus{Succeeded: 1})

	out := c.RunK8sJob(context.Background(), spec)
	if out.Err != nil {
		t.Fatalf("期望成功，实际 %v", out.Err)
	}
	if out.Phase != "succeeded" || out.ExitCode != 0 {
		t.Fatalf("phase/exit 不符：%s/%d", out.Phase, out.ExitCode)
	}
	if out.Output != "hi\n" {
		t.Fatalf("日志未回传：%q", out.Output)
	}
	if out.Artifacts["k8s_pod"] != "crony-21-0-abc12" {
		t.Fatalf("必须留下 Pod 名，实际 %v", out.Artifacts)
	}
}

func TestRunK8sJobImagePullBackOff(t *testing.T) {
	spec := testSpec()
	broken := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "crony-21-0-bad", Namespace: "crony-jobs"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ImagePullBackOff", Message: "Back-off pulling image",
				}},
			}},
		},
	}
	c := fakeRunningClient(t, spec, broken, batchv1.JobStatus{Failed: 1})

	out := c.RunK8sJob(context.Background(), spec)
	if out.Err == nil {
		t.Fatalf("期望失败")
	}
	if out.ExitCode != -1 {
		t.Fatalf("根本没跑起来时退出码必须是 -1，实际 %d", out.ExitCode)
	}
	// 只报"退出码 -1"对排查毫无帮助：等待原因必须落 artifacts。
	if out.Artifacts["k8s_reason"] != "ImagePullBackOff" {
		t.Fatalf("必须记录等待原因，实际 %v", out.Artifacts)
	}
	// 镜像仓库瞬时不可用属于可重试；重试不会好的配置错误才标记 Fatal。
	if !retry.Classify(out.Err).Retryable {
		t.Fatalf("ImagePullBackOff 应按可重试处理：%v", out.Err)
	}
}

func TestRunK8sJobRejectsDisallowedImage(t *testing.T) {
	c := NewWithInterface(fake.NewSimpleClientset(), Config{Namespace: "crony-jobs", AllowImages: []string{"busybox*"}})
	spec := testSpec()
	spec.Image = "evil/coinminer:latest"
	out := c.RunK8sJob(context.Background(), spec)
	if out.Err == nil || !retry.Classify(out.Err).Retryable == false {
		t.Fatalf("越权镜像必须直接失败且不可重试，实际 %v", out.Err)
	}
	if !strings.Contains(out.Err.Error(), "不在允许列表") {
		t.Fatalf("错误信息应说明白被哪条边界拦住：%v", out.Err)
	}
}

// TestRunK8sJobTimeoutKeepsJobAndExplainsCause 盯住一个**真集群实测踩到**的缺陷：
//
// 镜像 tag 不存在时，Pod 一直 ImagePullBackOff，Job 永远不会到达终态，
// 于是我们等满超时。旧实现只报"等待 Job 结束超时"并**把 Job 一起删了**——
// 结果是最该看到的两样东西（真实原因、失败现场）同时消失。
//
// 这里用 fake clientset 把"状态永远不是终态"冻住（Job 对象不会被更新），
// 逼出超时路径，然后逐条断言：
//   - 错误里必须带 Pod 卡住的原因（否则用户只能看到"超时"）；
//   - Job 必须保留（TTL 窗口内还要能 kubectl describe）；
//   - Pod 必须被停掉（否则任务其实还在集群里跑）。
func TestRunK8sJobTimeoutKeepsJobAndExplainsCause(t *testing.T) {
	spec := testSpec()
	spec.Timeout = 80 * time.Millisecond

	cs := fake.NewSimpleClientset()
	name := JobName(spec)
	if _, err := cs.BatchV1().Jobs("crony-jobs").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "crony-jobs"},
		// 空 Status = 永远不是终态
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("预置 Job: %v", err)
	}
	stuck := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crony-21-0-stuck",
			Namespace: "crony-jobs",
			Labels:    map[string]string{"job-name": name},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ImagePullBackOff",
					Message: `Back-off pulling image "busybox:no-such-tag"`,
				}},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods("crony-jobs").Create(context.Background(), stuck, metav1.CreateOptions{}); err != nil {
		t.Fatalf("预置 Pod: %v", err)
	}

	c := NewWithInterface(cs, Config{
		Namespace:               "crony-jobs",
		AllowImages:             []string{"busybox*"},
		PollInterval:            5 * time.Millisecond,
		TTLSecondsAfterFinished: 600,
	})
	c.logFn = func(context.Context, string, string, string, int64) (string, error) { return "", nil }

	out := c.RunK8sJob(context.Background(), spec)
	if out.Phase != "timeout" {
		t.Fatalf("期望超时，实际 phase=%s err=%v", out.Phase, out.Err)
	}
	if out.Err == nil {
		t.Fatal("超时必须返回错误")
	}
	if !strings.Contains(out.Err.Error(), "ImagePullBackOff") || !strings.Contains(out.Err.Error(), "no-such-tag") {
		t.Fatalf("超时错误必须带上 Pod 卡住的真实原因（含镜像名），实际: %v", out.Err)
	}
	if out.Artifacts["k8s_reason"] != "ImagePullBackOff" {
		t.Fatalf("原因必须落 artifacts，实际 %v", out.Artifacts)
	}
	// 现场保留：Job 还在（fake 的 Delete 会真的删掉对象，所以这条是硬断言）
	if _, err := cs.BatchV1().Jobs("crony-jobs").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("超时不应删掉 Job（TTL 窗口内还要能查现场）：%v", err)
	}
	// 工作负载必须停下
	if _, err := cs.CoreV1().Pods("crony-jobs").Get(context.Background(), stuck.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("超时后 Pod 必须被停掉，否则任务还在集群里跑")
	}
}

// TestRunK8sJobTimeoutDeletesJobWhenTTLDisabled：显式配 TTL=0（"立刻回收"）时，
// 连 Job 一起删——这是明确取舍，不是默认行为。
func TestRunK8sJobTimeoutDeletesJobWhenTTLDisabled(t *testing.T) {
	spec := testSpec()
	spec.Timeout = 60 * time.Millisecond

	cs := fake.NewSimpleClientset()
	name := JobName(spec)
	if _, err := cs.BatchV1().Jobs("crony-jobs").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "crony-jobs"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("预置 Job: %v", err)
	}
	c := NewWithInterface(cs, Config{
		Namespace:               "crony-jobs",
		AllowImages:             []string{"busybox*"},
		PollInterval:            5 * time.Millisecond,
		TTLSecondsAfterFinished: 0,
	})
	c.logFn = func(context.Context, string, string, string, int64) (string, error) { return "", nil }

	out := c.RunK8sJob(context.Background(), spec)
	if out.Phase != "timeout" {
		t.Fatalf("期望超时，实际 %s", out.Phase)
	}
	if _, err := cs.BatchV1().Jobs("crony-jobs").Get(context.Background(), name, metav1.GetOptions{}); err == nil {
		t.Fatal("TTL=0 表示立刻回收，超时后 Job 不应残留")
	}
}

// TestRunK8sJobTimeoutRemembersReasonAfterPodGone 复现真集群里的**真实时序**：
//
// Job 的 activeDeadlineSeconds 到点时，Job 控制器会先把 Pod 回收；我们带着
// deadline 去读 Pod 时它已经不在了。旧实现因此只报"超时"，
// 而原因（ImagePullBackOff）其实在等待期间一直在 Pod 状态里。
//
// 这里用 reactor 让"第一次 list 有 Pod、之后就没有了"，把它钉住。
func TestRunK8sJobTimeoutRemembersReasonAfterPodGone(t *testing.T) {
	spec := testSpec()
	spec.Timeout = 80 * time.Millisecond

	cs := fake.NewSimpleClientset()
	name := JobName(spec)
	if _, err := cs.BatchV1().Jobs("crony-jobs").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "crony-jobs"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("预置 Job: %v", err)
	}
	podCalls := 0
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		podCalls++
		if podCalls > 1 {
			// Pod 已被 Job 控制器回收：超时时现场已经读不到了
			return true, &corev1.PodList{}, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "crony-21-0-vanished",
				Namespace: "crony-jobs",
				Labels:    map[string]string{"job-name": name},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "main",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason:  "ImagePullBackOff",
						Message: `Back-off pulling image "busybox:no-such-tag"`,
					}},
				}},
			},
		}}}, nil
	})

	c := NewWithInterface(cs, Config{
		Namespace:               "crony-jobs",
		AllowImages:             []string{"busybox*"},
		PollInterval:            5 * time.Millisecond,
		TTLSecondsAfterFinished: 600,
	})
	c.logFn = func(context.Context, string, string, string, int64) (string, error) { return "", nil }

	out := c.RunK8sJob(context.Background(), spec)
	if out.Phase != "timeout" {
		t.Fatalf("期望超时，实际 %s", out.Phase)
	}
	if out.Err == nil || !strings.Contains(out.Err.Error(), "ImagePullBackOff") || !strings.Contains(out.Err.Error(), "no-such-tag") {
		t.Fatalf("Pod 被回收后仍然必须报出等待期间采集到的原因，实际: %v", out.Err)
	}
	if out.Artifacts["k8s_reason"] != "ImagePullBackOff" {
		t.Fatalf("原因必须落 artifacts，实际 %v", out.Artifacts)
	}
}

func TestReadOpsAreEnumeratedAndNamespaceChecked(t *testing.T) {
	c := NewWithInterface(fake.NewSimpleClientset(), Config{Namespace: "crony-jobs", AllowImages: []string{"*"}})
	// 未知 op 必须被拒（没有命令字符串，所以注入面为零；但枚举本身要守）。
	if _, err := c.Read(context.Background(), ReadRequest{Op: "exec"}); err == nil {
		t.Fatalf("未列出的只读操作必须被拒绝")
	}
	// 读也要过命名空间白名单。
	if _, err := c.Read(context.Background(), ReadRequest{Op: ReadOpPods, Namespace: "prod"}); err == nil {
		t.Fatalf("越权命名空间的读必须被拒绝")
	}
	if _, ok := ParseReadOp("POD_LOGS"); !ok {
		t.Fatalf("op 解析应大小写不敏感")
	}
}
