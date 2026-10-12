package handler

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/autonomy"
	"github.com/tmnhs/crony/common/pkg/chaos"
	"github.com/tmnhs/crony/common/pkg/scheduling"
	"github.com/tmnhs/crony/common/pkg/shadow"
)

// 阶段 4-D / 阶段 5：运维智能化的读取与评估接口。
//
// 全部为"只读或纯计算"，唯一的写是 SLO 契约维护与自治信用状态迁移——
// 它们都不直接改生产执行，而是把"调度为什么这么选 / 系统达标吗 / 这轮该不该放权"
// 变成可解释的事实。因此权限与其它 agent 读取接口一致：**登录即可**。
type OpsRouter struct{}

var defaultOpsRouter = new(OpsRouter)

// ---------- 多目标调度 ----------

// SchedulerAllocate POST /ops/scheduler/allocate 展示"若把这个任务分派出去，会选哪个节点、为什么"。
//
// 这是**只读解释**：它不真正改派任务（改派仍走既有分配路径），
// 只把打分过程摊开，让运维能回答"为什么不是那台机器"。
func (o *OpsRouter) SchedulerAllocate(c *gin.Context) {
	tag := "[ops/scheduler/allocate]"
	var req struct {
		JobID int `json:"job_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !isEOF(err) {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	job := &models.Job{}
	if req.JobID > 0 {
		job = &models.Job{ID: req.JobID}
		if err := job.FindById(); err != nil {
			resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 任务不存在: "+err.Error(), c)
			return
		}
	}
	dec := service.DefaultSchedulerService.ExplainAllocation(job)
	resp.OkWithDetailed(gin.H{
		"decision":    dec,
		"requirement": service.DefaultSchedulerService.BuildRequirement(job),
	}, "ok", c)
}

// SchedulerPolicy POST /ops/scheduler/policy 读调度策略（权重与维度说明）。
func (o *OpsRouter) SchedulerPolicy(c *gin.Context) {
	resp.OkWithDetailed(gin.H{
		"weights": scheduling.DefaultWeights(),
		"dimensions": []gin.H{
			{"key": "load", "desc": "负载（相对容量归一：任务数 / 并发上限）"},
			{"key": "capacity", "desc": "容量（未打满时容量大者优先）"},
			{"key": "affinity", "desc": "亲和（命中 prefer_node / 原节点）"},
			{"key": "success", "desc": "健康度（近期成功率；样本 <3 时按中性处理）"},
		},
		"hard_constraints": []string{
			"节点必须可达（status != ConnFail）",
			"必须满足 job.node_selector.require_labels 全部标签",
			"必须存在 job.node_selector.require_tools 声明的能力标签（tool:<name>）",
		},
		"note": "硬约束不满足即拒绝，绝不靠权重\"扣分\"放行；无可用节点时宁可不派发。",
	}, "ok", c)
}

// ---------- SLO 契约 ----------

// SloSearch POST /ops/slo/search
func (o *OpsRouter) SloSearch(c *gin.Context) {
	tag := "[ops/slo/search]"
	var req struct {
		JobID int `json:"job_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !isEOF(err) {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	list, err := service.DefaultSloService.SearchSlo(req.JobID)
	if err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 查询失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{"list": list, "total": len(list)}, "ok", c)
}

// SloSave POST /ops/slo/save
func (o *OpsRouter) SloSave(c *gin.Context) {
	tag := "[ops/slo/save]"
	var req models.SloPolicy
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	if err := service.DefaultSloService.SaveSlo(&req); err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 保存失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{"policy": req}, "saved", c)
}

// SloEvaluate POST /ops/slo/evaluate 评估契约（不触发任何动作；违约只回报事实）。
func (o *OpsRouter) SloEvaluate(c *gin.Context) {
	tag := "[ops/slo/evaluate]"
	var req struct {
		JobID int `json:"job_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !isEOF(err) {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	list, err := service.DefaultSloService.EvaluateSlo(req.JobID)
	if err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 评估失败: "+err.Error(), c)
		return
	}
	breached := 0
	for _, e := range list {
		if e.Report.Breached {
			breached++
		}
	}
	resp.OkWithDetailed(gin.H{"list": list, "total": len(list), "breached": breached}, "ok", c)
}

// SloAct POST /ops/slo/act 触发一次违约处置（人工）。
//
// 与 /ops/slo/evaluate 的区别：evaluate 只回答"达标没有"，act 会**按契约里的
// breach_action 动手**（通知 / 诊断）。它是人工显式触发，因此不受自动开关
// `slo.breach-action-enabled` 约束（该开关只决定周期循环要不要自动动手）；
// 冷却默认生效，`force=true` 可绕过（人工排障时"再看一次"）。
func (o *OpsRouter) SloAct(c *gin.Context) {
	tag := "[ops/slo/act]"
	var req struct {
		JobID    int  `json:"job_id"`
		PolicyID int  `json:"policy_id"`
		Force    bool `json:"force"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !isEOF(err) {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	svc := service.DefaultSloBreachService()
	if svc == nil {
		resp.FailWithMessage(resp.ERROR, tag+" 违约处置服务未装配", c)
		return
	}
	list, err := service.DefaultSloService.EvaluateSlo(req.JobID)
	if err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 评估失败: "+err.Error(), c)
		return
	}
	// policy_id>0 时只处置这一条契约：人工排障通常只针对某个契约，
	// 无差别地对全部违约契约动手（可能真的发通知）不是人工触发的本意。
	if req.PolicyID > 0 {
		filtered := make([]service.SloEvaluation, 0, 1)
		for _, e := range list {
			if e.Policy.ID == req.PolicyID {
				filtered = append(filtered, e)
			}
		}
		list = filtered
	}
	rep := svc.HandleBreaches(c.Request.Context(), list, service.SloBreachOptions{
		Allowed:        true,
		BypassCooldown: req.Force,
		ExecutedBy:     "manual",
	})
	resp.OkWithDetailed(gin.H{"report": rep}, "ok", c)
}

// ---------- 指标看板 ----------

// MetricsOverview POST /ops/metrics/overview 运维核心指标汇总。
func (o *OpsRouter) MetricsOverview(c *gin.Context) {
	resp.OkWithDetailed(service.DefaultMetricsService.Overview(), "ok", c)
}

// ---------- 轨迹评测 ----------

// EvalScore POST /ops/eval/score 对一次会话轨迹做确定性回归打分。
func (o *OpsRouter) EvalScore(c *gin.Context) {
	tag := "[ops/eval/score]"
	var req struct {
		SessionID     string   `json:"session_id"`
		ExpectedTools []string `json:"expected_tools"`
		MaxSteps      int      `json:"max_steps"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.SessionID) == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 必须提供 session_id", c)
		return
	}
	res, err := service.DefaultEvalService.ScoreSession(req.SessionID, req.ExpectedTools, req.MaxSteps)
	if err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 打分失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{"result": res}, "ok", c)
}

// ---------- 影子执行对账（阶段 5） ----------

// ShadowReconcile POST /ops/shadow/reconcile 对账基线 vs 影子执行的观测。
//
// 这是纯计算：调用方传入两份 Observation（真实采集由基础设施提供），
// 返回差异与"是否允许放量"。任何差异即拒绝放量。
func (o *OpsRouter) ShadowReconcile(c *gin.Context) {
	tag := "[ops/shadow/reconcile]"
	var req struct {
		Baseline  shadow.Observation `json:"baseline"`
		Candidate shadow.Observation `json:"candidate"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	diff := shadow.Reconcile(req.Baseline, req.Candidate)
	resp.OkWithDetailed(gin.H{"diff": diff, "allow_promotion": shadow.AllowPromotion(diff)}, "ok", c)
}

// ---------- 混沌考核（阶段 5） ----------

// ChaosCatalog POST /ops/chaos/catalog 预置故障目录。
func (o *OpsRouter) ChaosCatalog(c *gin.Context) {
	resp.OkWithDetailed(gin.H{
		"catalog": chaos.Catalog(),
		"note":    "受控、可复现、可回滚；severity 越高影响面越大，越高越应只在预发使用。",
	}, "ok", c)
}

// ChaosGrade POST /ops/chaos/grade 汇总一次考核并给出驾照分。
func (o *OpsRouter) ChaosGrade(c *gin.Context) {
	tag := "[ops/chaos/grade]"
	var req struct {
		Attempts []chaos.Attempt `json:"attempts"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !isEOF(err) {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{"score": chaos.Grade(req.Attempts)}, "ok", c)
}

// ---------- 自治信用 ----------
func (o *OpsRouter) AutonomyList(c *gin.Context) {
	list, err := service.DefaultAutonomyService.ListAutonomy()
	if err != nil {
		resp.FailWithMessage(resp.ERROR, "[ops/autonomy/list] 查询失败: "+err.Error(), c)
		return
	}
	// 附上每条的"实际生效等级"（放行判定用的那一个：冻结回落 L1，且不低于 L1）。
	out := make([]gin.H, 0, len(list))
	for i := range list {
		r := &list[i]
		credit := autonomy.Credit{Level: autonomy.Level(r.Level), Frozen: r.Frozen}
		out = append(out, gin.H{
			"scope": r.Scope, "level": r.Level, "level_name": autonomy.Level(r.Level).Name(),
			// effective_level 与工具网关用的是同一个函数（EnforcementLevel），
			// 避免"表里看到的等级"与"决定放行时用的等级"变成两套口径。
			"effective_level": autonomy.EnforcementLevel(credit).Name(),
			"streak":          r.Streak, "total_up": r.TotalUp, "total_down": r.TotalDown,
			"frozen": r.Frozen, "frozen_reason": r.FrozenReason, "updated": r.Updated,
		})
	}
	resp.OkWithDetailed(gin.H{"list": out, "total": len(out)}, "ok", c)
}

// AutonomyObserve POST /ops/autonomy/observe 记录一次动作结果，推进信用（升级/降级/冻结）。
func (o *OpsRouter) AutonomyObserve(c *gin.Context) {
	tag := "[ops/autonomy/observe]"
	var req struct {
		Scope   string `json:"scope"`
		Success *bool  `json:"success"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Success == nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 需要 scope 与 success", c)
		return
	}
	row, err := service.DefaultAutonomyService.ObserveAutonomy(req.Scope, *req.Success)
	if err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 更新失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{"credit": row}, "ok", c)
}

// AutonomyUnfreeze POST /ops/autonomy/unfreeze 人工解冻。
func (o *OpsRouter) AutonomyUnfreeze(c *gin.Context) {
	tag := "[ops/autonomy/unfreeze]"
	var req struct {
		Scope string `json:"scope"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Scope) == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 需要 scope", c)
		return
	}
	row, err := service.DefaultAutonomyService.UnfreezeAutonomy(req.Scope)
	if err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 解冻失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{"credit": row}, "ok", c)
}

// AutonomyEffective POST /ops/autonomy/effective 解释某工具当下的实际生效自治级别。
//
// 这是"只读解释"：它把 min(配置上限, 信用级别) 这条判据摊开展示，
// 让人能回答"这个写动作为什么要审批 / 为什么不自动放行"——与网关走同一条计算路径。
func (o *OpsRouter) AutonomyEffective(c *gin.Context) {
	tag := "[ops/autonomy/effective]"
	var req struct {
		Tool string `json:"tool"`
		Env  string `json:"env"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Tool) == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 需要 tool（可带 env）", c)
		return
	}
	resp.OkWithDetailed(service.DefaultAutonomyService.EffectiveForTool(req.Tool, req.Env), "ok", c)
}
