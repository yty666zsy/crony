package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/common/models"
	"gorm.io/gorm"
)

// 执行事实订阅（SSE）的无数据库测试。
//
// 为什么必须有无数据库版本：SSE 的价值全在"帧序列"上——首帧 snapshot、只推增量、
// 无变化发心跳、终态主动 end。这些语义如果只靠真库测，回归一次要起 MySQL，
// 没人会在改前端时跑它，于是悄悄退化。这里用可注入的假事实源把语义钉死。

// fakeRunFactSource 按"每次轮询推进一档"的脚本化事实源。
//
// 关键细节：handler 一轮要读两次（先 run，再取该 run 的步骤增量），所以假源必须保证
// **同一次轮询内的 run 与步骤来自同一档**，否则测的是假源的时序错乱而非 handler 的语义。
type fakeRunFactSource struct {
	mu      sync.Mutex
	scripts []fakeState // 每次轮询消耗一档；用完后一直返回最后一档
	idx     int         // 已开始的轮询档位（LoadRun 消费后 +1）
	loadErr error
}

type fakeState struct {
	run   *models.JobRun
	steps []models.JobRunStep
	// err 非空表示这一次读取返回该错误（用于错误/未落库路径）。
	err error
}

// pick 取当前档位；advance=true 表示这是一轮的开始（LoadRun），取完推进指针。
func (f *fakeRunFactSource) pick(advance bool) fakeState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return fakeState{err: f.loadErr}
	}
	if len(f.scripts) == 0 {
		return fakeState{err: fmt.Errorf("无脚本")}
	}
	i := f.idx
	if advance {
		if i < len(f.scripts) {
			f.idx++
		}
	} else if i > 0 {
		i-- // 同一轮内的第二次读取：回退到本轮那一档
	}
	if i >= len(f.scripts) {
		i = len(f.scripts) - 1
	}
	return f.scripts[i]
}

func (f *fakeRunFactSource) LoadRun(string) (*models.JobRun, error) {
	st := f.pick(true)
	if st.err != nil {
		return nil, st.err
	}
	return st.run, nil
}

func (f *fakeRunFactSource) LatestRunByJob(int) (*models.JobRun, error) {
	return f.LoadRun("")
}

func (f *fakeRunFactSource) LoadSteps(_ string, after int) ([]models.JobRunStep, error) {
	st := f.pick(false)
	if st.err != nil {
		return nil, st.err
	}
	out := make([]models.JobRunStep, 0, len(st.steps))
	for _, s := range st.steps {
		if s.StepIndex > after {
			out = append(out, s)
		}
	}
	return out, nil
}

// runWithStream 同步跑完一次订阅并返回 SSE 原始报文。
func runWithStream(t *testing.T, cfg runStreamConfig, src RunFactSource) (string, time.Duration) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/job/run/stream", nil)
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStreamWith(c, cfg, src)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("订阅没有在 10s 内结束：终态/断开必须主动关闭连接")
	}
	return rec.Body.String(), time.Since(start)
}

// parseSSE 把报文解析成 (event, data) 列表。
func parseSSE(t *testing.T, raw string) []struct {
	Event string
	Data  sseEvent
} {
	t.Helper()
	out := make([]struct {
		Event string
		Data  sseEvent
	}, 0)
	for _, frame := range strings.Split(raw, "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		var (
			event string
			data  string
		)
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event == "" {
			t.Fatalf("SSE 帧缺少 event 行：%q", frame)
		}
		var payload sseEvent
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("SSE data 不是合法 JSON（%v）：%q", err, data)
		}
		out = append(out, struct {
			Event string
			Data  sseEvent
		}{event, payload})
	}
	return out
}

func runRow(runID string, status models.RunStatus) *models.JobRun {
	return &models.JobRun{
		RunID: runID, JobID: 42, JobName: "backup", Spec: "0 0 2 * * *",
		Status: int(status), StartedAt: 100, StepCount: 1, Created: 100,
	}
}

// TestRunStreamEmitsSnapshotThenIncrements：帧序列是契约的核心。
// 期望：snapshot（含已有步骤）→ 新步骤 step → run 状态变化 → end(final)。
func TestRunStreamEmitsSnapshotThenIncrements(t *testing.T) {
	running := runRow("j42-t100", models.RunStatusRunning)
	success := runRow("j42-t100", models.RunStatusSuccess)
	success.FinishedAt = 160
	success.ExitCode = 0
	success.StepCount = 2
	success.SuccessSteps = 2

	src := &fakeRunFactSource{scripts: []fakeState{
		{run: running, steps: []models.JobRunStep{{RunID: "j42-t100", StepIndex: 0, Name: "dump", Kind: "shell", Status: int(models.RunStatusSuccess)}}},
		{run: running, steps: []models.JobRunStep{
			{RunID: "j42-t100", StepIndex: 0, Name: "dump", Kind: "shell"},
			{RunID: "j42-t100", StepIndex: 1, Name: "upload", Kind: "shell"},
		}},
		{run: success, steps: []models.JobRunStep{{RunID: "j42-t100", StepIndex: 0}, {RunID: "j42-t100", StepIndex: 1}}},
	}}

	raw, elapsed := runWithStream(t, runStreamConfig{
		RunID: "j42-t100", After: -1, Poll: 2 * time.Millisecond, MaxLife: 5 * time.Second,
	}, src)
	frames := parseSSE(t, raw)

	var kinds []string
	for _, f := range frames {
		kinds = append(kinds, f.Event)
	}
	want := []string{"snapshot", "step", "step", "run", "end"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("帧序列 = %v，want %v（原始报文：%s）", kinds, want, raw)
	}
	if frames[0].Data.Run == nil || frames[0].Data.Run.RunID != "j42-t100" {
		t.Fatalf("首帧必须是带 run 的全量快照，实际 %+v", frames[0].Data)
	}
	// 首帧只带 run；已有步骤紧随其后以 step 事件逐个补齐（前端按 order 追加即可收敛）。
	// 契约：run 与 step 分开推，前端先更新头部状态再追加时间线。
	if len(frames[0].Data.Steps) != 0 {
		t.Fatalf("snapshot 帧不应内嵌步骤（步骤走独立 step 事件）：%+v", frames[0].Data.Steps)
	}
	// 步骤按 step_index 严格递增推送：已有的 0 先补，第二轮只推新增的 1。
	if frames[1].Data.Steps[0].StepIndex != 0 {
		t.Fatalf("snapshot 之后应先补既有的 step_index=0：%+v", frames[1].Data.Steps)
	}
	if frames[2].Data.Steps[0].StepIndex != 1 {
		t.Fatalf("第二轮必须只推新增的 step_index=1（增量语义被破坏，可能重复推了旧步骤）：%+v", frames[2].Data.Steps)
	}
	// seq 必须自增且连续
	for i, f := range frames {
		if f.Data.Seq != int64(i+1) {
			t.Fatalf("第 %d 帧 seq = %d，want %d", i, f.Data.Seq, i+1)
		}
	}
	end := frames[len(frames)-1].Data
	if end.Reason != "final" || end.Status != "success" {
		t.Fatalf("end 事件必须说明终态，实际 %+v", end)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("终态后必须立刻关闭连接，实际耗时 %s", elapsed)
	}
}

// TestRunStreamOnlyPushesChangesAndHeartbeats：没有变化时**不能**重复推 run，
// 但必须定期发心跳（否则中间代理会掐断空闲连接）。
func TestRunStreamOnlyPushesChangesAndHeartbeats(t *testing.T) {
	old := runStreamHeartbeatTicks
	runStreamHeartbeatTicks = 2
	defer func() { runStreamHeartbeatTicks = old }()

	running := runRow("j42-t100", models.RunStatusRunning)
	st := fakeState{run: running, steps: []models.JobRunStep{{RunID: "j42-t100", StepIndex: 0}}}
	success := runRow("j42-t100", models.RunStatusSuccess)
	src := &fakeRunFactSource{scripts: []fakeState{
		st, st, st, st, st, st, // 连续 6 轮无变化 → 应出现 2 次心跳（阈值 2）
		{run: success, steps: st.steps},
	}}

	raw, _ := runWithStream(t, runStreamConfig{
		RunID: "j42-t100", After: -1, Poll: 2 * time.Millisecond, MaxLife: 5 * time.Second,
	}, src)
	frames := parseSSE(t, raw)

	var snapshots, heartbeats, runs int
	for _, f := range frames {
		switch f.Event {
		case "snapshot":
			snapshots++
		case "heartbeat":
			heartbeats++
		case "run":
			runs++
		}
	}
	if snapshots != 1 {
		t.Fatalf("snapshot 只能出现在首帧，实际 %d 次", snapshots)
	}
	if runs != 1 {
		t.Fatalf("状态只变了一次，run 事件应恰好 1 次（不能因无关字段抖动重复推），实际 %d 次", runs)
	}
	if heartbeats < 2 {
		t.Fatalf("无变化时必须按阈值发心跳，实际 %d 次（报文：%s）", heartbeats, raw)
	}
	// 心跳不能携带 run/steps，避免前端把心跳当成数据更新
	for _, f := range frames {
		if f.Event == "heartbeat" && (f.Data.Run != nil || len(f.Data.Steps) != 0) {
			t.Fatalf("心跳帧不应携带数据：%+v", f.Data)
		}
	}
}

// TestRunStreamJobModeFollowsLatestRun：job_id 模式下任务被再次触发时，
// 必须切到新执行、**重置步骤游标**，并且换成 snapshot（重置语义：
// 前端据此丢弃上一次执行的步骤，否则两条执行的步骤会混在一条时间线上）。
func TestRunStreamJobModeFollowsLatestRun(t *testing.T) {
	first := runRow("j42-t100", models.RunStatusRunning)
	second := runRow("j42-t101", models.RunStatusRunning)
	stepOf := func(runID string, idx int) []models.JobRunStep {
		return []models.JobRunStep{{RunID: runID, StepIndex: idx}}
	}
	src := &fakeRunFactSource{scripts: []fakeState{
		{run: first, steps: stepOf("j42-t100", 0)},  // t100 的第 0 步
		{run: first, steps: stepOf("j42-t100", 0)},  // 无变化
		{run: second, steps: stepOf("j42-t101", 0)}, // 切到 t101：必须重置游标，重推 step 0
		{run: runRow("j42-t101", models.RunStatusSuccess), steps: stepOf("j42-t101", 0)},
	}}

	raw, _ := runWithStream(t, runStreamConfig{
		JobID: 42, After: -1, Poll: 2 * time.Millisecond, MaxLife: 5 * time.Second,
	}, src)
	frames := parseSSE(t, raw)

	var snapshots []string
	var stepsByRun = map[string][]int{}
	for _, f := range frames {
		if f.Data.Run != nil && f.Event == "snapshot" {
			snapshots = append(snapshots, f.Data.Run.RunID)
		}
		for _, s := range f.Data.Steps {
			stepsByRun[s.RunID] = append(stepsByRun[s.RunID], s.StepIndex)
		}
	}
	if len(snapshots) != 2 || snapshots[0] != "j42-t100" || snapshots[1] != "j42-t101" {
		t.Fatalf("首帧与换执行时都应发 snapshot（重置语义），实际 %v（报文：%s）", snapshots, raw)
	}
	if got := stepsByRun["j42-t101"]; len(got) == 0 || got[0] != 0 {
		t.Fatalf("切到新执行后步骤游标必须归零（必须重推 t101 的 step 0），实际 %v", stepsByRun)
	}
	if got := stepsByRun["j42-t100"]; len(got) != 1 || got[0] != 0 {
		t.Fatalf("t100 的第 0 步只应推送一次，实际 %v", got)
	}
	last := frames[len(frames)-1]
	if last.Event != "end" || last.Data.Reason != "final" {
		t.Fatalf("job_id 模式下执行已终态时必须 end(final)，实际 %+v", last)
	}
}

// TestRunStreamRunIDModeKeepsWaitingWhenNotPersisted：执行还没落库（节点刚触发）
// 不是错误，必须继续等而不是报错/断开——否则前端会陷入"重连风暴"。
func TestRunStreamRunIDModeKeepsWaitingWhenNotPersisted(t *testing.T) {
	old := runStreamHeartbeatTicks
	runStreamHeartbeatTicks = 2
	defer func() { runStreamHeartbeatTicks = old }()

	running := runRow("j42-t100", models.RunStatusRunning)
	src := &fakeRunFactSource{scripts: []fakeState{
		{err: fmt.Errorf("wrapped: %w", gorm.ErrRecordNotFound)},
		{err: fmt.Errorf("wrapped: %w", gorm.ErrRecordNotFound)},
		{run: running, steps: []models.JobRunStep{{RunID: "j42-t100", StepIndex: 0}}},
		{run: running, steps: []models.JobRunStep{{RunID: "j42-t100", StepIndex: 0}}},
		{run: runRow("j42-t100", models.RunStatusFailed), steps: []models.JobRunStep{{RunID: "j42-t100", StepIndex: 0}}},
	}}
	raw, _ := runWithStream(t, runStreamConfig{
		RunID: "j42-t100", After: -1, Poll: 2 * time.Millisecond, MaxLife: 5 * time.Second,
	}, src)
	frames := parseSSE(t, raw)
	if len(frames) == 0 || frames[0].Event != "heartbeat" {
		t.Fatalf("未落库期间应发心跳等待，首帧实际 %+v（报文：%s）", frames[0], raw)
	}
	last := frames[len(frames)-1]
	if last.Event != "end" || last.Data.Status != "failed" {
		t.Fatalf("执行最终失败时必须 end(failed)，实际 %+v", last)
	}
}

// TestRunStreamSourceErrorEndsWithError：数据源故障要显式告知并关闭，
// 不能静默变成"永远转圈的界面"。
func TestRunStreamSourceErrorEndsWithError(t *testing.T) {
	src := &fakeRunFactSource{loadErr: fmt.Errorf("db is down")}
	raw, _ := runWithStream(t, runStreamConfig{
		RunID: "j42-t100", After: -1, Poll: 2 * time.Millisecond, MaxLife: 5 * time.Second,
	}, src)
	frames := parseSSE(t, raw)
	if frames[0].Event != "error" || frames[1].Event != "end" {
		t.Fatalf("数据源故障应为 error+end，实际 %s", raw)
	}
	if strings.Contains(raw, "db is down") {
		t.Fatal("错误详情不能把底层数据库错误原文透给前端")
	}
	if frames[1].Data.Reason != "error" {
		t.Fatalf("end.reason = %q，want error", frames[1].Data.Reason)
	}
}

// TestRunStreamMaxLifeEnds：连接寿命到点必须主动结束（否则就是一条永不关闭的轮询）。
func TestRunStreamMaxLifeEnds(t *testing.T) {
	running := runRow("j42-t100", models.RunStatusRunning)
	src := &fakeRunFactSource{scripts: []fakeState{{run: running, steps: []models.JobRunStep{{RunID: "j42-t100", StepIndex: 0}}}}}
	raw, elapsed := runWithStream(t, runStreamConfig{
		RunID: "j42-t100", After: -1, Poll: 5 * time.Millisecond, MaxLife: 120 * time.Millisecond,
	}, src)
	frames := parseSSE(t, raw)
	last := frames[len(frames)-1]
	if last.Event != "end" || last.Data.Reason != "timeout" {
		t.Fatalf("寿命到点应 end(timeout)，实际 %+v", last)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("超时结束不够及时：%s", elapsed)
	}
}

// TestRunStreamHeaderAndClientDisconnect：SSE 必需的响应头 + 客户端断开后立即返回。
func TestRunStreamHeaderAndClientDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/job/run/stream", nil).WithContext(ctx)
	c.Request = req

	running := runRow("j42-t100", models.RunStatusRunning)
	src := &fakeRunFactSource{scripts: []fakeState{{run: running}}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStreamWith(c, runStreamConfig{RunID: "j42-t100", After: -1, Poll: 10 * time.Millisecond, MaxLife: time.Minute}, src)
	}()
	// 等到首帧写出后再断开，确保测的是"运行中断开"而不是"还没开始就取消"。
	deadline := time.Now().Add(2 * time.Second)
	for rec.Body.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("客户端断开后订阅必须立即返回（否则连接与 goroutine 一起泄漏）")
	}
	h := rec.Header()
	if !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Content-Type = %q", h.Get("Content-Type"))
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q（SSE 必须禁缓存）", h.Get("Cache-Control"))
	}
	if h.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("X-Accel-Buffering = %q（不关掉 nginx 缓冲，SSE 会退化成一次性输出）", h.Get("X-Accel-Buffering"))
	}
}

// TestParseRunStreamConfig：参数收敛规则（含越界取边界、缺参报错）。
func TestParseRunStreamConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		query   string
		wantErr bool
		check   func(t *testing.T, cfg runStreamConfig)
	}{
		{query: "", wantErr: true},
		{query: "job_id=abc", wantErr: true},
		{query: "job_id=0", wantErr: true},
		{query: "run_id=r1", check: func(t *testing.T, cfg runStreamConfig) {
			if cfg.Poll != time.Second || cfg.After != -1 {
				t.Fatalf("默认值错：%+v", cfg)
			}
		}},
		{query: "run_id=r1&interval_ms=1", check: func(t *testing.T, cfg runStreamConfig) {
			if cfg.Poll != runStreamIntervalMin {
				t.Fatalf("低于下限必须收敛到下限，实际 %s", cfg.Poll)
			}
		}},
		{query: "run_id=r1&interval_ms=999999", check: func(t *testing.T, cfg runStreamConfig) {
			if cfg.Poll != runStreamIntervalMax {
				t.Fatalf("高于上限必须收敛到上限，实际 %s", cfg.Poll)
			}
		}},
		{query: "run_id=r1&max_seconds=1", check: func(t *testing.T, cfg runStreamConfig) {
			if cfg.MaxLife != 30*time.Second {
				t.Fatalf("寿命下限应为 30s，实际 %s", cfg.MaxLife)
			}
		}},
		{query: "run_id=r1&after_step=3", check: func(t *testing.T, cfg runStreamConfig) {
			if cfg.After != 3 {
				t.Fatalf("after_step 未生效：%+v", cfg)
			}
		}},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/job/run/stream?"+tc.query, nil)
		cfg, err := parseRunStreamConfig(c)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("query=%q 应报错", tc.query)
			}
			continue
		}
		if err != nil {
			t.Fatalf("query=%q 不应报错：%v", tc.query, err)
		}
		tc.check(t, cfg)
	}
}

// TestRunStreamRouteRegistered：路由必须真的挂上。
//
// 这类"新接口忘了注册"的疏漏在编译期与单测里都不会报错，
// 只有到前端联调时才发现 404——所以用注册表本身做断言。
func TestRunStreamRouteRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterRouters(r)
	found := false
	for _, info := range r.Routes() {
		if info.Method == http.MethodGet && info.Path == "/job/run/stream" {
			found = true
		}
	}
	if !found {
		var runRoutes []string
		for _, info := range r.Routes() {
			if strings.Contains(info.Path, "/job/run") {
				runRoutes = append(runRoutes, info.Method+" "+info.Path)
			}
		}
		t.Fatalf("GET /job/run/stream 未注册；当前 run 相关路由：%v", runRoutes)
	}
}
