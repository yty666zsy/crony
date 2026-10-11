package handler_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/node/internal/handler"
)

// TestHTTPHandlerRunDoesNotPanicWithoutRuntime
//
// 原测试请求写死的 http://localhost:8089/ping，且会一路走到 JobProc.Start()，
// 在那里的 config.GetConfigModels().System.JobProcTtl 上空指针 panic。
//
// HTTPHandler.Run 的第一步就是写 etcd 执行记录，所以没有加载配置、没有 etcd
// 的环境下（即单元测试环境）正确行为是"返回错误"，而不是崩溃。
// 这里用本地 httptest 服务替代外部依赖，校验的就是这一点。
func TestHTTPHandlerRunDoesNotPanicWithoutRuntime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ping"))
	}))
	defer server.Close()

	var httpHandler handler.HTTPHandler
	job := &handler.Job{
		Job: &models.Job{
			Name:       "get",
			Command:    server.URL,
			HttpMethod: models.HTTPMethodGet,
			Timeout:    3000,
		},
	}
	if _, err := httpHandler.Run(job); err == nil {
		t.Fatal("Run() 在未加载配置/未连接 etcd 的环境下必须返回错误")
	}
}

// TestJobProcStartRequiresConfig 固定 JobProc.Start 的错误语义：
// 配置缺失时返回 ErrConfigNotLoaded（旧实现是空指针 panic）。
func TestJobProcStartRequiresConfig(t *testing.T) {
	proc := &handler.JobProc{
		JobProc: &models.JobProc{
			ID:       1,
			JobID:    1,
			NodeUUID: "test-node",
		},
	}
	err := proc.Start()
	if !errors.Is(err, handler.ErrConfigNotLoaded) {
		t.Fatalf("Start() error = %v, want ErrConfigNotLoaded", err)
	}
	// Start 失败后必须恢复为未运行状态，否则后续 Stop() 会永久阻塞。
	proc.Stop()
}
