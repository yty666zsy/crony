package service

import (
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
)

// F1 回归：节点事件处理不得再内联 Sleep(5s)。
//
// 旧实现把 Sleep(5s)+分配 直接写在 setNodeList 里，于是
//   - admin 启动时 extractNodes 对每个已注册节点各等 5s（20 节点 ≈ 100s 起不来）；
//   - watcher 处理每个 PUT 事件各等 5s（节点列表/调度长时间滞后）。
//
// 现在要求：立刻登记节点，分配请求交给后台 worker。
func TestSetNodeListDoesNotBlock(t *testing.T) {
	w := &NodeWatcherService{
		nodeList: make(map[string]models.Node),
		assignCh: make(chan struct{}, 1),
	}
	val := `{"uuid":"n1","pid":"1","status":1}`

	start := time.Now()
	w.setNodeList("n1", val)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("setNodeList 阻塞了 %v：不得在事件路径里 Sleep", d)
	}
	if _, ok := w.nodeList["n1"]; !ok {
		t.Fatal("setNodeList 应立即把节点登记进 nodeList（调度器马上可见）")
	}
	if len(w.assignCh) != 1 {
		t.Fatalf("应投递 1 个分配请求，实际 %d", len(w.assignCh))
	}
}

// 多个节点事件应合并成 1 个待处理分配请求（容量 1 通道），避免重复分配风暴。
func TestSetNodeListCoalescesAssignRequests(t *testing.T) {
	w := &NodeWatcherService{
		nodeList: make(map[string]models.Node),
		assignCh: make(chan struct{}, 1),
	}
	for i := 0; i < 20; i++ {
		w.setNodeList("n"+string(rune('a'+i)), `{"uuid":"x","pid":"1","status":1}`)
	}
	if len(w.nodeList) != 20 {
		t.Fatalf("20 个事件应登记 20 个节点，实际 %d", len(w.nodeList))
	}
	if len(w.assignCh) != 1 {
		t.Fatalf("20 个节点事件应合并为 1 个分配请求，实际 %d", len(w.assignCh))
	}
}

// assignCh 未初始化（例如测试里用结构体字面量构造）时不得 panic 或阻塞。
func TestRequestAssignWithNilChannelIsSafe(t *testing.T) {
	w := &NodeWatcherService{nodeList: make(map[string]models.Node)}
	done := make(chan struct{})
	go func() { w.requestAssign(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("requestAssign 在 assignCh 为 nil 时阻塞了")
	}
}
