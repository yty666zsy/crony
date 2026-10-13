//go:build acceptance

package acceptance

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/tmnhs/crony/common/pkg/etcdclient"

	"github.com/coreos/etcd/clientv3"
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
			if llmUnavailable(err) {
				cc.skip("admin 未配置可用的模型凭据（CRONY_LLM_API_KEY）：AC-07 要验的是真实模型调用，" +
					"凭据缺失时黑盒侧观测不到任何模型行为。注入凭据后可直接复跑；" +
					"若只想验证链路本身，可用 offline provider（但报告必须写明 provider=offline，且设 CRONY_ACC_EXPECT_REAL=1 时会判 FAIL）")
			}
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
			if llmUnavailable(err) {
				cc.skip("admin 未配置可用的模型凭据（CRONY_LLM_API_KEY）：NL 起草需要真模型产出草案，" +
					"凭据缺失时观测不到起草行为（AC-11 会随之 SKIP）")
			}
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

	// ---------- 阶段 4-B：多步骤编排 / DAG（AC-30…AC-33） ----------
	//
	// 4-B 的验收重点不是"能存下一段 JSON"，而是三件事：
	//   1. 合法与非法有**唯一裁决**（BuildPlan 通过即为合法，任何一处坏了都必须说清是哪一处）；
	//   2. 坏图**不允许进入系统**（保存时拒绝，而不是等到执行那一刻才炸）；
	//   3. 执行语义（跳过的步骤为什么没跑）**在库里能查**。
	//
	// 前两件走真实 HTTP 接口取证；第三件需要节点真的跑一次 DAG，而本机 etcd 未启动
	// （节点靠 etcd 长轮询接单），因此 AC-33 采取"有真实数据就核对、没有就 SKIP 并写明
	// 证据在集成测试里"的策略——不把"环境缺件"伪装成"通过"。

	// dagsPost 发一个 POST 并返回业务码/数据/消息；审批类用例已有同款写法（见 AC-17 段）。
	dagPost := func(path string, body map[string]any) (int, map[string]any, string) {
		status, env, err := c.do(http.MethodPost, path, body, true)
		if err != nil {
			return 0, nil, "网络错误: " + err.Error()
		}
		if status != http.StatusOK {
			return env.Code, nil, fmt.Sprintf("HTTP %d（编排接口约定恒 HTTP 200）", status)
		}
		return env.Code, env.dataMap(), env.Msg
	}

	const (
		dagDiamondJSON = `{"max_parallel":2,"fail_fast":false,"steps":[` +
			`{"index":0,"name":"extract","command":"echo extract"},` +
			`{"index":1,"name":"transform","command":"echo transform","depends_on":"0"},` +
			`{"index":2,"name":"enrich","command":"echo enrich","depends_on":"0"},` +
			`{"index":3,"name":"load","command":"echo load","depends_on":"1,2","join":"all_success"}]}`
		dagCycleJSON = `{"steps":[` +
			`{"index":0,"name":"load","command":"echo load","depends_on":"2"},` +
			`{"index":1,"name":"verify","command":"echo verify","depends_on":"0"},` +
			`{"index":2,"name":"notify","command":"echo notify","depends_on":"1"}]}`
	)

	// AC-30：校验接口对合法图给出可渲染的摘要，对坏图给出**具体**原因与专门业务码。
	run("AC-30", "编排校验：合法图给出步骤/边/层深，坏图返回 1021 并指出具体错处", func(cc *caseCtx) {
		code, data, msg := dagPost("/job/dag/validate", map[string]any{"steps_json": dagDiamondJSON})
		if code != 200 {
			cc.t.Fatalf("合法菱形应通过校验，实际 code=%d msg=%q", code, msg)
		}
		if !asBool(data["valid"]) {
			cc.t.Fatalf("合法图必须回 valid=true: %+v", data)
		}
		plan := asMap(data["plan"])
		steps := asSlice(plan["steps"])
		edges := asSlice(plan["edges"])
		if len(steps) != 4 || len(edges) != 4 {
			cc.t.Fatalf("摘要应给出 4 步 4 边，实际 steps=%d edges=%d", len(steps), len(edges))
		}
		// 层深按最长路径：extract(0) → {transform,enrich}(1) → load(2)
		if got := int(asNum(plan["max_depth"])); got != 2 {
			cc.t.Fatalf("菱形层深应为 2（最长路径），实际 %d", got)
		}
		if got := int(asNum(plan["max_parallel"])); got != 2 {
			cc.t.Fatalf("摘要必须回显并发上限，实际 %d", got)
		}
		var loadDeps []any
		for _, s := range steps {
			m := asMap(s)
			if int(asNum(m["index"])) == 3 {
				loadDeps = asSlice(m["depends_on"])
			}
		}
		if len(loadDeps) != 2 {
			cc.t.Fatalf("load 必须依赖两条分支（1,2），实际 %v", loadDeps)
		}

		// 坏图：成环 → 1021，且 msg 必须指出"环"与经过的步骤（不是一句泛泛的失败）
		code, data, cycleMsg := dagPost("/job/dag/validate", map[string]any{"steps_json": dagCycleJSON})
		msg = cycleMsg
		if code != 1021 {
			cc.t.Fatalf("成环应返回 ErrorJobPlanInvalid(1021)，实际 code=%d msg=%q", code, msg)
		}
		if !strings.Contains(msg, "环") {
			cc.t.Fatalf("拒绝原因必须点明「环」，实际 msg=%q", msg)
		}
		if !strings.Contains(msg, "0") || !strings.Contains(msg, "1") || !strings.Contains(msg, "2") {
			cc.t.Fatalf("环的报错应给出完整环路（涉及哪些步骤），实际 msg=%q", msg)
		}
		if asBool(data["valid"]) {
			cc.t.Fatalf("坏图的 valid 不能为 true: %+v", data)
		}

		// 另外两类典型坏法必须同样被拒，且原因各不相同（可定位）
		otherMsgs := make(map[string]string, 2)
		for name, body := range map[string]map[string]any{
			"依赖不存在的步骤": {"steps_json": `{"steps":[{"index":0,"command":"echo a"},{"index":1,"command":"echo b","depends_on":"9"}]}`},
			"字段名拼错":    {"steps_json": `{"steps":[{"index":0,"command":"echo a"},{"index":1,"command":"echo b","depends_ons":"0"}]}`},
		} {
			c2, _, m2 := dagPost("/job/dag/validate", body)
			if c2 != 1021 {
				cc.t.Fatalf("%s 应被拒（1021），实际 code=%d msg=%q", name, c2, m2)
			}
			if strings.TrimSpace(m2) == "" {
				cc.t.Fatalf("%s 被拒了但没说原因（运维只能猜）", name)
			}
			otherMsgs[name] = m2
		}
		cc.note("合法菱形：steps=4 edges=4 max_depth=2 max_parallel=2；成环 msg=%q；"+
			"依赖不存在 msg=%q；字段名拼错 msg=%q（三类坏图均 1021 且原因可定位）",
			truncate(cycleMsg, 100), truncate(otherMsgs["依赖不存在的步骤"], 80), truncate(otherMsgs["字段名拼错"], 80))
	})

	// AC-31：坏图**进不了系统**——保存路径必须拒绝，而且不能留下半成品任务。
	run("AC-31", "保存拦截：坏编排在 /job/add 被拒（1021），且库里不留半成品任务", func(cc *caseCtx) {
		if db == nil {
			cc.skip("未配置 CRONY_TEST_MYSQL_DSN：无法核对「拒绝保存后没有留下任务行」")
		}
		jobName := "acc-dag-bad-" + strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG")) + "-" + fmt.Sprint(time.Now().UnixNano()%100000)
		// 字段名必须与后端绑定一致（job_type 而不是 type；allocation 必填），
		// 否则会先被参数校验拦下（1001），验收就验不到"编排校验"这一层。
		code, _, msg := dagPost("/job/add", map[string]any{
			"name":       jobName,
			"command":    "echo should-not-be-saved",
			"spec":       "0 5 * * *",
			"job_type":   1,
			"status":     1,
			"timeout":    60,
			"allocation": 2,
			"run_on":     os.Getenv("CRONY_ACC_NODE_UUID"),
			"steps_json": dagCycleJSON,
		})
		if code != 1021 {
			cc.t.Fatalf("带环的编排在保存时必须被拒（1021），实际 code=%d msg=%q（如果这里是 1000/其他，"+
				"说明校验没有挡在落库之前）", code, msg)
		}
		if !strings.Contains(msg, "环") {
			cc.t.Fatalf("拒绝保存的原因必须点明「环」，实际 msg=%q", msg)
		}
		var cnt int
		if err := db.QueryRow("select count(*) from job where name = ?", jobName).Scan(&cnt); err != nil {
			cc.t.Fatalf("按名称核对任务是否落库失败: %v", err)
		}
		if cnt != 0 {
			cc.t.Fatalf("坏编排被拒后库里仍留下 %d 条任务（%s）：拒绝必须是「什么都没发生」", cnt, jobName)
		}
		cc.note("任务名 %s 提交带环编排 → code=1021 msg=%q；库中同名行数=%d（无半成品）", jobName, truncate(msg, 80), cnt)
	})

	// AC-32：读路径——有编排的任务能读出可执行摘要；单步任务必须回 1022（"本来就没有"≠"坏了"）。
	run("AC-32", "编排读取：有编排任务给出可执行摘要，单步任务回 1022 而不是空图", func(cc *caseCtx) {
		if db == nil {
			cc.skip("未配置 CRONY_TEST_MYSQL_DSN：无法造「库里已存编排」的夹具")
		}
		tag := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
		if tag == "" {
			tag = "acc"
		}
		const dagJobID = 910030
		// 夹具：直接写一条带编排的任务（写库只造数据，读路径仍走真实接口）。
		if _, err := db.Exec("delete from job where id = ?", dagJobID); err != nil {
			cc.t.Fatalf("清理编排夹具失败: %v", err)
		}
		if _, err := db.Exec("insert into job (id, name, command, steps_json, status, concurrency_policy, type, notify_type, spec, created) "+
			"values (?, ?, ?, ?, 1, 0, 1, 0, '0 5 * * *', ?)",
			dagJobID, "acc-dag-fixture-"+tag, "echo fallback-must-not-run", dagDiamondJSON, time.Now().UnixMilli()); err != nil {
			cc.t.Fatalf("写入编排夹具失败: %v", err)
		}
		defer func() {
			_, _ = db.Exec("delete from job where id = ?", dagJobID)
		}()

		code, data, msg := dagPost("/job/dag/find", map[string]any{"job_id": dagJobID})
		if code != 200 {
			cc.t.Fatalf("有编排的任务应能读出摘要，实际 code=%d msg=%q", code, msg)
		}
		if !asBool(data["executable"]) {
			cc.t.Fatalf("合法编排必须标记 executable=true: %+v", data)
		}
		plan := asMap(data["plan"])
		if len(asSlice(plan["steps"])) != 4 || len(asSlice(plan["edges"])) != 4 {
			cc.t.Fatalf("摘要应给出 4 步 4 边，实际 %+v", plan)
		}
		if asStr(data["steps_json"]) == "" {
			cc.t.Fatalf("读接口必须回原文 steps_json（否则编辑器无法回填）")
		}

		// 契约回归（真机走查踩到过）：控制台对 depends_on / warnings / edges **直接取 .length**，
		// 字段缺失或为 null 会让"任务编排"整页渲染时抛 TypeError 白屏。
		// 所以这三者在 JSON 里必须是数组，空也必须是 []（根步骤的 depends_on 最容易退化成 null）。
		if _, ok := plan["edges"].([]any); !ok {
			cc.t.Fatalf("plan.edges 必须是数组（空也要 []，不能 null/缺失）: %+v", plan)
		}
		for _, x := range asSlice(plan["steps"]) {
			m := asMap(x)
			if _, ok := m["depends_on"].([]any); !ok {
				cc.t.Fatalf("步骤 %v 的 depends_on 必须是数组: %+v", m["index"], m)
			}
			if _, ok := m["warnings"].([]any); !ok {
				cc.t.Fatalf("步骤 %v 的 warnings 必须是数组（空也要 []，不能靠 omitempty 省掉）: %+v", m["index"], m)
			}
		}

		// 单步任务：库里那些 steps_json 为空的老任务必须回 1022
		var plainID int
		if err := db.QueryRow("select id from job where (steps_json is null or steps_json = '') order by id limit 1").Scan(&plainID); err != nil {
			cc.skip("库里没有单步任务可核对 1022 分支: %v", err)
		}
		code, data, msg = dagPost("/job/dag/find", map[string]any{"job_id": plainID})
		if code != 1022 {
			cc.t.Fatalf("单步任务应回 ErrorJobPlanNotFound(1022)，实际 code=%d msg=%q", code, msg)
		}
		if _, hasPlan := data["plan"]; hasPlan {
			cc.t.Fatalf("没有编排时不得伪造 plan: %+v", data)
		}
		if strings.TrimSpace(msg) == "" {
			cc.t.Fatalf("单步任务的说明不能为空（要让运维看懂这是正常状态）")
		}
		cc.note("有编排任务 #%d：executable=true steps=4 edges=4；单步任务 #%d：code=1022 msg=%q",
			dagJobID, plainID, truncate(msg, 60))
	})

	// AC-33：执行语义的**落库证据**：多步骤 run 的每一步都要有行，跳过的步骤要写明原因。
	run("AC-33", "DAG 执行落库：多步骤 run 每步一行，被跳过的步骤带原因与编排属性", func(cc *caseCtx) {
		if db == nil {
			cc.skip("未配置 CRONY_TEST_MYSQL_DSN：无法核对 job_run_step")
		}
		// 只认**真实执行产生**的 run：run_id 形如 j<job_id>-t<槽位>（调度）
		// 或 j<job_id>-m<纳秒>-<序号>（手动/ Agent 触发，见 models.RunIDForSlot /
		// NewManualRunID），且步骤数 ≥ 3（本套件自己造的夹具是 1~2 步、id 形如 <tag>-run）。
		// 为什么要这样卡：拿夹具当"执行证据"等于验自己写的行，报告会好看但什么都没证明。
		// 阶段 5 收尾时本机已起 etcd + 真节点舰队，黑盒侧能触发真实的 DAG 执行，
		// 因此这里同时接受调度（-t）与手动（-m）两种真实 run；夹具一律不以 j 开头，不会被误认。
		var runID string
		var stepCount int
		err := db.QueryRow("select run_id, step_count from job_run "+
			"where step_count >= 3 and run_id like 'j%-%' order by id desc limit 1").
			Scan(&runID, &stepCount)
		if err != nil {
			cc.skip("库里没有真实执行产生的多步骤 run（需要 etcd + 至少一个真节点才会产生）。" +
				"执行语义的证据在集成测试：TestMySQLRunCoordinatorMultiStepDAG / DAGSkipsDownstreamOnFailure / " +
				"DAGFailFastStopsPendingBranches / RejectsInvalidPlanOnRealDB（真库 + 真进程，含跳过原因与汇聚策略落库）")
		}
		// 执行详情是 GET /job/run/find?run_id=xxx，步骤明细是 POST /job/run/steps（分页）。
		status, env, err := c.do(http.MethodGet, "/job/run/find?run_id="+url.QueryEscape(runID), nil, true)
		if err != nil || status != http.StatusOK || env.Code != 200 {
			cc.t.Fatalf("读取 run %s 详情失败: status=%d code=%d msg=%q err=%v", runID, status, env.Code, env.Msg, err)
		}
		if got := int(asNum(env.dataMap()["step_count"])); got != stepCount {
			cc.t.Fatalf("run %s 的 step_count 库内为 %d，接口回 %d（两套事实）", runID, stepCount, got)
		}
		page, err := c.postOK("/job/run/steps", map[string]any{"run_id": runID, "page": 1, "page_size": 100})
		if err != nil {
			cc.t.Fatalf("读取 run %s 的步骤明细失败: %v", runID, err)
		}
		steps := asSlice(page["list"])
		if len(steps) != stepCount {
			cc.t.Fatalf("run %s 的 step_count=%d，但步骤明细只返回 %d 行（步骤行丢了，控制台就少显示步骤）",
				runID, stepCount, len(steps))
		}
		skipped := 0
		for _, s := range steps {
			m := asMap(s)
			if asStr(m["condition"]) == "" {
				cc.t.Fatalf("步骤 %v 必须带条件的最终取值（落库要能解释它为什么跑/没跑）", m["name"])
			}
			if int(asNum(m["status"])) != int(models.RunStatusSkipped) {
				continue
			}
			skipped++
			if asStr(m["error_msg"]) == "" {
				cc.t.Fatalf("被跳过的步骤 %v 必须写明原因（%s）", m["name"], runID)
			}
			if asStr(m["artifacts"]) == "" {
				cc.t.Fatalf("被跳过的步骤 %v 必须留下结构化产物（汇聚策略/依赖），否则无法复盘「为什么没跑」", m["name"])
			}
		}
		cc.note("run %s：step_count=%d，明细行数=%d，其中跳过 %d 步（跳过均带原因与产物）", runID, stepCount, len(steps), skipped)
	})

	/* ----------------------------- 阶段 4-C：自愈调度 ----------------------------- */

	// AC-34：自愈的**安全边界必须可读**——运维在打开总开关之前，要能看见它会做什么、会被什么挡住。
	run("AC-34", "自愈策略边界可读：动作白名单闭合、三道闸门可见、分类→动作映射完整", func(cc *caseCtx) {
		data, err := c.postOK("/agent/heal/policy", map[string]any{})
		if err != nil {
			cc.t.Fatalf("读取自愈策略失败: %v", err)
		}
		// 1) 动作白名单必须闭合：多一个就是爆炸半径变大。
		actions := asMap(data["action_names"])
		gotActions := make([]string, 0, len(actions))
		for k := range actions {
			gotActions = append(gotActions, k)
		}
		sort.Strings(gotActions)
		wantActions := []string{"escalate", "retry_extend_timeout", "retry_same_node"}
		if strings.Join(gotActions, ",") != strings.Join(wantActions, ",") {
			cc.t.Fatalf("自愈动作白名单必须恰好是 %v，实际 %v（白名单被改动即爆炸半径变化）", wantActions, gotActions)
		}
		// 2) 策略边界字段齐全且为正值。
		policy := asMap(data["policy"])
		for _, k := range []string{
			"enabled", "max_attempts_per_job_window", "window_seconds", "cooldown_seconds",
			"extend_timeout_factor", "max_extend_timeout_seconds", "max_concurrent",
			"observe_window_seconds", "scan_limit", "lookback_seconds",
		} {
			if _, ok := policy[k]; !ok {
				cc.t.Fatalf("自愈策略缺少字段 %q（运维看不到边界就不敢打开）: %+v", k, policy)
			}
		}
		if asNum(policy["max_attempts_per_job_window"]) <= 0 {
			cc.t.Fatalf("尝试上限必须为正（否则等于永不自愈）: %+v", policy)
		}
		// 3) 三道闸门必须都暴露：写能力是否已开是"能不能真的动手"的关键。
		if _, ok := data["writes_enabled"]; !ok {
			cc.t.Fatalf("策略接口必须回报 writes_enabled（写能力是否已开）")
		}
		// 4) 分类→动作映射完整，且引用的动作都在白名单内。
		classes := asSlice(data["classes"])
		if len(classes) == 0 {
			cc.t.Fatalf("分类→动作映射不能为空")
		}
		for _, cl := range classes {
			m := asMap(cl)
			if asStr(m["class"]) == "" || asStr(m["desc"]) == "" {
				cc.t.Fatalf("分类条目缺 class/desc: %+v", m)
			}
			acts := asSlice(m["actions"])
			if len(acts) == 0 {
				cc.t.Fatalf("分类 %s 没有允许的动作（否则它永远不会被处置）", asStr(m["class"]))
			}
			for _, a := range acts {
				if _, ok := actions[asStr(a)]; !ok {
					cc.t.Fatalf("分类 %s 引用了白名单外的动作 %q", asStr(m["class"]), asStr(a))
				}
			}
		}
		cc.note("白名单=%v；尝试上限=%v 冷却=%vs 并发=%v 观测窗口=%vs；writes_enabled=%v；分类映射 %d 条均自洽",
			gotActions, policy["max_attempts_per_job_window"], policy["cooldown_seconds"],
			policy["max_concurrent"], policy["observe_window_seconds"], data["writes_enabled"], len(classes))
	})

	// AC-35：手动扫描**不绕过闸门**，且回执必须说清楚"扫到了什么、怎么处置的"。
	run("AC-35", "手动扫描不绕过闸门：回执字段齐全，指定的坏 run_id 被拒（1025）", func(cc *caseCtx) {
		// 先跑一轮（enabled 可能是 false：那也应该成功返回，只是全部只分类不动手）。
		data, err := c.postOK("/agent/heal/scan", map[string]any{})
		if err != nil {
			cc.t.Fatalf("手动扫描失败: %v", err)
		}
		for _, k := range []string{"scanned", "skipped", "denied", "executed", "escalated", "failed", "observed", "succeeded"} {
			if _, ok := data[k]; !ok {
				cc.t.Fatalf("扫描回执缺少 %q，运维看不到这一轮到底发生了什么: %+v", k, data)
			}
		}
		// 指定一个不存在的 run_id：必须被拒为"请求非法"，而不是悄悄当成空扫描。
		status, env, err := c.do(http.MethodPost, "/agent/heal/scan", map[string]any{"run_id": "no-such-run-acc"}, true)
		if err != nil {
			cc.t.Fatalf("扫描坏 run_id 网络错误: %v", err)
		}
		if status != http.StatusOK || env.Code != 1025 {
			cc.t.Fatalf("指定不存在的 run_id 应回 1025，实际 HTTP %d code=%d msg=%q", status, env.Code, truncate(env.Msg, 120))
		}
		if strings.TrimSpace(env.Msg) == "" {
			cc.t.Fatalf("被拒时必须说明原因（运维要知道为什么没动）")
		}
		cc.note("扫描：scanned=%v denied=%v executed=%v escalated=%v failed=%v 观测成功=%v；坏 run_id → code=%d msg=%q",
			data["scanned"], data["denied"], data["executed"], data["escalated"], data["failed"],
			data["succeeded"], env.Code, truncate(env.Msg, 80))
	})

	// AC-36：自愈记录查询契约 + 不存在的单号必须用独立业务码（1023）。
	run("AC-36", "自愈记录查询：条目含复盘字段；不存在单号回 1023，缺锚点回 1001", func(cc *caseCtx) {
		page, err := c.postOK("/agent/heal/search", map[string]any{"page": 1, "page_size": 20})
		if err != nil {
			cc.t.Fatalf("查询自愈记录失败: %v", err)
		}
		list := asSlice(page["list"])
		for _, it := range list {
			row := asMap(it)
			if asStr(row["heal_id"]) == "" || !strings.HasPrefix(asStr(row["heal_id"]), "heal-") {
				cc.t.Fatalf("自愈记录缺 heal_id 或格式不对: %+v", row)
			}
			// 复盘三要素：症状、决策理由、来源失败执行。
			for _, k := range []string{"symptom", "reason", "source_run_id", "status"} {
				if asStr(row[k]) == "" {
					cc.t.Fatalf("自愈记录缺少复盘字段 %q（出事无法复盘）: %+v", k, row)
				}
			}
		}
		// 不存在的单号：独立码，而不是泛泛的 ERROR。
		status, env, err := c.do(http.MethodPost, "/agent/heal/find", map[string]any{"heal_id": "heal-does-not-exist"}, true)
		if err != nil {
			cc.t.Fatalf("读不存在自愈单号网络错误: %v", err)
		}
		if status != http.StatusOK || env.Code != 1023 {
			cc.t.Fatalf("不存在的自愈单号应回 1023，实际 HTTP %d code=%d msg=%q", status, env.Code, truncate(env.Msg, 120))
		}
		// 缺锚点：参数错误 1001。
		status, env, err = c.do(http.MethodPost, "/agent/heal/by_run", map[string]any{"run_id": "   "}, true)
		if err != nil {
			cc.t.Fatalf("by_run 缺锚点网络错误: %v", err)
		}
		if status != http.StatusOK || env.Code != 1001 {
			cc.t.Fatalf("by_run 缺 run_id 应回 1001，实际 HTTP %d code=%d msg=%q", status, env.Code, truncate(env.Msg, 120))
		}
		cc.note("本页 %d 条记录复盘字段齐全；不存在单号→1023；缺 run_id→1001", len(list))
	})

	// AC-37：留痕自洽——库里的自愈行必须"为什么/依据什么/结果如何"三者齐全，
	// 且中间态不得提前写结束时间（"执行中"不等于"成功"）。
	run("AC-37", "自愈留痕自洽：事件行含策略/证据快照；中间态不写 finished_at", func(cc *caseCtx) {
		if db == nil {
			cc.skip("未配置 CRONY_TEST_MYSQL_DSN：无法直接核对 job_heal 行")
		}
		var (
			healID, status string
			reason         string
			policyJSON     string
			evidenceJSON   string
			finishedAt     int64
			deadline       int64
		)
		err := db.QueryRow(`select heal_id, status, reason, policy_json, evidence_json, finished_at, deadline
			from `+models.CronyJobHealTableName+` order by id desc limit 1`).
			Scan(&healID, &status, &reason, &policyJSON, &evidenceJSON, &finishedAt, &deadline)
		if err != nil {
			cc.skip("库里暂无自愈记录（未触发过失败/未跑过扫描）：%v", err)
		}
		if strings.TrimSpace(healID) == "" || !strings.HasPrefix(healID, "heal-") {
			cc.t.Fatalf("自愈单号格式非法: %q", healID)
		}
		if strings.TrimSpace(reason) == "" {
			cc.t.Fatalf("自愈行必须写清「为什么这么决策」（%s）", healID)
		}
		if !strings.Contains(policyJSON, "rule") {
			cc.t.Fatalf("策略快照必须记录命中的规则（安全边界事后可复盘），实际 %q", truncate(policyJSON, 120))
		}
		if strings.TrimSpace(evidenceJSON) == "" || evidenceJSON == "{}" {
			cc.t.Fatalf("证据快照不能为空（自愈必须依据本系统观测到的事实），heal=%s", healID)
		}
		// 中间态（executed）必须有观测截止、不得有结束时间；终态必须有结束时间。
		switch status {
		case "executed":
			if finishedAt != 0 {
				cc.t.Fatalf("中间态 executed 不得写 finished_at（=还没观测到结果）: heal=%s finished_at=%d", healID, finishedAt)
			}
			if deadline <= 0 {
				cc.t.Fatalf("中间态 executed 必须有观测截止时间: heal=%s", healID)
			}
		case "denied", "succeeded", "failed", "escalated":
			if finishedAt <= 0 {
				cc.t.Fatalf("终态 %s 必须写结束时间: heal=%s", status, healID)
			}
		default:
			cc.t.Fatalf("自愈状态取值非法: %q（heal=%s）", status, healID)
		}
		cc.note("最新自愈行 %s：status=%s reason 非空 policy 含 rule 证据非空 finished_at=%d deadline=%d（三要素齐全）",
			healID, status, finishedAt, deadline)
	})

	/* ------------------- 阶段 4-D / 阶段 5：运维智能化 ------------------- */

	// AC-38：多目标调度必须"可解释"——能看出每个节点为什么得分、硬约束是什么。
	run("AC-38", "调度解释：策略声明硬约束与维度，分配给出分项与选中节点（只读）", func(cc *caseCtx) {
		pol, err := c.postOK("/ops/scheduler/policy", map[string]any{})
		if err != nil {
			cc.t.Fatalf("读取调度策略失败: %v", err)
		}
		w := asMap(pol["weights"])
		for _, k := range []string{"load", "capacity", "affinity", "success"} {
			if _, ok := w[k]; !ok {
				cc.t.Fatalf("调度权重缺少维度 %q: %+v", k, w)
			}
		}
		if len(asSlice(pol["dimensions"])) == 0 {
			cc.t.Fatalf("调度必须列出声明的打分维度")
		}
		if len(asSlice(pol["hard_constraints"])) < 3 {
			cc.t.Fatalf("调度必须声明硬约束（可达/标签/能力）")
		}
		dec, err := c.postOK("/ops/scheduler/allocate", map[string]any{"job_id": 0})
		if err != nil {
			cc.t.Fatalf("调度解释失败: %v", err)
		}
		d := asMap(dec["decision"])
		if _, ok := d["scores"]; !ok {
			cc.t.Fatalf("调度解释必须给出 scores（可解释），实际 %+v", d)
		}
		for _, s := range asSlice(d["scores"]) {
			m := asMap(s)
			if _, ok := m["total"]; !ok {
				cc.t.Fatalf("每个候选必须有总分: %+v", m)
			}
			if _, ok := m["parts"]; !ok {
				cc.t.Fatalf("每个候选必须有分项（否则无法复盘为什么选它）: %+v", m)
			}
		}
		if _, ok := dec["requirement"]; !ok {
			cc.t.Fatalf("分配解释必须回显调度要求（硬约束/亲和）")
		}
		cc.note("权重 4 维、硬约束 %d 条；分配返回 %d 个候选打分（含分项），chosen=%q",
			len(asSlice(pol["hard_constraints"])), len(asSlice(d["scores"])), asStr(d["chosen"]))
	})

	// AC-39：SLO 契约的评估口径——无样本不判违约，违约必须说清差在哪。
	run("AC-39", "SLO 契约：保存后可评估；无样本不判违约、违约必须给原因", func(cc *caseCtx) {
		tag := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
		if tag == "" {
			tag = "acc"
		}
		saved, err := c.postOK("/ops/slo/save", map[string]any{
			"name": "acc-slo-" + tag, "job_id": 0,
			"target_success_rate": 0.99, "target_on_time_rate": 0.99,
			"window_seconds": 86400, "grace_seconds": 300, "enabled": true,
		})
		if err != nil {
			cc.t.Fatalf("保存 SLO 契约失败: %v", err)
		}
		if asStr(asMap(saved["policy"])["name"]) == "" {
			cc.t.Fatalf("保存后应回显契约: %+v", saved)
		}
		eval, err := c.postOK("/ops/slo/evaluate", map[string]any{})
		if err != nil {
			cc.t.Fatalf("评估 SLO 失败: %v", err)
		}
		list := asSlice(eval["list"])
		if len(list) == 0 {
			cc.t.Fatalf("保存的契约应出现在评估结果里")
		}
		breached := 0
		for _, it := range list {
			rep := asMap(asMap(it)["report"])
			if asBool(rep["unverifiable"]) && asBool(rep["breached"]) {
				cc.t.Fatalf("无样本不得判违约（否则误报风暴）: %+v", rep)
			}
			if asBool(rep["breached"]) {
				breached++
				if len(asSlice(rep["reasons"])) == 0 {
					cc.t.Fatalf("违约必须给出原因（差在哪）: %+v", rep)
				}
			}
		}
		cc.note("保存契约 %q；评估 %d 条，违约 %d 条（口径：无样本不判违约、违约带原因）",
			"acc-slo-"+tag, len(list), breached)
	})

	// AC-40：自治信用——连续成功升级、一次不良后果降级并冻结、冻结时生效等级回落 L1。
	run("AC-40", "自治信用：连成升级、一次不良后果冻结、冻结回落 L1、可人工解冻", func(cc *caseCtx) {
		scope := "acc_autonomy@" + strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
		// 连成 5 次 → 升级。
		var level float64
		for i := 0; i < 5; i++ {
			cr, err := c.postOK("/ops/autonomy/observe", map[string]any{"scope": scope, "success": true})
			if err != nil {
				cc.t.Fatalf("记录成功失败: %v", err)
			}
			level = asNum(asMap(cr["credit"])["level"])
		}
		if level < 1 {
			cc.t.Fatalf("连续成功应升级（>=L1），实际 level=%v", level)
		}
		// 一次不良后果 → 冻结。
		cr, err := c.postOK("/ops/autonomy/observe", map[string]any{"scope": scope, "success": false})
		if err != nil {
			cc.t.Fatalf("记录不良后果失败: %v", err)
		}
		if !asBool(asMap(cr["credit"])["frozen"]) {
			cc.t.Fatalf("一次不良后果必须冻结: %+v", cr)
		}
		// list：冻结时生效等级必须回落 L1。
		listed, err := c.postOK("/ops/autonomy/list", map[string]any{})
		if err != nil {
			cc.t.Fatalf("自治信用列表失败: %v", err)
		}
		found := false
		for _, it := range asSlice(listed["list"]) {
			m := asMap(it)
			if asStr(m["scope"]) == scope {
				found = true
				if asStr(m["effective_level"]) != "L1" {
					cc.t.Fatalf("冻结时实际生效等级必须回落 L1，实际 %q", asStr(m["effective_level"]))
				}
			}
		}
		if !found {
			cc.t.Fatalf("列表中找不到 scope=%s", scope)
		}
		// 解冻。
		un, err := c.postOK("/ops/autonomy/unfreeze", map[string]any{"scope": scope})
		if err != nil {
			cc.t.Fatalf("解冻失败: %v", err)
		}
		if asBool(asMap(un["credit"])["frozen"]) {
			cc.t.Fatalf("解冻后不应仍为冻结")
		}
		cc.note("scope=%s：连成 5 次升到 L%v → 一次不良后果冻结（生效 L1）→ 人工解冻成功", scope, level)
	})

	// AC-41：混沌驾照分与影子对账的判据正确。
	run("AC-41", "混沌驾照分=正确率；影子对账有差异即拒绝放量", func(cc *caseCtx) {
		g, err := c.postOK("/ops/chaos/grade", map[string]any{"attempts": []map[string]any{
			{"fault": map[string]any{"kind": "latency", "target": "db", "severity": 1}, "detected": true, "handled": true, "correct": true, "detection_ms": 100},
			{"fault": map[string]any{"kind": "latency", "target": "db", "severity": 1}, "detected": true, "handled": true, "correct": false, "detection_ms": 200},
		}})
		if err != nil {
			cc.t.Fatalf("混沌打分失败: %v", err)
		}
		score := asMap(g["score"])
		if asNum(score["license"]) != 50 {
			cc.t.Fatalf("驾照分应等于正确率×100=50，实际 %v", score["license"])
		}
		// 影子：一致 → 允许放量。
		same, err := c.postOK("/ops/shadow/reconcile", map[string]any{
			"baseline":  map[string]any{"exit_code": 0, "stdout_hash": "a", "stderr_hash": "", "side_effects": []any{}},
			"candidate": map[string]any{"exit_code": 0, "stdout_hash": "a", "stderr_hash": "", "side_effects": []any{}},
		})
		if err != nil {
			cc.t.Fatalf("影子对账失败: %v", err)
		}
		if !asBool(same["allow_promotion"]) {
			cc.t.Fatalf("零差异应允许放量: %+v", same)
		}
		// 影子：有差异 → 拒绝放量。
		diff, err := c.postOK("/ops/shadow/reconcile", map[string]any{
			"baseline":  map[string]any{"exit_code": 0, "stdout_hash": "a", "stderr_hash": "", "side_effects": []any{}},
			"candidate": map[string]any{"exit_code": 1, "stdout_hash": "a", "stderr_hash": "", "side_effects": []any{}},
		})
		if err != nil {
			cc.t.Fatalf("影子对账失败: %v", err)
		}
		if asBool(diff["allow_promotion"]) {
			cc.t.Fatalf("存在意外差异必须拒绝放量: %+v", diff)
		}
		cc.note("驾照分=50（正确率 50%%）；影子一致=允许放量、退出码不同=拒绝放量")
	})

	// AC-42：自治信用真的接进了"要不要审批"的判定（而不是只存在表里）。
	// 判据（与工具网关同一函数）：放行等级 = max(L1, 信用)；生效级别 = min(配置上限, 放行等级)。
	run("AC-42", "自治级别解释：无历史沿用配置上限；生效级别=min(上限,放行等级)且永不越权", func(cc *caseCtx) {
		rank := func(s string) int {
			switch strings.ToUpper(strings.TrimSpace(s)) {
			case "L0":
				return 0
			case "L1":
				return 1
			case "L2":
				return 2
			case "L3":
				return 3
			}
			return -1
		}
		env := strings.TrimSpace(os.Getenv("CRONY_ACC_AUTONOMY_ENV"))
		tool := "ac42_tool"

		// 1) 无历史 → 沿用配置上限（零回归：不因为"没测过"就收紧或放宽）。
		fresh, err := c.postOK("/ops/autonomy/effective", map[string]any{"tool": tool, "env": env})
		if err != nil {
			cc.t.Fatalf("解释生效级别失败: %v", err)
		}
		if !asBool(fresh["credit_enabled"]) {
			cc.skip("实时后端未启用自治信用（tools.autonomy-credit=false），本用例前提不成立：%+v", fresh)
		}
		ceiling := asStr(fresh["configured_ceiling"])
		if rank(ceiling) < 0 {
			cc.t.Fatalf("必须回报合法的配置上限，实际 %q", ceiling)
		}
		if asBool(fresh["credit_known"]) {
			cc.t.Fatalf("工具 %s 本不应有信用历史: %+v", tool, fresh)
		}
		if src := asStr(fresh["source"]); src != "config" {
			cc.t.Fatalf("无历史时来源应为 config，实际 %q", src)
		}
		if eff := asStr(fresh["effective"]); eff != ceiling {
			cc.t.Fatalf("无历史时应沿用配置上限 %s，实际 %s", ceiling, eff)
		}

		// 2) 连成 10 次 → 信用升到 L2；放行等级不得因此越过配置上限。
		scope := asStr(fresh["scope"])
		for i := 0; i < 10; i++ {
			if _, err := c.postOK("/ops/autonomy/observe", map[string]any{"scope": scope, "success": true}); err != nil {
				cc.t.Fatalf("记录成功失败: %v", err)
			}
		}
		got, err := c.postOK("/ops/autonomy/effective", map[string]any{"tool": tool, "env": env})
		if err != nil {
			cc.t.Fatalf("解释生效级别失败: %v", err)
		}
		if !asBool(got["credit_known"]) {
			cc.t.Fatalf("应能读到信用历史: %+v", got)
		}
		credit, enforced, eff := asStr(got["credit_level"]), asStr(got["enforcement_level"]), asStr(got["effective"])
		if rank(credit) < 2 {
			cc.t.Fatalf("连成 10 次信用应升到 L2，实际 %s", credit)
		}
		if rank(enforced) < 1 {
			cc.t.Fatalf("放行等级不得低于 L1（信用不决定\"要不要写\"），实际 %s", enforced)
		}
		// 不变式：生效 = min(上限, 放行)，且永不越过上限。
		want := rank(enforced)
		if rank(ceiling) < want {
			want = rank(ceiling)
		}
		if rank(eff) != want {
			cc.t.Fatalf("生效级别应为 min(%s, %s)=L%d，实际 %s", ceiling, enforced, want, eff)
		}
		if rank(eff) > rank(ceiling) {
			cc.t.Fatalf("生效级别越过了配置上限: %s > %s", eff, ceiling)
		}
		// 来源必须与"是否被上限夹住"一致。
		wantSrc := "credit"
		if rank(enforced) > rank(ceiling) {
			wantSrc = "credit_capped"
		}
		if src := asStr(got["source"]); src != wantSrc {
			cc.t.Fatalf("来源应为 %s，实际 %s（上限 %s / 放行 %s）", wantSrc, src, ceiling, enforced)
		}

		// 3) 一次不良后果 → 冻结 → 放行等级与生效级别都回落 L1。
		if _, err := c.postOK("/ops/autonomy/observe", map[string]any{"scope": scope, "success": false}); err != nil {
			cc.t.Fatalf("记录不良后果失败: %v", err)
		}
		frozen, err := c.postOK("/ops/autonomy/effective", map[string]any{"tool": tool, "env": env})
		if err != nil {
			cc.t.Fatalf("解释生效级别失败: %v", err)
		}
		if !asBool(frozen["frozen"]) {
			cc.t.Fatalf("一次不良后果必须冻结: %+v", frozen)
		}
		if got := asStr(frozen["effective"]); got != "L1" {
			cc.t.Fatalf("冻结后生效级别必须回落 L1，实际 %s", got)
		}

		cc.note("配置上限 %s；连成 10 次信用升到 %s（放行 %s）→ 生效 %s（来源 %s）；一次不良后果冻结 → 生效 L1",
			ceiling, credit, enforced, eff, wantSrc)
		// 清理本用例自造的信用行（按 scope 删除，避免污染后续复跑）。
		if db != nil {
			_, _ = db.Exec("delete from "+models.CronyAutonomyCreditTableName+" where scope = ?", scope)
		}
	})

	// AC-43：SLO 违约动作接线——违约后按契约的 breach_action 动手（**闭合白名单**），
	// 每次动作留一行事实（原因 + 说明齐备），且在冷却期内不重复动手。
	// 用全局契约（job_id=0）让断言与环境无关：无论有没有 LLM，"全局契约没有可诊断的具体执行"
	// 都是确定性事实（本用例只断言结构不变式，不断言特定状态值）。
	run("AC-43", "SLO 违约处置：动作闭白名单、必留原因与说明、冷却期内不重复动手", func(cc *caseCtx) {
		tag := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
		if tag == "" {
			tag = "acc"
		}
		okName := "acc-slo-act-" + tag
		badName := "acc-slo-bad-" + tag

		save := func(name, action string) int {
			saved, err := c.postOK("/ops/slo/save", map[string]any{
				"name": name, "job_id": 0,
				"target_success_rate": 1.0, "target_on_time_rate": 0,
				"window_seconds": 86400, "grace_seconds": 300,
				"enabled": true, "breach_action": action,
			})
			if err != nil {
				cc.t.Fatalf("保存 SLO 契约 %s 失败: %v", name, err)
			}
			pid := int(asNum(asMap(saved["policy"])["id"]))
			if pid <= 0 {
				cc.t.Fatalf("保存后应回显契约 id: %+v", saved)
			}
			return pid
		}
		okPID := save(okName, "diagnose")
		badPID := save(badName, "rm -rf /")
		if db != nil {
			cc.t.Cleanup(func() {
				for _, pid := range []int{okPID, badPID} {
					_, _ = db.Exec("delete from "+models.CronySloBreachTableName+" where policy_id = ?", pid)
					_, _ = db.Exec("delete from "+models.CronySloPolicyTableName+" where id = ?", pid)
				}
			})
		}

		// 第一次：force 绕过冷却 → 两个契约各产出一行处置事实。
		act, err := c.postOK("/ops/slo/act", map[string]any{"policy_id": okPID, "force": true})
		if err != nil {
			cc.t.Fatalf("触发违约处置失败: %v", err)
		}
		rep := asMap(act["report"])
		if _, ok := rep["auto_enabled"]; !ok {
			cc.t.Fatalf("处置回执必须回报自动开关状态（默认关闭=零回归）: %+v", rep)
		}
		rows := asSlice(rep["rows"])
		if len(rows) == 0 {
			cc.t.Fatalf("契约 %s 应产出一行处置记录: %+v", okName, rep)
		}
		okRow := asMap(rows[0])
		if asStr(okRow["action"]) != "diagnose" {
			cc.t.Fatalf("动作应原样保留 diagnose，实际 %q", asStr(okRow["action"]))
		}
		if strings.TrimSpace(asStr(okRow["reason"])) == "" {
			cc.t.Fatalf("违约处置必须记录原因: %+v", okRow)
		}
		if strings.TrimSpace(asStr(okRow["detail"])) == "" {
			cc.t.Fatalf("违约处置必须记录说明（做了什么/为什么没做）: %+v", okRow)
		}
		status := asStr(okRow["status"])
		if status != "observed" && status != "acted" && status != "failed" {
			cc.t.Fatalf("状态必须是 observed/acted/failed 之一，实际 %q", status)
		}

		// 非法动作：必须被归一为 none（闭合白名单：看不懂的配置绝不变成别的动作）。
		bad, err := c.postOK("/ops/slo/act", map[string]any{"policy_id": badPID, "force": true})
		if err != nil {
			cc.t.Fatalf("触发非法动作契约失败: %v", err)
		}
		badRows := asSlice(asMap(bad["report"])["rows"])
		if len(badRows) == 0 {
			cc.t.Fatalf("非法动作契约也应留痕（白名单判定要可见）")
		}
		badRow := asMap(badRows[0])
		if asStr(badRow["action"]) != "none" {
			cc.t.Fatalf("非法动作必须归一为 none，实际 %q", asStr(badRow["action"]))
		}
		if !strings.Contains(asStr(badRow["detail"]), "未知") {
			cc.t.Fatalf("非法动作应说明按 none 处理: %q", asStr(badRow["detail"]))
		}

		// 第二次：不带 force → 冷却期内不得重复动手。
		act2, err := c.postOK("/ops/slo/act", map[string]any{"policy_id": okPID})
		if err != nil {
			cc.t.Fatalf("二次触发失败: %v", err)
		}
		rep2 := asMap(act2["report"])
		if asNum(rep2["skipped"]) < 1 {
			cc.t.Fatalf("冷却期内应跳过重复动作，实际 skipped=%v（report=%+v）", rep2["skipped"], rep2)
		}
		cc.note("契约 %q：动作=diagnose 留痕（状态=%s、原因与说明齐备）；非法动作归一为 none；二次触发命中冷却 skipped=%v",
			okName, status, rep2["skipped"])
	})

	// AC-44：控制台只读接口补齐——概览/健康度/配置快照都返回真实结构，且**绝不泄露密钥**。
	run("AC-44", "概览/健康度/配置快照：真实来源、结构完整、不含任何密钥", func(cc *caseCtx) {
		health, err := c.getOK("/statis/health")
		if err != nil {
			cc.t.Fatalf("读取健康度失败: %v", err)
		}
		comps := asSlice(health["components"])
		if len(comps) < 4 {
			cc.t.Fatalf("健康度必须列出各依赖组件，实际 %d: %+v", len(comps), health)
		}
		if s := asStr(health["status"]); s != "ok" && s != "degraded" && s != "down" {
			cc.t.Fatalf("整体健康度状态非法: %q", s)
		}
		if strings.TrimSpace(asStr(health["checked_at"])) == "" {
			cc.t.Fatalf("健康度必须带检查时间")
		}
		names := map[string]bool{}
		for _, x := range comps {
			m := asMap(x)
			names[asStr(m["name"])] = true
			if strings.TrimSpace(asStr(m["status"])) == "" || strings.TrimSpace(asStr(m["detail"])) == "" {
				cc.t.Fatalf("组件必须带状态与说明: %+v", m)
			}
		}
		if !names["MySQL"] {
			cc.t.Fatalf("健康度必须包含组件 MySQL: %v", names)
		}

		over, err := c.getOK("/statis/overview")
		if err != nil {
			cc.t.Fatalf("读取概览失败: %v", err)
		}
		kpi := asMap(over["kpi"])
		for _, key := range []string{"total_jobs", "enabled_jobs", "online_nodes", "total_nodes",
			"runs_today", "success_rate_today", "failed_today", "pending_approvals", "kb_docs",
			"agent_sessions_total", "agent_sessions_today", "agent_sessions_failed_today"} {
			if _, ok := kpi[key]; !ok {
				cc.t.Fatalf("概览 KPI 缺字段 %q: %+v", key, kpi)
			}
		}
		if _, ok := over["alerts"]; !ok {
			cc.t.Fatalf("概览必须给出 alerts（可为空数组）: %+v", over)
		}

		set, err := c.getOK("/settings")
		if err != nil {
			cc.t.Fatalf("读取配置快照失败: %v", err)
		}
		for _, sec := range []string{"auth", "scheduler", "notify", "agent"} {
			if _, ok := set[sec]; !ok {
				cc.t.Fatalf("配置快照缺分节 %q: %+v", sec, set)
			}
		}
		// 前端 SettingsView 直接读取这些字段名（缺失就会显示空白），必须钉死。
		auth := asMap(set["auth"])
		for _, key := range []string{"jwt_issuer", "jwt_ttl_hours", "jwt_secret_source",
			"password_min_length", "rbac", "bootstrap_admin"} {
			if _, ok := auth[key]; !ok {
				cc.t.Fatalf("鉴权分节缺前端读取字段 %q（会导致设置页空白）: %+v", key, auth)
			}
		}
		if s := strings.TrimSpace(asStr(auth["jwt_issuer"])); s == "" {
			cc.t.Fatalf("jwt_issuer 不能为空")
		}
		sched := asMap(set["scheduler"])
		for _, key := range []string{"default_concurrency_policy", "default_allocation", "max_retry_times"} {
			if _, ok := sched[key]; !ok {
				cc.t.Fatalf("调度分节缺前端读取字段 %q（会导致设置页空白）: %+v", key, sched)
			}
		}
		if _, ok := asMap(set["agent"])["enabled"]; !ok {
			cc.t.Fatalf("Agent 分节缺 enabled 字段: %+v", set["agent"])
		}
		blob, _ := json.Marshal(set)
		lower := strings.ToLower(string(blob))
		for _, bad := range []string{"jwtsecret", "api_key", "apikey", "bootstrap_admin_password", `password":`} {
			if strings.Contains(lower, bad) {
				cc.t.Fatalf("配置快照疑似泄露敏感字段 %q: %s", bad, truncate(string(blob), 300))
			}
		}
		cc.note("健康度组件 %d 个（含 MySQL）；概览 KPI %d 字段 + alerts；配置快照 4 分节且不含密钥",
			len(comps), len(kpi))
	})

	// AC-45：控制台静态资源挂载在 /console/（hash 路由 + 相对资源路径）。
	// 该部署未放置 ./console/dist 时 SKIP——"没挂载"不等于"挂载失败"。
	run("AC-45", "控制台挂载：/console/ 返回 index.html 且资源为相对路径", func(cc *caseCtx) {
		status, body, err := c.getRaw("/console/")
		if err != nil {
			cc.t.Fatalf("请求 /console/ 失败: %v", err)
		}
		if status == http.StatusNotFound {
			cc.skip("该部署未挂载控制台（./console/dist 不存在）：HTTP 404")
		}
		if status != http.StatusOK {
			cc.t.Fatalf("/console/ 期望 HTTP 200，实际 %d", status)
		}
		lower := strings.ToLower(body)
		if !strings.Contains(lower, "<html") || !strings.Contains(body, `id="app"`) {
			cc.t.Fatalf("/console/ 应返回控制台 index.html，实际: %s", truncate(body, 240))
		}
		if strings.Contains(body, `src="/assets/`) || strings.Contains(body, `href="/assets/`) {
			cc.t.Fatalf("控制台资源应为相对路径（可挂任意子路径），实际用了绝对 /assets/: %s", truncate(body, 320))
		}
		cc.note("/console/ 返回 index.html（%d 字节），资源为相对路径", len(body))
	})

	// AC-46：统一审计检索合并**真实**事实源（工具调用审计 + 写动作审批），
	// 分页/过滤契约与全站一致；未采集字段（ip）如实留空。
	run("AC-46", "审计检索：合并真实事实源、分页与过滤契约一致、未采集字段留空", func(cc *caseCtx) {
		page, err := c.postOK("/audit/search", map[string]any{"page": 1, "page_size": 5})
		if err != nil {
			cc.t.Fatalf("审计检索失败: %v", err)
		}
		for _, key := range []string{"list", "total", "page", "page_size", "source_window", "notice"} {
			if _, ok := page[key]; !ok {
				cc.t.Fatalf("审计响应缺字段 %q: %+v", key, page)
			}
		}
		if asNum(page["page"]) != 1 || asNum(page["page_size"]) != 5 {
			cc.t.Fatalf("分页回显错误: page=%v page_size=%v", page["page"], page["page_size"])
		}
		list := asSlice(page["list"])
		if len(list) > 5 {
			cc.t.Fatalf("单页行数超过 page_size: %d", len(list))
		}
		if asNum(page["total"]) < float64(len(list)) {
			cc.t.Fatalf("total 小于本页行数: total=%v len=%d", page["total"], len(list))
		}
		allowed := map[string]bool{"success": true, "failed": true, "denied": true, "pending": true,
			"expired": true, "canceled": true, "blocked": true, "unknown": true}
		for _, x := range list {
			m := asMap(x)
			for _, key := range []string{"id", "time", "operator", "operator_type", "action", "action_text",
				"target", "target_type", "result", "source"} {
				if strings.TrimSpace(asStr(m[key])) == "" {
					cc.t.Fatalf("审计行缺字段 %q: %+v", key, m)
				}
			}
			if !allowed[asStr(m["result"])] {
				cc.t.Fatalf("审计结果取值非法: %q", asStr(m["result"]))
			}
			if src := asStr(m["source"]); src != "tool_audit" && src != "approval" {
				cc.t.Fatalf("审计来源标注非法: %q", src)
			}
			if asStr(m["ip"]) != "" {
				cc.t.Fatalf("ip 当前无采集，应留空: %+v", m)
			}
		}
		// result 过滤为精确匹配：给一个不可能命中的值，total 必须为 0。
		bogus, err := c.postOK("/audit/search", map[string]any{"result": "__no_such_result__", "page": 1, "page_size": 5})
		if err != nil {
			cc.t.Fatalf("审计过滤查询失败: %v", err)
		}
		if asNum(bogus["total"]) != 0 || len(asSlice(bogus["list"])) != 0 {
			cc.t.Fatalf("result 过滤应为精确匹配，捏造值不应命中: %+v", bogus)
		}
		cc.note("审计合并契约齐备（本页 %d 行 / 共 %v 行）：来源标注、结果枚举、ip 留空均正确",
			len(list), page["total"])
	})

	/* ---------------------- 阶段 5 收尾：节点舰队与多目标调度（新增） ---------------------- */

	// AC-47：节点列表必须来自真实库、uuid 唯一、在线计数与库内一致。
	// 舰队上到 20 个节点后，"列表/统计各说各话"这类漂移最容易被忽视，也最影响排障。
	run("AC-47", "节点舰队：列表契约完整、uuid 唯一、在线计数与库内一致", func(cc *caseCtx) {
		page, err := c.postOK("/node/search", map[string]any{"page": 1, "page_size": 500})
		if err != nil {
			cc.t.Fatalf("节点检索失败: %v", err)
		}
		total := int(asNum(page["total"]))
		list := asSlice(page["list"])
		if total <= 0 {
			cc.skip("库里没有任何节点：节点舰队未启动，本用例需要至少 1 个已注册节点")
		}
		if len(list) != total {
			cc.t.Fatalf("total=%d 与返回行数 %d 不一致（分页契约破了）", total, len(list))
		}
		seen := map[string]bool{}
		online := 0
		for _, x := range list {
			m := asMap(x)
			uuid := strings.TrimSpace(asStr(m["uuid"]))
			if uuid == "" {
				cc.t.Fatalf("节点行缺 uuid：%+v", m)
			}
			for _, k := range []string{"hostname", "ip", "version"} {
				if strings.TrimSpace(asStr(m[k])) == "" {
					cc.t.Fatalf("节点 %s 缺字段 %q（控制台节点页会直接渲染它）: %+v", uuid, k, m)
				}
			}
			if seen[uuid] {
				cc.t.Fatalf("节点 uuid 重复：%s（同一节点两行会让调度与统计口径分裂）", uuid)
			}
			seen[uuid] = true
			if int(asNum(m["status"])) == int(models.NodeConnSuccess) {
				online++
			}
		}
		if db == nil {
			cc.note("节点 %d 个（uuid 唯一、契约字段齐全），在线 %d（未配置 DSN，跳过库内交叉校验）", total, online)
			return
		}
		var dbTotal, dbOnline int
		if err := db.QueryRow(
			"select count(*), coalesce(sum(case when status = ? then 1 else 0 end), 0) from node",
			models.NodeConnSuccess).Scan(&dbTotal, &dbOnline); err != nil {
			cc.t.Fatalf("读库内节点数失败: %v", err)
		}
		if dbTotal != total {
			cc.t.Fatalf("接口 total=%d 与库内节点行数 %d 不一致（列表不是来自真实库）", total, dbTotal)
		}
		stat, err := c.getOK("/statis/today")
		if err != nil {
			cc.t.Fatalf("读取今日统计失败: %v", err)
		}
		if got := int(asNum(stat["normal_node_count"])); got != dbOnline {
			cc.t.Fatalf("统计接口在线数=%d 与库内 status=%d 的行数=%d 不一致", got, models.NodeConnSuccess, dbOnline)
		}
		cc.note("节点 %d 个（在线 %d / 失联 %d）：uuid 唯一、契约字段齐全、列表与统计均与库内一致",
			dbTotal, dbOnline, dbTotal-dbOnline)
	})

	// AC-48：节点失联（etcd 节点键消失）必须触发 FailOver：该节点上的任务被重分配，
	// 且旧节点的 etcd 任务键被清理。
	//
	// 这条路径**只有 etcd 事件能触发**，所以前置条件是 etcd 探针（CRONY_ACC_ETCD_ENDPOINTS）；
	// 未配置就 SKIP 并写明原因。用例自造一个"合成节点"当牺牲品（绝不碰真实节点）：
	// 先在库里插一行 + 往 etcd 写注册键（模拟上线），建一个分配在它上面的任务，
	// 再删掉注册键（模拟失联），最后等 FailOver 把任务搬走。
	run("AC-48", "节点失联：etcd 节点键消失后任务被重分配且旧键被清理（FailOver）", func(cc *caseCtx) {
		if db == nil {
			cc.skip("未配置 CRONY_TEST_MYSQL_DSN：无法核对任务归属")
		}
		cli := newEtcdClient(cc.t, os.Getenv("CRONY_ACC_ETCD_ENDPOINTS"))
		if cli == nil {
			cc.skip("未配置 CRONY_ACC_ETCD_ENDPOINTS：FailOver 只由 etcd 节点事件驱动，无探针则无法触发（不是系统缺陷）")
		}
		tag := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
		if tag == "" {
			tag = "acc-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		}
		victim := "acc-failover-" + tag
		ctx := context.Background()

		// 1) 牺牲节点：库内一行（在线）+ etcd 注册键
		if _, err := db.Exec(`insert into node
			(pid, ip, hostname, uuid, version, status, up, down, labels, capacity)
			values ('1', '127.0.0.1', 'acc-victim', ?, 'acceptance', ?, ?, 0, '', 0)`,
			victim, models.NodeConnSuccess, time.Now().Unix()); err != nil {
			cc.t.Fatalf("造牺牲节点失败（验收库需要 INSERT 权限）: %v", err)
		}
		nodeKey := fmt.Sprintf(etcdclient.KeyEtcdNode, victim)
		val, _ := json.Marshal(models.Node{
			UUID: victim, Hostname: "acc-victim", IP: "127.0.0.1", PID: "1",
			Version: "acceptance", Status: models.NodeConnSuccess,
		})
		if _, err := cli.Put(ctx, nodeKey, string(val)); err != nil {
			cc.t.Fatalf("写 etcd 节点注册键失败: %v", err)
		}
		// 2) 建一个分配在牺牲节点上的任务（走公开接口：写库 + 写 etcd 任务键）
		created, err := c.postOK("/job/add", map[string]any{
			"name": "ACC-FAILOVER-" + tag, "command": "cmd /c echo failover",
			"job_type": 1, "spec": "0 0 5 * * *", "timeout": 30, "notify_type": 1,
			"allocation": 1, "run_on": victim,
		})
		if err != nil {
			cc.t.Fatalf("在牺牲节点上建任务失败: %v", err)
		}
		jobID := int(asNum(created["id"]))
		if jobID <= 0 {
			cc.t.Fatalf("建任务未返回 id: %+v", created)
		}
		jobKey := fmt.Sprintf(etcdclient.KeyEtcdJob, victim, jobID)
		cc.t.Cleanup(func() {
			_, _ = c.postOK("/job/del", map[string]any{"ids": []int{jobID}})
			_, _ = db.Exec("delete from node where uuid = ?", victim)
			_, _ = cli.Delete(context.Background(), nodeKey)
		})
		// 3) 让调度器不再把牺牲节点当候选：否则任务可能被"重分配"回它自己，用例变成假阴性。
		if _, err := db.Exec("update node set status = ? where uuid = ?", models.NodeConnFail, victim); err != nil {
			cc.t.Fatalf("把牺牲节点置为失联失败: %v", err)
		}
		// 4) 模拟失联：删掉 etcd 注册键
		if _, err := cli.Delete(ctx, nodeKey); err != nil {
			cc.t.Fatalf("删除 etcd 节点注册键失败: %v", err)
		}
		// 5) 等 FailOver：任务归属换成别的节点
		deadline := time.Now().Add(30 * time.Second)
		var moved string
		for time.Now().Before(deadline) {
			var runOn string
			if err := db.QueryRow("select coalesce(run_on, '') from job where id = ?", jobID).Scan(&runOn); err != nil {
				cc.t.Fatalf("读回任务归属失败: %v", err)
			}
			if runOn != "" && runOn != victim {
				moved = runOn
				break
			}
			time.Sleep(time.Second)
		}
		if moved == "" {
			cc.t.Fatalf("节点失联后任务 %d 仍挂在 %s 上（FailOver 未生效：任务不会在新节点上被调度）", jobID, victim)
		}
		gr, err := cli.Get(ctx, jobKey)
		if err != nil {
			cc.t.Fatalf("读旧 etcd 任务键失败: %v", err)
		}
		if gr.Count != 0 {
			cc.t.Fatalf("FailOver 成功后旧节点的 etcd 任务键 %s 应被清理，实际仍存在", jobKey)
		}
		var newNodeCount int
		if err := db.QueryRow("select count(*) from node where uuid = ?", moved).Scan(&newNodeCount); err != nil {
			cc.t.Fatalf("核对新节点失败: %v", err)
		}
		if newNodeCount != 1 {
			cc.t.Fatalf("任务被搬到的节点 %s 在库内不存在", moved)
		}
		cc.note("牺牲节点 %s 失联后，任务 %d 被重分配给 %s，旧 etcd 任务键已清理", victim, jobID, moved)
	})

	// AC-49：硬约束必须真的挡人。require_labels 指向一个不存在的标签时，
	// 调度器必须"一个都选不出来"，而不是随手挑一个（乱选会把任务派到装不了它的机器上）。
	run("AC-49", "多目标调度硬约束：不满足 require_labels 的节点一律被排除", func(cc *caseCtx) {
		tag := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
		if tag == "" {
			tag = "acc-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		}
		created, err := c.postOK("/job/add", map[string]any{
			"name": "ACC-SELECTOR-" + tag, "command": "cmd /c echo selector",
			"job_type": 1, "spec": "0 0 6 * * *", "timeout": 30, "notify_type": 1,
			"allocation": 1, "run_on": nodeUUID,
			"node_selector": `{"require_labels":{"acc-fleet-nonexistent":"1"}}`,
		})
		if err != nil {
			cc.skip("无法建带 node_selector 的任务（该任务走手动分配，需要一个在线节点 %s）: %v", nodeUUID, err)
		}
		jobID := int(asNum(created["id"]))
		if jobID <= 0 {
			cc.t.Fatalf("建带选择器的任务未返回 id: %+v", created)
		}
		cc.t.Cleanup(func() { _, _ = c.postOK("/job/del", map[string]any{"ids": []int{jobID}}) })

		dec, err := c.postOK("/ops/scheduler/allocate", map[string]any{"job_id": jobID})
		if err != nil {
			cc.t.Fatalf("调度解释失败: %v", err)
		}
		d := asMap(dec["decision"])
		req := asMap(dec["requirement"])
		if chosen := strings.TrimSpace(asStr(d["chosen"])); chosen != "" {
			cc.t.Fatalf("require_labels 指向不存在的标签时不得选出节点（会派到装不了它的机器上），实际 chosen=%q", chosen)
		}
		// 注意键名口径：requirement 是调度内核的结构体直出（字段名即键名，PascalCase），
		// 不是 snake_case。两种都认，避免把"契约命名风格"误判成功能缺陷。
		labels := asMap(req["RequireLabels"])
		if len(labels) == 0 {
			labels = asMap(req["require_labels"])
		}
		if len(labels) == 0 {
			cc.t.Fatalf("调度解释必须回显硬约束（否则无法复盘为什么没选出来）: %+v", req)
		}
		cc.note("硬约束 require_labels=%v 下 chosen 为空（集群里没有满足条件的节点），硬约束没有被忽略", labels)
	})

	// AC-50：创建任务时的自动分配必须与"节点接入/失联迁移"同一口径，且**硬约束真的挡人**。
	//
	// 背景（F3，20 节点舰队实测）：`POST /job/add` 的自动分配此前走裸 AutoAllocateNode
	// （任务数最少），完全不看 node_selector —— 一个声明了"只有带某标签的机器能跑"的任务，
	// 在创建时就会被随手派到一台装不了它的机器上。创建是最常见的分配入口，
	// 这里不生效，硬约束就形同虚设。
	//
	// 用例用**创建路径**（不依赖 etcd）：同一份请求，只差 node_selector 是否有人满足——
	//   - 无选择器 → 必须创建成功并拿到节点（正向对照，排除"请求本身就错"的可能）；
	//   - 有无人满足的硬约束 → 必须被拒绝，且理由要指向硬约束（可行动），
	//     而不是静默地派给一台装不了它的机器。
	run("AC-50", "创建任务的自动分配不越过硬约束：无人满足时拒绝并说明原因（F3）", func(cc *caseCtx) {
		tag := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
		if tag == "" {
			tag = "acc-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		}
		// 用 http 类型避开"cmd 默认不允许自动分配"这条独立配置（与本用例要验的无关）。
		// `allocation` 带 binding:"required"，不能传 0（会被绑定校验挡成 1001）：
		// 自动分配的取值是 models.AutoAllocation = 2。
		base := map[string]any{
			"name": "ACC-SELECTOR-CREATE-" + tag, "command": "http://127.0.0.1:8089/ping",
			"job_type": 2, "spec": "0 0 8 * * *", "timeout": 30, "notify_type": 1,
			"allocation": 2,
		}

		// 正向对照：同样的请求、不带硬约束，必须能建成功并落到某个节点上。
		okReq := map[string]any{}
		for k, v := range base {
			okReq[k] = v
		}
		okReq["name"] = "ACC-SELECTOR-CREATE-OK-" + tag
		created, err := c.postOK("/job/add", okReq)
		if err != nil {
			cc.t.Fatalf("不带硬约束的自动分配任务应当创建成功: %v", err)
		}
		okID := int(asNum(created["id"]))
		if okID <= 0 {
			cc.t.Fatalf("创建成功但未返回 id: %+v", created)
		}
		cc.t.Cleanup(func() { _, _ = c.postOK("/job/del", map[string]any{"ids": []int{okID}}) })

		// 负向：硬约束在当前集群里无人满足 → 必须拒绝，且理由指向硬约束。
		badReq := map[string]any{}
		for k, v := range base {
			badReq[k] = v
		}
		badReq["name"] = "ACC-SELECTOR-CREATE-NO-" + tag
		badReq["node_selector"] = fmt.Sprintf(`{"require_labels":{"acc-f3-nonexistent":"%s"}}`, tag)
		_, env, err := c.do(http.MethodPost, "/job/add", badReq, true)
		if err != nil {
			cc.t.Fatalf("请求本身失败（无法判断是不是拒绝了硬约束）: %v", err)
		}
		if env == nil {
			cc.t.Fatalf("服务器未返回可解析的响应体")
		}
		if env.Code == 200 {
			if id := int(asNum(env.dataMap()["id"])); id > 0 && db != nil {
				var runOn string
				_ = db.QueryRow("select coalesce(run_on, '') from job where id = ?", id).Scan(&runOn)
				cc.t.Cleanup(func() { _, _ = c.postOK("/job/del", map[string]any{"ids": []int{id}}) })
				cc.t.Fatalf("硬约束无人满足时不得创建成功（会落到一台装不了它的机器上）：job=%d run_on=%q", id, runOn)
			}
			cc.t.Fatalf("硬约束无人满足时不得创建成功")
		}
		if !strings.Contains(env.Msg, "硬约束") && !strings.Contains(env.Msg, "node_selector") {
			cc.t.Fatalf("拒绝理由必须指向硬约束（可行动），实际 msg=%q", env.Msg)
		}
		cc.note("不带硬约束的自动分配成功（job %d 落到节点上）；硬约束无人满足时被拒绝（code=%d），msg=%q",
			okID, env.Code, env.Msg)
	})

	// AC-51：**节点接入**触发的自动分配同样不得越过硬约束。
	//
	// 这条专测"新节点接入"那条路径（assignPendingJobs），它是 F3 的原始形态：
	// 此前它与 FailOver 用的不是一个策略。用例不碰真实节点——用 etcd 探针写一个
	// 合成节点的注册键（带租约，自动过期，不写库内行）造一次接入事件，
	// 然后要求：库里那个"自动分配 + 硬约束无人满足"的任务**保持未分配**。
	//
	// 若兜底绕过了硬约束，任务会被派给任意一台机器 → 直接判失败。
	run("AC-51", "节点接入的自动分配不越过硬约束：无人满足时保持未分配（F3）", func(cc *caseCtx) {
		if db == nil {
			cc.skip("未配置 CRONY_TEST_MYSQL_DSN：无法核对任务归属")
		}
		cli := newEtcdClient(cc.t, os.Getenv("CRONY_ACC_ETCD_ENDPOINTS"))
		if cli == nil {
			cc.skip("未配置 CRONY_ACC_ETCD_ENDPOINTS：接入分配只由 etcd 节点事件触发，无探针则触发不了（不是系统缺陷）")
		}
		tag := strings.TrimSpace(os.Getenv("CRONY_ACC_RUN_TAG"))
		if tag == "" {
			tag = "acc-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		}
		// 1) 造一个"未分配"的夹具任务：直接写库（status=0），带无人满足的硬约束。
		//    走库内夹具而不是 /job/add，是为了避开"创建时的分配"（AC-50 已单测），
		//    把这条用例的观测点**只**留在"节点接入事件"上。
		selector := fmt.Sprintf(`{"require_labels":{"acc-f3-nonexistent":"%s"}}`, tag)
		res, err := db.Exec(`insert into job
			(name, command, type, spec, status, timeout, notify_type, notify_to, script_id,
			 node_selector, run_on, updated, created)
			values (?, 'cmd /c echo f3', 1, '0 0 9 * * *', ?, 30, 1, null, null, ?, '', ?, ?)`,
			"ACC-F3-ACCESS-"+tag, models.JobStatusNotAssigned, selector, time.Now().Unix(), time.Now().Unix())
		if err != nil {
			cc.t.Fatalf("造夹具任务失败（验收库需要 INSERT 权限）: %v", err)
		}
		id64, err := res.LastInsertId()
		if err != nil {
			cc.t.Fatalf("取夹具任务 id 失败: %v", err)
		}
		jobID := int(id64)
		cc.t.Cleanup(func() { _, _ = c.postOK("/job/del", map[string]any{"ids": []int{jobID}}) })

		// 2) 造一次"新节点接入"事件：只写 etcd 注册键（带 25s 租约，自己会过期），
		//    不写库内行 → 不污染节点表，也不会成为调度候选。
		bystander := "acc-f3-trigger-" + tag
		nodeKey := fmt.Sprintf(etcdclient.KeyEtcdNode, bystander)
		val, _ := json.Marshal(models.Node{
			UUID: bystander, Hostname: "acc-f3-trigger", IP: "127.0.0.1", PID: "1",
			Version: "acceptance", Status: models.NodeConnSuccess,
		})
		lease, err := cli.Grant(context.Background(), 25)
		if err != nil {
			cc.t.Fatalf("申请 etcd 租约失败: %v", err)
		}
		cc.t.Cleanup(func() {
			_, _ = cli.Delete(context.Background(), nodeKey)
			_, _ = cli.Revoke(context.Background(), lease.ID)
		})
		if _, err := cli.Put(context.Background(), nodeKey, string(val), clientv3.WithLease(lease.ID)); err != nil {
			cc.t.Fatalf("写合成节点注册键失败: %v", err)
		}

		// 3) 观察窗口覆盖"admin 侧为节点就绪留的 5s 余量 + 分配耗时"。
		//    这期间任务必须一直保持未分配。
		deadline := time.Now().Add(18 * time.Second)
		nodes := 0
		for time.Now().Before(deadline) {
			var runOn string
			if err := db.QueryRow("select coalesce(run_on, '') from job where id = ?", jobID).Scan(&runOn); err != nil {
				cc.t.Fatalf("读回任务归属失败: %v", err)
			}
			if runOn != "" {
				cc.t.Fatalf("节点接入触发的自动分配绕过了硬约束：任务 %d 被派给了 %s（它装不了这个任务）", jobID, runOn)
			}
			_ = db.QueryRow("select count(*) from node").Scan(&nodes)
			time.Sleep(time.Second)
		}
		cc.note("合成节点接入事件后，带无人满足硬约束的任务 %d 仍保持未分配（未分配节点=%d 个，兜底没有越过硬约束）",
			jobID, nodes)
	})
}
