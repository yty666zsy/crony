package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRolloutExecutor 集群写动作的测试替身。
type fakeRolloutExecutor struct {
	got   K8sRolloutRequest
	calls int
	res   Result
	err   error
}

func (f *fakeRolloutExecutor) RolloutRestart(_ context.Context, req K8sRolloutRequest) (Result, error) {
	f.calls++
	f.got = req
	return f.res, f.err
}

// 写工具的静态属性必须是"写"，否则自治级别与审批闸门会按只读放行。
func TestK8sRolloutToolIsWriteAndNotReadOnly(t *testing.T) {
	tool := &K8sRolloutTool{}
	if tool.Name() != "k8s_rollout_restart" {
		t.Fatalf("工具名不符：%s", tool.Name())
	}
	if tool.ReadOnly() {
		t.Fatalf("写工具不能声明只读")
	}
	if tool.Risk() != RiskWrite {
		t.Fatalf("滚动重启是可逆写，风险级别应为 write，实际 %s", tool.Risk())
	}
	if tool.Risk() == RiskDestructive {
		t.Fatalf("重启可逆，不应被标成 destructive（过度分级会让审批失去区分度）")
	}
}

// 爆炸半径：Targets 的口径必须与只读证据的 ref 一致（审批看到的影响面 = 审计里的影响面）。
func TestK8sRolloutToolTargets(t *testing.T) {
	tool := &K8sRolloutTool{}
	got := tool.Targets(map[string]interface{}{"deployment": "web", "namespace": "prod"})
	if len(got) != 1 || got[0] != "deployment/prod/web" {
		t.Fatalf("目标口径不符：%v", got)
	}
	// 省略命名空间时用 default——与 k8s 的真实默认一致，不能留空。
	got = tool.Targets(map[string]interface{}{"deployment": "web"})
	if len(got) != 1 || got[0] != "deployment/default/web" {
		t.Fatalf("省略命名空间时应落到 default：%v", got)
	}
	// 缺目标时返回空：没有爆炸半径的写动作不该被"估成 0"。
	if got := tool.Targets(map[string]interface{}{}); len(got) != 0 {
		t.Fatalf("缺 deployment 时不应给出目标：%v", got)
	}
}

func TestK8sRolloutToolRequiresArgs(t *testing.T) {
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"缺 deployment", map[string]interface{}{"reason": "进程僵住了"}},
		{"缺 reason", map[string]interface{}{"deployment": "web"}},
		{"reason 只有空白", map[string]interface{}{"deployment": "web", "reason": "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeRolloutExecutor{}
			_, err := NewK8sRolloutTool(exec).Execute(context.Background(), tc.args)
			if err == nil || !errors.Is(err, ErrInvalidArgs) {
				t.Fatalf("参数不合法必须返回 ErrInvalidArgs，实际 %v", err)
			}
			// 关键：参数不合法时**不能**触达集群。
			if exec.calls != 0 {
				t.Fatalf("参数不合法不应触达集群，实际调用 %d 次", exec.calls)
			}
		})
	}
}

func TestK8sRolloutToolSuccess(t *testing.T) {
	exec := &fakeRolloutExecutor{res: Result{OK: true}}
	res, err := NewK8sRolloutTool(exec).Execute(context.Background(), map[string]interface{}{
		"deployment": "web", "namespace": "prod", "reason": "内存泄漏需要清缓存",
	})
	if err != nil {
		t.Fatalf("不应抛异常：%v", err)
	}
	if !res.OK {
		t.Fatalf("期望成功：%+v", res)
	}
	if exec.got.Namespace != "prod" || exec.got.Deployment != "web" || exec.got.Reason == "" {
		t.Fatalf("请求未完整透传：%+v", exec.got)
	}
	if !strings.Contains(res.Summary, "prod/web") {
		t.Fatalf("摘要应说明白重启了谁：%q", res.Summary)
	}
}

func TestK8sRolloutToolExecutorFailureIsObservation(t *testing.T) {
	exec := &fakeRolloutExecutor{err: errors.New("apiserver 不可达")}
	res, err := NewK8sRolloutTool(exec).Execute(context.Background(), map[string]interface{}{
		"deployment": "web", "reason": "清缓存",
	})
	if err != nil {
		t.Fatalf("执行方 error 不应变成调用方异常：%v", err)
	}
	if res.OK || !strings.Contains(res.Err, "apiserver") {
		t.Fatalf("失败原因必须回灌：%+v", res)
	}
}

func TestK8sRolloutToolNilExecutorDenied(t *testing.T) {
	_, err := NewK8sRolloutTool(nil).Execute(context.Background(), map[string]interface{}{
		"deployment": "web", "reason": "清缓存",
	})
	if err == nil || !errors.Is(err, ErrToolDenied) {
		t.Fatalf("未装配执行器必须返回 ErrToolDenied，实际 %v", err)
	}
}

// 注册条件：allow_write=false（默认）时写工具**根本不存在**。
func TestRegisterWriteToolsDefaultClosed(t *testing.T) {
	r, err := NewReadonlyRegistry(DefaultPolicy(), Sources{Logs: &fakeLogSource{}, K8s: &fakeK8sSource{}})
	if err != nil {
		t.Fatalf("注册只读工具失败: %v", err)
	}
	names, err := RegisterWriteTools(r, WriteSources{Retry: &stubRetryExecutor{}, Rollout: &fakeRolloutExecutor{}})
	if err != nil {
		t.Fatalf("RegisterWriteTools: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("默认策略下不应注册任何写工具，实际 %v", names)
	}
	for _, n := range r.Names() {
		if n == "k8s_rollout_restart" || n == "job_retry" {
			t.Fatalf("默认策略下写工具不该存在：%s", n)
		}
	}
}

// 打开写能力、但没接入集群时：job_retry 注册、k8s_rollout_restart 不注册。
func TestRegisterWriteToolsWithoutCluster(t *testing.T) {
	policy := DefaultPolicy()
	policy.AllowWrite = true
	r, err := NewReadonlyRegistry(policy, Sources{Logs: &fakeLogSource{}})
	if err != nil {
		t.Fatalf("注册只读工具失败: %v", err)
	}
	names, err := RegisterWriteTools(r, WriteSources{Retry: &stubRetryExecutor{}})
	if err != nil {
		t.Fatalf("RegisterWriteTools: %v", err)
	}
	if len(names) != 1 || names[0] != "job_retry" {
		t.Fatalf("只应注册 job_retry，实际 %v", names)
	}
}

// 打开写能力且接入了集群：两个写工具都在，且都声明为 write。
func TestRegisterWriteToolsWithCluster(t *testing.T) {
	policy := DefaultPolicy()
	policy.AllowWrite = true
	r, err := NewReadonlyRegistry(policy, Sources{Logs: &fakeLogSource{}})
	if err != nil {
		t.Fatalf("注册只读工具失败: %v", err)
	}
	names, err := RegisterWriteTools(r, WriteSources{Retry: &stubRetryExecutor{}, Rollout: &fakeRolloutExecutor{}})
	if err != nil {
		t.Fatalf("RegisterWriteTools: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("应注册两个写工具，实际 %v", names)
	}
	for _, n := range names {
		tool, ok := r.Get(n)
		if !ok {
			t.Fatalf("注册表里找不到 %s", n)
		}
		if tool.Risk() != RiskWrite || tool.ReadOnly() {
			t.Fatalf("%s 必须是 write 且非只读，实际 risk=%s readonly=%v", n, tool.Risk(), tool.ReadOnly())
		}
	}
}
