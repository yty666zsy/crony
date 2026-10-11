//go:build acceptance

// 验收底座：HTTP 客户端、SSE 读取、只读 SQL、三态报告。
//
// 设计口径（很重要）：
//   - **PASS/FAIL/SKIP 三态**。SKIP 表示"前置条件不存在，本次没有观测到任何东西"，
//     它不是通过，也不允许被算成通过；报告里必须写明跳过原因。
//   - 断言只针对"被测系统对外承诺的东西"（HTTP 状态、信封 code、字段存在性、
//     预算快照语义、审计行事实），不针对实现细节，避免验收变成"第二份单元测试"。
package acceptance

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ---------- 环境 ----------

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return def
	}
	return n
}

// ---------- 信封与客户端 ----------

// envelope 是 crony 全站统一响应：HTTP 恒 200 + {code,data,msg}。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// dataMap 把 data 解成 map：黑盒断言不依赖后端结构体定义。
func (e *envelope) dataMap() map[string]any {
	m := map[string]any{}
	if len(e.Data) > 0 {
		_ = json.Unmarshal(e.Data, &m)
	}
	return m
}

type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base string) *client {
	return &client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 180 * time.Second},
	}
}

func (c *client) do(method, path string, body any, auth bool) (int, *envelope, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = b
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	env := &envelope{}
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, env)
	}
	return res.StatusCode, env, nil
}

// postOK 发一次已鉴权 POST，并要求 HTTP 200 且 code=200，返回 data。
func (c *client) postOK(path string, body any) (map[string]any, error) {
	status, env, err := c.do(http.MethodPost, path, body, true)
	if err != nil {
		return nil, fmt.Errorf("POST %s 网络错误: %w", path, err)
	}
	if status != http.StatusOK || env.Code != 200 {
		return nil, fmt.Errorf("POST %s 期望 HTTP 200/code 200，实际 HTTP %d/code %d msg=%q",
			path, status, env.Code, truncate(env.Msg, 200))
	}
	return env.dataMap(), nil
}

// login 用真实口令换 token；口令永远不进报告（失败信息里也不回显）。
func (c *client) login(user, pass string) error {
	status, env, err := c.do(http.MethodPost, "/login", map[string]any{"username": user, "password": pass}, false)
	if err != nil {
		return err
	}
	if status != http.StatusOK || env.Code != 200 {
		return fmt.Errorf("登录被拒：HTTP %d/code %d msg=%q", status, env.Code, truncate(env.Msg, 120))
	}
	token := asStr(env.dataMap()["token"])
	if token == "" {
		return fmt.Errorf("登录返回 code=200 但 data.token 为空")
	}
	c.token = token
	return nil
}

// sseResult 记录一个时间窗内收到的事件（超出窗口即主动断开，不算错误）。
type sseResult struct {
	Events []string
	Data   map[string][]string
}

func (r *sseResult) has(name string) bool {
	for _, e := range r.Events {
		if e == name {
			return true
		}
	}
	return false
}

// sse 在 window 内读事件流；到点主动断开。连接错误不返回 error——
// "不存在的 run_id 只发心跳"这类用例本身就以"窗口内没有终态事件"为判据。
func (c *client) sse(path string, window time.Duration) (*sseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// 与控制台 stream.js 一致：显式声明要 SSE，避免被中间层当成普通请求处理。
	req.Header.Set("Accept", "text/event-stream")
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("建立 SSE 连接失败: %w", err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return nil, fmt.Errorf("Content-Type 应为 text/event-stream，实际 %q（多半被普通 JSON 错误信封取代）", ct)
	}
	out := &sseResult{Data: map[string][]string{}}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	cur := ""
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			cur = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if cur == "" {
				cur = "message"
			}
			out.Data[cur] = append(out.Data[cur], d)
		case line == "":
			if cur != "" {
				out.Events = append(out.Events, cur)
				cur = ""
			}
		}
	}
	return out, nil
}

// ---------- JSON 取值小工具 ----------

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func asStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asNum(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------- 只读 SQL ----------

// openDB 打开验收用只读连接；DSN 未配置时返回 nil（相关用例会 SKIP 并写明原因）。
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CRONY_TEST_MYSQL_DSN"))
	if dsn == "" {
		return nil
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开验收库失败: %v", err)
	}
	db.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("验收库连不上（DSN=%s）: %v", redactDSN(dsn), err)
	}
	return db
}

// redactDSN 抹掉口令后再进报告。
func redactDSN(dsn string) string {
	at := strings.Index(dsn, "@")
	colon := strings.Index(dsn, ":")
	if at > 0 && colon > 0 && colon < at {
		return dsn[:colon+1] + "***" + dsn[at:]
	}
	return dsn
}

// ---------- 三态报告 ----------

const (
	statusPass = "PASS"
	statusFail = "FAIL"
	statusSkip = "SKIP"
)

type result struct {
	ID     string
	Name   string
	Status string
	Detail string
}

type report struct {
	mu     sync.Mutex
	items  []result
	start  time.Time
	facts  []string
	envDSN string
}

func newReport() *report {
	return &report{start: time.Now()}
}

func (r *report) fact(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.facts = append(r.facts, fmt.Sprintf(format, args...))
}

func (r *report) add(id, name, status, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, result{ID: id, Name: name, Status: status, Detail: detail})
}

func (r *report) counts() (pass, fail, skip int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, it := range r.items {
		switch it.Status {
		case statusPass:
			pass++
		case statusFail:
			fail++
		case statusSkip:
			skip++
		}
	}
	return
}

// markdown 生成可归档的验收报告（真实文件，不是内存里的字符串）。
func (r *report) markdown() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	pass, fail, skip := 0, 0, 0
	for _, it := range r.items {
		switch it.Status {
		case statusPass:
			pass++
		case statusFail:
			fail++
		case statusSkip:
			skip++
		}
	}
	var b strings.Builder
	b.WriteString("# crony AI-Agent 改造 · 案例验收结果\n\n")
	fmt.Fprintf(&b, "- 运行时间：%s（用时 %.1fs）\n", r.start.Format("2006-01-02 15:04:05"), time.Since(r.start).Seconds())
	for _, f := range r.facts {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	fmt.Fprintf(&b, "- 结论：**通过 %d / 失败 %d / 跳过 %d**\n\n", pass, fail, skip)
	b.WriteString("> 口径：`SKIP` 表示前置条件不存在、本次**没有观测到任何东西**，\n")
	b.WriteString("> 既不算通过也不算失败；报告逐条写明跳过原因。\n\n")
	b.WriteString("| 用例 | 名称 | 状态 | 证据 / 说明 |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	icon := map[string]string{statusPass: "✅ 通过", statusFail: "❌ 失败", statusSkip: "⏭️ 跳过"}
	for _, it := range r.items {
		detail := strings.ReplaceAll(it.Detail, "|", "\\|")
		detail = strings.ReplaceAll(detail, "\n", " ")
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", it.ID, it.Name, icon[it.Status], detail)
	}
	return b.String()
}

func (r *report) write(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if dir := path[:strings.LastIndexAny(path, `\/`)+1]; dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(r.markdown()), 0o644)
}
