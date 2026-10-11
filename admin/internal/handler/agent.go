package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agent"
	"github.com/tmnhs/crony/common/pkg/agentruntime"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// AgentRouter 阶段 2（Agent 内核）接口。字段与语义严格对应 PHASE2-接口契约.md。
type AgentRouter struct{}

var defaultAgentRouter = &AgentRouter{}

// AgentRouter 暴露给路由注册。
func AgentRouterInstance() *AgentRouter { return defaultAgentRouter }

// maxObservationBytes 单步观测在响应里的展示上限（完整内容在 trace_json 里）。
const maxObservationBytes = 8 * 1024

// ---------- §1 会话列表 ----------

func (a *AgentRouter) SessionSearch(c *gin.Context) {
	var req request.AgentSessionSearch
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/session/search] 请求参数错误: "+err.Error(), c)
		return
	}
	if req.PageSize <= 0 || req.PageSize > 200 {
		req.PageSize = 20
	}
	if req.Page <= 0 {
		req.Page = 1
	}
	list, total, err := models.ListAgentSessions(models.AgentSessionQuery{
		JobID:    req.JobID,
		RunID:    strings.TrimSpace(req.RunID),
		Kind:     strings.TrimSpace(req.Kind),
		Status:   strings.TrimSpace(req.Status),
		NodeUUID: strings.TrimSpace(req.NodeUUID),
		Page:     req.Page,
		PageSize: req.PageSize,
	})
	if err != nil {
		logger.GetLogger().Error("[agent/session/search] 查询会话失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/session/search] 查询会话失败", c)
		return
	}
	views := make([]agentSessionView, 0, len(list))
	for _, s := range list {
		views = append(views, sessionView(s))
	}
	resp.OkWithData(resp.PageResult{List: views, Total: total, Page: req.Page, PageSize: req.PageSize}, c)
}

// ---------- §2 会话详情（轨迹回放） ----------

func (a *AgentRouter) SessionFind(c *gin.Context) {
	var req request.AgentSessionFind
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/session/find] 请求参数错误: "+err.Error(), c)
		return
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		resp.FailWithMessage(resp.ErrorAgentSessionNotFound, "[agent/session/find] session_id 不能为空", c)
		return
	}
	session := &models.AgentSession{SessionID: sessionID}
	if err := session.FindBySessionID(); err != nil {
		if models.IsRecordNotFound(err) {
			resp.FailWithMessage(resp.ErrorAgentSessionNotFound, "[agent/session/find] 会话不存在: "+sessionID, c)
			return
		}
		logger.GetLogger().Error("[agent/session/find] 查询会话失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/session/find] 查询会话失败", c)
		return
	}
	steps, err := models.ListAgentSteps(sessionID)
	if err != nil {
		logger.GetLogger().Error("[agent/session/find] 查询轨迹失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/session/find] 查询轨迹失败", c)
		return
	}

	view := a.sessionDetail(session, steps)
	resp.OkWithData(view, c)
}

// sessionDetail 组装契约 §2 的响应。
func (a *AgentRouter) sessionDetail(session *models.AgentSession, steps []*models.AgentStep) agentSessionDetailView {
	out := agentSessionDetailView{agentSessionView: sessionView(session)}
	out.System = session.SystemPrompt
	out.Context = decodeStringList(session.ContextJSON)
	out.Warnings = decodeStringList(session.WarningsJSON)
	out.Steps = make([]agentStepView, 0, len(steps))
	for _, s := range steps {
		out.Steps = append(out.Steps, stepView(s, maxObservationBytes))
	}

	var trace agent.Trace
	if strings.TrimSpace(session.TraceJSON) != "" {
		if err := json.Unmarshal([]byte(session.TraceJSON), &trace); err != nil {
			logger.GetLogger().Warn("[agent/session/find] 轨迹 JSON 解析失败（会话 " + session.SessionID + "）: " + err.Error())
		}
	}
	policy := tools.DefaultPolicy()
	if rt := service.DefaultAgentRuntime(); rt != nil {
		policy = rt.Policy()
	}
	policy = policy.Normalize()
	truncated := 0
	for _, st := range trace.Steps {
		if st.Truncated {
			truncated++
		}
	}
	remaining := policy.MaxCallsPerSession - trace.ToolCalls
	if remaining < 0 {
		remaining = 0
	}
	out.ToolStats = agentToolStatsView{
		Calls:     trace.ToolStats.Calls,
		Denied:    trace.ToolStats.Denied,
		Truncated: truncated,
		Remaining: remaining,
		PerTool:   trace.ToolStats.PerTool,
	}
	if len(out.Warnings) == 0 {
		out.Warnings = orEmpty(trace.Warnings)
	}
	out.Evidence = evidenceViews(trace.Evidence)
	out.Budget = agentBudgetView{
		Calls:         trace.Budget.Calls,
		Tokens:        trace.Budget.Tokens,
		DurationMS:    trace.Budget.Elapsed.Milliseconds(),
		MaxCalls:      trace.Budget.Budget.MaxCalls,
		MaxTokens:     trace.Budget.Budget.MaxTokens,
		MaxDurationMS: trace.Budget.Budget.MaxDuration.Milliseconds(),
	}
	out.Result = a.sessionResult(session)
	return out
}

// sessionResult 按会话类型解析结构化结论（契约 §2：诊断→diagnosis、草案→draft、其余 null）。
func (a *AgentRouter) sessionResult(session *models.AgentSession) interface{} {
	if strings.TrimSpace(session.ResultJSON) == "" {
		return nil
	}
	switch session.Kind {
	case models.AgentKindDiagnose:
		var d agent.Diagnosis
		if err := json.Unmarshal([]byte(session.ResultJSON), &d); err != nil {
			logger.GetLogger().Warn("[agent/session/find] 诊断结论解析失败: " + err.Error())
			return nil
		}
		return diagnosisView(d, session)
	case models.AgentKindNLJob:
		var d agent.JobDraft
		if err := json.Unmarshal([]byte(session.ResultJSON), &d); err != nil {
			logger.GetLogger().Warn("[agent/session/find] 草案解析失败: " + err.Error())
			return nil
		}
		return draftView(d, session.SessionID)
	default:
		// job（节点侧巡检）会话：结论是"报告文本"，存在轨迹与 job_run_step 输出里，
		// 这里不额外造结构（避免前端以为有一个不存在的结构化结论）。
		return nil
	}
}

// ---------- §3.1 读取已存诊断 ----------

func (a *AgentRouter) DiagnosisFind(c *gin.Context) {
	var req request.AgentDiagnosisFind
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/diagnosis/find] 请求参数错误: "+err.Error(), c)
		return
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		resp.FailWithMessage(resp.ErrorAgentRunNotFound, "[agent/diagnosis/find] run_id 不能为空", c)
		return
	}
	session, err := models.FindLatestAgentSession(runID, models.AgentKindDiagnose)
	if err != nil {
		logger.GetLogger().Error("[agent/diagnosis/find] 查询诊断会话失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/diagnosis/find] 查询诊断会话失败", c)
		return
	}
	// "还没诊断过"是正常状态，不是错误（契约 §3.1）：前端据此显示"立即诊断"。
	if session == nil {
		resp.OkWithData(gin.H{"found": false, "session_id": "", "diagnosis": nil}, c)
		return
	}
	var d agent.Diagnosis
	if strings.TrimSpace(session.ResultJSON) != "" {
		if err := json.Unmarshal([]byte(session.ResultJSON), &d); err != nil {
			logger.GetLogger().Error("[agent/diagnosis/find] 诊断结论解析失败: " + err.Error())
			resp.FailWithMessage(resp.ERROR, "[agent/diagnosis/find] 诊断结论已损坏（会话 "+session.SessionID+"）", c)
			return
		}
	}
	resp.OkWithData(gin.H{
		"found":      true,
		"session_id": session.SessionID,
		"diagnosis":  diagnosisView(d, session),
	}, c)
}

// ---------- §3.2 触发一次诊断 ----------

func (a *AgentRouter) DiagnosisRun(c *gin.Context) {
	var req request.AgentDiagnosisRun
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/diagnosis/run] 请求参数错误: "+err.Error(), c)
		return
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		resp.FailWithMessage(resp.ErrorAgentRunNotFound, "[agent/diagnosis/run] run_id 不能为空", c)
		return
	}
	rt, ok := a.requireRuntime(c, "[agent/diagnosis/run]")
	if !ok {
		return
	}

	res, err := rt.RunDiagnosis(c.Request.Context(), agentruntime.DiagnosisRequest{RunID: runID, Force: req.Force})
	if err != nil {
		code, msg := a.mapRuntimeError("agent/diagnosis/run", err)
		resp.FailWithMessage(code, msg, c)
		return
	}
	if res == nil {
		resp.FailWithMessage(resp.ERROR, "[agent/diagnosis/run] 诊断没有返回结果", c)
		return
	}

	d := res.Diagnosis
	session := &models.AgentSession{SessionID: res.SessionID}
	if res.SessionID != "" {
		if err := session.FindBySessionID(); err != nil {
			// 落库失败时（res.PersistError 非空）这里查不到：不阻断返回，
			// 但必须把"结论不可回放"如实告诉调用方。
			session = nil
		}
	}
	data := gin.H{
		"found":      true,
		"session_id": res.SessionID,
		"diagnosis":  diagnosisView(d, session),
	}
	if res.Replayed {
		data["replayed"] = true
	}
	if res.PersistError != "" {
		data["persist_error"] = res.PersistError
	}
	resp.OkWithData(data, c)
}

// ---------- §4 只读工具清单 ----------

func (a *AgentRouter) ToolList(c *gin.Context) {
	rt, ok := a.requireRuntime(c, "[agent/tool/list]")
	if !ok {
		return
	}
	reg := rt.Agent().Registry()
	specs := reg.Specs()
	policy := reg.Policy()
	resp.OkWithData(gin.H{
		"tools":  toolViews(specs, policy),
		"policy": toolPolicyView(policy),
	}, c)
}

// ---------- §5.1 起草 ----------

func (a *AgentRouter) NLJobDraft(c *gin.Context) {
	var req request.AgentNLJobDraft
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/nljob/draft] 请求参数错误: "+err.Error(), c)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/nljob/draft] text 不能为空", c)
		return
	}
	if msg, ok := checkNodeExists(req.NodeUUID); !ok {
		resp.FailWithMessage(resp.ErrorAgentUnavailable, "[agent/nljob/draft] "+msg, c)
		return
	}
	rt, ok := a.requireRuntime(c, "[agent/nljob/draft]")
	if !ok {
		return
	}

	res, err := rt.RunDraftJob(c.Request.Context(), agentruntime.DraftRequest{
		Text:     req.Text,
		NodeUUID: strings.TrimSpace(req.NodeUUID),
		Timezone: req.Timezone,
	})
	if err != nil && res == nil {
		code, msg := a.mapRuntimeError("agent/nljob/draft", err)
		resp.FailWithMessage(code, msg, c)
		return
	}
	data := gin.H{"draft": draftView(res.Draft, res.SessionID)}
	if res.PersistError != "" {
		data["persist_error"] = res.PersistError
	}
	if err != nil {
		// 草案不可用（模型没给出合法表达式等）：草案本体照常返回（含触发回显为空），
		// 同时给出 1009，让前端明确"这份草案不能用"。
		data["error"] = err.Error()
		resp.FailWithDetailed(resp.ErrorAgentDraftInvalid, data, "[agent/nljob/draft] "+err.Error(), c)
		return
	}
	resp.OkWithData(data, c)
}

// ---------- §5.2 确认创建 ----------

func (a *AgentRouter) NLJobConfirm(c *gin.Context) {
	var req request.AgentNLJobConfirm
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/nljob/confirm] 请求参数错误: "+err.Error(), c)
		return
	}
	// 必须显式确认：不传 confirmed 或传 false 一律拒绝（这是"人工闸门"的接口落点）。
	if !req.Confirmed {
		resp.FailWithMessage(resp.ErrorAgentDraftInvalid, "[agent/nljob/confirm] 必须显式 confirmed=true 才能创建任务（确认前不会写入调度器）", c)
		return
	}
	rt, ok := a.requireRuntime(c, "[agent/nljob/confirm]")
	if !ok {
		return
	}
	session, draft, err := rt.LoadDraft(req.SessionID)
	if err != nil {
		if errors.Is(err, agentruntime.ErrSessionNotFound) {
			resp.FailWithMessage(resp.ErrorAgentSessionNotFound, "[agent/nljob/confirm] "+err.Error(), c)
			return
		}
		logger.GetLogger().Error("[agent/nljob/confirm] 读取草案失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/nljob/confirm] 读取草案失败: "+err.Error(), c)
		return
	}
	if session != nil && session.Status == models.AgentStatusCanceled {
		resp.FailWithMessage(resp.ErrorAgentDraftInvalid, "[agent/nljob/confirm] 该草案会话已取消，不能创建任务", c)
		return
	}

	// overrides 只允许改四个字段（契约 §5.2），且同样过校验。
	draft = applyDraftOverrides(draft, req.Overrides)
	// 服务端复验：不信任浏览器回传的任何字段，也不信任"起草时验过了"。
	validated, verr := agent.ValidateDraft(draft, time.Now(), draft.Timezone)
	if verr != nil {
		resp.FailWithMessage(resp.ErrorAgentDraftInvalid, "[agent/nljob/confirm] "+verr.Error(), c)
		return
	}

	job, warnings := jobFromDraft(validated)
	if err := job.Check(); err != nil {
		resp.FailWithMessage(resp.ErrorJobFormat, "[agent/nljob/confirm] 任务校验失败: "+err.Error(), c)
		return
	}
	// 一律 status=0（未分配）：确认只是"落成任务定义"，不会自动派发到节点执行。
	job.Status = models.JobStatusNotAssigned
	job.RunOn = ""
	job.Created = time.Now().Unix()
	id, err := job.Insert()
	if err != nil {
		logger.GetLogger().Error("[agent/nljob/confirm] 创建任务失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/nljob/confirm] 创建任务失败", c)
		return
	}
	logger.GetLogger().Info(fmt.Sprintf("[agent/nljob/confirm] 已创建任务 id=%d（未分配）spec=%s source=agent session=%s",
		id, validated.Spec, req.SessionID))

	nextTriggers := make([]string, 0, len(validated.NextTriggers))
	for _, t := range validated.NextTriggers {
		nextTriggers = append(nextTriggers, t.Format(time.RFC3339))
	}
	resp.OkWithData(gin.H{
		"job_id":          id,
		"spec":            validated.Spec,
		"status":          models.JobStatusNotAssigned,
		"next_triggers":   nextTriggers,
		"trigger_echo":    validated.TriggerEcho,
		"created":         true,
		"warnings":        orEmpty(append(validated.Warnings, warnings...)),
		"session_id":      req.SessionID,
		"requires_assign": true,
		"assign_hint":     "任务已创建但处于未分配状态：请在任务列表里指定节点后才会真正开始调度执行。",
	}, c)
}

// applyDraftOverrides 应用白名单内的覆盖字段（纯函数，便于测试）。
//
// "不改"的表示法（契约 §5.2）：字符串空串、超时 <= 0、重试次数 < 0。
// retry_times 用 *int 区分"没传"（nil）与"显式改成 0"（合法值）；
// 负数一律视为"不改"——与 timeout_seconds 的 <= 0 保持一致。
// （此前负数会被写进草案并触发 1009 校验失败，与契约/前端注释说法不符，
//   真实联调时前端注释写着"空串 / 0 / -1 表示不改"，代码却不认 -1。）
func applyDraftOverrides(d agent.JobDraft, ov request.AgentNLJobOverrides) agent.JobDraft {
	out := d
	if name := strings.TrimSpace(ov.Name); name != "" {
		out.Name = name
	}
	if spec := strings.TrimSpace(ov.Spec); spec != "" {
		out.Spec = spec
	}
	if ov.TimeoutSeconds > 0 {
		out.TimeoutSeconds = ov.TimeoutSeconds
	}
	// RetryTimes 是 *int：nil 表示未修改（0 是合法值，不能用零值判空）。
	if ov.RetryTimes != nil && *ov.RetryTimes >= 0 {
		out.RetryTimes = *ov.RetryTimes
	}
	return out
}

// jobFromDraft 把已复验的草案转成 models.Job（**不落库**）。
//
// 两处需要说明的转换：
//  1. cmd 类型：Job 只有字符串 Command，执行时由 SplitCmd 还原 argv，
//     因此必须用 models.SerializeArgv 序列化（含空白/引号的参数会被正确加引号）；
//  2. 含 shell 语法的命令（管道/重定向等，草案里 RawArgv=false）：
//     阶段 1 的 cmd 执行语义是"直接 argv 执行"，不做 shell 解释，
//     所以这里显式包装成 `sh -c "<script>"`，让 argv 模式也能执行管道。
//     包装会带来一条告警（用户能在确认响应里看到），不做静默处理。
func jobFromDraft(d agent.JobDraft) (*models.Job, []string) {
	job := &models.Job{
		Name:              d.Name,
		Spec:              d.Spec,
		Timeout:           d.TimeoutSeconds,
		RetryTimes:        d.RetryTimes,
		RetryInterval:     int64(d.RetryInterval),
		ConcurrencyPolicy: d.ConcurrencyValue,
		Status:            models.JobStatusNotAssigned,
		Note:              strings.TrimSpace("由自然语言创建：" + d.Description),
		Created:           time.Now().Unix(),
	}
	warnings := make([]string, 0, 1)
	switch d.JobType {
	case "http":
		job.Type = models.JobTypeHttp
		job.Command = d.HTTP.URL
		job.HttpMethod = models.HTTPMethodGet
		if strings.EqualFold(d.HTTP.Method, "POST") {
			job.HttpMethod = models.HTTPMethodPost
		}
	default:
		job.Type = models.JobTypeCmd
		if d.RawArgv {
			job.Command = models.SerializeArgv(d.Command)
		} else {
			script := d.Command[0]
			job.Command = models.SerializeArgv([]string{"sh", "-c", script})
			warnings = append(warnings,
				"该命令含 shell 语法（管道/重定向/变量展开），已包装为 `sh -c \"…\"` 执行；Windows 节点需确保 sh 可用。")
		}
	}
	return job, warnings
}

// ---------- 内部工具 ----------

// requireRuntime 取 Agent 运行时；未配置时直接回业务码并返回 false。
func (a *AgentRouter) requireRuntime(c *gin.Context, tag string) (*agentruntime.Runtime, bool) {
	rt := service.DefaultAgentRuntime()
	if rt != nil {
		return rt, true
	}
	code, msg := agentDisabledCode()
	resp.FailWithMessage(code, tag+" "+msg, c)
	return nil, false
}

// agentDisabledCode 区分"完全没配"（1007）与"显式关闭"（1011）。
func agentDisabledCode() (int, string) {
	cfg := config.GetConfigModels()
	if cfg != nil && strings.TrimSpace(cfg.LLM.Provider) != "" && !cfg.LLM.Enabled {
		return resp.ErrorAgentDisabled, "Agent 能力已关闭（llm.enabled=false）：请在配置里启用后再试。"
	}
	return resp.ErrorAgentNotConfigured, "未配置模型（llm.provider/base_url/model 缺失）：Agent 能力不可用。"
}

// mapRuntimeError 把运行时错误映射成业务码（msg 必须可读且指出下一步动作）。
func (a *AgentRouter) mapRuntimeError(tag string, err error) (int, string) {
	switch {
	case err == nil:
		return resp.SUCCESS, ""
	case errors.Is(err, agentruntime.ErrRunNotFound):
		return resp.ErrorAgentRunNotFound, "[" + tag + "] " + err.Error()
	case errors.Is(err, agentruntime.ErrSessionNotFound):
		return resp.ErrorAgentSessionNotFound, "[" + tag + "] " + err.Error()
	case errors.Is(err, agent.ErrDraftInvalid):
		return resp.ErrorAgentDraftInvalid, "[" + tag + "] " + err.Error()
	case errors.Is(err, agent.ErrNoProvider):
		code, msg := agentDisabledCode()
		return code, "[" + tag + "] " + msg
	default:
		return resp.ERROR, "[" + tag + "] " + err.Error()
	}
}

// checkNodeExists 校验目标节点（node_uuid 为空表示"不指定"，允许）。
func checkNodeExists(uuid string) (string, bool) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return "", true
	}
	node := &models.Node{UUID: uuid}
	if err := node.FindByUUID(); err != nil {
		return "目标节点不存在或未注册: " + uuid, false
	}
	return "", true
}

func decodeStringList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	return orEmpty(out)
}
