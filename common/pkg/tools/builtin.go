package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
)

// 本文件实现阶段 2 的**只读工具集**（方案 §6 阶段 2 第 2 条）：
//
//	shell_readonly / http_get / node_status / log_search / mysql_query_readonly
//
// 三个"需要数据源"的工具（node_status、log_search、mysql_query_readonly）
// 通过接口注入实现，好处有两个：
//  1. common 包不依赖 admin/node 的数据层，测试不需要起数据库；
//  2. **没接数据源就不注册该工具**——模型看不到用不了的工具，
//     就不会把步数浪费在"调用一个必然失败的工具"上。

// ---------------------------------------------------------------------------
// shell_readonly
// ---------------------------------------------------------------------------

// ShellRunner 命令执行器（可注入，便于跨平台测试与单元测试）。
type ShellRunner func(ctx context.Context, name string, args []string) (stdout, stderr string, exitCode int, err error)

// DefaultShellRunner 直接执行可执行文件，**不经过 shell**。
//
// 这是"只读 shell"能成立的关键：没有 shell，就不存在命令注入、
// 不存在变量展开、不存在管道与重定向。
func DefaultShellRunner(ctx context.Context, name string, args []string) (string, string, int, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", "", -1, fmt.Errorf("命令 %s 不存在: %w", name, err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	stdoutText := stdout.String()
	stderrText := stderr.String()
	if err == nil {
		return stdoutText, stderrText, 0, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return stdoutText, stderrText, exitErr.ExitCode(), nil
	}
	return stdoutText, stderrText, -1, err
}

// ShellTool 只读 shell 工具。
type ShellTool struct {
	runner  ShellRunner
	timeout time.Duration
	policy  Policy
}

// NewShellTool 使用真实执行器与默认策略。
func NewShellTool() *ShellTool {
	return NewShellToolWithPolicy(DefaultPolicy())
}

// NewShellToolWithPolicy 使用真实执行器 + 指定策略（目录白名单来自这里）。
func NewShellToolWithPolicy(policy Policy) *ShellTool {
	return &ShellTool{runner: DefaultShellRunner, timeout: 15 * time.Second, policy: policy.Normalize()}
}

// NewShellToolWithRunner 注入执行器（测试用）。
func NewShellToolWithRunner(r ShellRunner, timeout time.Duration) *ShellTool {
	if r == nil {
		r = DefaultShellRunner
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &ShellTool{runner: r, timeout: timeout, policy: DefaultPolicy()}
}

// Name 实现 Tool。
func (t *ShellTool) Name() string { return "shell_readonly" }

// Description 实现 Tool。
func (t *ShellTool) Description() string {
	return "在 crony 节点上执行**只读**诊断命令并返回输出。允许的命令：" +
		strings.Join(AllowedShellCommands(), "、") +
		"（df=磁盘使用率，ps=进程列表，tail=看日志尾部，journalctl=systemd 日志）。" +
		"不接受管道、重定向或其他命令；tail 只能读取 /var/log 下的文件。"
}

// Schema 实现 Tool。
func (t *ShellTool) Schema() *jsonschema.Schema {
	commands := AllowedShellCommands()
	enum := make([]interface{}, 0, len(commands))
	for _, c := range commands {
		enum = append(enum, c)
	}
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"command": jsonschema.String("要执行的命令名").WithEnum(enum...),
		"args":    jsonschema.Array(jsonschema.String("单个参数"), "参数列表，例如 [\"-h\"] 或 [\"-n\",\"50\",\"/var/log/crony/node.log\"]").WithItems(0, 8),
	}, "command")
}

// ReadOnly 实现 Tool。
func (t *ShellTool) ReadOnly() bool { return true }

// Timeout 实现 Tool。
func (t *ShellTool) Timeout() time.Duration { return t.timeout }

// Execute 实现 Tool。
func (t *ShellTool) Execute(ctx context.Context, args map[string]interface{}) (Result, error) {
	command := llm.StringOr(args, "command", "")
	rawArgs := stringList(args["args"])
	// 用工具自己的策略复核（目录白名单必须与注册表策略一致，否则等于留了后门）
	if err := GuardShellCommand(command, rawArgs, t.policy); err != nil {
		return Result{}, err
	}
	stdout, stderr, code, err := t.runner(ctx, command, rawArgs)
	if err != nil {
		return Result{OK: false, Summary: "命令未能执行", Err: llm.Redact(err.Error())}, nil
	}
	output := stdout
	if strings.TrimSpace(stderr) != "" {
		if output != "" {
			output += "\n"
		}
		output += "[stderr]\n" + stderr
	}
	res := Result{OK: code == 0, Output: output}
	if code != 0 {
		res.Summary = fmt.Sprintf("%s %s 退出码 %d", command, strings.Join(rawArgs, " "), code)
		res.Err = fmt.Sprintf("退出码 %d", code)
	} else {
		res.Summary = summarizeShell(command, output, rawArgs)
	}
	res.Meta = map[string]interface{}{"command": command, "args": rawArgs, "exit_code": code}
	res.Evidence = []Evidence{{
		Source: "shell",
		Ref:    command + " " + strings.Join(rawArgs, " "),
		Quote:  firstLines(output, 12),
	}}
	return res, nil
}

// summarizeShell 把输出压成"一句话结论"。
//
// 为什么要做：模型面对 200 行 `df` 输出很难自己抓重点，
// 而在工具层做这个摘要既便宜又确定（不需要再花一次模型调用）。
func summarizeShell(command, output string, args []string) string {
	switch command {
	case "df":
		// 找出使用率最高的挂载点
		best, bestMount := -1, ""
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if strings.HasSuffix(f, "%") {
					var pct int
					if _, err := fmt.Sscanf(strings.TrimSuffix(f, "%"), "%d", &pct); err == nil {
						if pct > best {
							best = pct
							if len(fields) > i+1 {
								bestMount = fields[len(fields)-1]
							}
						}
					}
					break
				}
			}
		}
		if best >= 0 {
			return fmt.Sprintf("最高磁盘使用率 %d%%（挂载点 %s）", best, bestMount)
		}
		return "df 执行成功"
	case "ps":
		return fmt.Sprintf("进程列表共 %d 行", maxInt(countLines(output)-1, 0))
	case "tail", "journalctl":
		errCount := countKeywords(output, "error", "failed", "panic", "fatal", "timeout")
		return fmt.Sprintf("日志 %d 行，命中错误关键字 %d 处（参数: %s）", countLines(output), errCount, strings.Join(args, " "))
	default:
		return fmt.Sprintf("%s 执行成功（输出 %d 行）", command, countLines(output))
	}
}

// ---------------------------------------------------------------------------
// http_get
// ---------------------------------------------------------------------------

// HTTPGetTool 只读 HTTP 探测工具。
//
// 定位：**健康检查与依赖探活**，不是通用爬虫。因此：
//   - 只允许 GET（写方法一律不在工具集里）；
//   - 目标必须在 AllowHosts 内；默认 AllowHosts 为空 = 完全禁网；
//   - 每一跳重定向都要重新过白名单（否则一个 302 就能把请求带到内网）；
//   - 只回填文本类响应体（二进制/图片对诊断无意义，只会撑爆上下文）。
type HTTPGetTool struct {
	client     *http.Client
	allowHosts []string
	maxBytes   int
	timeout    time.Duration
}

// NewHTTPGetTool 创建工具。allowHosts 为空时该工具仍会注册，但每次调用都会被拒绝
// （显式失败比"悄悄不存在"更容易排查配置问题）。
func NewHTTPGetTool(allowHosts []string, timeout time.Duration, maxBytes int) *HTTPGetTool {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	t := &HTTPGetTool{allowHosts: allowHosts, maxBytes: maxBytes, timeout: timeout}
	t.client = &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("重定向次数超过 3 次")
			}
			if _, err := GuardURL(req.URL.String(), allowHosts); err != nil {
				return fmt.Errorf("重定向目标被拒绝: %w", err)
			}
			return nil
		},
	}
	return t
}

// Name 实现 Tool。
func (t *HTTPGetTool) Name() string { return "http_get" }

// Description 实现 Tool。
func (t *HTTPGetTool) Description() string {
	if len(t.allowHosts) == 0 {
		return "发起只读 HTTP GET 请求做健康检查（当前未配置 AllowHosts，**所有请求都会被拒绝**）"
	}
	return "发起只读 HTTP GET 请求做健康检查，只允许访问: " + strings.Join(t.allowHosts, "、")
}

// Schema 实现 Tool。
func (t *HTTPGetTool) Schema() *jsonschema.Schema {
	maxBytes := t.maxBytes
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"url":       jsonschema.String("完整的 http/https URL"),
		"max_bytes": jsonschema.Integer("最多返回多少字节正文（默认 8192）").WithRange(256, float64(maxBytes)),
	}, "url")
}

// ReadOnly 实现 Tool。
func (t *HTTPGetTool) ReadOnly() bool { return true }

// Timeout 实现 Tool。
func (t *HTTPGetTool) Timeout() time.Duration { return t.timeout }

// Execute 实现 Tool。
func (t *HTTPGetTool) Execute(ctx context.Context, args map[string]interface{}) (Result, error) {
	raw := llm.StringOr(args, "url", "")
	target, err := GuardURL(raw, t.allowHosts)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidArgs, err)
	}
	req.Header.Set("User-Agent", "crony-agent/1.0 (read-only health check)")
	resp, err := t.client.Do(req)
	if err != nil {
		// 策略拒绝（例如重定向跳向元数据端点）必须是**硬错误**：
		// 让模型分清"这条路径不被允许"与"对端服务有问题"——两者的后续动作完全不同。
		if errors.Is(err, ErrToolDenied) || errors.Is(err, ErrReadonlyViolation) || errors.Is(err, ErrInvalidArgs) {
			return Result{}, err
		}
		return Result{OK: false, Summary: "请求失败", Err: llm.Redact(err.Error())}, nil
	}
	defer resp.Body.Close()

	limit := t.maxBytes
	if v := int(llm.FloatOr(args, "max_bytes", float64(limit))); v > 0 && v < limit {
		limit = v
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	truncated := len(body) > limit
	if truncated {
		body = body[:limit]
	}
	contentType := resp.Header.Get("Content-Type")
	res := Result{
		OK:        resp.StatusCode < 400,
		Truncated: truncated,
		Meta: map[string]interface{}{
			"status_code":  resp.StatusCode,
			"content_type": contentType,
			"bytes":        len(body),
			"host":         target.Hostname(),
		},
	}
	if readErr != nil {
		res.OK = false
		res.Err = fmt.Sprintf("读取响应失败: %v", readErr)
	}
	res.Summary = fmt.Sprintf("GET %s → HTTP %d（%d 字节正文）", target.String(), resp.StatusCode, len(body))
	res.Evidence = []Evidence{{Source: "http", Ref: target.String(), Quote: fmt.Sprintf("HTTP %d %s", resp.StatusCode, firstLines(string(body), 3))}}
	if isTextual(contentType) {
		res.Output = string(body)
	} else {
		res.Output = fmt.Sprintf("（非文本响应，%s，%d 字节，已省略正文）", contentType, len(body))
	}
	return res, nil
}

// isTextual 判断响应类型是否值得回填给模型。
func isTextual(contentType string) bool {
	ct := strings.ToLower(contentType)
	if ct == "" {
		return true
	}
	for _, prefix := range []string{"text/", "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/javascript"} {
		if strings.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// node_status
// ---------------------------------------------------------------------------

// NodeStatus 节点状态快照（由 node 侧实现填充）。
type NodeStatus struct {
	NodeUUID      string    `json:"node_uuid"`
	Hostname      string    `json:"hostname"`
	Online        bool      `json:"online"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	RunningJobs   int       `json:"running_jobs"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemPercent    float64   `json:"mem_percent"`
	DiskPercent   float64   `json:"disk_percent"`
	LoadAvg       string    `json:"load_avg"`
	Version       string    `json:"version"`
}

// NodeStatusSource 节点状态数据源。
type NodeStatusSource interface {
	NodeStatus(ctx context.Context, nodeUUID string) (NodeStatus, error)
}

// NodeStatusTool 查询节点状态。
type NodeStatusTool struct{ src NodeStatusSource }

// NewNodeStatusTool 创建工具。
func NewNodeStatusTool(src NodeStatusSource) *NodeStatusTool { return &NodeStatusTool{src: src} }

// Name 实现 Tool。
func (t *NodeStatusTool) Name() string { return "node_status" }

// Description 实现 Tool。
func (t *NodeStatusTool) Description() string {
	return "查询 crony 节点的在线状态、心跳时间、正在运行的任务数与资源水位（CPU/内存/磁盘）。"
}

// Schema 实现 Tool。
func (t *NodeStatusTool) Schema() *jsonschema.Schema {
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"node": jsonschema.String("节点 UUID；留空表示查询当前节点"),
	})
}

// ReadOnly 实现 Tool。
func (t *NodeStatusTool) ReadOnly() bool { return true }

// Timeout 实现 Tool。
func (t *NodeStatusTool) Timeout() time.Duration { return 5 * time.Second }

// Execute 实现 Tool。
func (t *NodeStatusTool) Execute(ctx context.Context, args map[string]interface{}) (Result, error) {
	if t.src == nil {
		return Result{}, fmt.Errorf("%w: node_status 数据源未接入", ErrToolDenied)
	}
	node := llm.StringOr(args, "node", "")
	st, err := t.src.NodeStatus(ctx, node)
	if err != nil {
		return Result{OK: false, Summary: "查询节点状态失败", Err: llm.Redact(err.Error())}, nil
	}
	online := "离线"
	if st.Online {
		online = "在线"
	}
	output := strings.Join([]string{
		fmt.Sprintf("node_uuid=%s", st.NodeUUID),
		fmt.Sprintf("hostname=%s", st.Hostname),
		fmt.Sprintf("online=%s", online),
		fmt.Sprintf("last_heartbeat=%s", formatTime(st.LastHeartbeat)),
		fmt.Sprintf("running_jobs=%d", st.RunningJobs),
		fmt.Sprintf("cpu_percent=%.1f", st.CPUPercent),
		fmt.Sprintf("mem_percent=%.1f", st.MemPercent),
		fmt.Sprintf("disk_percent=%.1f", st.DiskPercent),
		fmt.Sprintf("load_avg=%s", st.LoadAvg),
		fmt.Sprintf("version=%s", st.Version),
	}, "\n")
	res := Result{
		OK:      true,
		Output:  output,
		Summary: fmt.Sprintf("节点 %s %s，运行中任务 %d，磁盘 %.1f%%", firstNonEmpty(st.Hostname, st.NodeUUID), online, st.RunningJobs, st.DiskPercent),
		Meta:    map[string]interface{}{"online": st.Online, "running_jobs": st.RunningJobs},
		Evidence: []Evidence{{
			Source: "node",
			Ref:    firstNonEmpty(st.NodeUUID, node, "local"),
			Quote:  fmt.Sprintf("online=%s last_heartbeat=%s running_jobs=%d disk=%.1f%%", online, formatTime(st.LastHeartbeat), st.RunningJobs, st.DiskPercent),
		}},
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// log_search
// ---------------------------------------------------------------------------

// LogQuery 日志检索条件。
type LogQuery struct {
	Keyword      string `json:"keyword"`
	JobID        int    `json:"job_id"`
	RunID        string `json:"run_id"`
	Level        string `json:"level"`
	SinceMinutes int    `json:"since_minutes"`
	Limit        int    `json:"limit"`
}

// LogHit 一条命中的日志。
type LogHit struct {
	Source    string    `json:"source"`
	Ref       string    `json:"ref"`
	Line      string    `json:"line"`
	Timestamp time.Time `json:"timestamp"`
}

// LogSource 日志数据源。
type LogSource interface {
	SearchLogs(ctx context.Context, q LogQuery) ([]LogHit, error)
}

// LogSearchTool 日志检索工具。
type LogSearchTool struct{ src LogSource }

// NewLogSearchTool 创建工具。
func NewLogSearchTool(src LogSource) *LogSearchTool { return &LogSearchTool{src: src} }

// Name 实现 Tool。
func (t *LogSearchTool) Name() string { return "log_search" }

// Description 实现 Tool。
func (t *LogSearchTool) Description() string {
	return "按关键字/任务/运行 ID 检索任务日志。至少给出 keyword、job_id、run_id 之一，避免全量拉取。"
}

// Schema 实现 Tool。
func (t *LogSearchTool) Schema() *jsonschema.Schema {
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"keyword":       jsonschema.String("关键字（子串匹配，例如 timeout / connection refused）"),
		"job_id":        jsonschema.Integer("任务 ID").WithRange(0, 1e12),
		"run_id":        jsonschema.String("运行 ID（形如 j12-t1791708060）"),
		"level":         jsonschema.String("日志级别").WithEnum("debug", "info", "warn", "error"),
		"since_minutes": jsonschema.Integer("回溯多少分钟（默认 60）").WithRange(1, 10080),
		"limit":         jsonschema.Integer("最多返回多少条（默认 50）").WithRange(1, 200),
	})
}

// ReadOnly 实现 Tool。
func (t *LogSearchTool) ReadOnly() bool { return true }

// Timeout 实现 Tool。
func (t *LogSearchTool) Timeout() time.Duration { return 10 * time.Second }

// Execute 实现 Tool。
func (t *LogSearchTool) Execute(ctx context.Context, args map[string]interface{}) (Result, error) {
	if t.src == nil {
		return Result{}, fmt.Errorf("%w: log_search 数据源未接入", ErrToolDenied)
	}
	q := LogQuery{
		Keyword:      llm.StringOr(args, "keyword", ""),
		JobID:        int(llm.FloatOr(args, "job_id", 0)),
		RunID:        llm.StringOr(args, "run_id", ""),
		Level:        llm.StringOr(args, "level", ""),
		SinceMinutes: int(llm.FloatOr(args, "since_minutes", 60)),
		Limit:        int(llm.FloatOr(args, "limit", 50)),
	}
	if q.Keyword == "" && q.JobID == 0 && q.RunID == "" {
		return Result{}, fmt.Errorf("%w: 至少给出 keyword、job_id、run_id 之一（禁止全量拉日志）", ErrInvalidArgs)
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.SinceMinutes <= 0 {
		q.SinceMinutes = 60
	}
	hits, err := t.src.SearchLogs(ctx, q)
	if err != nil {
		return Result{OK: false, Summary: "日志检索失败", Err: llm.Redact(err.Error())}, nil
	}
	var b strings.Builder
	evidence := make([]Evidence, 0, 5)
	for i, hit := range hits {
		b.WriteString(fmt.Sprintf("[%s] %s %s\n", formatTime(hit.Timestamp), hit.Ref, hit.Line))
		if i < 5 {
			evidence = append(evidence, Evidence{Source: "log", Ref: firstNonEmpty(hit.Ref, hit.Source), Quote: hit.Line})
		}
	}
	res := Result{
		OK:       true,
		Output:   b.String(),
		Summary:  fmt.Sprintf("命中 %d 条日志（关键字=%q job_id=%d run_id=%q，回溯 %d 分钟）", len(hits), q.Keyword, q.JobID, q.RunID, q.SinceMinutes),
		Evidence: evidence,
		Meta:     map[string]interface{}{"hits": len(hits)},
	}
	if len(hits) == 0 {
		res.Summary = fmt.Sprintf("没有命中日志（关键字=%q job_id=%d run_id=%q）——这本身是重要证据：说明失败可能未写入日志", q.Keyword, q.JobID, q.RunID)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// mysql_query_readonly
// ---------------------------------------------------------------------------

// SQLSource 只读 SQL 数据源。
//
// 实现方（node/admin 侧）**必须**使用只读账号：工具层的语句守卫是防"模型乱来"，
// 数据库账号权限才是最后一道防线（两层都要有，缺一不可）。
type SQLSource interface {
	QueryReadonly(ctx context.Context, query string) ([]map[string]interface{}, error)
}

// SQLTool 只读 SQL 工具。
type SQLTool struct {
	src     SQLSource
	maxRows int
}

// NewSQLTool 创建工具。
func NewSQLTool(src SQLSource, maxRows int) *SQLTool {
	if maxRows <= 0 {
		maxRows = 100
	}
	return &SQLTool{src: src, maxRows: maxRows}
}

// Name 实现 Tool。
func (t *SQLTool) Name() string { return "mysql_query_readonly" }

// Description 实现 Tool。
func (t *SQLTool) Description() string {
	return fmt.Sprintf("对 crony 元数据库执行**只读 SELECT** 查询（必须带 LIMIT，且不超过 %d 行）。禁止任何写操作、DDL、注释与多语句。", t.maxRows)
}

// Schema 实现 Tool。
func (t *SQLTool) Schema() *jsonschema.Schema {
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"query": jsonschema.String("单条 SELECT 语句，必须以 LIMIT n 结尾"),
	}, "query")
}

// ReadOnly 实现 Tool。
func (t *SQLTool) ReadOnly() bool { return true }

// Timeout 实现 Tool。
func (t *SQLTool) Timeout() time.Duration { return 10 * time.Second }

// Execute 实现 Tool。
func (t *SQLTool) Execute(ctx context.Context, args map[string]interface{}) (Result, error) {
	if t.src == nil {
		return Result{}, fmt.Errorf("%w: mysql_query_readonly 数据源未接入", ErrToolDenied)
	}
	query := llm.StringOr(args, "query", "")
	guarded, err := GuardSQL(query, t.maxRows)
	if err != nil {
		return Result{}, err
	}
	rows, err := t.src.QueryReadonly(ctx, guarded)
	if err != nil {
		return Result{OK: false, Summary: "查询失败", Err: llm.Redact(err.Error()), Meta: map[string]interface{}{"query": guarded}}, nil
	}
	out, err := json.Marshal(rows)
	if err != nil {
		return Result{OK: false, Summary: "结果序列化失败", Err: err.Error()}, nil
	}
	res := Result{
		OK:      true,
		Output:  string(out),
		Summary: fmt.Sprintf("查询返回 %d 行", len(rows)),
		Meta:    map[string]interface{}{"rows": len(rows), "query": guarded},
		Evidence: []Evidence{{
			Source: "sql",
			Ref:    truncateRef(guarded, 120),
			Quote:  firstLines(string(out), 3),
		}},
	}
	if len(rows) == 0 {
		res.Summary = "查询返回 0 行——这本身是证据：说明该条件在库中不存在记录"
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// 组装
// ---------------------------------------------------------------------------

// Sources 三个可注入数据源。为 nil 的工具不会被注册。
type Sources struct {
	NodeStatus NodeStatusSource
	Logs       LogSource
	SQL        SQLSource
}

// NewReadonlyRegistry 创建"只读工具集"注册表。
//
// 参数：
//   - policy：策略（AllowHosts、ShellReadPaths、SQLMaxRows 等）；
//   - src：数据源，nil 的对应工具不注册；
//   - opts：审计回调等。
func NewReadonlyRegistry(policy Policy, src Sources, opts ...Option) (*Registry, error) {
	policy = policy.Normalize()
	r := NewRegistry(policy, opts...)
	if err := r.Register(NewShellToolWithPolicy(policy)); err != nil {
		return nil, err
	}
	if err := r.Register(NewHTTPGetTool(policy.AllowHosts, policy.DefaultTimeout, 8192)); err != nil {
		return nil, err
	}
	if src.NodeStatus != nil {
		if err := r.Register(NewNodeStatusTool(src.NodeStatus)); err != nil {
			return nil, err
		}
	}
	if src.Logs != nil {
		if err := r.Register(NewLogSearchTool(src.Logs)); err != nil {
			return nil, err
		}
	}
	if src.SQL != nil {
		if err := r.Register(NewSQLTool(src.SQL, policy.SQLMaxRows)); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// ---- 小工具函数 ----

func stringList(v interface{}) []string {
	switch typed := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return typed
	case nil:
		return nil
	default:
		return nil
	}
}

func countLines(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

func countKeywords(s string, keywords ...string) int {
	lower := strings.ToLower(s)
	n := 0
	for _, kw := range keywords {
		n += strings.Count(lower, kw)
	}
	return n
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func truncateRef(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
