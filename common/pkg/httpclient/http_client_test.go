package httpclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// http-client

// TestGetUsesLocalServer 原测试请求写死的 http://localhost:8089/ping，
// 依赖外部服务的运行状态，在任何干净环境里都会失败。
// 这里改为自建 httptest 服务，测试变得自洽（hermetic）。
func TestGetUsesLocalServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"test"}`))
	}))
	defer server.Close()

	result, err := Get(server.URL, 300)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !strings.Contains(result, "test") {
		t.Fatalf("Get() = %q, want body containing \"test\"", result)
	}
}

func TestGetRejectsNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if _, err := Get(server.URL, 300); err == nil {
		t.Fatal("Get() must return an error when the status code is not 200")
	}
}

func TestPostJsonUsesLocalServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-type"); got != "application/json" {
			t.Errorf("content-type = %q, want application/json", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	result, err := PostJson(server.URL, `{"name":"test"}`, 300)
	if err != nil {
		t.Fatalf("PostJson() error = %v", err)
	}
	if result != "ok" {
		t.Fatalf("PostJson() = %q, want \"ok\"", result)
	}
}
