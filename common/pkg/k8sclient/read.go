package k8sclient

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tmnhs/crony/common/pkg/retry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fatal 标记"重试也不会好"的错误（配置错误、越权、目标不存在）。
// 与 executor 侧同一约定：跨包传递不丢失语义。
func fatal(err error) error {
	if err == nil {
		return nil
	}
	return retry.Fatal(err)
}

// ReadOp 只读查询类型。
//
// 为什么是"固定枚举"而不是转发一段 kubectl 命令：
//   - 固定枚举意味着**没有命令字符串**，也就没有命令注入面（与 tools 包
//     "不经过 shell" 的立场一致）；
//   - 每个 op 的返回都是我们渲染好的文本 + 证据引用，模型拿不到任意字段，
//     也就不会把 Secret 内容之类的敏感数据读进上下文。
type ReadOp string

const (
	// ReadOpPods 列出 Pod（含重启次数与等待原因——诊断 CrashLoop/ImagePull 的第一入口）。
	ReadOpPods ReadOp = "pods"
	// ReadOpPodLogs 取某个 Pod 的容器日志尾部。
	ReadOpPodLogs ReadOp = "pod_logs"
	// ReadOpDescribePod 单 Pod 的结构化摘要（状态、节点、容器、条件、最近事件）。
	ReadOpDescribePod ReadOp = "describe_pod"
	// ReadOpEvents 命名空间内的最近事件（Unschedulable/BackOff/FailedMount 都在这）。
	ReadOpEvents ReadOp = "events"
	// ReadOpDeployments 列出 Deployment 与其可用副本数。
	ReadOpDeployments ReadOp = "deployments"
	// ReadOpNodes 列出集群节点与状态（区分"我的 Pod 坏了"与"整台机器坏了"）。
	ReadOpNodes ReadOp = "nodes"
)

// ReadOps 允许的只读操作（工具层直接用它做白名单，避免两处各写一份）。
func ReadOps() []string {
	return []string{
		string(ReadOpPods), string(ReadOpPodLogs), string(ReadOpDescribePod),
		string(ReadOpEvents), string(ReadOpDeployments), string(ReadOpNodes),
	}
}

// ParseReadOp 解析 op（大小写不敏感）。
func ParseReadOp(raw string) (ReadOp, bool) {
	switch ReadOp(strings.ToLower(strings.TrimSpace(raw))) {
	case ReadOpPods:
		return ReadOpPods, true
	case ReadOpPodLogs:
		return ReadOpPodLogs, true
	case ReadOpDescribePod:
		return ReadOpDescribePod, true
	case ReadOpEvents:
		return ReadOpEvents, true
	case ReadOpDeployments:
		return ReadOpDeployments, true
	case ReadOpNodes:
		return ReadOpNodes, true
	default:
		return "", false
	}
}

// ReadRequest 一次只读查询。
type ReadRequest struct {
	Op        ReadOp
	Namespace string
	// Name Pod / Deployment 名。
	Name string
	// Container 容器名（pod_logs 用；空 = 第一个容器）。
	Container string
	// Selector Pod 标签选择器（pods 用，例如 app=web）。
	Selector string
	// TailLines 日志尾部行数（pod_logs 用，默认 200，上限 2000）。
	TailLines int64
	// Limit 列表行数上限（默认 50，上限 200）。
	Limit int
}

// ReadResult 只读查询结果。
type ReadResult struct {
	// Text 渲染后的文本（直接进提示词）。
	Text string
	// Refs 证据引用（形如 `k8s:pod/prod/web-1`）。
	//
	// 为什么单独给引用而不是让模型从文本里"抄"：证据引用的校验必须是机械的，
	// 模型抄错一个字符就会让"有证据"变成"捏造证据"。引用由我们生成，模型只能选择引用哪一条。
	Refs []string
}

const (
	defaultReadLimit = 50
	maxReadLimit     = 200
	defaultTailLines = 200
	maxTailLines     = 2000
)

// Read 执行一次只读查询。
//
// 所有查询都必须过命名空间白名单——**包括 get/list 在内的读**：
// 能读到 prod 的 Secret 名字、Pod 环境变量同样是信息泄露，
// 而 Agent 的上下文最终会进入模型提供方。
func (c *Client) Read(ctx context.Context, req ReadRequest) (ReadResult, error) {
	if !c.Enabled() {
		return ReadResult{}, fatal(fmt.Errorf("k8sclient: 集群客户端未初始化"))
	}
	ns := c.checkNamespace(req.Namespace)
	if req.Op != ReadOpNodes && !c.NamespaceAllowed(ns) {
		return ReadResult{}, fatal(fmt.Errorf("k8sclient: 命名空间 %q 不在允许列表内（默认只允许 %q）", ns, c.cfg.Namespace))
	}
	if req.Limit <= 0 {
		req.Limit = defaultReadLimit
	}
	if req.Limit > maxReadLimit {
		req.Limit = maxReadLimit
	}
	if req.TailLines <= 0 {
		req.TailLines = defaultTailLines
	}
	if req.TailLines > maxTailLines {
		req.TailLines = maxTailLines
	}

	switch req.Op {
	case ReadOpPods:
		return c.readPods(ctx, ns, req)
	case ReadOpPodLogs:
		return c.readPodLogs(ctx, ns, req)
	case ReadOpDescribePod:
		return c.readDescribePod(ctx, ns, req)
	case ReadOpEvents:
		return c.readEvents(ctx, ns, req)
	case ReadOpDeployments:
		return c.readDeployments(ctx, ns, req)
	case ReadOpNodes:
		return c.readNodes(ctx, req)
	default:
		return ReadResult{}, fatal(fmt.Errorf("k8sclient: 不支持的只读操作 %q", req.Op))
	}
}

func (c *Client) readPods(ctx context.Context, ns string, req ReadRequest) (ReadResult, error) {
	pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: req.Selector})
	if err != nil {
		return ReadResult{}, err
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	var b strings.Builder
	fmt.Fprintf(&b, "命名空间 %s 的 Pod（选择器 %q，共 %d 个）：\n", ns, req.Selector, len(pods.Items))
	res := ReadResult{}
	for i, p := range pods.Items {
		if i >= req.Limit {
			fmt.Fprintf(&b, "…（已省略 %d 个）\n", len(pods.Items)-req.Limit)
			break
		}
		restarts, reason := podContainerStatus(&p)
		ready := "0/0"
		if len(p.Status.ContainerStatuses) > 0 {
			readyN := 0
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Ready {
					readyN++
				}
			}
			ready = fmt.Sprintf("%d/%d", readyN, len(p.Status.ContainerStatuses))
		}
		line := fmt.Sprintf("- %s  phase=%s ready=%s restarts=%d node=%s", p.Name, p.Status.Phase, ready, restarts, p.Spec.NodeName)
		if reason != "" {
			line += " reason=" + reason
		}
		b.WriteString(line + "\n")
		res.Refs = append(res.Refs, "k8s:pod/"+ns+"/"+p.Name)
	}
	if len(pods.Items) == 0 {
		// "没有 Pod"本身是证据（说明没有部署/选择器写错），要明确写出来。
		b.WriteString("（没有任何 Pod：可能是选择器写错，或该工作负载根本没部署）\n")
	}
	res.Text = b.String()
	return res, nil
}

func (c *Client) readDescribePod(ctx context.Context, ns string, req ReadRequest) (ReadResult, error) {
	if strings.TrimSpace(req.Name) == "" {
		return ReadResult{}, fatal(fmt.Errorf("k8sclient: describe_pod 需要 name"))
	}
	pod, err := c.cs.CoreV1().Pods(ns).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		return ReadResult{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Pod %s/%s\n", ns, pod.Name)
	fmt.Fprintf(&b, "phase=%s node=%s qos=%s\n", pod.Status.Phase, pod.Spec.NodeName, pod.Status.QOSClass)
	if pod.Status.Reason != "" {
		fmt.Fprintf(&b, "reason=%s message=%s\n", pod.Status.Reason, pod.Status.Message)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		fmt.Fprintf(&b, "容器 %s: ready=%v restarts=%d image=%s\n", cs.Name, cs.Ready, cs.RestartCount, cs.Image)
		if w := cs.State.Waiting; w != nil {
			fmt.Fprintf(&b, "  等待中: %s %s\n", w.Reason, w.Message)
		}
		if t := cs.State.Terminated; t != nil {
			fmt.Fprintf(&b, "  已终止: exit=%d reason=%s message=%s\n", t.ExitCode, t.Reason, truncate(t.Message, 200))
		}
		if l := cs.LastTerminationState.Terminated; l != nil {
			// 上一次终止原因：区分"一直在崩"与"崩过一次后来好了"的关键。
			fmt.Fprintf(&b, "  上次终止: exit=%d reason=%s\n", l.ExitCode, l.Reason)
		}
	}
	for _, cond := range pod.Status.Conditions {
		fmt.Fprintf(&b, "条件 %s=%s reason=%s\n", cond.Type, cond.Status, cond.Reason)
	}
	evts, err := c.cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + pod.Name})
	if err == nil {
		for i, e := range evts.Items {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "事件 [%s] %s: %s\n", e.Type, e.Reason, truncate(e.Message, 200))
		}
	}
	return ReadResult{Text: b.String(), Refs: []string{"k8s:pod/" + ns + "/" + pod.Name}}, nil
}

func (c *Client) readPodLogs(ctx context.Context, ns string, req ReadRequest) (ReadResult, error) {
	if strings.TrimSpace(req.Name) == "" {
		return ReadResult{}, fatal(fmt.Errorf("k8sclient: pod_logs 需要 name"))
	}
	container := req.Container
	if container == "" {
		container = "main"
	}
	logs, err := c.podLogs(ctx, ns, req.Name, container, req.TailLines)
	if err != nil {
		return ReadResult{}, err
	}
	ref := "k8s:pod_logs/" + ns + "/" + req.Name + "/" + container
	return ReadResult{Text: logs, Refs: []string{ref}}, nil
}

func (c *Client) readEvents(ctx context.Context, ns string, req ReadRequest) (ReadResult, error) {
	events, err := c.cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ReadResult{}, err
	}
	sort.Slice(events.Items, func(i, j int) bool {
		return events.Items[i].LastTimestamp.After(events.Items[j].LastTimestamp.Time)
	})
	var b strings.Builder
	fmt.Fprintf(&b, "命名空间 %s 的最近事件（共 %d 条）：\n", ns, len(events.Items))
	for i, e := range events.Items {
		if i >= req.Limit {
			break
		}
		fmt.Fprintf(&b, "- [%s] %s %s/%s: %s\n",
			e.Type, e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, truncate(e.Message, 200))
	}
	return ReadResult{Text: b.String(), Refs: []string{"k8s:events/" + ns}}, nil
}

func (c *Client) readDeployments(ctx context.Context, ns string, req ReadRequest) (ReadResult, error) {
	deps, err := c.cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ReadResult{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "命名空间 %s 的 Deployment（共 %d 个）：\n", ns, len(deps.Items))
	res := ReadResult{}
	for i, d := range deps.Items {
		if i >= req.Limit {
			break
		}
		fmt.Fprintf(&b, "- %s desired=%d ready=%d available=%d image=%s\n",
			d.Name, derefInt32(d.Spec.Replicas), d.Status.ReadyReplicas, d.Status.AvailableReplicas, firstImage(d.Spec.Template.Spec.Containers))
		res.Refs = append(res.Refs, "k8s:deploy/"+ns+"/"+d.Name)
	}
	res.Text = b.String()
	return res, nil
}

func (c *Client) readNodes(ctx context.Context, req ReadRequest) (ReadResult, error) {
	nodes, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ReadResult{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "集群节点（共 %d 个）：\n", len(nodes.Items))
	res := ReadResult{}
	for i, n := range nodes.Items {
		if i >= req.Limit {
			break
		}
		ready := "Unknown"
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				ready = string(cond.Status)
			}
		}
		fmt.Fprintf(&b, "- %s ready=%s roles=%s kubelet=%s\n",
			n.Name, ready, nodeRoles(n), n.Status.NodeInfo.KubeletVersion)
		res.Refs = append(res.Refs, "k8s:node/"+n.Name)
	}
	res.Text = b.String()
	return res, nil
}

// podLogs 默认的日志实现（通过 client-go 的 raw 响应读取，便于按 tail 限制流量）。
func (c *Client) podLogs(ctx context.Context, namespace, pod, container string, tailLines int64) (string, error) {
	opts := &corev1.PodLogOptions{Container: container}
	if tailLines > 0 {
		t := tailLines
		opts.TailLines = &t
	}
	raw, err := c.cs.CoreV1().Pods(namespace).GetLogs(pod, opts).DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("k8sclient: 读取 %s/%s 容器 %s 日志失败: %w", namespace, pod, container, err)
	}
	return string(raw), nil
}

// ---------------------------------------------------------------------------
// 写动作：rollout restart（可逆写）
// ---------------------------------------------------------------------------

// RolloutRestart 触发 Deployment 滚动重启。
//
// 为什么选它作为**第一个**写工具：
//   - 它是运维最常用的"救命动作"（配置没变但进程僵住了、连接池泄漏、需要清缓存）；
//   - 它是**可逆的**：重启不改变期望状态，只是把 Pod 换一批；出问题再重启一次即可，
//     不会造成数据丢失（与删库/改网络有本质区别）；
//   - 它天然有明确的目标（deployment 名），因此爆炸半径可以被写清楚。
//
// 实现方式与 kubectl rollout restart 一致：给 Pod 模板打一个时间戳注解，
// 让 Deployment 认为模板变了从而滚动更新。**不重建 Deployment 对象**，
// 也不碰 replicas/image——那是另一个风险级别的事。
func (c *Client) RolloutRestart(ctx context.Context, namespace, deployment, requestedBy string) error {
	if !c.Enabled() {
		return fatal(fmt.Errorf("k8sclient: 集群客户端未初始化"))
	}
	ns := c.checkNamespace(namespace)
	if !c.NamespaceAllowed(ns) {
		return fatal(fmt.Errorf("k8sclient: 命名空间 %q 不在允许列表内（默认只允许 %q）", ns, c.cfg.Namespace))
	}
	name := strings.TrimSpace(deployment)
	if name == "" {
		return fatal(fmt.Errorf("k8sclient: rollout restart 需要 deployment 名"))
	}
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"crony.io/restartedAt":%q,"crony.io/restartedBy":%q}}}}}`,
		c.nowFn().UTC().Format("2006-01-02T15:04:05Z"), requestedBy)
	if _, err := c.cs.AppsV1().Deployments(ns).Patch(ctx, name, "application/merge-patch+json", []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("k8sclient: 重启 %s/%s 失败: %w", ns, name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func podContainerStatus(p *corev1.Pod) (restarts int32, reason string) {
	for _, cs := range p.Status.ContainerStatuses {
		restarts += cs.RestartCount
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			reason = cs.State.Waiting.Reason
		}
	}
	if reason == "" && p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	return restarts, reason
}

func firstImage(containers []corev1.Container) string {
	if len(containers) == 0 {
		return ""
	}
	return containers[0].Image
}

func derefInt32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

func nodeRoles(n corev1.Node) string {
	roles := make([]string, 0, 3)
	for k := range n.Labels {
		if strings.HasPrefix(k, "node-role.kubernetes.io/") {
			roles = append(roles, strings.TrimPrefix(k, "node-role.kubernetes.io/"))
		}
	}
	if len(roles) == 0 {
		return "<none>"
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}
