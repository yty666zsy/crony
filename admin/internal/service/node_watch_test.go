package service

import (
	"strings"
	"testing"
)

// TestNodeWatcherWatchWithoutEtcdReturnsError 锁定"etcd 不可用不能让 admin 崩溃"。
//
// 真实启动测试里发现：etcd 未初始化时 NodeWatcherService.client 为 nil，
// Watch() 直接解引用导致 panic，整个控制面（连 /login）都起不来——
// 而控制面 HTTP API 本身并不依赖 etcd。现在要求返回可读错误，
// 由调用方决定降级继续服务还是 fail fast。
func TestNodeWatcherWatchWithoutEtcdReturnsError(t *testing.T) {
	w := &NodeWatcherService{}
	err := w.Watch()
	if err == nil {
		t.Fatal("etcd 未初始化时 Watch() 必须返回错误，而不是 panic 或静默成功")
	}
	if !strings.Contains(err.Error(), "etcd") {
		t.Fatalf("错误信息应点明 etcd 未初始化，实际：%v", err)
	}
}

// TestNodeWatcherWatcherNoEtcdReturnsImmediately 确认后台 watch 函数在无客户端时
// 立即返回：同步调用它即可证明——若它 panic 或阻塞在 watch 上，本用例会失败/超时。
// （panic 若发生在 goroutine 内会直接终止进程，所以这里刻意不起 goroutine。）
func TestNodeWatcherWatcherNoEtcdReturnsImmediately(t *testing.T) {
	w := &NodeWatcherService{}
	w.watcher()
}
