//go:build acceptance

package acceptance

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
)

// caseCtx 给每个用例一个"可写证据"的地方：跳过时也必须留下原因（SKIP ≠ 通过）。
type caseCtx struct {
	t      *testing.T
	detail string
}

func (c *caseCtx) note(format string, args ...any) {
	c.detail = fmt.Sprintf(format, args...)
}

func (c *caseCtx) skip(format string, args ...any) {
	c.note(format, args...)
	c.t.Skipf(format, args...)
}

// TestAcceptanceSuite 是验收主入口：一整套黑盒用例，产出三态报告。
//
// 环境变量见 acceptance/README.md。前置条件缺失时**跳过**（并在报告里写明原因），
// 而不是把"没测到"伪装成"测过了"。
func TestAcceptanceSuite(t *testing.T) {
	base := envOr("CRONY_ACC_BASE", "http://127.0.0.1:8089")
	user := envOr("CRONY_ACC_USER", "admin")
	pass := strings.TrimSpace(os.Getenv("CRONY_ACC_PASS"))
	runID := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_ID"))
	nodeUUID := envOr("CRONY_ACC_NODE_UUID", "node-it-01")
	expectReal := strings.TrimSpace(os.Getenv("CRONY_ACC_EXPECT_REAL")) == "1"
	reportPath := strings.TrimSpace(os.Getenv("CRONY_ACC_REPORT"))
	sseShort := time.Duration(envInt("CRONY_ACC_SSE_SHORT_SECONDS", 16)) * time.Second
	sseRun := time.Duration(envInt("CRONY_ACC_SSE_RUN_SECONDS", 20)) * time.Second

	rep := newReport()
	rep.fact("被测后端：%s", base)
	rep.fact("验收库：%s", redactDSN(envOr("CRONY_TEST_MYSQL_DSN", "（未配置 → SQL 断言类用例跳过）")))
	rep.fact("目标节点：%s", nodeUUID)
	rep.fact("真模型用例：%s", map[bool]string{true: "要求 provider 非 offline", false: "未强制（未设 CRONY_ACC_EXPECT_REAL=1）"}[expectReal])
	defer func() {
		if err := rep.write(reportPath); err != nil {
			t.Errorf("写验收报告失败: %v", err)
		}
	}()

	run := func(id, name string, fn func(*caseCtx)) {
		t.Run(id, func(t *testing.T) {
			cc := &caseCtx{t: t, detail: name}
			defer func() {
				status := statusPass
				switch {
				case t.Skipped():
					status = statusSkip
				case t.Failed():
					status = statusFail
				}
				rep.add(id, name, status, cc.detail)
			}()
			fn(cc)
		})
	}

	if pass == "" {
		t.Skip("CRONY_ACC_PASS 未设置：验收必须对着真实环境跑，本程序不做任何口令猜测 —— 整套跳过")
	}

	c := newClient(base)
	loginErr := c.login(user, pass)
	db := openDB(t)
	if db != nil {
		// 用 t.Cleanup 而不是 defer 关连接：Go 里 defer 先于 t.Cleanup 执行，
		// 而夹具清理是在 t.Cleanup 里注册的——若这里 defer 关库，
		// 夹具清理就会撞上"database is closed"，**静默留下脏数据**
		// （实测过：3 条 run 夹具 + 6 条步骤残留）。Cleanup 是后进先出，
		// 这里先注册关库，夹具清理（后注册）就能先跑。
		t.Cleanup(func() { db.Close() })
	}

	// 用例间共享的事实（顺序执行，t.Run 不并行）。
	var (
		lastSession      string
		lastDraftSession string
	)

	// ---------- 前置夹具：一条**真实终态**的执行记录 ----------
	//
	// AC-06（SSE 终态）与 AC-07/08/09/15（诊断正路径）都需要一条已结束的 job_run。
	// 这条记录有两种来源：
	//   1) 调用方通过 CRONY_ACC_RUN_ID 指定（例如刚跑过一次真任务）；
	//   2) 没有指定时**由本程序自建夹具**（跑完自删）。
	//
	// 为什么必须支持自建：验收要"一条命令可复跑"。此前这两个用例依赖一个只在
	// 开发机上存在过的 run_id，换台机器后就会直接 FAIL（或更糟：悄悄 SKIP），
	// 而"从生产 API 读一条真实终态运行记录"这件事本身与此无关。
	//
	// 夹具只写 job_run/job_run_step（admin 侧的诊断正是从这两张表取材料，
	// 见 common/pkg/agentruntime/loadrun.go），不碰 job 表，避免污染任务列表。
	runRecordSource := "环境变量 CRONY_ACC_RUN_ID（调用方提供）"
	if runID == "" && db != nil {
		runID = provisionRunFixture(t, db)
		runRecordSource = "本程序自建夹具（跑完自删）"
	} else if runID == "" {
		runRecordSource = "不可用（既没有 CRONY_ACC_RUN_ID，也没有可写数据库）"
	}
	rep.fact("诊断/SSE 使用的 run 记录：%s（run_id=%s）", runRecordSource,
		map[bool]string{true: runID, false: "（无）"}[runID != ""])

	// ---------- 平台面：鉴权 / 能力面 / 检索 ----------

	run("AC-01", "未鉴权访问 /agent/* 被拒", func(cc *caseCtx) {
		anon := newClient(base)
		status, env, err := anon.do(http.MethodPost, "/agent/tool/list", map[string]any{}, false)
		if err != nil {
			cc.t.Fatalf("请求失败: %v", err)
		}
		if status == http.StatusOK && env.Code == 200 {
			cc.t.Fatalf("未带 token 竟拿到 tool/list（HTTP %d/code %d）", status, env.Code)
		}
		cc.note("HTTP %d / code %d msg=%q", status, env.Code, truncate(env.Msg, 80))
	})

	run("AC-02", "登录：正确口令签发 token；错误口令被拒", func(cc *caseCtx) {
		if loginErr != nil {
			cc.t.Fatalf("正确口令登录失败: %v", loginErr)
		}
		bad := newClient(base)
		status, env, err := bad.do(http.MethodPost, "/login",
			map[string]any{"username": user, "password": pass + "-definitely-wrong"}, false)
		if err != nil {
			cc.t.Fatalf("错误口令请求失败: %v", err)
		}
		if status == http.StatusOK && env.Code == 200 {
			cc.t.Fatal("错误口令竟登录成功")
		}
		cc.note("正确口令 token 长度 %d；错误口令 HTTP %d/code %d", len(c.token), status, env.Code)
	})

	run("AC-03", "工具能力面：只读白名单、风险声明与配额来自后端注册表", func(cc *caseCtx) {
		data, err := c.postOK("/agent/tool/list", map[string]any{})
		if err != nil {
			cc.t.Fatal(err)
		}
		list := asSlice(data["tools"])
		names := make([]string, 0, len(list))
		for _, tv := range list {
			m := asMap(tv)
			name := asStr(m["name"])
			names = append(names, name)
			if !asBool(m["readonly"]) {
				cc.t.Fatalf("工具 %s 的 readonly 不是 true：默认部署（allow-write=false）不应注册写工具", name)
			}
			// 阶段 4：只读工具必须显式声明风险级别（缺字段说明视图层退回硬编码了）
			if got := asStr(m["risk"]); got != "read" {
				cc.t.Fatalf("工具 %s 的 risk 应为 read，实际 %q（缺字段/取值错误都说明风险声明没走注册表）", name, got)
			}
		}
		sort.Strings(names)
		want := []string{"http_get", "log_search", "mysql_query_readonly", "node_probe", "node_status", "shell_readonly"}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			cc.t.Fatalf("工具集合不符（期望 6 个只读工具）: %v", names)
		}
		policy := asMap(data["policy"])
		if asNum(policy["max_calls_per_session"]) <= 0 {
			cc.t.Fatal("缺少 max_calls_per_session 配额")
		}
		// 阶段 4 契约：策略视图必须暴露风险/自治/审批相关字段，且默认**关着**
		if asBool(policy["allow_write"]) || asBool(policy["allow_destructive"]) {
			cc.t.Fatalf("默认部署不得放开写动作：allow_write=%v allow_destructive=%v",
				policy["allow_write"], policy["allow_destructive"])
		}
		if got := asStr(policy["autonomy"]); got != "L1" {
			cc.t.Fatalf("默认自治级别应为 L1（写动作一律人工审批），实际 %q", got)
		}
		if asNum(policy["approval_ttl_seconds"]) <= 0 || asNum(policy["max_concurrent_calls"]) <= 0 ||
			asNum(policy["max_calls_per_minute"]) <= 0 {
			cc.t.Fatalf("策略视图缺少阶段 4 的速率/并发/审批字段: %v", policy)
		}
		cc.note("%d 个工具（全部只读且 risk=read）: %s；autonomy=%v allow_write=%v max_calls_per_session=%v sql_max_rows=%v ttl=%vs",
			len(names), strings.Join(names, ","), policy["autonomy"], policy["allow_write"],
			policy["max_calls_per_session"], policy["sql_max_rows"], policy["approval_ttl_seconds"])
	})

	run("AC-04", "会话检索：分页可用且条目含控制台契约字段", func(cc *caseCtx) {
		data, err := c.postOK("/agent/session/search", map[string]any{"page": 1, "page_size": 2})
		if err != nil {
			cc.t.Fatal(err)
		}
		total := int(asNum(data["total"]))
		items := asSlice(data["list"])
		if total <= 0 || len(items) == 0 {
			cc.t.Fatalf("会话检索为空（total=%d）——没有可回放的轨迹，验收失去意义", total)
		}
		if len(items) > 2 {
			cc.t.Fatalf("page_size=2 但返回 %d 条：分页未生效", len(items))
		}
		first := asMap(items[0])
		for _, k := range []string{"session_id", "kind", "status_text", "provider", "model_calls", "created"} {
			if _, ok := first[k]; !ok {
				cc.t.Fatalf("会话条目缺少契约字段 %s", k)
			}
		}
		cc.note("total=%d 本页 %d 条；首条 %s kind=%s provider=%s created=%s",
			total, len(items), asStr(first["session_id"]), asStr(first["kind"]),
			asStr(first["provider"]), asStr(first["created"]))
	})

	// ---------- SSE：负向（不提前终态）与正向（能拿到终态） ----------

	run("AC-05", "SSE 不存在的 run_id：只心跳、不提前下发终态", func(cc *caseCtx) {
		path := "/job/run/stream?run_id=" + url.QueryEscape("acceptance-nonexistent-"+strconv.FormatInt(time.Now().UnixNano(), 10)) + "&interval_ms=1000"
		res, err := c.sse(path, sseShort)
		if err != nil {
			cc.t.Fatal(err)
		}
		if res.has("end") {
			cc.t.Fatalf("不存在的 run_id 竟提前发终态：%v", res.Events)
		}
		hb := len(res.Data["heartbeat"])
		if hb == 0 {
			cc.t.Fatalf("窗口 %.0fs 内没有心跳（事件=%v）", sseShort.Seconds(), res.Events)
		}
		cc.note("窗口 %.0fs 内事件=%v，心跳 %d 次，未提前 end", sseShort.Seconds(), res.Events, hb)
	})

	run("AC-06", "SSE 真实执行：能观测到 snapshot/step 与终态 end", func(cc *caseCtx) {
		if runID == "" {
			cc.skip("没有可用的 run 记录（既未设置 CRONY_ACC_RUN_ID，也没有可写验收库来自建夹具）：无法验收真实执行的流式终态")
		}
		res, err := c.sse("/job/run/stream?run_id="+url.QueryEscape(runID)+"&interval_ms=1000", sseRun)
		if err != nil {
			cc.t.Fatal(err)
		}
		if !res.has("end") {
			cc.t.Fatalf("窗口 %.0fs 内没等到终态 end（事件=%v）", sseRun.Seconds(), res.Events)
		}
		cc.note("事件序列=%v；end 数据=%s", res.Events, truncate(strings.Join(res.Data["end"], ";"), 120))
	})

	// ---------- Agent 诊断：正路径 / 预算语义 / 幂等回放 ----------

	run("AC-07", "诊断正路径（真模型）：有模型调用、有工具轨迹、有结构化结论", func(cc *caseCtx) {
		if runID == "" {
			cc.skip("没有可用的 run 记录（既未设置 CRONY_ACC_RUN_ID，也没有可写验收库来自建夹具）：无法验收真实诊断")
		}
		data, err := c.postOK("/agent/diagnosis/run", map[string]any{
			"run_id": runID, "force": true, "goal": "排查失败原因，并确认端口监听状态",
		})
		if err != nil {
			cc.t.Fatal(err)
		}
		lastSession = asStr(data["session_id"])
		if lastSession == "" {
			cc.t.Fatal("响应缺少 session_id")
		}
		if len(asMap(data["diagnosis"])) == 0 {
			cc.t.Fatal("响应缺少 diagnosis 结论")
		}
		detail, err := c.postOK("/agent/session/find", map[string]any{"session_id": lastSession})
		if err != nil {
			cc.t.Fatal(err)
		}
		if asNum(detail["model_calls"]) <= 0 {
			cc.t.Fatalf("model_calls=0：本次没有真的调用模型（provider=%s）", asStr(detail["provider"]))
		}
		if expectReal && asStr(detail["provider"]) == "offline" {
			cc.t.Fatal("要求真实 provider，实际是 offline（配置里 llm 段或凭据没生效）")
		}
		if len(asSlice(detail["steps"])) == 0 {
			cc.t.Fatal("steps 为空：没有落轨迹，控制台无法回放")
		}
		diag := asMap(data["diagnosis"])
		cc.note("%s provider=%s model=%s model_calls=%.0f tool_calls=%.0f 证据=%d confidence=%.0f evidence_sufficient=%v 未验证节点断言=%d steps=%d",
			lastSession, asStr(detail["provider"]), asStr(detail["model"]),
			asNum(detail["model_calls"]), asNum(detail["tool_calls"]), int(asNum(detail["evidence_count"])),
			asNum(diag["confidence"]), asBool(diag["evidence_sufficient"]),
			len(asSlice(diag["unverified_node_claims"])), len(asSlice(detail["steps"])))
	})

	run("AC-08", "预算按运行计（不得是进程级）：budget.duration_ms 必须只覆盖本次运行", func(cc *caseCtx) {
		if runID == "" {
			cc.skip("CRONY_ACC_RUN_ID 未设置")
		}
		if lastSession == "" {
			cc.skip("依赖 AC-07 产出的会话，AC-07 未成功")
		}
		// 排序很关键：本套件此前已执行 AC-05（8s 窗口）+ AC-06（最长 20s）+ AC-07（真诊断），
		// 即 admin 进程运行时长必然已经**远大于**单次诊断的墙钟耗时。
		// 因此若预算计时仍从进程启动算起（历史缺陷），本断言会红。
		sw := time.Now()
		data, err := c.postOK("/agent/diagnosis/run", map[string]any{
			"run_id": runID, "force": true, "goal": "确认预算口径：只统计本次运行",
		})
		if err != nil {
			cc.t.Fatal(err)
		}
		wallMS := time.Since(sw).Milliseconds()
		sessionID := asStr(data["session_id"])
		detail, err := c.postOK("/agent/session/find", map[string]any{"session_id": sessionID})
		if err != nil {
			cc.t.Fatal(err)
		}
		budget := asMap(detail["budget"])
		dur := int64(asNum(budget["duration_ms"]))
		maxDur := int64(asNum(budget["max_duration_ms"]))
		calls := int(asNum(budget["calls"]))
		maxCalls := int(asNum(budget["max_calls"]))
		if calls <= 0 {
			cc.t.Fatalf("budget.calls=%d：预算快照没有记录本次用量", calls)
		}
		if maxDur > 0 && dur > maxDur {
			cc.t.Fatalf("budget.duration_ms=%d 超上限 %d", dur, maxDur)
		}
		if dur > wallMS+3000 {
			cc.t.Fatalf("budget.duration_ms=%d 远超本次运行墙钟 %dms：计时仍在从进程启动起算（进程级预算缺陷回归）",
				dur, wallMS)
		}
		if maxCalls > 0 && calls > maxCalls {
			cc.t.Fatalf("budget.calls=%d 超上限 %d", calls, maxCalls)
		}
		cc.note("第 2 次运行：墙钟 %dms；budget calls=%d/%d duration=%dms/%dms tokens=%v —— 与上一次会话各自独立",
			wallMS, calls, maxCalls, dur, maxDur, budget["tokens"])
		lastSession = sessionID
	})

	run("AC-09", "幂等回放：force=false 不重复烧模型、不新增会话", func(cc *caseCtx) {
		if runID == "" {
			cc.skip("CRONY_ACC_RUN_ID 未设置")
		}
		before, err := c.postOK("/agent/session/search", map[string]any{"page": 1, "page_size": 1})
		if err != nil {
			cc.t.Fatal(err)
		}
		totalBefore := int(asNum(before["total"]))
		data, err := c.postOK("/agent/diagnosis/run", map[string]any{"run_id": runID, "force": false})
		if err != nil {
			cc.t.Fatal(err)
		}
		after, err := c.postOK("/agent/session/search", map[string]any{"page": 1, "page_size": 1})
		if err != nil {
			cc.t.Fatal(err)
		}
		totalAfter := int(asNum(after["total"]))
		if !asBool(data["replayed"]) {
			cc.t.Fatal("force=false 未走回放：会重复消耗模型额度")
		}
		if totalAfter != totalBefore {
			cc.t.Fatalf("回放路径新增了会话（total %d → %d）", totalBefore, totalAfter)
		}
		cc.note("replayed=true，会话总数 %d→%d 未变，复用 session=%s", totalBefore, totalAfter, asStr(data["session_id"]))
	})

	// ---------- 自然语言建任务：起草不落库、确认只落"未分配" ----------

	run("AC-10", "NL 起草：产出结构化草案但**不**创建任务", func(cc *caseCtx) {
		before, err := c.postOK("/job/search", map[string]any{"page": 1, "page_size": 1})
		if err != nil {
			cc.t.Fatal(err)
		}
		totalBefore := int(asNum(before["total"]))
		data, err := c.postOK("/agent/nljob/draft", map[string]any{
			"text": "每天凌晨 3 点打印一行 hello 到日志", "node_uuid": nodeUUID, "timezone": "Asia/Shanghai",
		})
		if err != nil {
			cc.t.Fatal(err)
		}
		draft := asMap(data["draft"])
		// 注意字段位置：契约注释说 session_id 在 data 外层，实测它在 **draft 内部**
		// （控制台靠 normalizeDraft 把 draft 摊平到外层才能读到，见 console/src/api/agent.js）。
		// 这里按实测位置断言，并把差异记进报告。
		if asStr(draft["session_id"]) == "" {
			cc.t.Fatal("草案里没有 session_id：确认接口无法续接（前端向导会拿不到 session_id）")
		}
		// command 在契约里既可能是 argv 数组、也可能是整条字符串（模型两种都产出过），
		// 所以只要求"非空且能表达一条命令"。
		hasCommand := len(asSlice(draft["command"])) > 0 || asStr(draft["command"]) != ""
		if asStr(draft["name"]) == "" || asStr(draft["spec"]) == "" || !hasCommand {
			cc.t.Fatalf("草案缺少 name/spec/command: %v", draft)
		}
		// 起草的契约是"只给草案、不落库"：两个标志必须一真一假。
		if !asBool(draft["requires_confirmation"]) || asBool(draft["confirmed"]) {
			cc.t.Fatalf("草案标志不符合契约（requires_confirmation 应恒真、confirmed 应恒假）: requires_confirmation=%v confirmed=%v",
				draft["requires_confirmation"], draft["confirmed"])
		}
		if len(asSlice(draft["next_triggers"])) == 0 {
			cc.t.Fatal("草案没有触发回显 next_triggers：用户无法核对计划时间")
		}
		after, err := c.postOK("/job/search", map[string]any{"page": 1, "page_size": 1})
		if err != nil {
			cc.t.Fatal(err)
		}
		totalAfter := int(asNum(after["total"]))
		if totalAfter != totalBefore {
			cc.t.Fatalf("起草阶段不应创建任务（total %d → %d）", totalBefore, totalAfter)
		}
		lastDraftSession = asStr(draft["session_id"])
		cc.note("session=%s 草案 name=%q spec=%q command=%v raw_argv=%v 下次触发=%v 警告=%d；任务总数 %d→%d 未变",
			lastDraftSession, asStr(draft["name"]), asStr(draft["spec"]), draft["command"],
			draft["raw_argv"], asSlice(draft["next_triggers"]), len(asSlice(draft["warnings"])), totalBefore, totalAfter)
	})

	run("AC-11", "NL 确认：只落「未分配」任务定义，不派发执行", func(cc *caseCtx) {
		if lastDraftSession == "" {
			cc.skip("依赖 AC-10 产出的起草会话")
		}
		data, err := c.postOK("/agent/nljob/confirm", map[string]any{
			"session_id": lastDraftSession, "confirmed": true, "overrides": map[string]any{},
		})
		if err != nil {
			cc.t.Fatal(err)
		}
		jobID := int(asNum(data["job_id"]))
		if jobID <= 0 {
			cc.t.Fatalf("确认创建未返回 job_id: %v", data)
		}
		if got := int(asNum(data["status"])); got != 0 {
			cc.t.Fatalf("确认创建的任务必须 status=0（未分配、不自动执行），实际 %d", got)
		}
		if !asBool(data["requires_assign"]) {
			cc.t.Fatal("响应未标注 requires_assign=true：用户无法知道还需分配节点")
		}
		cleaned := "（未清理：未配置 CRONY_TEST_MYSQL_DSN，报告里保留 job_id 供人工清理）"
		if db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := db.ExecContext(ctx, "delete from job where id = ?", jobID)
			cancel()
			if err != nil {
				cleaned = fmt.Sprintf("（清理失败，需人工删除 job_id=%d: %v）", jobID, err)
			} else {
				cleaned = fmt.Sprintf("已回收验收产生的 job_id=%d", jobID)
			}
		}
		cc.note("job_id=%d spec=%s status=0 requires_assign=true；%s", jobID, asStr(data["spec"]), cleaned)
	})

	// ---------- 探测通道与节点水位（SQL 事实断言） ----------

	run("AC-12", "探测审计行字段完整（done 行必须带 exit_code 与节点主机名）", func(cc *caseCtx) {
		if db == nil {
			cc.skip("CRONY_TEST_MYSQL_DSN 未配置：无法核对审计行")
		}
		rows, err := db.Query("select request_id, command, status, exit_code, node_hostname, coalesce(error,'') " +
			"from node_probe_request order by id desc limit 20")
		if err != nil {
			cc.t.Fatalf("查询审计表失败: %v", err)
		}
		defer rows.Close()
		allowed := map[string]bool{"pending": true, "running": true, "done": true, "failed": true, "timed_out": true, "expired": true}
		total, done, expired := 0, 0, 0
		for rows.Next() {
			var requestID, command, status, hostname, errText string
			var exitCode sql.NullInt64
			if err := rows.Scan(&requestID, &command, &status, &exitCode, &hostname, &errText); err != nil {
				cc.t.Fatalf("扫描审计行失败: %v", err)
			}
			total++
			if !allowed[status] {
				cc.t.Fatalf("审计行 %s 状态非法: %q", requestID, status)
			}
			if requestID == "" || command == "" {
				cc.t.Fatalf("审计行缺少 request_id/command: %+v", requestID)
			}
			switch status {
			case "done":
				done++
				if !exitCode.Valid {
					cc.t.Fatalf("done 行 %s 没有 exit_code：无法判断命令真实成败", requestID)
				}
				if strings.TrimSpace(hostname) == "" {
					cc.t.Fatalf("done 行 %s 没有节点主机名：证据无法归属到具体节点", requestID)
				}
			case "expired":
				expired++
			}
		}
		if err := rows.Err(); err != nil {
			cc.t.Fatalf("遍历审计行失败: %v", err)
		}
		if total == 0 {
			cc.skip("node_probe_request 暂无行：本轮没有节点侧探测可验收")
		}
		cc.note("最近 %d 行：done=%d expired=%d；done 行均带 exit_code 与 hostname", total, done, expired)
	})

	run("AC-13", "expired 语义：必须自述「没人领取/被取消」，不得被读成「命令执行失败」", func(cc *caseCtx) {
		if db == nil {
			cc.skip("CRONY_TEST_MYSQL_DSN 未配置")
		}
		rows, err := db.Query("select request_id, exit_code, coalesce(error,'') from node_probe_request " +
			"where status = 'expired' order by id desc limit 5")
		if err != nil {
			cc.t.Fatalf("查询 expired 行失败: %v", err)
		}
		defer rows.Close()
		n, legacyNoReason := 0, 0
		for rows.Next() {
			var requestID, errText string
			var exitCode sql.NullInt64
			if err := rows.Scan(&requestID, &exitCode, &errText); err != nil {
				cc.t.Fatalf("扫描失败: %v", err)
			}
			n++
			// 判定只针对**最新这一条**（离当前构建最近的那次运行产物）。
			// 更早的行可能来自中间版本的二进制（实测 2026-10-11 22:33 两台旧进程留下的
			// 两行 error 为空），把它们当成本次缺陷会导致"拿历史数据判当前代码"。
			if n > 1 {
				if strings.TrimSpace(errText) == "" {
					legacyNoReason++
				}
				continue
			}
			if !exitCode.Valid || exitCode.Int64 != -1 {
				cc.t.Fatalf("expired 行 %s 的 exit_code 应为 -1，实际 %+v", requestID, exitCode)
			}
			if strings.TrimSpace(errText) == "" {
				cc.t.Fatalf("expired 行 %s 没写原因：会被误读成命令执行失败", requestID)
			}
			if !strings.Contains(errText, "没有领取") && !strings.Contains(errText, "取消") {
				cc.t.Fatalf("expired 行 %s 的原因没有说明是「没人领取/被取消」：%q", requestID, truncate(errText, 120))
			}
			// 允许**明确的否定句式**（"这不等于命令执行失败"），
			// 但它不能是平铺直叙的"命令执行失败"——那才是把 expired 读成失败。
			if strings.Contains(errText, "执行失败") || strings.Contains(errText, "命令失败") {
				negated := strings.Contains(errText, "不等于") || strings.Contains(errText, "不能") ||
					strings.Contains(errText, "不应") || strings.Contains(errText, "并非")
				if !negated {
					cc.t.Fatalf("expired 行 %s 的原因被表述成命令失败：%q", requestID, truncate(errText, 140))
				}
			}
			for _, banned := range []string{"exit status", "returned non-zero", "退出码非零"} {
				if strings.Contains(errText, banned) {
					cc.t.Fatalf("expired 行 %s 的原因带有命令失败措辞（%q）：%q", requestID, banned, truncate(errText, 140))
				}
			}
		}
		if err := rows.Err(); err != nil {
			cc.t.Fatalf("遍历失败: %v", err)
		}
		if n == 0 {
			cc.skip("本次库里没有 expired 行：该语义需要「节点离线」场景才能验收（不是通过）")
		}
		cc.note("最新 1 条 expired 行：exit_code=-1 且原因自述为没人领取/被取消；"+
			"另有 %d/%d 条更早的历史行缺少原因文本（来自旧版本进程，不计入本次判定）", legacyNoReason, n-1)
	})

	run("AC-14", "节点资源水位：目标节点有新鲜的真数据（含容量）", func(cc *caseCtx) {
		if db == nil {
			cc.skip("CRONY_TEST_MYSQL_DSN 未配置：无法读取 node_metric")
		}
		// 只看**目标节点**这一行：别的节点的旧数据不能拿来给本次通过背书。
		var (
			rows      int
			uuid      string
			collected int64
			cpu       float64
			mem       float64
			disk      float64
			cores     int
			memTotal  int64
			diskTotal int64
		)
		err := db.QueryRow(`select count(*) from node_metric where node_uuid = ?`, nodeUUID).Scan(&rows)
		if err != nil {
			cc.t.Fatalf("查询 node_metric 失败: %v", err)
		}
		if rows == 0 {
			cc.skip(fmt.Sprintf("node_metric 里没有节点 %s 的水位行：节点侧水位发布器没在跑"+
				"（阶段 3 收尾项，AC-14 专门盯它；跳过不等于通过）", nodeUUID))
		}
		err = db.QueryRow(`select node_uuid, collected_at, cpu_percent, mem_percent, disk_percent,
				cpu_cores, mem_total_mb, disk_total_mb
			from node_metric where node_uuid = ? order by id desc limit 1`, nodeUUID).
			Scan(&uuid, &collected, &cpu, &mem, &disk, &cores, &memTotal, &diskTotal)
		if err != nil {
			cc.t.Fatalf("读取节点水位失败: %v", err)
		}
		if strings.TrimSpace(uuid) == "" {
			cc.t.Fatal("node_metric 有行但 node_uuid 为空，水位无法归属")
		}
		// collected_at 单位必须能被读取方正确还原（admin 按 Unix 秒）。
		now := time.Now()
		freshness := now.Sub(time.Unix(collected, 0))
		if collected <= 0 || freshness < -2*time.Minute || freshness > 10*time.Minute {
			cc.t.Fatalf("collected_at=%d 不是「刚刚采集」的时间（距今 %s）："+
				"要么发布器没在跑，要么写入方与读取方对时间单位理解不同", collected, freshness.Round(time.Second))
		}
		for _, p := range []struct {
			name string
			v    float64
		}{{"cpu_percent", cpu}, {"mem_percent", mem}, {"disk_percent", disk}} {
			if p.v < 0 || p.v > 100 {
				cc.t.Fatalf("%s=%.2f 不在 0~100 之间：水位不是真采集出来的", p.name, p.v)
			}
		}
		// 容量：只有百分比时模型无法判断"90% 是 4GB 还是 512GB"，所以容量必须有真值。
		if cores <= 0 || memTotal <= 0 || diskTotal <= 0 {
			cc.t.Fatalf("容量字段为 0（cores=%d mem_total_mb=%d disk_total_mb=%d）："+
				"要么采集失败却照样落库（伪造 0），要么发布器没带容量", cores, memTotal, diskTotal)
		}
		// 全零水位是最典型的"假数据"：真机上 CPU/内存/磁盘不可能是三个 0。
		if cpu == 0 && mem == 0 && disk == 0 {
			cc.t.Fatal("cpu/mem/disk 全为 0：这是伪造的假水位，不是真采集")
		}
		cc.note("节点 %s 水位采集于 %s（距今 %s）：cpu=%.2f%% mem=%.2f%% disk=%.2f%%；"+
			"容量 %d 核 / 内存 %dMB / 磁盘 %dMB", uuid, time.Unix(collected, 0).Format("15:04:05"),
			freshness.Round(time.Second), cpu, mem, disk, cores, memTotal, diskTotal)
	})

	run("AC-15", "会话详情契约字段齐全（控制台回放面板依赖）", func(cc *caseCtx) {
		if lastSession == "" {
			cc.skip("依赖 AC-07/AC-08 产出的会话")
		}
		detail, err := c.postOK("/agent/session/find", map[string]any{"session_id": lastSession})
		if err != nil {
			cc.t.Fatal(err)
		}
		required := []string{
			"session_id", "kind", "status", "status_text", "stop_reason", "stop_reason_text",
			"provider", "model", "model_calls", "tool_calls", "total_tokens", "duration_ms",
			"budget", "tool_stats", "steps", "evidence", "created", "created_ms",
		}
		missing := make([]string, 0, len(required))
		for _, k := range required {
			if _, ok := detail[k]; !ok {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			cc.t.Fatalf("控制台契约字段缺失: %v", missing)
		}
		stats := asMap(detail["tool_stats"])
		perToolRaw, ok := stats["per_tool"]
		if !ok {
			cc.t.Fatal("tool_stats 缺少 per_tool（控制台分布面板依赖它；即使没调工具也必须是 {}）")
		}
		// per_tool 与 calls 必须自洽：只报总数、不报分布（或反之）都会让
		// "模型到底用了哪些工具"变成不可核对的说法。
		sum := 0
		for _, v := range asMap(perToolRaw) {
			sum += int(asNum(v))
		}
		calls := int(asNum(stats["calls"]))
		if sum != calls {
			cc.t.Fatalf("tool_stats 自洽性检查失败：per_tool 合计=%d 但 calls=%d（per_tool=%v）", sum, calls, perToolRaw)
		}
		cc.note("%d 个契约字段齐全；tool_stats=%v；per_tool 计数与 calls 自洽（合计 %d）；budget=%v",
			len(required), stats, sum, asMap(detail["budget"]))
	})

	// ---------- 阶段 4：工具审计 ----------

	run("AC-16", "工具调用审计落库：每次调用一行事实（含被拒的），且能追到会话/运行", func(cc *caseCtx) {
		if db == nil {
			cc.skip("未提供 CRONY_TEST_MYSQL_DSN：审计表核查需要只读 SQL 通道")
		}
		// 表必须存在且可读——这条在**任何分支**下都要成立，
		// 否则"没有审计行"可能只是因为表根本没建（把缺陷伪装成"本轮没调工具"）。
		var tableCount int
		if err := db.QueryRow("select count(*) from information_schema.tables " +
			"where table_schema = database() and table_name = 'agent_tool_audit'").Scan(&tableCount); err != nil {
			cc.t.Fatalf("检查 agent_tool_audit 是否存在失败: %v", err)
		}
		if tableCount == 0 {
			cc.t.Fatal("agent_tool_audit 表不存在：AutoMigrate 未包含审计表，工具调用不可能留痕")
		}

		// 本套件自己产生的两个会话：AC-07 诊断会话 + AC-10 起草会话。
		// 审计事实必须能追到**具体会话**，而不是只能查全库最近若干行。
		// 注意：AC-16 允许单独运行（`-run TestAcceptanceSuite/AC-16`），
		// 此时没有会话——不跳过，直接走下面的"主动触发"分支，保证这条用例自给自足。
		sessions := make([]string, 0, 3)
		if lastSession != "" {
			sessions = append(sessions, lastSession)
		}
		if lastDraftSession != "" {
			sessions = append(sessions, lastDraftSession)
		}

		// 会话视角的汇总（tool_stats）与"每次调用一行的事实"必须一致：
		// 只有汇总没有事实，事后无法回答"到底哪一次调用被哪道闸拦了"。
		totalCalls := 0
		for _, sid := range sessions {
			detail, err := c.postOK("/agent/session/find", map[string]any{"session_id": sid})
			if err != nil {
				// 会话可能因 AC-10 的清理而消失：单条取不到不影响整体判定
				cc.note("会话 %s 详情取不到（%v），跳过其 calls 口径", sid, err)
				continue
			}
			totalCalls += int(asNum(asMap(detail["tool_stats"])["calls"]))
		}

		type auditRow struct {
			sessionID, tool, decision, reason, kind, runID string
			errMsg                                         string
			ok                                             bool
			created, createdMS                             int64
		}
		const auditCols = "select session_id, tool, decision, reason, agent_kind, run_id, ok, err, created, created_ms " +
			"from agent_tool_audit where session_id in ("
		// 占位符数量必须跟着**实际查询的会话数**走：写死成初始 len(sessions) 会让
		// "主动触发"那次查询参数个数不匹配而静默拿不到行（错误被忽略 → 用例误判成链路断了）。
		auditQuery := func(ids []string) (string, []any) {
			q := auditCols
			args := make([]any, 0, len(ids))
			for i, sid := range ids {
				if i > 0 {
					q += ","
				}
				q += "?"
				args = append(args, sid)
			}
			return q + ") order by id asc", args
		}

		var got []auditRow
		loadRows := func(ids []string) error {
			if len(ids) == 0 {
				return nil
			}
			q, args := auditQuery(ids)
			sqlRows, err := db.Query(q, args...)
			if err != nil {
				return err
			}
			defer sqlRows.Close()
			for sqlRows.Next() {
				var r auditRow
				if err := sqlRows.Scan(&r.sessionID, &r.tool, &r.decision, &r.reason, &r.kind, &r.runID, &r.ok, &r.errMsg, &r.created, &r.createdMS); err != nil {
					return err
				}
				if r.sessionID != "" {
					got = append(got, r)
				}
			}
			return sqlRows.Err()
		}
		if err := loadRows(sessions); err != nil {
			cc.t.Fatalf("查询 agent_tool_audit 失败: %v", err)
		}

		// 模型行为不可控：本轮会话恰好一次工具都没调时，"审计无行"是**正确**结果。
		// 但直接 SKIP 等于把"审计链路是否真的在工作"永远挂在模型心情上，
		// 所以这里再**主动触发**：造一条"信息量极低"的 run（只有退出码、无步骤无日志，
		// 见 provisionBareRunFixture 的注释），逼诊断 Agent 去节点上取证。
		// 最多试 2 次（换 run、换说法）——重试封顶，不做"烧 token 抽卡"。
		provoked, provokedCalls := "", 0
		provokeGoals := []string{
			"诊断任务[验收诊断取证夹具]（id=910007）本次运行失败的原因；" +
				"记录里只有『命令退出码 1』，没有步骤与日志 —— 请在目标节点上实际取证" +
				"（node_probe / node_status / log_search），不要凭猜测断言",
			"按巡检流程排查任务 910007 为何失败：先用 node_status 看目标节点是否在线与资源水位，" +
				"再用 node_probe 确认 13307 端口是否在监听，必要时用 log_search 查日志；" +
				"没有证据就不要下结论",
		}
		for attempt := 1; attempt <= len(provokeGoals) && len(got) == 0 && totalCalls == 0 && db != nil; attempt++ {
			bare := provisionBareRunFixtureN(t, db, attempt)
			resp, err := c.postOK("/agent/diagnosis/run", map[string]any{
				"run_id": bare, "force": true, "goal": provokeGoals[attempt-1],
			})
			if err != nil {
				cc.note("第 %d 次主动触发诊断失败（不影响审计判定）：%v", attempt, err)
				continue
			}
			provoked = asStr(resp["session_id"])
			if provoked == "" {
				continue
			}
			sessions = append(sessions, provoked)
			if detail, err := c.postOK("/agent/session/find", map[string]any{"session_id": provoked}); err == nil {
				provokedCalls = int(asNum(asMap(detail["tool_stats"])["calls"]))
			}
			totalCalls += provokedCalls
			if err := loadRows([]string{provoked}); err != nil {
				cc.t.Fatalf("查询主动触发会话的审计行失败: %v", err)
			}
			cc.note("第 %d 次主动触发：会话 %s，工具调用 %d 次，审计事实 %d 行",
				attempt, provoked, provokedCalls, len(got))
		}
		if len(got) == 0 {
			if totalCalls == 0 {
				cc.skip("本轮会话（含 %d 次主动触发）都没有发生工具调用（tool_stats.calls=0）："+
					"审计链路无法从黑盒侧触发；该链路由真库集成测试 common/pkg/agentruntime/audit_mysql_test.go 覆盖",
					len(sessions))
			}
			cc.t.Fatalf("会话统计 reports calls=%d，但 agent_tool_audit 里没有对应事实行——审计链路断了", totalCalls)
		}
		if provoked != "" {
			cc.note("原会话未调工具；主动触发无证据诊断会话 %s（calls=%d）后拿到审计事实", provoked, provokedCalls)
		}

		// 每条事实都必须自洽：决策必填、原因在"非放行"时必须写明、时间双口径非零
		for _, r := range got {
			if r.tool == "" || r.decision == "" {
				cc.t.Fatalf("审计行缺少 tool/decision: %+v", r)
			}
			if r.sessionID == "" || r.kind == "" {
				cc.t.Fatalf("审计行必须能追到会话与场景（session_id/agent_kind 不得为空）: %+v", r)
			}
			switch r.decision {
			case "allowed", "approval_executed":
				// 放行 + 执行失败是**正常事实**（工具本身报错、命令不存在等），
				// 但它必须留下可诊断的原因（reason 或 err 至少有一个非空）；
				// 两者都空才是缺陷：审计会变成"某次调用失败了，但没人知道为什么"。
				// 这条断言最初写反了（把"有原因"当成失败），是 AC-16 自己在
				// 真机第一次抓到"allowed + 执行失败"的行时暴露出来的。
				if !r.ok && r.reason == "" && r.errMsg == "" {
					cc.t.Fatalf("放行但执行失败的行必须写明失败原因（reason/err 至少一个非空）: %+v", r)
				}
			case "denied", "approval_required":
				if r.reason == "" {
					cc.t.Fatalf("被拦/转审批的行必须写明原因（哪道闸）: %+v", r)
				}
			default:
				cc.t.Fatalf("未知决策取值 %q（前端审计面板会渲染不出来）", r.decision)
			}
			if r.created <= 0 || r.createdMS <= 0 {
				cc.t.Fatalf("审计行时间必须双口径非零: %+v", r)
			}
		}
		// 事实数量与汇总数量要对得上（被拒调用不计入 calls，所以是 >= 而不是 ==）
		if len(got) < totalCalls {
			cc.t.Fatalf("审计事实行数（%d）少于会话统计的 calls（%d）：有调用没有落痕", len(got), totalCalls)
		}
		cc.note("%d 行审计事实（%d 个会话、calls 合计 %d）；首行 session=%s tool=%s decision=%s ok=%v reason=%s err=%s kind=%s run=%s",
			len(got), len(sessions), totalCalls, got[0].sessionID, got[0].tool, got[0].decision,
			got[0].ok, truncate(got[0].reason, 40), truncate(got[0].errMsg, 60), got[0].kind, got[0].runID)
	})

	// ---------- 阶段 4.2：写动作审批（AC-17…AC-25） ----------
	//
	// 这一组用例的被测对象是"模型想动手时，谁来踩刹车"。部署形态是
	// allow_write=false（AC-03 已断言），因此**黑盒侧只能验证"不执行"这一侧**：
	// 写工具根本不在能力面里、审批单即使被批准也不会执行、参数不一致会被拦。
	// "批准后真的执行了一次"需要写权限打开，由真库集成测试覆盖
	// （admin/internal/service/approval_integration_test.go，构建标签 integration）。
	// 这个分工是有意的：不去为了让用例好看而临时改部署配置。

	run("AC-17", "写动作能力面：allow_write=false 时写工具不可被调用", func(cc *caseCtx) {
		_, env, err := c.do(http.MethodPost, "/agent/tool/list", map[string]any{}, true)
		if err != nil {
			cc.t.Fatalf("请求失败: %v", err)
		}
		data := env.dataMap()
		policy := asMap(data["policy"])
		if asBool(policy["allow_write"]) {
			cc.t.Fatalf("部署策略 allow_write=true，本用例的前提（只读部署）不成立：" +
				"请在验收环境保持 allow_write=false，否则写动作审批的「不执行」一侧无从验证")
		}
		tools0 := asSlice(data["tools"])
		var writeNames []string
		for _, t0 := range tools0 {
			m := asMap(t0)
			risk := asStr(m["risk"])
			if risk == "write" || risk == "destructive" {
				writeNames = append(writeNames, asStr(m["name"]))
			}
			if asStr(m["name"]) == "job_retry" {
				writeNames = append(writeNames, "job_retry")
			}
		}
		if len(writeNames) > 0 {
			cc.t.Fatalf("只读部署里出现了写工具 %v：写动作必须「不注册」而不是「注册了再拦」", writeNames)
		}
		// 审计是"模型试过什么"的唯一事实来源：只读部署下不该有任何写类调用留痕。
		if db != nil {
			since := time.Now().Add(-10 * time.Minute).Unix()
			var n int
			if err := db.QueryRow(`select count(*) from agent_tool_audit
				where tool = 'job_retry' and created >= ?`, since).Scan(&n); err != nil {
				cc.t.Fatalf("查询审计失败: %v", err)
			}
			if n != 0 {
				cc.t.Fatalf("只读部署里出现了 %d 条 job_retry 调用留痕：能力面与审计不一致", n)
			}
			var nw int
			if err := db.QueryRow(`select count(*) from agent_tool_audit
				where risk in ('write','destructive') and created >= ?`, since).Scan(&nw); err != nil {
				cc.t.Fatalf("查询审计失败: %v", err)
			}
			if nw != 0 {
				cc.t.Fatalf("只读部署里出现了 %d 条写类调用留痕", nw)
			}
		}
		cc.note("策略 allow_write=false / 自治 %s；能力面 %d 个工具，其中写工具 0 个；审计中写类调用 0 条",
			asStr(policy["autonomy"]), len(tools0))
	})

	// approvalDecision 调一次审批决策接口：返回（业务码、data、msg）。
	approvalDecision := func(path, approvalID, note string) (int, map[string]any, string, error) {
		body := map[string]any{"approval_id": approvalID}
		if note != "" {
			body["note"] = note
		}
		status, env, err := c.do(http.MethodPost, path, body, true)
		if err != nil {
			return 0, nil, "", fmt.Errorf("POST %s 网络错误: %w", path, err)
		}
		if status != http.StatusOK {
			return env.Code, nil, env.Msg, fmt.Errorf("POST %s HTTP %d（审批接口约定恒 HTTP 200）", path, status)
		}
		return env.Code, env.dataMap(), env.Msg, nil
	}
	// auditRowsFor 统计某张审批单在审计表里留下的调用行数。
	// 这是"第二次决策有没有重新走执行路径"唯一可观测的副作用。
	auditRowsFor := func(cc *caseCtx, approvalID string) int {
		var n int
		if err := db.QueryRow(`select count(*) from agent_tool_audit where approval_id = ?`, approvalID).Scan(&n); err != nil {
			cc.t.Fatalf("查询审批单审计行失败: %v", err)
		}
		return n
	}
	requireDB := func(cc *caseCtx) {
		if db == nil {
			cc.skip("未配置 CRONY_TEST_MYSQL_DSN（或验收库不可写）：无法自备审批单夹具，" +
				"而审批单的正常来源是模型请求写动作——只读部署里造不出来")
		}
	}

	run("AC-18", "策略未开时批准也不执行（fail-closed，code 1017）", func(cc *caseCtx) {
		requireDB(cc)
		id := provisionApprovalFixture(t, db, approvalFixture{Reason: "验收夹具：策略未开时的批准必须被复核拦下"})
		code, data, msg, err := approvalDecision("/agent/approval/approve", id, "验证 fail-closed")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code != 1017 {
			cc.t.Fatalf("策略未开时批准应返回 1017（执行前复核未通过），实际 code=%d msg=%q", code, truncate(msg, 200))
		}
		if asBool(data["executed"]) {
			cc.t.Fatalf("复核未通过竟然报告 executed=true：写动作被真的执行了")
		}
		row := loadApprovalRow(t, db, id)
		if row.Status != "blocked" {
			cc.t.Fatalf("复核未通过的单子应落 blocked（决策已消耗、动作从未执行），实际 %s", row.Status)
		}
		if strings.TrimSpace(row.ExecutionResult) == "" {
			cc.t.Fatalf("复核未通过必须写明原因（回看时要知道是哪一道闸）：%+v", row)
		}
		if row.DecidedBy == "" || row.DecidedAt == 0 {
			cc.t.Fatalf("决策人与时间必须落库：%+v", row)
		}
		if row.ExecutedAt != 0 {
			cc.t.Fatalf("复核未通过不得留下「已执行」时间戳：%+v", row)
		}
		cc.note("code=1017 msg=%s；单子状态 %s，执行结果=%s", truncate(msg, 90), row.Status, truncate(row.ExecutionResult, 80))
	})

	run("AC-19", "一次批准一次执行：第二次决策是冲突且不重走执行路径", func(cc *caseCtx) {
		requireDB(cc)
		id := provisionApprovalFixture(t, db, approvalFixture{Reason: "验收夹具：验证同一张单子只能被决策一次"})
		code1, _, msg1, err := approvalDecision("/agent/approval/approve", id, "第一次批准")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code1 != 1017 {
			cc.t.Fatalf("首次批准在只读部署下应被复核拦下（1017），实际 code=%d msg=%q", code1, truncate(msg1, 160))
		}
		row1 := loadApprovalRow(t, db, id)
		rows1 := auditRowsFor(cc, id)

		code2, data2, msg2, err := approvalDecision("/agent/approval/approve", id, "第二次批准（模拟重复点击/并发）")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code2 != 1014 {
			cc.t.Fatalf("已决策的单子再次批准必须返回 1014（状态已变更），实际 code=%d msg=%q", code2, truncate(msg2, 160))
		}
		if asBool(data2["executed"]) {
			cc.t.Fatalf("第二次批准竟报告 executed=true")
		}
		row2 := loadApprovalRow(t, db, id)
		rows2 := auditRowsFor(cc, id)
		if rows2 != rows1 {
			cc.t.Fatalf("第二次决策又走了一遍执行路径（审计行 %d → %d）：一次批准必须至多一次执行", rows1, rows2)
		}
		if row2.ExecutedAt != row1.ExecutedAt || row2.Status != row1.Status || row2.DecidedBy != row1.DecidedBy {
			cc.t.Fatalf("第二次决策改写了已决策单子：决策前 %+v / 决策后 %+v", row1, row2)
		}
		cc.note("首次 code=%d（状态→%s，审计 %d 行）；二次 code=%d；executed_at 未变（%d）",
			code1, row1.Status, rows1, code2, row2.ExecutedAt)
	})

	run("AC-20", "TOCTOU：批准 A 却执行 B 一律拒绝（code 1016，且不碰执行路径）", func(cc *caseCtx) {
		requireDB(cc)
		id := provisionApprovalFixture(t, db, approvalFixture{
			Args:         map[string]any{"job_id": float64(910007), "reason": "验收夹具：批准的是这个任务"},
			HashForOther: map[string]any{"job_id": float64(999999), "reason": "但存档哈希对的是另一个任务"},
			Reason:       "验收夹具：复现「批准与执行之间参数被改」",
		})
		code, data, msg, err := approvalDecision("/agent/approval/approve", id, "看起来没问题")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code != 1016 {
			cc.t.Fatalf("参数哈希与存档不一致必须返回 1016，实际 code=%d msg=%q", code, truncate(msg, 200))
		}
		if asBool(data["executed"]) {
			cc.t.Fatalf("参数不一致竟然执行了")
		}
		row := loadApprovalRow(t, db, id)
		if row.Status != "blocked" {
			cc.t.Fatalf("参数不一致应落 blocked（复核发生在网关之前），实际 %s", row.Status)
		}
		if n := auditRowsFor(cc, id); n != 0 {
			cc.t.Fatalf("参数不一致时不该触达工具网关，但审计表里有 %d 行", n)
		}
		cc.note("code=1016 msg=%s；单子状态 %s，工具网关审计 0 行（复核发生在网关之前）",
			truncate(msg, 80), row.Status)
	})

	run("AC-21", "过期单子不能再批准（code 1015，且不执行）", func(cc *caseCtx) {
		requireDB(cc)
		id := provisionApprovalFixture(t, db, approvalFixture{
			ExpiresIn: -2 * time.Minute,
			Reason:    "验收夹具：过期单子（写动作的合理性依赖当时现场）",
		})
		code, data, msg, err := approvalDecision("/agent/approval/approve", id, "来晚了")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code != 1015 {
			cc.t.Fatalf("过期后批准必须返回 1015，实际 code=%d msg=%q", code, truncate(msg, 200))
		}
		if asBool(data["executed"]) {
			cc.t.Fatalf("过期单子竟然执行了")
		}
		row := loadApprovalRow(t, db, id)
		if row.Status != "expired" {
			cc.t.Fatalf("过期单子应被惰性清理成 expired（「过期」与「执行失败」必须能区分），实际 %s", row.Status)
		}
		if row.ExecutedAt != 0 {
			cc.t.Fatalf("过期单子不得有执行时间戳：%+v", row)
		}
		if n := auditRowsFor(cc, id); n != 0 {
			cc.t.Fatalf("过期单子不得触达工具网关，但审计表里有 %d 行", n)
		}
		cc.note("code=1015 状态=%s 执行时间=%d 审计 0 行；msg=%s", row.Status, row.ExecutedAt, truncate(msg, 60))
	})

	run("AC-22", "驳回：不执行、留意见、状态落 rejected、二次决策冲突", func(cc *caseCtx) {
		requireDB(cc)
		id := provisionApprovalFixture(t, db, approvalFixture{Reason: "验收夹具：驳回路径"})
		code, data, msg, err := approvalDecision("/agent/approval/reject", id, "现场已恢复，不需要重跑")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code != 200 {
			cc.t.Fatalf("驳回是正常业务动作，应 code=200，实际 %d msg=%q", code, truncate(msg, 200))
		}
		if asBool(data["executed"]) {
			cc.t.Fatalf("驳回竟然执行了动作")
		}
		row := loadApprovalRow(t, db, id)
		if row.Status != "rejected" {
			cc.t.Fatalf("驳回后状态应为 rejected，实际 %s", row.Status)
		}
		if row.DecidedBy == "" || !strings.Contains(row.DecisionNote, "现场已恢复") {
			cc.t.Fatalf("审批意见必须原样落库（回看时要看理由）：%+v", row)
		}
		if n := auditRowsFor(cc, id); n != 0 {
			cc.t.Fatalf("驳回不得触达工具网关，但审计表里有 %d 行", n)
		}
		code2, _, msg2, err := approvalDecision("/agent/approval/reject", id, "再点一次")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code2 != 1014 {
			cc.t.Fatalf("已驳回的单子再次决策应 1014，实际 %d msg=%q", code2, truncate(msg2, 120))
		}
		cc.note("驳回 code=200 状态=%s 审批人=%s；二次决策 code=%d；网关审计 0 行",
			row.Status, row.DecidedBy, code2)
	})

	run("AC-23", "审批参数校验与撤销：不存在 1013、空单号 1001、已决策不可撤 1014", func(cc *caseCtx) {
		code, _, msg, err := approvalDecision("/agent/approval/approve", "ap-does-not-exist-acc", "")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code != 1013 {
			cc.t.Fatalf("不存在的单号应返回 1013，实际 %d msg=%q", code, truncate(msg, 160))
		}
		code2, _, msg2, err := approvalDecision("/agent/approval/approve", "", "")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code2 != 1001 {
			cc.t.Fatalf("空 approval_id 应返回 1001（参数错误），实际 %d msg=%q", code2, truncate(msg2, 160))
		}
		requireDB(cc)
		// 已决策（rejected）的单子不可撤销：撤销只对 pending 有意义。
		id := provisionApprovalFixture(t, db, approvalFixture{Status: "rejected", Reason: "验收夹具：已决策不可撤销"})
		code3, _, msg3, err := approvalDecision("/agent/approval/cancel", id, "撤回")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code3 != 1014 {
			cc.t.Fatalf("已决策的单子不可撤销，应返回 1014，实际 %d msg=%q", code3, truncate(msg3, 160))
		}
		// pending 的单子可以撤销。
		id2 := provisionApprovalFixture(t, db, approvalFixture{Reason: "验收夹具：待审批可撤销"})
		code4, data4, msg4, err := approvalDecision("/agent/approval/cancel", id2, "不用了")
		if err != nil {
			cc.t.Fatalf("%v", err)
		}
		if code4 != 200 {
			cc.t.Fatalf("pending 单子撤销应 code=200，实际 %d msg=%q", code4, truncate(msg4, 160))
		}
		if got := asStr(asMap(data4["approval"])["status"]); got != "canceled" {
			cc.t.Fatalf("撤销后状态应为 canceled，实际 %q", got)
		}
		if row := loadApprovalRow(t, db, id2); row.Status != "canceled" || row.ExecutedAt != 0 {
			cc.t.Fatalf("撤销后的库内状态不符：%+v", row)
		}
		cc.note("不存在单号 code=1013；空单号 code=1001；已决策不可撤 code=1014；pending 撤销 code=200")
	})

	run("AC-24", "审批接口不可匿名调用（谁批的必须能追到人）", func(cc *caseCtx) {
		anon := newClient(base)
		status, env, err := anon.do(http.MethodPost, "/agent/approval/approve",
			map[string]any{"approval_id": "ap-anonymous", "decided_by": "root"}, false)
		if err != nil {
			cc.t.Fatalf("请求失败: %v", err)
		}
		if status == http.StatusOK && env.Code == 200 {
			cc.t.Fatalf("匿名请求竟能批准审批单（HTTP %d/code %d）", status, env.Code)
		}
		st2, env2, err := anon.do(http.MethodPost, "/agent/approval/search", map[string]any{}, false)
		if err != nil {
			cc.t.Fatalf("请求失败: %v", err)
		}
		if st2 == http.StatusOK && env2.Code == 200 {
			cc.t.Fatalf("匿名请求竟能列出审批单（HTTP %d/code %d）", st2, env2.Code)
		}
		cc.note("匿名 approve：HTTP %d/code %d；匿名 search：HTTP %d/code %d",
			status, env.Code, st2, env2.Code)
	})

	run("AC-25", "审计查询接口：决策可解释、字段可渲染、审批单可回溯", func(cc *caseCtx) {
		data, err := c.postOK("/agent/audit/search", map[string]any{"page": 1, "page_size": 50})
		if err != nil {
			cc.t.Fatalf("查询审计失败: %v", err)
		}
		list := asSlice(data["list"])
		total := int(asNum(data["total"]))
		if total == 0 || len(list) == 0 {
			cc.skip("审计表当前没有事实行（本环境尚未发生过工具调用）：审计查询的字段契约无从核对")
		}
		known := map[string]bool{"allowed": true, "denied": true, "approval_required": true, "approval_executed": true}
		approvalLinked := 0
		for _, item := range list {
			m := asMap(item)
			if asStr(m["audit_id"]) == "" || asStr(m["session_id"]) == "" || asStr(m["tool"]) == "" {
				cc.t.Fatalf("审计行缺少可渲染的主键字段: %+v", m)
			}
			d := asStr(m["decision"])
			if !known[d] {
				cc.t.Fatalf("未知决策取值 %q（前端审计面板渲染不出来）", d)
			}
			if asStr(m["decision_text"]) == "" {
				cc.t.Fatalf("决策必须带中文解释（前端只渲染不推断）: %+v", m)
			}
			if asStr(m["risk"]) == "" {
				cc.t.Fatalf("审计行必须写明风险级别: %+v", m)
			}
			if d == "denied" || d == "approval_required" {
				if asStr(m["reason"]) == "" {
					cc.t.Fatalf("被拦/转审批的行必须写明是哪道闸拒的: %+v", m)
				}
			}
			if asStr(m["approval_id"]) != "" {
				approvalLinked++
			}
			if int64(asNum(m["created_ms"])) <= 0 {
				cc.t.Fatalf("审计行时间必须双口径非零: %+v", m)
			}
		}
		cc.note("total=%d，本页 %d 行全部字段自洽；其中带审批单号的行 %d 行（审批→执行可回溯）",
			total, len(list), approvalLinked)
	})

	// ---------- 阶段 4-A：Agent 记忆 / 经验库（AC-26…AC-29） ----------
	//
	// 这一段的取证策略与前四阶段不同：经验库的数据来源是"会话结束后由旁路沉淀"，
	// 而"模型这次调了哪些工具"是随机的。如果只靠跑一次真诊断来等数据，用例会时红时绿
	// （既可能因为没有工具调用而查不到行，也可能因为模型心情不同而条数不同）。
	//
	// 因此把"库里有行"和"这些行怎么被用"分开取证：
	//   - AC-26 看**真实沉淀**是否发生（数据来自真实会话，允许 SKIP 并写明原因）；
	//   - AC-27/28/29 用夹具（直接写库）+ 真实接口取证，保证可复跑、结果稳定。
	// 夹具只写 agent_memory 一张表，跑完按 memory_id 自删。

	// AC-26：真实诊断会话结束后，经验库里必须出现指向该会话的行。
	run("AC-26", "经验沉淀：真实诊断会话结束后产出「症状→动作→结果」条目", func(cc *caseCtx) {
		if db == nil {
			cc.skip("未配置 CRONY_TEST_MYSQL_DSN：无法核对经验行是否真的落库")
		}
		if lastSession == "" {
			cc.skip("本次套件没有成功的诊断会话（AC-07 未产出）：没有可核对的沉淀来源")
		}
		var cnt int
		if err := db.QueryRow("select count(*) from "+models.CronyAgentMemoryTableName+
			" where session_id = ?", lastSession).Scan(&cnt); err != nil {
			cc.t.Fatalf("按 session_id 统计经验行失败: %v", err)
		}
		if cnt == 0 {
			// 不判失败：诊断会话是否沉淀取决于它有没有产出结构化结论，
			// 而"模型这次没给结构化 JSON"属于模型行为。但这必须**说清楚**，
			// 否则"经验库一直空着"会被当成"功能正常"。
			cc.skip("会话 %s 没有沉淀任何经验行（可能该次结论未产出可解析的 JSON 结论）", lastSession)
		}
		// 行本身必须满足契约：症状非空、动作有类型、结果取值在册、计数至少 1。
		rows, err := db.Query("select memory_id, symptom, action_kind, outcome, occurrence_count, "+
			"success_count, fail_count, blocked_count, denied_count, advised_count, enabled "+
			"from "+models.CronyAgentMemoryTableName+" where session_id = ?", lastSession)
		if err != nil {
			cc.t.Fatalf("读取经验行失败: %v", err)
		}
		defer rows.Close()
		kindOK := map[string]bool{"tool": true, "advice": true}
		outcomeOK := map[string]bool{"success": true, "failed": true, "blocked": true, "denied": true, "advised": true}
		checked := 0
		for rows.Next() {
			var (
				id, symptom, kind, outcome string
				occ, sc, fc, bc, dc, ac    int
				enabled                    bool
			)
			if err := rows.Scan(&id, &symptom, &kind, &outcome, &occ, &sc, &fc, &bc, &dc, &ac, &enabled); err != nil {
				cc.t.Fatalf("扫描经验行失败: %v", err)
			}
			if strings.TrimSpace(id) == "" || !strings.HasPrefix(id, "mem-") {
				cc.t.Fatalf("经验行缺少可回溯的单号: %q", id)
			}
			if strings.TrimSpace(symptom) == "" {
				cc.t.Fatalf("经验行的症状为空（无法用于相似召回）: %s", id)
			}
			if !kindOK[kind] {
				cc.t.Fatalf("未知动作类型 %q（%s）", kind, id)
			}
			if !outcomeOK[outcome] {
				cc.t.Fatalf("未知结果取值 %q（%s）", outcome, id)
			}
			if occ != sc+fc+bc+dc+ac {
				cc.t.Fatalf("出现次数必须等于各结果计数之和: %s occ=%d s=%d f=%d b=%d d=%d a=%d",
					id, occ, sc, fc, bc, dc, ac)
			}
			if occ < 1 {
				cc.t.Fatalf("新沉淀的行出现次数至少为 1: %s", id)
			}
			if !enabled {
				cc.t.Fatalf("新沉淀的经验默认必须是启用状态（否则沉淀了却不参与召回）: %s", id)
			}
			checked++
		}
		if err := rows.Err(); err != nil {
			cc.t.Fatalf("遍历经验行失败: %v", err)
		}
		// 控制面也必须能看到同一批数据（接口与库不能是两套事实）。
		apiData, err := c.postOK("/agent/memory/search", map[string]any{"session_id": lastSession, "page": 1, "page_size": 20})
		if err != nil {
			cc.t.Fatalf("按会话查经验失败: %v", err)
		}
		if got := int(asNum(apiData["total"])); got != cnt {
			cc.t.Fatalf("接口与库口径不一致：库 %d 行 / 接口 %d 行", cnt, got)
		}
		src := asMap(apiData["source"])
		if !asBool(src["advisory_only"]) || asBool(src["usable_as_evidence"]) || asBool(src["editable"]) {
			cc.t.Fatalf("经验来源必须自述「仅线索、不可编辑」: %+v", src)
		}
		cc.note("会话 %s 沉淀 %d 条经验，逐行核对症状/动作类型/结果取值/计数自洽，接口与库口径一致",
			lastSession, checked)
	})

	// AC-27：召回命中相关经验；停用后必须**不再**出现在召回结果里。
	run("AC-27", "召回与停用：相似症状能命中，被停用的经验立刻退出召回", func(cc *caseCtx) {
		if db == nil {
			cc.skip("未配置 CRONY_TEST_MYSQL_DSN：无法造召回夹具")
		}
		fx := provisionMemoryFixture(t, db, memoryFixture{
			// 症状里带唯一标记，保证命中一定来自本夹具而不是历史数据。
			Symptom:    "验收夹具症状 端口 13309 未监听 服务未启动",
			ActionText: "node_probe port=13309",
			Tool:       "node_probe",
			Outcome:    "failed",
			Fail:       2,
			TargetNode: nodeUUID,
		})
		query := "验收夹具症状 端口 13309 未监听"

		hit := func() map[string]any {
			data, err := c.postOK("/agent/memory/recall", map[string]any{
				"symptom": query, "agent_kind": "diagnose", "target_node": nodeUUID, "top_k": 5,
			})
			if err != nil {
				cc.t.Fatalf("召回请求失败: %v", err)
			}
			// 接口契约：召回结果只能当线索。
			if !asBool(data["advisory_only"]) || asBool(data["usable_as_evidence"]) || asStr(data["notice"]) == "" {
				cc.t.Fatalf("召回接口必须自述「仅线索、不可作证据」: advisory=%v usable=%v notice=%q",
					data["advisory_only"], data["usable_as_evidence"], asStr(data["notice"]))
			}
			if tokens := asSlice(data["query_tokens"]); len(tokens) == 0 {
				cc.t.Fatalf("召回必须回报查询词元（否则「为什么没召回」无法排查）: %+v", data)
			}
			for _, it := range asSlice(data["hits"]) {
				if asStr(asMap(it)["memory_id"]) == fx.MemoryID {
					return asMap(it)
				}
			}
			return nil
		}

		h := hit()
		if h == nil {
			cc.t.Fatalf("相关经验未被召回（memory_id=%s）：症状=%q", fx.MemoryID, query)
		}
		if asStr(h["outcome_text"]) == "" {
			cc.t.Fatalf("命中必须带中文结果解释: %+v", h)
		}
		if len(asSlice(h["reasons"])) == 0 || len(asSlice(h["matched"])) == 0 {
			cc.t.Fatalf("命中必须可解释（matched/reasons 均非空）: %+v", h)
		}
		if !asBool(h["is_failed"]) {
			cc.t.Fatalf("这条经验的结果是 failed，is_failed 必须为 true（否则前端会把它渲染成成功）: %+v", h)
		}

		// 停用必须写原因：空原因被拒，且状态不变。
		status, env, err := c.do(http.MethodPost, "/agent/memory/disable",
			map[string]any{"memory_id": fx.MemoryID, "reason": "   "}, true)
		if err != nil {
			cc.t.Fatalf("停用请求失败: %v", err)
		}
		if status != http.StatusOK || env.Code == 200 {
			cc.t.Fatalf("无原因的停用必须被拒（HTTP %d/code %d）", status, env.Code)
		}
		if !fx.EnabledNow(t, db) {
			cc.t.Fatal("被拒的停用竟然改动了状态")
		}

		// 正常停用 → 立刻退出召回。
		_, err = c.postOK("/agent/memory/disable", map[string]any{
			"memory_id": fx.MemoryID, "reason": "验收：验证停用后不再影响模型",
		})
		if err != nil {
			cc.t.Fatalf("停用失败: %v", err)
		}
		if h2 := hit(); h2 != nil {
			cc.t.Fatalf("被停用的经验仍然被召回（撤下动作是假的）: %+v", h2)
		}
		// 停用审计三件事必须落库（谁/为什么/何时）。
		var by, reason string
		var at int64
		if err := db.QueryRow("select disabled_by, disabled_reason, disabled_at from "+
			models.CronyAgentMemoryTableName+" where memory_id = ?", fx.MemoryID).Scan(&by, &reason, &at); err != nil {
			cc.t.Fatalf("读停用审计失败: %v", err)
		}
		if strings.TrimSpace(by) == "" || strings.TrimSpace(reason) == "" || at <= 0 {
			cc.t.Fatalf("停用必须记录 谁/为什么/何时：by=%q reason=%q at=%d", by, reason, at)
		}

		// 重新启用 → 恢复召回，且停用审计字段被清空。
		if _, err := c.postOK("/agent/memory/enable", map[string]any{"memory_id": fx.MemoryID}); err != nil {
			cc.t.Fatalf("启用失败: %v", err)
		}
		if hit() == nil {
			cc.t.Fatal("重新启用后应恢复召回")
		}
		if err := db.QueryRow("select disabled_by, disabled_reason, disabled_at from "+
			models.CronyAgentMemoryTableName+" where memory_id = ?", fx.MemoryID).Scan(&by, &reason, &at); err != nil {
			cc.t.Fatalf("读启用后的状态失败: %v", err)
		}
		if by != "" || reason != "" || at != 0 {
			cc.t.Fatalf("重新启用必须清空停用审计字段: by=%q reason=%q at=%d", by, reason, at)
		}
		cc.note("夹具 %s：召回命中且可解释 → 无原因停用被拒 → 停用后退出召回（审计三件事落库）→ 启用后恢复召回",
			fx.MemoryID)
	})

	// AC-28：经验绝不能变成"本次证据"。
	run("AC-28", "经验不是证据：接口声明不可作证据，会话注入的是「历史提示」而非观测", func(cc *caseCtx) {
		// (1) 接口层：召回与列表都必须把这条约束写在返回值里，而不是只写在文档里。
		recallData, err := c.postOK("/agent/memory/recall", map[string]any{
			"symptom": "验收：经验是否会被当成证据", "top_k": 1,
		})
		if err != nil {
			cc.t.Fatalf("召回请求失败: %v", err)
		}
		if !asBool(recallData["advisory_only"]) || asBool(recallData["usable_as_evidence"]) {
			cc.t.Fatalf("召回接口未声明「仅线索」: %+v", recallData)
		}
		listData, err := c.postOK("/agent/memory/search", map[string]any{"page": 1, "page_size": 5})
		if err != nil {
			cc.t.Fatalf("经验列表请求失败: %v", err)
		}
		src := asMap(listData["source"])
		if !asBool(src["advisory_only"]) || asBool(src["usable_as_evidence"]) {
			cc.t.Fatalf("经验列表来源块未声明「仅线索」: %+v", src)
		}
		if !asBool(src["disable_requires_reason"]) {
			cc.t.Fatalf("经验库必须声明「停用需写原因」（治理口径）: %+v", src)
		}

		// (2) 运行时层：真的被注入过经验的那次会话，必须是「注入条款 + 明确的免责声明」，
		//     且其证据里不能出现经验条目号（出现即说明经验被当成了观测）。
		if db == nil {
			cc.skip("接口层已核对通过；未配置 CRONY_TEST_MYSQL_DSN，无法核对运行时注入的那一侧")
		}
		var sessionID, promptVersion, warningsJSON, traceJSON, resultJSON string
		err = db.QueryRow("select session_id, prompt_version, warnings_json, trace_json, result_json from "+
			models.CronyAgentSessionTableName+
			" where kind = 'diagnose' and result_json like '%\"memory_used\"%'"+
			" order by created desc limit 1").Scan(&sessionID, &promptVersion, &warningsJSON, &traceJSON, &resultJSON)
		if err != nil {
			cc.skip("接口层已核对通过；库里还没有「用过历史经验」的诊断会话（模型是否召回取决于症状相似度），" +
				"运行时注入这一侧本次没有观测到（单测 diagnose_memory_test.go 覆盖了守门逻辑）")
		}
		if promptVersion != "diagnose-v5" {
			cc.t.Fatalf("注入经验的会话必须记录为 diagnose-v5（提示词版本是复盘依据），实际 %q", promptVersion)
		}
		if !strings.Contains(warningsJSON, "历史经验") {
			cc.t.Fatalf("注入经验时必须留下「历史经验不是本次证据」的警示，实际 warnings=%s", truncate(warningsJSON, 300))
		}
		if strings.Contains(traceJSON, "mem-") && !strings.Contains(warningsJSON, "未观测") {
			cc.t.Fatalf("轨迹里出现经验条目号却没有「未观测」警示（经验可能被当成了证据）: trace 片段=%s",
				truncate(traceJSON, 300))
		}
		cc.note("接口层：recall/list 均声明 advisory_only=true & usable_as_evidence=false；"+
			"运行时：会话 %s 记为 %s 且带「历史经验不是证据」警示", sessionID, promptVersion)
	})

	// AC-29：统计口径自洽（成功率的分母只含真正执行过的动作）。
	run("AC-29", "经验统计：分组与总数自洽，成功率分母只含执行过的动作", func(cc *caseCtx) {
		data, err := c.postOK("/agent/memory/stats", map[string]any{})
		if err != nil {
			cc.t.Fatalf("统计请求失败: %v", err)
		}
		total := int(asNum(data["total"]))
		enabled := int(asNum(data["enabled"]))
		disabled := int(asNum(data["disabled"]))
		if enabled+disabled != total {
			cc.t.Fatalf("enabled(%d)+disabled(%d) 应等于 total(%d)", enabled, disabled, total)
		}
		byOutcome := asMap(data["by_outcome"])
		sum := 0
		for _, v := range byOutcome {
			sum += int(asNum(v))
		}
		if sum != total {
			cc.t.Fatalf("by_outcome 之和 %d 应等于 total %d", sum, total)
		}
		successSum := int(asNum(data["success_sum"]))
		failSum := int(asNum(data["fail_sum"]))
		if successSum+int(asNum(byOutcome["success"])) < successSum {
			cc.t.Fatalf("成功率分子与分组计数不自洽: %+v", data)
		}
		if asBool(data["success_rate_known"]) {
			want := float64(successSum) / float64(successSum+failSum)
			if got := asNum(data["success_rate"]); got < want-1e-9 || got > want+1e-9 {
				cc.t.Fatalf("成功率应等于 成功/(成功+失败)=%.6f，实际 %.6f", want, got)
			}
			if successSum+failSum == 0 {
				cc.t.Fatalf("声明成功率已知时，分母不能是 0: %+v", data)
			}
		}
		def := asMap(data["definition"])
		if asStr(def["success_rate"]) == "" || len(asSlice(def["excluded"])) == 0 {
			cc.t.Fatalf("统计必须自述口径（否则「成功率」只是个数字）: %+v", data)
		}
		// 与库对账：总量必须一致（接口与库不能是两套事实）。
		if db != nil {
			var dbTotal int
			if err := db.QueryRow("select count(*) from " + models.CronyAgentMemoryTableName).Scan(&dbTotal); err != nil {
				cc.t.Fatalf("按库统计经验总数失败: %v", err)
			}
			if dbTotal != total {
				cc.t.Fatalf("统计接口 total=%d 与库实际 %d 不一致", total, dbTotal)
			}
		}
		cc.note("total=%d（enabled %d / disabled %d），by_outcome 分组和=%d，成功 %d / 失败 %d，口径自述齐全",
			total, enabled, disabled, sum, successSum, failSum)
	})
}
