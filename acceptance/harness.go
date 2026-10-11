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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	_ "github.com/go-sql-driver/mysql"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agentmemory"
	"github.com/tmnhs/crony/common/pkg/tools"
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

// truncate 按字节截断，但**不切断多字节字符**。
//
// 为什么较真：验收报告是给人看的交付物，一个被切半的汉字会让整个文件变成
// 非法 UTF-8，编辑器与流水线都打不开（实测：报告第 1831 字节出现悬挂的 E6，
// 严格 UTF-8 解码直接失败）。common/pkg/llm.Truncate 早就是这么做的，这里以前漏了。
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut <= 0 {
		return "…"
	}
	return s[:cut] + "…"
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

// provisionRunFixture 造一条**真实终态**的失败执行记录，供 SSE 与诊断用例使用。
//
// 为什么允许这里写库：验收必须能自己准备前置条件。AC-06/AC-07 要验的是
// "从生产 API 读取一条已结束的执行并流式/诊断它"，而不是"某个 run_id 恰好存在"。
// 夹具只写 job_run + job_run_step（诊断材料来源见 common/pkg/agentruntime/loadrun.go），
// 不写 job 表；测试结束自动删除，不留脏数据。
//
// 返回夹具的 run_id；清理逻辑通过 t.Cleanup 注册。
func provisionRunFixture(t *testing.T, db *sql.DB) string {
	t.Helper()
	return provisionRunFixtureAs(t, db, "run", true)
}

// provisionBareRunFixture 造一条**没有任何步骤、没有任何证据**的失败执行记录。
//
// 用途与有步骤的夹具不同：诊断 Agent 的系统要求是"结论必须有证据"，
// 当库里什么材料都没有时，它只能去调工具（node_status / node_probe / log_search…）。
// 因此这条夹具是**从黑盒侧触发一次真实工具调用**的可重复手段——
// 否则"工具审计是否落库"就只能指望模型当天心情好，验收结果时红时绿。
//
// run_id 与主夹具不同（-bare 后缀），阶段之间互不干扰。
//
// 关键细节：主记录的 `error_msg` 刻意写成**本地材料无法回答**的形态
// （"连接被拒绝，但没说端口是否在监听"）。第一版夹具直接复用了主夹具的
// error_msg（里面写着"引号测试代码，不是真实故障"），结果模型仅凭这条记录
// 就能下结论，一次工具都不调——夹具自己把题目答了，也就无法触发工具审计。
func provisionBareRunFixture(t *testing.T, db *sql.DB) string {
	t.Helper()
	return provisionRunFixtureAs(t, db, "bare", false)
}

// provisionBareRunFixtureN 同上，但允许造多条（每条的 run_id 不同）。
//
// 为什么需要多条：AC-16 的"主动取证"依赖模型真的去调工具，而模型是随机的。
// 单次尝试不成立时再试一次（换一条 run、换一种说法），比"第一次不动手就判 SKIP"
// 更能反映真实能力；但重试次数必须封顶，否则验收变成"烧 token 抽卡"。
func provisionBareRunFixtureN(t *testing.T, db *sql.DB, n int) string {
	t.Helper()
	return provisionRunFixtureAs(t, db, fmt.Sprintf("bare%d", n), false)
}

// provisionRunFixtureAs 是两种夹具的共同实现：withSteps=false 时只写主记录。
func provisionRunFixtureAs(t *testing.T, db *sql.DB, suffix string, withSteps bool) string {
	t.Helper()
	tag := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
	if tag == "" {
		tag = "acc-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	runID := tag + "-" + suffix
	now := time.Now()
	started := now.Add(-3 * time.Second)

	// status=3 是 RunStatusFailed（见 common/models/run.go）：诊断用例需要"失败"
	// 才有可解释的东西；同时它是**终态**，SSE 应立即下发 end。
	// step_count/error_msg 随夹具形态变化：bare 夹具不写步骤，且 error_msg
	// 只描述现象（连接被拒），不提供"是不是夹具/是不是端口没监听"的答案。
	stepCount := 2
	errMsg := "验收夹具：第 2 步失败（引号测试代码，不是真实故障）"
	output := "step1 ok"
	cmdText := `sh -c "exit 7"`
	jobName := "验收夹具任务"
	if !withSteps {
		stepCount = 0
		// 这两个字段的取值是**实测调出来的**，不是随手写的：
		// 只要 error_msg 或 command 里已经含有失败原因（"连接被拒绝"/"exit 7"），
		// 模型一步就能下结论、一次工具都不调。真实历史上唯一能稳定触发工具调用的
		// 那条记录长这样：脚本调用 + error_msg 只有"命令退出码 1" —— 信息量极低，
		// 模型只能去节点上取证（16 个有工具轨迹的会话全部来自这一类 run）。
		errMsg = "命令退出码 1"
		output = ""
		cmdText = `powershell -NoProfile -File scripts\check_db_port.ps1 -Port 13307`
		jobName = "验收诊断取证夹具"
	}
	res, err := db.Exec(`insert into job_run
		(run_id, job_id, job_name, spec, command, job_type, node_uuid, hostname, ip,
		 trigger_source, triggered_by, status, attempt, scheduled_time, started_at, finished_at,
		 duration_ms, exit_code, step_count, success_steps, failed_steps, retry_count,
		 error_msg, output, created, updated)
		values (?, 910007, ?, '0 0 3 * * *', ?, 1, ?, 'acc-host', '127.0.0.1',
		 1, 'acceptance', 3, 1, ?, ?, ?, 3000, 7, ?, 0, 1, 0,
		 ?, ?, ?, ?)`,
		runID, jobName, cmdText, envOr("CRONY_ACC_NODE_UUID", "node-it-01"),
		now.Add(-3*time.Second).Unix(), started.Unix(), now.Unix(),
		stepCount, errMsg, output, now.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("造 run 夹具失败（验收库需要 INSERT 权限）: %v", err)
	}
	affected, _ := res.RowsAffected()
	if affected != 1 {
		t.Fatalf("造 run 夹具影响行数 %d，期望 1", affected)
	}
	// 两条步骤：一条成功、一条失败（诊断的"哪一步失败"材料）。
	steps := []struct {
		idx     int
		name    string
		status  int
		code    int
		output  string
		errMsg  string
		elapsed int64
	}{
		{0, "准备环境", 2, 0, "step1 ok", "", 900},
		{1, "执行清理", 3, 7, "", "引号测试代码：第 2 步退出码 7", 2100},
	}
	if withSteps {
		for _, s := range steps {
			if _, err := db.Exec(`insert into job_run_step
			(run_id, step_index, name, kind, `+"`condition`"+`, command, status, exit_code,
			 retry_count, started_at, finished_at, duration_ms, output, error_msg, created)
			values (?, ?, ?, 'cmd', 'on_success', ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
				runID, s.idx, s.name, "echo "+s.name, s.status, s.code,
				started.Unix(), now.Unix(), s.elapsed, s.output, s.errMsg, now.Unix()); err != nil {
				t.Fatalf("造步骤夹具失败: %v", err)
			}
		}
	}

	t.Cleanup(func() {
		// 清理必须按依赖顺序：先步骤，后主记录。
		// 失败要**报错**（t.Errorf，而不是 t.Logf）：留在验收库里的夹具行会让
		// 下一轮"按最新行判定"的用例读到脏数据，静默通过比失败更糟。
		// 实测过的坑：主用例里 `defer db.Close()` 早于 t.Cleanup 执行，
		// 清理全部失败却只 Logf，结果留下 3 条 run + 6 条步骤。
		if _, err := db.Exec("delete from job_run_step where run_id = ?", runID); err != nil {
			t.Errorf("清理步骤夹具失败（验收库可能残留 %s）: %v", runID, err)
		}
		if _, err := db.Exec("delete from job_run where run_id = ?", runID); err != nil {
			t.Errorf("清理 run 夹具失败（验收库可能残留 %s）: %v", runID, err)
		}
	})
	return runID
}

// ---------- 审批单夹具（阶段 4.2） ----------

// approvalFixture 审批单夹具的描述。
//
// 为什么需要直接造审批单：审批单的正常来源是"模型请求写动作"，而在
// allow_write=false 的部署里模型拿不到写工具（这正是 AC-17 要验的事）。
// 于是"审批生命周期"的接口用例必须自备前置条件——否则要么测不到，
// 要么为了测试而临时打开写权限（那等于用被测对象的配置去迁就测试）。
type approvalFixture struct {
	SessionID string
	Tool      string
	// Args 存档参数（原文 JSON）；ArgsHash 为空时由 Args 现算。
	Args     map[string]any
	ArgsHash string
	// HashForOther 非空时：存档 Args 但哈希按这份"另一组参数"算，
	// 用于复现 TOCTOU（批准 A 执行 B）。
	HashForOther map[string]any
	// ExpiresIn 相对当前的到期时间（负值=已过期）。
	ExpiresIn time.Duration
	Status    string
	Reason    string
}

// provisionApprovalFixture 写入一张审批单夹具并注册自清理（返回 approval_id）。
//
// 只写 approval_request 一张表；清理时按 approval_id 删除。
// 哈希用被测代码里的 tools.ArgsHash：夹具必须是"格式合法"的记录，
// 否则用例测到的会是"夹具写坏了"而不是被测行为。
func provisionApprovalFixture(t *testing.T, db *sql.DB, spec approvalFixture) string {
	t.Helper()
	tag := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
	if tag == "" {
		tag = "acc-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	approvalID := "ap-" + tag + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if spec.Tool == "" {
		spec.Tool = "job_retry"
	}
	if spec.SessionID == "" {
		spec.SessionID = "acceptance:" + tag
	}
	if spec.Status == "" {
		spec.Status = "pending"
	}
	if spec.Args == nil {
		spec.Args = map[string]any{"job_id": float64(910007), "reason": "验收夹具：确认重跑一次"}
	}
	if spec.Reason == "" {
		spec.Reason = "验收夹具：模型自述的重跑理由（用于验证理由被原样存档）"
	}
	hash := spec.ArgsHash
	if hash == "" {
		if spec.HashForOther != nil {
			hash = tools.ArgsHash(spec.HashForOther)
		} else {
			hash = tools.ArgsHash(spec.Args)
		}
	}
	argsJSON, err := json.Marshal(spec.Args)
	if err != nil {
		t.Fatalf("夹具参数无法序列化: %v", err)
	}
	now := time.Now()
	expiresAt := now.Add(spec.ExpiresIn).Unix()
	if spec.ExpiresIn == 0 {
		expiresAt = now.Add(15 * time.Minute).Unix()
	}
	if _, err := db.Exec(`insert into approval_request
		(approval_id, session_id, tool, risk, args_json, args_hash, reason, target_node, impact,
		 requested_by, status, decided_by, decided_at, decision_note, executed_at, execution_result,
		 expires_at, created, created_ms, run_id)
		values (?, ?, ?, 'write', ?, ?, ?, 'job:910007', '1 个任务 / 1 台节点',
		 'diagnose', ?, '', 0, '', 0, '', ?, ?, ?, ?)`,
		approvalID, spec.SessionID, spec.Tool, string(argsJSON), hash, spec.Reason,
		spec.Status, expiresAt, now.Unix(), now.UnixMilli(), tag+"-approval-fixture"); err != nil {
		t.Fatalf("造审批单夹具失败（验收库需要 INSERT 权限）: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec("delete from approval_request where approval_id = ?", approvalID); err != nil {
			t.Errorf("清理审批单夹具失败（验收库可能残留 %s）: %v", approvalID, err)
		}
	})
	return approvalID
}

// approvalRow 审批单的读取结果（断言用）。
type approvalRow struct {
	Status          string
	DecidedBy       string
	DecisionNote    string
	DecidedAt       int64
	ExecutedAt      int64
	ExecutionResult string
}

// loadApprovalRow 从库里读审批单（夹具与断言都走它，保证列口径一致）。
func loadApprovalRow(t *testing.T, db *sql.DB, approvalID string) approvalRow {
	t.Helper()
	var out approvalRow
	row := db.QueryRow(`select status, decided_by, decision_note, decided_at, executed_at, execution_result
		from approval_request where approval_id = ?`, approvalID)
	if err := row.Scan(&out.Status, &out.DecidedBy, &out.DecisionNote, &out.DecidedAt, &out.ExecutedAt, &out.ExecutionResult); err != nil {
		t.Fatalf("读审批单 %s 失败: %v", approvalID, err)
	}
	return out
}

// ---------- 经验库夹具（阶段 4-A） ----------

// memoryFixture 经验条目的夹具描述。
//
// 为什么允许直接写库（与审批单夹具同样的理由）：经验的正常来源是"诊断会话结束后
// 由旁路沉淀"，而**会不会沉淀、沉淀几条取决于模型这次说了什么**。要让
// "召回命中 / 停用后退出召回 / 统计口径"这些契约可复跑，夹具必须自备前置条件；
// 否则验收会变成"看模型今天心情"。
type memoryFixture struct {
	MemoryID   string
	Symptom    string
	ActionText string
	ActionKind string
	Tool       string
	TargetNode string
	AgentKind  string
	Outcome    string
	// Success/Fail/Blocked/Denied/Advised 各结果计数；0 时按 Outcome 记 1 次。
	Success int
	Fail    int
	Blocked int
	Denied  int
	Advised int
	// Enabled 传 false 时造一条**已停用**的条目（用于验证"停用的不进召回"）。
	Enabled bool
	// EnabledSet 为 true 时按 Enabled 落库，否则一律落"启用"。
	EnabledSet bool
}

// EnabledNow 读回夹具当前是否启用（断言"被拒的停用没有改动状态"用）。
func (f memoryFixture) EnabledNow(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var enabled bool
	if err := db.QueryRow("select enabled from "+models.CronyAgentMemoryTableName+
		" where memory_id = ?", f.MemoryID).Scan(&enabled); err != nil {
		t.Fatalf("读经验夹具状态失败: %v", err)
	}
	return enabled
}

// provisionMemoryFixture 写入一条经验夹具并注册自清理（返回夹具描述）。
//
// 落库用的词元串（symptom_tokens）直接由**被测代码**的 agentmemory.TokenizeString 生成：
// 夹具必须是"格式合法、口径一致"的记录，否则用例测到的会是"夹具写坏了"。
func provisionMemoryFixture(t *testing.T, db *sql.DB, spec memoryFixture) memoryFixture {
	t.Helper()
	now := time.Now()
	if spec.MemoryID == "" {
		spec.MemoryID = "mem-acc-" + strconv.FormatInt(now.UnixNano(), 10)
	}
	if spec.AgentKind == "" {
		spec.AgentKind = "diagnose"
	}
	if spec.ActionKind == "" {
		spec.ActionKind = "tool"
	}
	if spec.Outcome == "" {
		spec.Outcome = "failed"
	}
	if spec.TargetNode == "" {
		spec.TargetNode = envOr("CRONY_ACC_NODE_UUID", "node-it-01")
	}
	// 计数：调用方没给具体分布时，按 outcome 记 1 次（保证 occurrence == 各计数之和，
	// 这正是 AC-26 断言的库内自洽条件）。
	if spec.Success+spec.Fail+spec.Blocked+spec.Denied+spec.Advised == 0 {
		switch spec.Outcome {
		case "success":
			spec.Success = 1
		case "failed":
			spec.Fail = 1
		case "blocked":
			spec.Blocked = 1
		case "denied":
			spec.Denied = 1
		case "advised":
			spec.Advised = 1
		}
	}
	occ := spec.Success + spec.Fail + spec.Blocked + spec.Denied + spec.Advised
	enabled := true
	if spec.EnabledSet {
		enabled = spec.Enabled
	}
	dedupeKey := models.MemoryDedupeKey(spec.Symptom, spec.ActionKind, spec.ActionText, spec.TargetNode)
	_, err := db.Exec("insert into "+models.CronyAgentMemoryTableName+` (
		memory_id, dedupe_key, session_id, run_id, job_id, agent_kind, symptom, symptom_tokens,
		action_kind, action_text, tool, target_node, risk, outcome, result_text, result_detail,
		occurrence_count, success_count, fail_count, blocked_count, denied_count, advised_count,
		enabled, disabled_by, disabled_reason, disabled_at,
		created, created_ms, updated, updated_ms, last_seen, last_seen_ms)
		values (?, ?, ?, ?, 910007, ?, ?, ?, ?, ?, ?, ?, 'read', ?, ?, '',
		?, ?, ?, ?, ?, ?, ?, '', '', 0, ?, ?, ?, ?, ?, ?)`,
		spec.MemoryID, dedupeKey, spec.SessionIDOr(), spec.MemoryID+"-run", spec.AgentKind,
		spec.Symptom, agentmemory.TokenizeString(spec.Symptom),
		spec.ActionKind, spec.ActionText, spec.Tool, spec.TargetNode, spec.Outcome,
		spec.OutcomeResultText(), occ, spec.Success, spec.Fail, spec.Blocked, spec.Denied, spec.Advised,
		enabled, now.Unix(), now.UnixMilli(), now.Unix(), now.UnixMilli(), now.Unix(), now.UnixMilli())
	if err != nil {
		t.Fatalf("造经验夹具失败（验收库需要 INSERT 权限）: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec("delete from "+models.CronyAgentMemoryTableName+" where memory_id = ?", spec.MemoryID); err != nil {
			t.Errorf("清理经验夹具失败（验收库可能残留 %s）: %v", spec.MemoryID, err)
		}
	})
	return spec
}

// SessionIDOr 夹具会话号（唯一，避免与真实会话混在一起被按会话查询命中）。
func (f memoryFixture) SessionIDOr() string {
	return "acceptance-memory:" + f.MemoryID
}

// OutcomeResultText 结果摘要：把"动作到底发生没发生"写清楚（与沉淀路径同口径）。
func (f memoryFixture) OutcomeResultText() string {
	switch f.Outcome {
	case "success":
		return "验收夹具：该处置本次执行成功"
	case "failed":
		return "验收夹具：该处置执行了但失败"
	case "blocked":
		return "验收夹具：被拦下转人工，本次**没有执行**"
	case "denied":
		return "验收夹具：被闸门拒绝，本次**没有执行**"
	default:
		return "验收夹具：只给了建议，本次没有动作"
	}
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
	// ToValidUTF8 是兜底：报告里会掺入被测系统的原始输出（err 文本、模型片段），
	// 只要有一处非法字节，整个报告文件在编辑器/流水线里就打不开。
	return os.WriteFile(path, []byte(strings.ToValidUTF8(r.markdown(), "\uFFFD")), 0o644)
}
