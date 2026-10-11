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

	run("AC-03", "工具能力面：只读白名单与配额来自后端注册表", func(cc *caseCtx) {
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
				cc.t.Fatalf("工具 %s 的 readonly 不是 true：阶段 2/3 不应注册写工具", name)
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
		cc.note("%d 个工具（全部只读）: %s；max_calls_per_session=%v max_output_bytes=%v sql_max_rows=%v",
			len(names), strings.Join(names, ","),
			policy["max_calls_per_session"], policy["max_output_bytes"], policy["sql_max_rows"])
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
}
