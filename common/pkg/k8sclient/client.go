// Package k8sclient 是 crony 与 Kubernetes 之间的**唯一**通道。
//
// 为什么单独成包（而不是把 client-go 直接写进 executor / tools）：
//   - executor 是"步骤执行"的通用抽象，不该依赖 k8s 库，否则执行层的
//     全部语义都要靠一个真集群才能测；
//   - tools 是安全网关，它的白名单/审计语义也不该被 k8s 的对象结构污染；
//   - 集中一处后，"允许访问哪些命名空间、允许用哪些镜像、Pod 带什么安全上下文"
//     这类**边界**只有一个答案，不会出现"执行器放行了、Agent 工具没放行"的错配。
//
// 设计取向与项目其它写能力一致：**默认关闭、显式打开**。
// 镜像是"在集群里跑任意代码"的载体，因此镜像白名单默认为空 = 不允许任何镜像；
// 命名空间白名单默认为空 = 只允许配置里的那一个命名空间。
package k8sclient

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tmnhs/crony/common/pkg/executor"
)

// Config k8s 接入配置（对应 configs 里的 k8s 段）。
//
// 这里的每个字段都对应一条边界，而不是"可选优化"：
// 少一条边界，Agent 或任务就能多碰一类东西。
type Config struct {
	// Kubeconfig 显式指定的 kubeconfig 路径。
	// 为空时按 in-cluster → KUBECONFIG 环境变量 → ~/.kube/config 的顺序尝试。
	Kubeconfig string
	// Context kubeconfig 里的 context 名（空 = current-context）。
	Context string
	// Namespace 默认命名空间（空 = default）。
	Namespace string

	// AllowNamespaces 允许的命名空间白名单。
	//
	// 空 = **只允许 Namespace 这一个**（不是"允许全部"）。
	// 需要放开多个命名空间时必须逐个写出来——集群里"顺手多给一个 ns"
	// 往往会连带把 prod 暴露出去。
	AllowNamespaces []string

	// AllowImages 允许的镜像白名单，支持结尾通配 `busybox*`。
	//
	// 空 = **不允许任何镜像**（默认关闭）。这是刻意的：
	// 创建 Pod 等于"在集群里执行一段任意代码"，它比"重跑一个失败任务"
	// 危险得多，不能因为"顺手能用"就默认打开。
	// 明确写 `["*"]` 表示"接受任意镜像"，属于显式承担风险的选择。
	AllowImages []string

	// Timeout 单次 Job 的默认墙钟上限（<=0 用 executor.DefaultTimeout）。
	Timeout time.Duration
	// PollInterval 等待 Job 结束的轮询间隔（默认 2s；测试可调小）。
	PollInterval time.Duration

	// TTLSecondsAfterFinished Job 结束后保留多久自动清理（默认 600s）。
	//
	// 为什么不是 0（立刻删）：失败现场（Pod 事件、上一次容器日志）是排查的关键证据，
	// 立刻删会让"为什么起不来"永远无法回答。留 10 分钟足够人去 kubectl describe。
	TTLSecondsAfterFinished int32

	// ServiceAccountName Pod 使用的服务账号（空 = default）。
	ServiceAccountName string
	// DefaultCPU / DefaultMemory 容器的 requests/limits（空 = 不设置）。
	DefaultCPU    string
	DefaultMemory string

	// AllowPrivilegeEscalation 允许容器提权（默认 false）。
	AllowPrivilegeEscalation bool
}

// Normalize 补齐默认值。
func (c Config) Normalize() Config {
	if strings.TrimSpace(c.Namespace) == "" {
		c.Namespace = "default"
	}
	c.Namespace = strings.TrimSpace(c.Namespace)
	c.Kubeconfig = strings.TrimSpace(c.Kubeconfig)
	c.Context = strings.TrimSpace(c.Context)
	if c.Timeout <= 0 {
		c.Timeout = executor.DefaultTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.TTLSecondsAfterFinished < 0 {
		c.TTLSecondsAfterFinished = 0
	} else if c.TTLSecondsAfterFinished == 0 {
		c.TTLSecondsAfterFinished = 600
	}
	return c
}

// ErrNotConfigured 表示没有可用的集群接入配置（kubeconfig 缺失等）。
//
// 调用方应当据此**不注册** k8s 能力，而不是注册一个"调用必失败"的工具：
// 那会让模型以为"探测过了，集群里什么都没有"。
var ErrNotConfigured = fmt.Errorf("k8sclient: 未找到可用的 kubeconfig（in-cluster/KUBECONFIG/~/.kube/config 均不可用）")

// Client 集群客户端。
type Client struct {
	cs  kubernetes.Interface
	cfg Config

	// logFn 取 Pod 容器日志。
	//
	// 之所以做成可注入而不是直接调 client-go：日志是"判定失败原因"的关键输入，
	// 它的失败路径（拿不到日志）必须能被测试覆盖，而 fake clientset 不实现
	// pods/log 的 raw 响应。
	logFn func(ctx context.Context, namespace, pod, container string, tailLines int64) (string, error)

	// nowFn 便于测试控制时间（等待循环的截止判断）。
	nowFn func() time.Time
}

// New 按配置建立客户端。找不到配置时返回 ErrNotConfigured（调用方据此降级）。
func New(cfg Config) (*Client, error) {
	cfg = cfg.Normalize()
	restCfg, err := buildRESTConfig(cfg)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8sclient: 创建客户端失败: %w", err)
	}
	return NewWithInterface(cs, cfg), nil
}

// NewWithInterface 用给定的 clientset 构造客户端（测试注入 fake 用）。
func NewWithInterface(cs kubernetes.Interface, cfg Config) *Client {
	cfg = cfg.Normalize()
	c := &Client{cs: cs, cfg: cfg, nowFn: time.Now}
	c.logFn = c.podLogs
	return c
}

// Config 返回归一化后的配置（只读用途）。
func (c *Client) Config() Config { return c.cfg }

// DefaultNamespace 默认命名空间。
func (c *Client) DefaultNamespace() string { return c.cfg.Namespace }

// Enabled 是否可用。
func (c *Client) Enabled() bool { return c != nil && c.cs != nil }

// buildRESTConfig 按 in-cluster → KUBECONFIG → ~/.kube/config 的顺序找配置。
//
// 顺序不能反：在集群里跑（Pod 内）时必须优先用 ServiceAccount，
// 否则会因为镜像里残留了一份旧 kubeconfig 而连到错误的集群。
func buildRESTConfig(cfg Config) (*rest.Config, error) {
	if cfg.Kubeconfig != "" {
		return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: cfg.Kubeconfig},
			&clientcmd.ConfigOverrides{CurrentContext: cfg.Context},
		).ClientConfig()
	}
	if inCluster, err := rest.InClusterConfig(); err == nil {
		return inCluster, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if env := strings.TrimSpace(os.Getenv("KUBECONFIG")); env != "" {
		rules.ExplicitPath = env
	}
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules,
		&clientcmd.ConfigOverrides{CurrentContext: cfg.Context},
	).ClientConfig()
	if err != nil {
		return nil, ErrNotConfigured
	}
	return restCfg, nil
}

// ---------------------------------------------------------------------------
// 边界校验
// ---------------------------------------------------------------------------

// checkImage 镜像白名单校验。
func (c *Client) checkImage(image string) error {
	image = strings.TrimSpace(image)
	if image == "" {
		return fmt.Errorf("k8sclient: 容器镜像为空")
	}
	if len(c.cfg.AllowImages) == 0 {
		return fmt.Errorf("k8sclient: 未配置 k8s.allow_images（空表示不允许任何镜像）；" +
			"这是刻意的默认关闭，请显式列出允许的镜像")
	}
	for _, pattern := range c.cfg.AllowImages {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if pattern == "*" || matchPrefixPattern(pattern, image) {
			return nil
		}
	}
	return fmt.Errorf("k8sclient: 镜像 %q 不在允许列表内", image)
}

// matchPrefixPattern 支持 `busybox*` 与精确匹配两种写法。
//
// 刻意不支持 `*busybox*`/正则：镜像名里的通配越灵活，"以为限制了、其实放开了"
// 的面就越大。前缀通配刚好够表达"允许某仓库下的镜像"。
func matchPrefixPattern(pattern, image string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(image, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == image
}

// checkNamespace 命名空间白名单校验。
func (c *Client) checkNamespace(ns string) string {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return c.cfg.Namespace
	}
	return ns
}

// NamespaceAllowed 该命名空间是否在白名单内。
func (c *Client) NamespaceAllowed(ns string) bool {
	ns = c.checkNamespace(ns)
	if ns == c.cfg.Namespace {
		return true
	}
	for _, allowed := range c.cfg.AllowNamespaces {
		if strings.TrimSpace(allowed) == ns {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 执行：在集群里跑一个 Job
// ---------------------------------------------------------------------------

// RunK8sJob 实现 executor.K8sRunner：创建 Job → 等结束 → 收日志 → 清理。
//
// 关键设计：**BackoffLimit = 0**。
// 重试是 crony 已有的能力（retry.Policy，带指数退避、总时长上限、可重试/不可重试分类），
// 若同时让 k8s 自己重试，就会出现"N×M 次执行"的隐形放大，而且失败的退出码
// 会落在最后那次尝试上，"到底失败了几次"谁都说不清。
func (c *Client) RunK8sJob(ctx context.Context, spec executor.K8sJobSpec) executor.K8sJobOutcome {
	out := executor.K8sJobOutcome{Namespace: c.checkNamespace(spec.Namespace), ExitCode: -1, Artifacts: map[string]string{}}
	if !c.Enabled() {
		out.Err = fmt.Errorf("k8sclient: 集群客户端未初始化")
		return out
	}
	if err := c.checkImage(spec.Image); err != nil {
		out.Err = fatal(err)
		return out
	}
	if !c.NamespaceAllowed(out.Namespace) {
		out.Err = fatal(fmt.Errorf("k8sclient: 命名空间 %q 不在允许列表内（默认只允许 %q）", out.Namespace, c.cfg.Namespace))
		return out
	}

	job, err := c.buildJob(spec, out.Namespace)
	if err != nil {
		out.Err = fatal(err)
		return out
	}

	created, err := c.cs.BatchV1().Jobs(out.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// 同一个 run/step 重复提交（重试或重复触发）：旧 Job 的结果对本次没有意义，
			// 清掉再建，避免"拿上一次的结论当这次的结论"。
			_ = c.cs.BatchV1().Jobs(out.Namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: ptrDeletion(metav1.DeletePropagationBackground)})
			created, err = c.cs.BatchV1().Jobs(out.Namespace).Create(ctx, job, metav1.CreateOptions{})
		}
		if err != nil {
			out.Err = fatal(fmt.Errorf("k8sclient: 创建 Job 失败: %w", err))
			return out
		}
	}
	out.JobName = created.Name
	out.Artifacts["k8s_job"] = created.Name

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = c.cfg.Timeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	verdict := c.waitJob(waitCtx, out.Namespace, created.Name)
	out.Phase = verdict.phase
	out.PodName = verdict.podName
	out.ExitCode = verdict.exitCode
	if out.PodName != "" {
		out.Artifacts["k8s_pod"] = out.PodName
	}
	if verdict.reason != "" {
		out.Artifacts["k8s_reason"] = verdict.reason
	}

	// 收日志：即使失败也要收（失败日志才是最有用的）。
	if out.PodName != "" {
		logs, logErr := c.logFn(waitCtx, out.Namespace, out.PodName, "main", 2000)
		if logErr != nil && verdict.err == nil {
			// 日志拿不到本身不是任务失败：任务已经成功了，只是证据缺一块。
			// 这种情况必须落 artifacts 说明，而不是让整个步骤变成失败。
			out.Artifacts["k8s_log_error"] = truncate(logErr.Error(), 200)
		}
		out.Output = logs
	}
	out.Err = verdict.err

	// 清理：配了 TTL 就交给集群自动回收（保留窗口内可人工查现场）；
	// 没配就立刻删，避免集群里堆积 Job。
	if c.cfg.TTLSecondsAfterFinished == 0 {
		_ = c.cs.BatchV1().Jobs(out.Namespace).Delete(context.Background(), created.Name,
			metav1.DeleteOptions{PropagationPolicy: ptrDeletion(metav1.DeletePropagationBackground)})
	}
	return out
}

// verdict 等待循环的判定结果。
type verdict struct {
	phase    string
	podName  string
	exitCode int
	reason   string
	err      error
}

// waitJob 轮询 Job 状态直到终态。
//
// 为什么用轮询而不是 watch：
//   - watch 需要处理 re-list、resourceVersion 过期、连接断开重连等一堆边界，
//     而这些边界一旦处理错，表现是"任务永远停在 Running"——比后者严重得多；
//   - 轮询虽然多几次请求（默认 2s 一次），但它的失败模式是"慢"，不是"错"。
//     运维系统里"慢"可以接受，"错"不可以。
func (c *Client) waitJob(ctx context.Context, namespace, name string) verdict {
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	lastErr := ""
	// stuck 记录"等待期间看到过的卡住原因"。
	//
	// 为什么必须在循环里采集、而不是等到超时再查现场：
	// 真集群实测——Job 的 activeDeadlineSeconds 到点时，Job 控制器会**先把 Pod 回收**，
	// 等我们带着 context deadline 去读 Pod 时它已经不存在了，于是最该看到的那条原因
	// （`ImagePullBackOff: busybox:no-such-tag not found`）恰好丢掉，用户只看到"超时"。
	// 等待期间 Pod 还在——那才是取证窗口。
	stuck := podStuck{}
	for {
		job, err := c.cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return c.deadlineVerdict(ctx, namespace, name, stuck)
			}
			// 读一次失败不致命（apiserver 抖动），记下来继续轮询。
			lastErr = err.Error()
		} else {
			if v, done := c.evaluateJob(ctx, namespace, job); done {
				return v
			}
			// 还没到终态：看一眼 Pod 卡在哪。每次轮询一次带标签过滤的 List，
			// 只在 k8s 任务运行期间发生，开销可忽略；换来的是"超时也有原因"。
			if pod, perr := c.podForJob(ctx, namespace, name); perr == nil && pod != nil {
				if stuck.podName == "" {
					stuck.podName = pod.Name
				}
				if r, d := podWaitingReason(pod); r != "" {
					stuck.reason, stuck.detail = r, d
				}
			}
		}
		select {
		case <-ctx.Done():
			return c.deadlineVerdict(ctx, namespace, name, stuck, lastErr)
		case <-ticker.C:
		}
	}
}

// podStuck 等待期间采集到的"卡住"证据（超时时 Pod 可能已不可读，只能靠它）。
type podStuck struct {
	reason  string
	detail  string
	podName string
}

// deadlineVerdict 超时/取消时的收尾。
//
// 这里有两件必须做对的事，都是**真集群实测**踩出来的：
//
//  1. **先取证，再收拾**。超时最常见的真实原因是 Pod 卡在某个等待状态
//     （ImagePullBackOff / CreateContainerConfigError / CrashLoopBackOff），
//     而不是"任务跑得太久"。只报"等待 Job 结束超时"等于把最有价值的那条信息扔掉：
//     实测里镜像是 `busybox:no-such-tag`，用户看到的却是"超时"，
//     于是他去查集群负载，而真相一直写在 Pod 的 Waiting.Message 里。
//  2. **停工作负载，但保留现场**。删 Pod（停掉还在跑的容器）而不是删整个 Job：
//     Job、事件、Pod 的终止原因在 TTL 窗口（默认 600s）内仍可 `kubectl describe`。
//     只有显式配了 TTL=0（"立刻回收"）才连 Job 一起删——那是一个明确的取舍，不是默认。
func (c *Client) deadlineVerdict(ctx context.Context, namespace, name string, during podStuck, extra ...string) verdict {
	v := verdict{phase: "timeout", exitCode: -1, podName: during.podName}
	reason, detail := during.reason, during.detail
	// 优先用"此刻还读得到"的 Pod 状态（信息更全：可能有 Terminated 的原因）；
	// 读不到就退回等待期间采集到的那份证据——这就是它存在的意义。
	if pod, err := c.podForJob(context.Background(), namespace, name); err == nil && pod != nil {
		v.podName = pod.Name
		if r, d := podStuckReason(pod); r != "" {
			reason, detail = r, d
		}
	}
	v.reason = reason

	// 停止工作负载：优先只删 Pod，让 Job 与事件活到 TTL 到点。
	// 注意顺序——先取证再删，否则 Pod 一删，"为什么卡住"就跟着没了。
	if v.podName != "" && c.cfg.TTLSecondsAfterFinished != 0 {
		_ = c.cs.CoreV1().Pods(namespace).Delete(context.Background(), v.podName, metav1.DeleteOptions{})
	} else {
		_ = c.cs.BatchV1().Jobs(namespace).Delete(context.Background(), name,
			metav1.DeleteOptions{PropagationPolicy: ptrDeletion(metav1.DeletePropagationBackground)})
	}

	if ctx.Err() != nil && strings.Contains(ctx.Err().Error(), "canceled") {
		v.err = context.Canceled
		return v
	}
	msg := "等待 Job 结束超时"
	if reason != "" {
		msg += "（Pod 卡在 " + reason
		if detail != "" {
			msg += "：" + truncate(detail, 200)
		}
		msg += "）"
	}
	if len(extra) > 0 && extra[0] != "" {
		msg += "（最近一次查询失败：" + extra[0] + "）"
	}
	v.err = fmt.Errorf("%s: %w", msg, context.DeadlineExceeded)
	return v
}

// podWaitingReason 只取"容器还在等"的原因（等待期间用）。
//
// 刻意不看 Terminated：等待循环里 Pod 的容器一旦终止，Job 基本就到达终态、
// 循环也该退出了；此时若把"上一次重启的终止原因"记成卡住原因，会把
// CrashLoopBackOff 的现场错误地描述成"容器已退出"。
func podWaitingReason(pod *corev1.Pod) (reason, detail string) {
	if pod == nil {
		return "", ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return w.Reason, w.Message
		}
	}
	return "", ""
}

// podStuckReason 提取 Pod "卡住"的物理解释（原因 + 细节）。
//
// 与 finishVerdict 的区别：那里还要判断"这个失败值不值得重试"（fatalReason），
// 这里只要**事实**——超时路径上不该混入重试语义，否则同一条错误会在两个地方被解读两次。
func podStuckReason(pod *corev1.Pod) (reason, detail string) {
	if pod == nil {
		return "", ""
	}
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return w.Reason, w.Message
		}
		if t := cs.State.Terminated; t != nil {
			if t.Reason != "" {
				reason = t.Reason
			}
			if t.Message != "" {
				detail = t.Message
			}
		}
	}
	return reason, detail
}

// evaluateJob 判断 Job 是否到达终态。
func (c *Client) evaluateJob(ctx context.Context, namespace string, job *batchv1.Job) (verdict, bool) {
	if job == nil {
		return verdict{}, false
	}
	failed := job.Status.Failed
	succeeded := job.Status.Succeeded
	// ActiveDeadlineSeconds 触发时，Job 会被标记 Failed 且 reason=DeadlineExceeded。
	if isDeadlineExceeded(job) {
		v := c.finishVerdict(ctx, namespace, job, "timeout", -1)
		v.err = fmt.Errorf("Job 超过集群侧时间上限: %w", context.DeadlineExceeded)
		return v, true
	}
	if succeeded > 0 {
		return c.finishVerdict(ctx, namespace, job, "succeeded", 0), true
	}
	if failed > 0 {
		// BackoffLimit=0：只要失败一次就是终态（重试交给 crony）。
		v := c.finishVerdict(ctx, namespace, job, "failed", -1)
		v.err = fmt.Errorf("容器退出码 %d", v.exitCode)
		return v, true
	}
	return verdict{}, false
}

// finishVerdict 取 Pod 名与退出码/等待原因。
func (c *Client) finishVerdict(ctx context.Context, namespace string, job *batchv1.Job, phase string, defaultExit int) verdict {
	v := verdict{phase: phase, exitCode: defaultExit}
	pod, err := c.podForJob(ctx, namespace, job.Name)
	if err != nil || pod == nil {
		if phase == "succeeded" {
			v.exitCode = 0
		}
		return v
	}
	v.podName = pod.Name
	if pod.Status.Reason != "" {
		v.reason = pod.Status.Reason
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if term := cs.State.Terminated; term != nil {
			v.exitCode = int(term.ExitCode)
			if term.Reason != "" {
				v.reason = term.Reason
			}
			return v
		}
		if waiting := cs.State.Waiting; waiting != nil && waiting.Reason != "" {
			// 典型的"根本没跑起来"：ImagePullBackOff / CreateContainerConfigError /
			// CrashLoopBackOff（配了 TTL 才会看到）。这类原因必须落 artifacts——
			// 只报"退出码 -1"对排查毫无帮助。
			v.reason = waiting.Reason
			v.err = fatalReason(waiting.Reason, waiting.Message)
			return v
		}
	}
	if phase == "succeeded" {
		v.exitCode = 0
	}
	return v
}

// podForJob 按 job-name 标签找 Pod（k8s 会自动打 `job-name=<job>` 标签）。
func (c *Client) podForJob(ctx context.Context, namespace, jobName string) (*corev1.Pod, error) {
	pods, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	// 多个 Pod（重试/驱逐）时取最新的那个：它才代表"最终发生了什么"。
	best := &pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = &pods.Items[i]
		}
	}
	return best, nil
}

// buildJob 构造 Job 对象（纯函数，可直接单测）。
func (c *Client) buildJob(spec executor.K8sJobSpec, namespace string) (*batchv1.Job, error) {
	if strings.TrimSpace(spec.Image) == "" {
		return nil, fmt.Errorf("k8sclient: 镜像为空")
	}
	backoff := int32(0)
	ttl := c.cfg.TTLSecondsAfterFinished
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "crony",
		"crony/run-id":                 sanitizeLabel(spec.RunID),
		"crony/job-id":                 fmt.Sprintf("%d", spec.JobID),
		"crony/step":                   fmt.Sprintf("%d", spec.StepIndex),
	}

	env := []corev1.EnvVar{
		{Name: "CRONY_RUN_ID", Value: spec.RunID},
		{Name: "CRONY_JOB_ID", Value: fmt.Sprintf("%d", spec.JobID)},
		{Name: "CRONY_STEP_INDEX", Value: fmt.Sprintf("%d", spec.StepIndex)},
	}
	// map 遍历顺序随机，排序保证"同样的请求产生同样的 Job 定义"（diff 友好、可回放）。
	for _, k := range sortedKeys(spec.Env) {
		env = append(env, corev1.EnvVar{Name: k, Value: spec.Env[k]})
	}

	container := corev1.Container{
		Name:                     "main",
		Image:                    spec.Image,
		Command:                  spec.Command,
		Env:                      env,
		ImagePullPolicy:          corev1.PullIfNotPresent,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
	if limits, err := c.resources(); err != nil {
		return nil, err
	} else if limits != nil {
		container.Resources = *limits
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        JobName(spec),
			Namespace:   namespace,
			Labels:      labels,
			Annotations: map[string]string{"crony/trace-id": spec.TraceID, "crony/step-name": spec.Name},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// 这个 Pod 没有任何理由访问 API Server：关掉 token 挂载，
					// 就算镜像被换成一个恶意程序，它也没有集群凭据可用。
					AutomountServiceAccountToken: ptrBool(false),
					ServiceAccountName:           c.cfg.ServiceAccountName,
					Containers:                   []corev1.Container{container},
				},
			},
		},
	}
	if d := spec.Timeout; d > 0 {
		secs := int64(d.Seconds())
		if secs > 0 {
			job.Spec.ActiveDeadlineSeconds = &secs
		}
	}
	if !c.cfg.AllowPrivilegeEscalation {
		job.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptrBool(false),
		}
	}
	return job, nil
}

// resources 解析配置里的 CPU/内存（在装配期就报错，而不是留给 k8s 拒收）。
func (c *Client) resources() (*corev1.ResourceRequirements, error) {
	if c.cfg.DefaultCPU == "" && c.cfg.DefaultMemory == "" {
		return nil, nil
	}
	req := corev1.ResourceList{}
	if c.cfg.DefaultCPU != "" {
		q, err := resource.ParseQuantity(c.cfg.DefaultCPU)
		if err != nil {
			return nil, fmt.Errorf("k8sclient: default_cpu 不是合法数量 %q: %w", c.cfg.DefaultCPU, err)
		}
		req[corev1.ResourceCPU] = q
	}
	if c.cfg.DefaultMemory != "" {
		q, err := resource.ParseQuantity(c.cfg.DefaultMemory)
		if err != nil {
			return nil, fmt.Errorf("k8sclient: default_memory 不是合法数量 %q: %w", c.cfg.DefaultMemory, err)
		}
		req[corev1.ResourceMemory] = q
	}
	return &corev1.ResourceRequirements{Requests: req, Limits: req}, nil
}

// JobName 生成确定性的 Job 名：同一个 (job_id, step, run_id) 永远得到同一个名字。
//
// 确定性有两个好处：重复提交能靠 AlreadyExists 被识别（而不是悄悄起两个 Pod），
// 以及排错时能直接说出"集群里那个 Job 叫什么"。
func JobName(spec executor.K8sJobSpec) string {
	sum := sha1.Sum([]byte(spec.RunID))
	name := fmt.Sprintf("crony-%d-%d-%s", spec.JobID, spec.StepIndex, hex.EncodeToString(sum[:])[:8])
	return sanitizeDNS(name)
}

// sanitizeDNS 把名字收敛成 RFC1123 label：小写字母/数字/`-`，最长 63。
//
// k8s 不接受大写、下划线、点，而 run_id 是人类可读的（可能含这些字符），
// 所以必须在提交前收敛；否则报错会出现在集群侧、离问题现场很远。
func sanitizeDNS(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	if out == "" {
		out = "crony-job"
	}
	return out
}

// sanitizeLabel 标签值同样有字符限制（但允许大小写与 `_`/`.`）。
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-_.")
	}
	return out
}

func isDeadlineExceeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Reason == "DeadlineExceeded" {
			return true
		}
	}
	return false
}

// fatalReason 把"容器根本没跑起来"的等待原因翻译成可操作错误，并区分是否可重试。
//
// 分界线：**"再排一次可能会好"**（拉取镜像的镜像仓库瞬时不可用、节点暂时没资源）
// 留在可重试一侧；**"重试也不会好"**（镜像名写错、配置引用不存在的 Secret/ConfigMap）
// 必须标记 Fatal，否则会把配置错误放大成重试风暴。
func fatalReason(reason, message string) error {
	msg := strings.TrimSpace(reason)
	if m := strings.TrimSpace(message); m != "" {
		msg += ": " + truncate(m, 200)
	}
	switch reason {
	case "ImagePullBackOff", "ErrImagePull":
		// 仓库不可达/认证失败往往是瞬时的：交给重试策略处理。
		return fmt.Errorf("容器镜像拉取失败（%s）", msg)
	case "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
		return fatal(fmt.Errorf("容器无法创建（%s）", msg))
	default:
		return fmt.Errorf("容器未启动（%s）", msg)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 手写插入排序：这里 n 极小（几个环境变量），不值得引 sort 的开销讨论，
	// 但与全项目保持一致的稳定输出更重要。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func ptrBool(b bool) *bool { return &b }

func ptrDeletion(p metav1.DeletionPropagation) *metav1.DeletionPropagation { return &p }
