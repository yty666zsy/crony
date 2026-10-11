package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeNodeStatusSource 固定返回一份节点快照（避免依赖真实数据库）。
type fakeNodeStatusSource struct{ st NodeStatus }

func (f *fakeNodeStatusSource) NodeStatus(ctx context.Context, nodeUUID string) (NodeStatus, error) {
	return f.st, nil
}

// 阶段 3 收尾：容量信息必须真的传到模型眼前。
//
// 背景：`node_metric` 一开始就存了 cpu_cores / mem_total_mb / disk_free_mb，
// 但工具结构体里没有对应字段，于是模型只知道"内存 92%"却不知道
// "92% 是 512GB 里的 92%"——"要不要扩容"这类判断因此无从下手。
func TestNodeStatusToolRendersCapacity(t *testing.T) {
	src := &fakeNodeStatusSource{st: NodeStatus{
		NodeUUID: "n1", Hostname: "host-1", Online: true,
		LastHeartbeat: time.Now().Add(-5 * time.Second), RunningJobs: 3,
		CPUPercent: 12.5, MemPercent: 87.5, DiskPercent: 91.0, LoadAvg: "0.80 0.60 0.40",
		CPUCores: 8, MemTotalMB: 16384, MemUsedMB: 14336,
		DiskTotalMB: 51200, DiskFreeMB: 4608, DiskPath: "/",
		CollectedAt: time.Now().Add(-10 * time.Second), Version: "v1.1.0",
	}}
	res, err := NewNodeStatusTool(src).Execute(context.Background(), map[string]interface{}{"node": "n1"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !res.OK {
		t.Fatalf("应成功: %+v", res)
	}
	for _, want := range []string{"容量：", "8 核", "内存 16.0GB", "磁盘 / 合计 50.0GB", "可用 4.5GB"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("工具输出缺少 %q:\n%s", want, res.Output)
		}
	}
	if cores, ok := res.Meta["cpu_cores"].(int); !ok || cores != 8 {
		t.Fatalf("meta.cpu_cores 不符: %+v", res.Meta)
	}
	if free, ok := res.Meta["disk_free_mb"].(int64); !ok || free != 4608 {
		t.Fatalf("meta.disk_free_mb 不符: %+v", res.Meta)
	}
}

// 旧版节点只上报百分比（没有容量字段）时，**不许**渲染成"0 核 0MB"——
// 那会让模型以为节点是台空机器。没有数据就不写。
func TestNodeStatusToolOmitsCapacityWhenAbsent(t *testing.T) {
	src := &fakeNodeStatusSource{st: NodeStatus{
		NodeUUID: "n2", Hostname: "host-2", Online: true,
		CPUPercent: 5, MemPercent: 50, DiskPercent: 10,
		CollectedAt: time.Now().Add(-time.Second),
	}}
	res, err := NewNodeStatusTool(src).Execute(context.Background(), map[string]interface{}{"node": "n2"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if strings.Contains(res.Output, "容量：") {
		t.Fatalf("没有容量数据时不应渲染容量段:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "0 核") || strings.Contains(res.Output, "0.0GB") {
		t.Fatalf("不得用 0 冒充缺失的容量:\n%s", res.Output)
	}
	if _, ok := res.Meta["cpu_cores"]; ok {
		t.Fatalf("未上报时不应写 meta.cpu_cores: %+v", res.Meta)
	}
}

// 未上报水位（CollectedAt 零值）时，必须显式说"未上报"，
// 且**不能**因为容量字段恰好有值就把它当成"已上报"。
func TestNodeStatusToolUnreportedStaysUnreported(t *testing.T) {
	src := &fakeNodeStatusSource{st: NodeStatus{
		NodeUUID: "n3", Hostname: "host-3", Online: false, CPUCores: 4,
	}}
	res, _ := NewNodeStatusTool(src).Execute(context.Background(), map[string]interface{}{"node": "n3"})
	if !strings.Contains(res.Output, "未上报") {
		t.Fatalf("未上报时必须明说:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "容量：") {
		t.Fatalf("未上报时不应渲染容量:\n%s", res.Output)
	}
	if reported, _ := res.Meta["metrics_reported"].(bool); reported {
		t.Fatalf("metrics_reported 应为 false: %+v", res.Meta)
	}
}
