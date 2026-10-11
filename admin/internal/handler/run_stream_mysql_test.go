//go:build integration

package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/middlerware"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
)

// 真实 MySQL 下的执行事实订阅测试。
//
// 为什么假源不够：假源绕过了 SQL，而这里最容易错的就是 SQL——
// 列名、`step_index > ?` 的增量条件、`ORDER BY step_index`、状态字段口径。
// 阶段 2 的教训很直接：三个只有真库才暴露的缺陷（1406/1366/空 system_prompt），
// 内存单测全绿。订阅接口同理，必须有一条真库用例钉住它的查询面。
//
// 运行：设置 CRONY_TEST_MYSQL_DSN 并加 -tags integration；未设置则跳过（默认 go test 不碰数据库）。
func TestIntegrationRunStreamAgainstRealMySQL(t *testing.T) {
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 订阅测试")
	}
	if _, err := dbclient.Init(dsn, "silent", 1, 5); err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db := dbclient.GetMysqlDB()
	if err := db.AutoMigrate(&models.JobRun{}, &models.JobRunStep{}); err != nil {
		t.Fatalf("建表失败（测试库需要 CREATE 权限）: %v", err)
	}
	// 只动本次运行自己写入的行：唯一前缀 + 结束前按前缀删除，绝不 truncate。
	tag := fmt.Sprintf("it-stream-%d", time.Now().UnixNano())
	runID := tag + "-r1"
	now := time.Now().Unix()
	defer func() {
		db.Exec("DELETE FROM "+models.CronyJobRunStepTableName+" WHERE run_id LIKE ?", tag+"%")
		db.Exec("DELETE FROM "+models.CronyJobRunTableName+" WHERE run_id LIKE ?", tag+"%")
	}()

	// 起始状态：running + 第 0 步
	run := &models.JobRun{
		RunID: runID, JobID: 900101, JobName: tag, Spec: "0 0 2 * * *",
		Command: "echo hi", JobType: 1, NodeUUID: tag, Status: int(models.RunStatusRunning),
		TriggerSource: int(models.TriggerManual), ScheduledTime: now, StartedAt: now, Created: now,
	}
	if err := db.Table(models.CronyJobRunTableName).Create(run).Error; err != nil {
		t.Fatalf("写入执行记录失败: %v", err)
	}
	step := func(idx int, status models.RunStatus) *models.JobRunStep {
		return &models.JobRunStep{
			RunID: runID, StepIndex: idx, Name: fmt.Sprintf("step-%d", idx), Kind: "shell",
			Condition: "on_success", Command: "echo hi", Status: int(status), Created: time.Now().Unix(),
		}
	}
	if err := db.Table(models.CronyJobRunStepTableName).Create(step(0, models.RunStatusSuccess)).Error; err != nil {
		t.Fatalf("写入步骤失败: %v", err)
	}

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/job/run/stream", nil)

	// 用生产实现（dbRunFactSource → dbStepsQuery），只把轮询间隔压到 20ms 让测试跑得快。
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunStreamWith(c, runStreamConfig{
			RunID: runID, After: -1, Poll: 20 * time.Millisecond, MaxLife: 20 * time.Second,
		}, dbRunFactSource{svc: service.DefaultJobRunService})
	}()

	// 模拟执行推进：新增第 1 步 → 变成终态成功
	time.Sleep(150 * time.Millisecond)
	if err := db.Table(models.CronyJobRunStepTableName).Create(step(1, models.RunStatusSuccess)).Error; err != nil {
		t.Fatalf("写入第 1 步失败: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := db.Table(models.CronyJobRunTableName).Where("run_id = ?", runID).Updates(map[string]interface{}{
		"status": int(models.RunStatusSuccess), "exit_code": 0, "finished_at": time.Now().Unix(),
		"step_count": 2, "success_steps": 2, "duration_ms": 300, "output": "hi",
	}).Error; err != nil {
		t.Fatalf("更新执行记录失败: %v", err)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("订阅未在 15s 内结束（真库模式下终态必须主动 end）")
	}

	raw := rec.Body.String()
	frames := parseSSE(t, raw)
	if len(frames) < 4 {
		t.Fatalf("真库订阅帧数过少：%d（报文：%s）", len(frames), raw)
	}
	if frames[0].Event != "snapshot" || frames[0].Data.Run == nil || frames[0].Data.Run.RunID != runID {
		t.Fatalf("首帧应是本执行的 snapshot，实际 %+v", frames[0])
	}
	if frames[0].Data.Run.Status != int(models.RunStatusRunning) {
		t.Fatalf("首帧状态应为 running，实际 %d", frames[0].Data.Run.Status)
	}

	var sawStep1, sawFinalRun, sawEnd bool
	for _, f := range frames {
		switch f.Event {
		case "step":
			if len(f.Data.Steps) == 1 && f.Data.Steps[0].StepIndex == 1 {
				sawStep1 = true
			}
			// 增量条件必须真的生效：不能把 step_index=0 重复推一遍
			if len(f.Data.Steps) == 1 && f.Data.Steps[0].StepIndex == 0 {
				// step_index=0 只在首轮补齐时出现一次；出现两次说明增量条件写错
				count := 0
				for _, g := range frames {
					if g.Event == "step" && len(g.Data.Steps) == 1 && g.Data.Steps[0].StepIndex == 0 {
						count++
					}
				}
				if count > 1 {
					t.Fatalf("step_index=0 被推送了 %d 次，增量条件（step_index > ?）失效", count)
				}
			}
		case "run":
			if f.Data.Run != nil && f.Data.Run.Status == int(models.RunStatusSuccess) {
				sawFinalRun = true
				if f.Data.Run.Output != "hi" || f.Data.Run.SuccessSteps != 2 {
					t.Fatalf("终态 run 事件字段不完整（真库读取口径有问题）：%+v", f.Data.Run)
				}
			}
		case "end":
			sawEnd = true
			if f.Data.Reason != "final" || f.Data.Status != "success" {
				t.Fatalf("end 事件应说明 final/success，实际 %+v", f.Data)
			}
		}
	}
	if !sawStep1 {
		t.Fatalf("未收到新增的 step_index=1 事件（增量查询有问题）：%s", raw)
	}
	if !sawFinalRun {
		t.Fatalf("未收到终态 run 事件：%s", raw)
	}
	if !sawEnd {
		t.Fatalf("未收到 end 事件：%s", raw)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Content-Type 应为 SSE，实际 %q", rec.Header().Get("Content-Type"))
	}
}

// TestIntegrationRunStreamOverHTTPWithJWT：把整条链路打通——
// 真实 TCP + gin 路由 + JWTAuth 中间件 + 真库数据源 + SSE 分帧。
//
// 为什么不能只靠 httptest.ResponseRecorder：recorder 是内存写入，
// 它不经过 net/http 的 chunked 传输，不会暴露"被中间层缓冲导致前端收不到增量"这类问题；
// 也不验证 JWT 头解析。这个用例用一个真的 HTTP 客户端边读边等，
// 语义上最接近"浏览器 fetch 流式读、服务端持续 flush"。
func TestIntegrationRunStreamOverHTTPWithJWT(t *testing.T) {
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 订阅测试")
	}
	if _, err := dbclient.Init(dsn, "silent", 1, 5); err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db := dbclient.GetMysqlDB()
	if err := db.AutoMigrate(&models.JobRun{}, &models.JobRunStep{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	tag := fmt.Sprintf("it-http-%d", time.Now().UnixNano())
	runID := tag + "-r1"
	now := time.Now().Unix()
	defer func() {
		db.Exec("DELETE FROM "+models.CronyJobRunStepTableName+" WHERE run_id LIKE ?", tag+"%")
		db.Exec("DELETE FROM "+models.CronyJobRunTableName+" WHERE run_id LIKE ?", tag+"%")
	}()

	if err := db.Table(models.CronyJobRunTableName).Create(&models.JobRun{
		RunID: runID, JobID: 900102, JobName: tag, Spec: "0 0 2 * * *",
		Command: "echo hi", JobType: 1, NodeUUID: tag, Status: int(models.RunStatusRunning),
		TriggerSource: int(models.TriggerManual), ScheduledTime: now, StartedAt: now, Created: now,
	}).Error; err != nil {
		t.Fatalf("写入执行记录失败: %v", err)
	}
	stepOf := func(idx int) *models.JobRunStep {
		return &models.JobRunStep{
			RunID: runID, StepIndex: idx, Name: fmt.Sprintf("step-%d", idx), Kind: "shell",
			Condition: "on_success", Command: "echo hi", Status: int(models.RunStatusSuccess),
			Created: time.Now().Unix(),
		}
	}
	if err := db.Table(models.CronyJobRunStepTableName).Create(stepOf(0)).Error; err != nil {
		t.Fatalf("写入步骤失败: %v", err)
	}

	// 用进程内签名密钥自身签发令牌：避免依赖环境变量与 sync.Once 的初始化顺序。
	key := middlerware.LoadSigningKey()
	signer := &middlerware.JWT{SigningKey: key}
	token, err := signer.CreateToken(signer.CreateClaims(middlerware.BaseClaims{
		ID: 1, UserName: "it-admin", Role: models.RoleAdmin,
	}))
	if err != nil {
		t.Fatalf("签发测试令牌失败: %v", err)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterRouters(engine)
	srv := httptest.NewServer(engine)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/job/run/stream?run_id="+runID+"&interval_ms=500&max_seconds=30", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发起订阅请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP 状态 = %d（%s），want 200", resp.StatusCode, string(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("Content-Type = %q，want text/event-stream（响应体：%s）", ct, string(body))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q，want no-store（SSE 不能被缓存）", resp.Header.Get("Cache-Control"))
	}

	// 边读边推进：读到首个 step 后再写第 1 步，最后置终态。
	// 这里手工做一个极简行状态机（event:/data: 两行一帧），
	// 不引入第二套"完整解析器"实现——前端那份解析器由 verify-phase2.mjs 覆盖。
	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(20 * time.Second)
	var events []string
	var sawStep0, sawStep1, sawEnd bool
	advanced, finished := false, false
	curEvent := ""
	for time.Now().Before(deadline) && !sawEnd {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("读取流失败: %v", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "event: "):
			curEvent = strings.TrimPrefix(trimmed, "event: ")
			events = append(events, curEvent)
		case strings.HasPrefix(trimmed, "data: "):
			var payload sseEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(trimmed, "data: ")), &payload); err != nil {
				t.Fatalf("data 行不是合法 JSON（%q）: %v", trimmed, err)
			}
			for _, s := range payload.Steps {
				switch s.StepIndex {
				case 0:
					sawStep0 = true
				case 1:
					sawStep1 = true
				}
			}
			if curEvent == "end" {
				sawEnd = true
				if payload.Reason != "final" {
					t.Fatalf("end.reason = %q，want final", payload.Reason)
				}
			}
		}
		if sawStep0 && !advanced {
			advanced = true
			if err := db.Table(models.CronyJobRunStepTableName).Create(stepOf(1)).Error; err != nil {
				t.Fatalf("写入第 1 步失败: %v", err)
			}
		}
		if sawStep1 && !finished {
			finished = true
			if err := db.Table(models.CronyJobRunTableName).Where("run_id = ?", runID).Updates(map[string]interface{}{
				"status": int(models.RunStatusSuccess), "finished_at": time.Now().Unix(),
				"step_count": 2, "success_steps": 2, "duration_ms": 120,
			}).Error; err != nil {
				t.Fatalf("更新执行记录失败: %v", err)
			}
		}
	}
	if !sawStep0 || !sawStep1 || !sawEnd {
		t.Fatalf("HTTP 订阅链路不完整：step0=%v step1=%v end=%v（事件序列：%v）",
			sawStep0, sawStep1, sawEnd, events)
	}
	if events[0] != "snapshot" {
		t.Fatalf("首帧应为 snapshot，实际 %v", events)
	}
	t.Logf("HTTP SSE 事件序列：%v", events)
}
func TestIntegrationRunStreamUnknownRunKeepsWaiting(t *testing.T) {
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 订阅测试")
	}
	if _, err := dbclient.Init(dsn, "silent", 1, 5); err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := dbclient.GetMysqlDB().AutoMigrate(&models.JobRun{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	old := runStreamHeartbeatTicks
	runStreamHeartbeatTicks = 2
	defer func() { runStreamHeartbeatTicks = old }()

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/job/run/stream", nil)

	cfg := runStreamConfig{
		RunID: fmt.Sprintf("it-missing-%d", time.Now().UnixNano()),
		After: -1, Poll: 20 * time.Millisecond, MaxLife: 150 * time.Millisecond,
	}
	RunStreamWith(c, cfg, dbRunFactSource{svc: service.DefaultJobRunService})

	frames := parseSSE(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatal("未收到任何帧")
	}
	for _, f := range frames {
		if f.Event == "error" {
			t.Fatalf("执行尚未落库不应报错（真库下 gorm.ErrRecordNotFound 必须被识别）：%s", rec.Body.String())
		}
	}
	if frames[0].Event != "heartbeat" {
		t.Fatalf("等待期间应发心跳，首帧实际 %s", frames[0].Event)
	}
	last := frames[len(frames)-1]
	if last.Event != "end" || last.Data.Reason != "timeout" {
		t.Fatalf("寿命到点应 end(timeout)，实际 %+v", last)
	}
}
