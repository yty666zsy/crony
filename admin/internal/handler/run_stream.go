package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
)

// 执行事实订阅（SSE）——阶段 2 收尾补齐的"稳定查询/**订阅**"接口。
//
// 为什么需要它：`/job/run/find`、`/job/run/steps` 只能轮询。轮询有两个真实问题：
//   - 一次执行可能几十秒到几十分钟，前端要么轮询过密（一直打库），要么过疏（看不到过程）；
//   - "这次执行现在到哪一步了"是运维最常盯的界面，需要的是**推送**。
//
// 设计立场（诚实说明，不吹成"实时消息总线"）：
//   - **没有进程内事件总线**：数据源就是数据库（job_run / job_run_step）。
//     节点与 admin 是不同进程，执行事实的唯一真相本来就在库里；引入内存总线会带来
//     "admin 重启丢事件""多实例只推自己收到的那份"这类更糟的语义。
//   - 服务端按固定间隔（默认 1s）**对比快照差异**，只推变化（run 变化 / 新增步骤），
//     无变化时只发心跳。因此延迟上界 = 轮询间隔：明确、可解释、可调。
//   - 首帧一定是全量 `snapshot`，之后才是增量；因此**断线重连必然收敛**。
//     但服务端不做历史事件重放（阶段 3 引入事件表后再谈）。
//   - 终态（success/failed/canceled/skipped/timeout）后主动发 `end` 并关闭连接，
//     不让浏览器挂着一条空流。
//   - 认证沿用 JWT：`EventSource` 不能带自定义头，所以前端必须用
//     **fetch + ReadableStream** 解析 SSE（见 console/src/api/stream.js），不要用 `new EventSource`。
//
// 契约（对齐 PHASE2-接口契约.md 第 9 节）：
//
//	GET /job/run/stream?run_id=xxx|job_id=123[&interval_ms=1000][&max_seconds=600][&after_step=-1]
//
//	event: snapshot | run | step | heartbeat | error | end
//	id: <连接内自增序号>
//	data: {"seq":1,"kind":"snapshot","server_time":1712345678,"run":{…},"steps":[…]}
type RunFactSource interface {
	// LoadRun 按 run_id 读取执行事实；不存在返回 models.IsRecordNotFound 可识别的错误。
	LoadRun(runID string) (*models.JobRun, error)
	// LatestRunByJob 取任务最近一次执行（job_id 订阅模式用）。
	LatestRunByJob(jobID int) (*models.JobRun, error)
	// LoadSteps 读取步骤增量：只返回 step_index > afterStep 的行，按 step_index 升序。
	LoadSteps(runID string, afterStep int) ([]models.JobRunStep, error)
}

// dbRunFactSource 生产实现：走既有 service 与同一张表，
// 保证"订阅到的步骤顺序"与 `/job/run/steps` 读到的完全一致。
type dbRunFactSource struct {
	svc *service.JobRunService
}

func (s dbRunFactSource) LoadRun(runID string) (*models.JobRun, error) {
	return s.svc.FindByRunID(runID)
}

func (s dbRunFactSource) LatestRunByJob(jobID int) (*models.JobRun, error) {
	return s.svc.FindLatestByJob(jobID)
}

func (s dbRunFactSource) LoadSteps(runID string, afterStep int) ([]models.JobRunStep, error) {
	return dbStepsQuery(runID, afterStep)
}

// dbStepsQuery 步骤增量查询：`step_index > after` 升序，单次上限 500 行
// （一次执行不会有几千步；上限是防御性的，避免异常数据把单帧撑爆）。
func dbStepsQuery(runID string, afterStep int) ([]models.JobRunStep, error) {
	steps := make([]models.JobRunStep, 0)
	err := dbclient.GetMysqlDB().Table(models.CronyJobRunStepTableName).
		Where("run_id = ? AND step_index > ?", runID, afterStep).
		Order("step_index asc").Limit(500).Find(&steps).Error
	return steps, err
}

// 可注入的量（测试用短间隔与低阈值，生产用默认值）。
var (
	// runStreamIntervalMin 轮询下限：再小就是拿数据库当消息队列。
	runStreamIntervalMin = 500 * time.Millisecond
	// runStreamIntervalMax 轮询上限：再慢就失去"看过程"的意义。
	runStreamIntervalMax = 5 * time.Second
	// runStreamHeartbeatTicks 连续多少个"无变化"的轮询后发一次心跳
	// （让浏览器与中间代理知道连接还活着）。
	runStreamHeartbeatTicks = 15
	// runStreamMaxSecondsDefault / Hard 连接时长上限：避免运维忘记关页面，
	// 变成一条永不关闭的数据库轮询。
	runStreamMaxSecondsDefault = 600
	runStreamMaxSecondsHard    = 3600
)

// runStreamConfig 一次订阅的解析结果。
type runStreamConfig struct {
	RunID   string
	JobID   int
	After   int
	Poll    time.Duration
	MaxLife time.Duration
}

// parseRunStreamConfig 解析并**收敛**查询参数：
// 越界一律取边界而不报错——订阅参数写错的糟糕结果是"看不到"，不该是"看不到还报错"。
func parseRunStreamConfig(c *gin.Context) (runStreamConfig, error) {
	cfg := runStreamConfig{
		RunID:   strings.TrimSpace(c.Query("run_id")),
		After:   -1,
		Poll:    time.Second,
		MaxLife: time.Duration(runStreamMaxSecondsDefault) * time.Second,
	}
	if raw := strings.TrimSpace(c.Query("job_id")); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 {
			return cfg, fmt.Errorf("job_id 必须是正整数")
		}
		cfg.JobID = id
	}
	if cfg.RunID == "" && cfg.JobID == 0 {
		return cfg, fmt.Errorf("run_id 与 job_id 至少提供一个")
	}
	if raw := strings.TrimSpace(c.Query("interval_ms")); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil {
			return cfg, fmt.Errorf("interval_ms 必须是整数毫秒")
		}
		cfg.Poll = clampDuration(time.Duration(ms)*time.Millisecond, runStreamIntervalMin, runStreamIntervalMax)
	}
	if raw := strings.TrimSpace(c.Query("max_seconds")); raw != "" {
		sec, err := strconv.Atoi(raw)
		if err != nil {
			return cfg, fmt.Errorf("max_seconds 必须是整数秒")
		}
		cfg.MaxLife = clampDuration(time.Duration(sec)*time.Second,
			30*time.Second, time.Duration(runStreamMaxSecondsHard)*time.Second)
	}
	if raw := strings.TrimSpace(c.Query("after_step")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return cfg, fmt.Errorf("after_step 必须是整数")
		}
		cfg.After = v
	}
	return cfg, nil
}

func clampDuration(v, lo, hi time.Duration) time.Duration {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// sseEvent 一个 SSE 事件的数据体。
type sseEvent struct {
	Seq        int64               `json:"seq"`
	Kind       string              `json:"kind"`
	ServerTime int64               `json:"server_time"`
	Run        *models.JobRun      `json:"run,omitempty"`
	Steps      []models.JobRunStep `json:"steps,omitempty"`
	// Reason / Status 仅 end 事件使用。
	Reason string `json:"reason,omitempty"`
	Status string `json:"status,omitempty"`
	// Detail 仅 error 事件使用（已脱敏：不暴露底层 SQL/DSN）。
	Detail string `json:"detail,omitempty"`
}

// sseWriter 极简 SSE 写出器：自己写帧格式，不引入额外依赖。
type sseWriter struct {
	c   *gin.Context
	seq int64
}

func (w *sseWriter) send(event string, payload sseEvent) error {
	w.seq++
	payload.Seq = w.seq
	payload.ServerTime = time.Now().Unix()
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// SSE 帧：id / event / data，空行结束。data 内不能有裸换行（json.Marshal 已保证）。
	if _, err := fmt.Fprintf(w.c.Writer, "id: %d\nevent: %s\ndata: %s\n\n", w.seq, event, raw); err != nil {
		return err
	}
	w.c.Writer.Flush()
	return nil
}

// RunStream 订阅一次执行（或一个任务的最新执行）的事实变化。
func (r *JobRunRouter) RunStream(c *gin.Context) {
	cfg, err := parseRunStreamConfig(c)
	if err != nil {
		// 参数错误在建立 SSE 之前返回**普通 JSON 信封**，前端据此走统一错误处理。
		resp.FailWithMessage(resp.ErrorRequestParameter, err.Error(), c)
		return
	}
	RunStreamWith(c, cfg, dbRunFactSource{svc: service.DefaultJobRunService})
}

// streamRunState 连接内已知的事实快照（用于差异比较与增量游标）。
type streamRunState struct {
	runID          string
	lastStep       int
	fingerprint    string
	exists         bool
	idleTicks      int
	sentFirstFrame bool
}

// RunStreamWith 订阅主体（数据源可注入：无数据库单测 + 真实库集成测试共用）。
func RunStreamWith(c *gin.Context, cfg runStreamConfig, src RunFactSource) {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("Connection", "keep-alive")
	// 反向代理（nginx）默认缓冲响应，必须显式关闭，否则 SSE 退化成"结束后一次性吐出"。
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.Flush()

	w := &sseWriter{c: c}
	ctx := c.Request.Context()

	ticker := time.NewTicker(cfg.Poll)
	defer ticker.Stop()
	// 连接寿命硬上限：到点必须主动结束，绝不留下永不关闭的轮询。
	life := time.NewTimer(cfg.MaxLife)
	defer life.Stop()

	state := streamRunState{lastStep: cfg.After}

	// tick 抓取一次事实并推送差异；返回 false 表示"该结束连接了"。
	tick := func() bool {
		run, err := resolveRun(src, cfg)
		if err != nil {
			if models.IsRecordNotFound(err) {
				// 执行还没落库（节点刚被触发、或库还没写完）不算错误：
				// 告诉前端"还没开始"，下一轮继续等，避免前端反复重连。
				state.idleTicks++
				if state.idleTicks >= runStreamHeartbeatTicks {
					state.idleTicks = 0
					if err := w.send("heartbeat", sseEvent{Kind: "heartbeat"}); err != nil {
						return false
					}
				}
				return true
			}
			logger.GetLogger().Error(fmt.Sprintf("[run_stream] load run error:%s", err.Error()))
			_ = w.send("error", sseEvent{Kind: "error", Detail: "读取执行事实失败，请稍后重试"})
			_ = w.send("end", sseEvent{Kind: "end", Reason: "error"})
			return false
		}
		// job_id 模式下任务被再次触发：切到新执行时步骤游标必须归零，
		// 否则新执行的步骤会被当成"已推送过"而永远看不到。
		subjectSwitched := state.exists && run.RunID != state.runID
		if subjectSwitched {
			state.lastStep = cfg.After
			state.fingerprint = ""
		}
		steps, err := src.LoadSteps(run.RunID, state.lastStep)
		if err != nil {
			logger.GetLogger().Error(fmt.Sprintf("[run_stream] load steps error:%s", err.Error()))
			_ = w.send("error", sseEvent{Kind: "error", Detail: "读取执行步骤失败，请稍后重试"})
			_ = w.send("end", sseEvent{Kind: "end", Reason: "error"})
			return false
		}
		// 顺序固定：先 run（前端先更新头部状态），再逐步追加时间线。
		fp := fingerprintRun(run)
		runChanged := !state.exists || fp != state.fingerprint
		if runChanged {
			kind := "run"
			// 首帧与"换了执行对象"都用 snapshot：snapshot 的契约是**重置**语义
			// （清空已渲染的步骤再应用），否则前端会把上一次执行的步骤与新的混在一条时间线上。
			if !state.sentFirstFrame || subjectSwitched {
				kind = "snapshot"
			}
			if err := w.send(kind, sseEvent{Kind: kind, Run: run}); err != nil {
				return false
			}
			state.sentFirstFrame = true
		}
		for _, st := range steps {
			row := st
			if err := w.send("step", sseEvent{Kind: "step", Steps: []models.JobRunStep{row}}); err != nil {
				return false
			}
			if row.StepIndex > state.lastStep {
				state.lastStep = row.StepIndex
			}
		}
		if !runChanged && len(steps) == 0 {
			state.idleTicks++
			if state.idleTicks >= runStreamHeartbeatTicks {
				state.idleTicks = 0
				if err := w.send("heartbeat", sseEvent{Kind: "heartbeat"}); err != nil {
					return false
				}
			}
		} else {
			state.idleTicks = 0
		}
		state.exists = true
		state.runID = run.RunID
		state.fingerprint = fp
		if models.RunStatus(run.Status).IsFinal() {
			_ = w.send("end", sseEvent{
				Kind: "end", Reason: "final",
				Status: models.RunStatus(run.Status).String(),
				Run:    run,
			})
			return false
		}
		return true
	}

	if !tick() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			// 客户端断开：直接返回，不再尝试写入（此时写必然失败）。
			return
		case <-life.C:
			_ = w.send("end", sseEvent{Kind: "end", Reason: "timeout"})
			return
		case <-ticker.C:
			if !tick() {
				return
			}
		}
	}
}

// resolveRun 取本次要观察的执行：run_id 优先；否则跟踪该任务的最新一次执行。
func resolveRun(src RunFactSource, cfg runStreamConfig) (*models.JobRun, error) {
	if cfg.RunID != "" {
		return src.LoadRun(cfg.RunID)
	}
	return src.LatestRunByJob(cfg.JobID)
}

// fingerprintRun 只把"会变化的字段"编进指纹。
// 不直接用整个结构体：Output/ErrorMsg 可能很大，且无关字段抖动会造成"没变却一直推"的假实时。
func fingerprintRun(run *models.JobRun) string {
	if run == nil {
		return ""
	}
	return fmt.Sprintf("%s|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%s|%s|%s",
		run.RunID, run.Status, run.ExitCode, run.StartedAt, run.FinishedAt, run.DurationMs,
		run.StepCount, run.SuccessSteps, run.FailedSteps, run.Attempt, run.RetryCount,
		run.ErrorMsg, run.Output, run.TraceID)
}
