package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeK8sSource 只读集群数据源的测试替身。
type fakeK8sSource struct {
	ops     []string
	text    string
	refs    []string
	err     error
	lastOp  string
	lastNs  string
	lastNam string
	calls   int
}

func (f *fakeK8sSource) Ops() []string {
	if f.ops != nil {
		return f.ops
	}
	return []string{"pods", "pod_logs", "describe_pod", "events", "deployments", "nodes"}
}

func (f *fakeK8sSource) Query(_ context.Context, op, namespace, name, _ /*container*/, _ /*selector*/ string, _ int64, _ int) (string, []string, error) {
	f.calls++
	f.lastOp, f.lastNs, f.lastNam = op, namespace, name
	return f.text, f.refs, f.err
}

func TestK8sReadonlyToolSuccess(t *testing.T) {
	src := &fakeK8sSource{
		text: "命名空间 crony-jobs 的 Pod（共 2 个）：\n- web-1 phase=Running\n- web-2 phase=CrashLoopBackOff\n",
		refs: []string{"k8s:pod/crony-jobs/web-1", "k8s:pod/crony-jobs/web-2"},
	}
	tool := NewK8sReadonlyTool(src)

	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"op": "pods", "namespace": "crony-jobs",
	})
	if err != nil {
		t.Fatalf("查询不应抛异常（工具失败是观测，不是异常）：%v", err)
	}
	if !res.OK {
		t.Fatalf("期望成功，实际 %+v", res)
	}
	if src.lastOp != "pods" || src.lastNs != "crony-jobs" {
		t.Fatalf("参数未透传：%s/%s", src.lastOp, src.lastNs)
	}
	// 证据引用必须由数据源生成、且以 k8s 为来源——claimguard 靠这个来源前缀
	// 判断"模型是不是真的看过集群"。
	if len(res.Evidence) != 2 {
		t.Fatalf("期望 2 条证据，实际 %d", len(res.Evidence))
	}
	for _, ev := range res.Evidence {
		if ev.Source != "k8s" {
			t.Fatalf("证据来源必须是 k8s，实际 %q", ev.Source)
		}
		if !strings.HasPrefix(ev.Ref, "k8s:") {
			t.Fatalf("证据引用格式不符：%q", ev.Ref)
		}
	}
}

func TestK8sReadonlyToolRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"缺少 op", map[string]interface{}{}, "缺少 op"},
		{"op 不在白名单", map[string]interface{}{"op": "exec"}, "不在允许列表"},
		{"pod_logs 缺 name", map[string]interface{}{"op": "pod_logs"}, "必须指定 name"},
		{"describe_pod 缺 name", map[string]interface{}{"op": "describe_pod"}, "必须指定 name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeK8sSource{text: "x"}
			res, err := NewK8sReadonlyTool(src).Execute(context.Background(), tc.args)
			if err != nil {
				t.Fatalf("参数错误不应作为异常抛出：%v", err)
			}
			if res.OK {
				t.Fatalf("期望被拒，实际成功")
			}
			if !strings.Contains(res.Summary+res.Err, tc.want) {
				t.Fatalf("拒绝原因不含 %q：%s / %s", tc.want, res.Summary, res.Err)
			}
			// 关键：被拒的调用**不能**真的打到集群（白名单在入口就把关）。
			if src.calls != 0 {
				t.Fatalf("被拒的调用不应触达数据源，实际调用了 %d 次", src.calls)
			}
		})
	}
}

func TestK8sReadonlyToolFailureIsObservation(t *testing.T) {
	src := &fakeK8sSource{err: errors.New("connection refused")}
	res, err := NewK8sReadonlyTool(src).Execute(context.Background(), map[string]interface{}{"op": "pods"})
	if err != nil {
		t.Fatalf("数据源失败不应中断会话（模型需要看到失败才能改变策略）：%v", err)
	}
	if res.OK {
		t.Fatalf("失败必须体现为 OK=false")
	}
	if !strings.Contains(res.Err, "connection refused") {
		t.Fatalf("失败原因必须回灌给模型：%q", res.Err)
	}
}

func TestK8sReadonlyToolSchemaEnumComesFromSource(t *testing.T) {
	src := &fakeK8sSource{ops: []string{"pods", "events"}}
	tool := NewK8sReadonlyTool(src)
	schema := tool.Schema()
	prop, ok := schema.Properties["op"]
	if !ok {
		t.Fatalf("Schema 必须有 op")
	}
	if len(prop.Enum) != 2 {
		t.Fatalf("枚举应来自数据源（避免工具说能做、实现其实不能），实际 %v", prop.Enum)
	}
	// 数据源没声明的 op 必须被拒——枚举与校验同一个来源。
	res, _ := tool.Execute(context.Background(), map[string]interface{}{"op": "nodes"})
	if res.OK {
		t.Fatalf("数据源未声明的操作必须被拒")
	}
}

func TestK8sReadonlyToolEmptyResultIsEvidence(t *testing.T) {
	src := &fakeK8sSource{}
	res, _ := NewK8sReadonlyTool(src).Execute(context.Background(), map[string]interface{}{"op": "pods"})
	if !res.OK {
		t.Fatalf("空结果是合法结果")
	}
	if !strings.Contains(res.Summary, "空结果") {
		t.Fatalf("空结果必须被明确说明（否则模型会把'没查到'当成'不存在'）：%q", res.Summary)
	}
}

func TestK8sReadonlyToolNilSourceDenied(t *testing.T) {
	_, err := NewK8sReadonlyTool(nil).Execute(context.Background(), map[string]interface{}{"op": "pods"})
	if err == nil || !errors.Is(err, ErrToolDenied) {
		t.Fatalf("未接入数据源必须返回 ErrToolDenied，实际 %v", err)
	}
}

// 工具是**只读**的：这条断言是安全声明的一部分，不能靠"看代码觉得是"。
func TestK8sReadonlyToolIsReadOnly(t *testing.T) {
	tool := &K8sReadonlyTool{}
	if !tool.ReadOnly() || tool.Risk() != RiskRead {
		t.Fatalf("k8s_readonly 必须声明为只读/read 风险，实际 readonly=%v risk=%s", tool.ReadOnly(), tool.Risk())
	}
	if tool.Name() != "k8s_readonly" {
		t.Fatalf("工具名不符：%s", tool.Name())
	}
}

// 未配置集群时工具**不存在**（而不是存在但调用必失败）。
func TestK8sToolNotRegisteredWithoutSource(t *testing.T) {
	policy := DefaultPolicy()
	policy.AllowWrite = true
	r, err := NewReadonlyRegistry(policy, Sources{Logs: &fakeLogSource{}, NodeStatus: &fakeNodeSource{}})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	for _, name := range r.Names() {
		if name == "k8s_readonly" {
			t.Fatalf("未装配集群数据源时不应注册 k8s_readonly")
		}
	}
}
