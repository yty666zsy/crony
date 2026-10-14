//go:build integration

package k8sclient

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/tmnhs/crony/common/pkg/executor"
)

// 真集群集成测试（`go test -tags integration ./common/pkg/k8sclient/...`）。
//
// 为什么 fake clientset 之外还需要它——fake 能证明**我们的逻辑**，
// 证明不了只有在真集群才成立的三件事：
//  1. **最小权限够不够用**：RBAC 少一条 `pods/log`，fake 永远不会报错，
//     线上才变成"任务跑成功了但日志取不回来"；
//  2. **Job 真的能被调度并跑完**：调度器、镜像拉取、日志子资源都在集群侧；
//  3. **滚动重启真的改了 Deployment**：patch 被 API 接受、注解语义正确。
//
// 缺省**跳过**（而不是失败）：没有集群时把"没环境"报成"代码坏了"是最坏的噪声。
// 需要两个可选入参（见文件末尾的用法说明）：
//
//	CRONY_K8S_IT_NODE_KUBECONFIG   → 用 crony-node SA 的 kubeconfig
//	CRONY_K8S_IT_ADMIN_KUBECONFIG  → 用 crony-admin SA 的 kubeconfig
//	CRONY_K8S_IT_NAMESPACE         → 默认 crony
//	CRONY_K8S_IT_DEPLOYMENT        → 滚动重启的目标（如 crony-demo-web）
func TestRealClusterNodeServiceAccount(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("CRONY_K8S_IT_NODE_KUBECONFIG"))
	if path == "" {
		t.Skip("未设置 CRONY_K8S_IT_NODE_KUBECONFIG，跳过真集群任务执行验证")
	}
	ns := itNamespace()

	c, err := New(Config{
		Kubeconfig:              path,
		Namespace:               ns,
		AllowNamespaces:         []string{ns},
		AllowImages:             []string{"busybox*"},
		TTLSecondsAfterFinished: 120,
		PollInterval:            time.Second,
	})
	if err != nil {
		t.Fatalf("按 kubeconfig 建客户端失败: %v", err)
	}

	spec := executor.K8sJobSpec{
		RunID:     fmt.Sprintf("it-node-%d", time.Now().Unix()),
		JobID:     999001,
		StepIndex: 0,
		Name:      "it-echo",
		Image:     "busybox:1.36",
		Command:   []string{"/bin/echo", "crony-it-ok"},
		Timeout:   90 * time.Second,
	}
	out := c.RunK8sJob(context.Background(), spec)
	if out.Err != nil {
		t.Fatalf("crony-node 在真集群里跑不出任务（RBAC 少了什么？）: phase=%s err=%v", out.Phase, out.Err)
	}
	if out.Phase != "succeeded" || out.ExitCode != 0 {
		t.Fatalf("phase/exit 不符: %s/%d", out.Phase, out.ExitCode)
	}
	if !strings.Contains(out.Output, "crony-it-ok") {
		t.Fatalf("容器日志没取回来（RBAC 少 pods/log？）: %q", out.Output)
	}
	t.Logf("真集群任务成功：job=%s pod=%s", out.JobName, out.PodName)

	// 越权校验：crony-node 不该有能力滚动重启（那是 admin 的权限）。
	// 这条比 `kubectl auth can-i` 更硬——它走的是**代码真实调用路径**。
	if dep := strings.TrimSpace(os.Getenv("CRONY_K8S_IT_DEPLOYMENT")); dep != "" {
		if err := c.RolloutRestart(context.Background(), ns, dep, "it-node-must-fail"); err == nil {
			t.Fatalf("crony-node 不该有 deployments.patch 权限（%s）", dep)
		} else {
			t.Logf("按设计被拒: %v", err)
		}
	}
}

func TestRealClusterAdminRolloutRestart(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("CRONY_K8S_IT_ADMIN_KUBECONFIG"))
	if path == "" {
		t.Skip("未设置 CRONY_K8S_IT_ADMIN_KUBECONFIG，跳过真集群滚动重启验证")
	}
	ns := itNamespace()
	dep := strings.TrimSpace(os.Getenv("CRONY_K8S_IT_DEPLOYMENT"))
	if dep == "" {
		t.Skip("未设置 CRONY_K8S_IT_DEPLOYMENT，跳过滚动重启验证")
	}

	c, err := New(Config{
		Kubeconfig:      path,
		Namespace:       ns,
		AllowNamespaces: []string{ns},
		// 必须显式放开镜像白名单：否则拒绝发生在**我们的配置闸门**上，
		// 就证明不了 API 侧真的拦住了 jobs.create（RBAC 才是这条断言的对象）。
		AllowImages: []string{"busybox*"},
	})
	if err != nil {
		t.Fatalf("按 kubeconfig 建客户端失败: %v", err)
	}

	before := restartedAt(t, c, ns, dep)
	beforePods := podsOfDeployment(t, c, ns, dep)
	if err := c.RolloutRestart(context.Background(), ns, dep, "integration-test"); err != nil {
		t.Fatalf("crony-admin 必须能滚动重启（RBAC 少了 deployments.patch？）: %v", err)
	}
	after := restartedAt(t, c, ns, dep)
	if after == "" || after == before {
		t.Fatalf("注解没变等于没重启：before=%q after=%q", before, after)
	}
	t.Logf("重启注解已写入：%q → %q", before, after)

	// 断言到"Pod 真的换了一批"为止——注解只是机制，Pod 换代才是运维看到的结果。
	deadline := time.Now().Add(90 * time.Second)
	for {
		now := podsOfDeployment(t, c, ns, dep)
		if len(now) > 0 && disjoint(beforePods, now) {
			t.Logf("滚动重启完成：Pod %v → %v", beforePods, now)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("90s 内 Pod 没有换代：before=%v now=%v", beforePods, now)
		}
		time.Sleep(2 * time.Second)
	}

	// 反向：crony-admin 是**只读 + 重启**的最小权限，不该能创建 Job（那是 crony-node 的权限）。
	spec := executor.K8sJobSpec{
		RunID:   fmt.Sprintf("it-admin-%d", time.Now().Unix()),
		JobID:   999002,
		Name:    "it-admin-must-fail",
		Image:   "busybox:1.36",
		Command: []string{"/bin/echo", "x"},
		Timeout: 30 * time.Second,
	}
	if out := c.RunK8sJob(context.Background(), spec); out.Err == nil {
		t.Fatalf("crony-admin 不该有 jobs.create 权限")
	} else if !strings.Contains(out.Err.Error(), "forbidden") {
		// 关键是"被谁拦住"：这里要的是 API 侧的 RBAC 拒绝，
		// 不是我们自己的配置闸门（那只能证明配置写对了，证明不了权限收得住）。
		t.Fatalf("拒绝必须来自 API 侧 RBAC（forbidden），实际: %v", out.Err)
	} else {
		t.Logf("按设计被拒: %v", out.Err)
	}
}

// podsOfDeployment 取某个 Deployment 名下的 Pod 名集合。
//
// 用 label selector（而不是 Pod 名前缀）判换代：Deployment 的 selector 是它的定义，
// 前缀会随 ReplicaSet 模板哈希变化，拿前缀做断言会在别人的命名规约下悄悄失效。
func podsOfDeployment(t *testing.T, c *Client, namespace, name string) []string {
	t.Helper()
	d, err := c.cs.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("读 Deployment %s/%s 失败: %v", namespace, name, err)
	}
	sel := labels.Set(d.Spec.Selector.MatchLabels).AsSelector().String()
	list, err := c.cs.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		t.Fatalf("按 selector %q 列 Pod 失败: %v", sel, err)
	}
	out := make([]string, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].Name)
	}
	return out
}

// disjoint 判断两批 Pod 名有没有重叠（有重叠 = 还有老 Pod 没退，换代没完成）。
func disjoint(before, now []string) bool {
	set := make(map[string]struct{}, len(before))
	for _, n := range before {
		set[n] = struct{}{}
	}
	for _, n := range now {
		if _, ok := set[n]; ok {
			return false
		}
	}
	return true
}

func itNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("CRONY_K8S_IT_NAMESPACE")); ns != "" {
		return ns
	}
	return "crony"
}

// restartedAt 读 Deployment 的重启注解。
//
// 注意断言的是 **crony.io/restartedAt**（crony 自己的注解），不是
// kubectl 的 kubectl.kubernetes.io/restartedAt：两者机制相同但键不同，
// 早期测试就是在这里写错键、把"重启成功"误报成"没重启"——
// 断言必须盯着实现真正写下的那一个，而不是我们记忆里的那个。
func restartedAt(t *testing.T, c *Client, namespace, name string) string {
	t.Helper()
	d, err := c.cs.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("读 Deployment %s/%s 失败: %v", namespace, name, err)
	}
	return d.Spec.Template.Annotations["crony.io/restartedAt"]
}
