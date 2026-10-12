package handler

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/middlerware"
	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/selfheal"
)

// 阶段 4-C：自愈调度控制面接口。
//
// 只提供**读**与"手动触发一次扫描"，不提供任何"直接执行某个自愈动作"的接口。
// 理由：自愈动作的合法性完全由策略闸门决定（白名单 + 次数 + 冷却 + 写能力）。
// 一旦开放"直接执行动作"的接口，就等于在闸门旁边开了一扇门，
// 而所有"为什么它能自动动手"的论证都会当场失效。
//
// 与 /agent/memory/* 一致全部用 POST（含查询）：控制台与网关只有一条调用形态，
// 查询条件（含中文状态/动作名）走请求体，不会被 URL 编码问题咬到。
type HealRouter struct{}

var defaultHealRouter = new(HealRouter)

func requireHealService(c *gin.Context, tag string) (*service.SelfHealService, bool) {
	svc := service.DefaultSelfHealService()
	if svc == nil {
		resp.FailWithMessage(resp.ERROR, tag+" 自愈调度未装配", c)
		return nil, false
	}
	return svc, true
}

// HealSearch POST /agent/heal/search 分页查询自愈记录。
func (h *HealRouter) HealSearch(c *gin.Context) {
	tag := "[agent/heal/search]"
	var req request.ReqJobHealSearch
	if err := c.ShouldBindJSON(&req); err != nil && !isEOF(err) {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	svc, ok := requireHealService(c, tag)
	if !ok {
		return
	}
	rows, total, err := svc.Search(&req)
	if err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 查询失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{
		"list": rows, "total": total, "page": req.Page, "page_size": req.PageSize,
	}, "ok", c)
}

// HealFind POST /agent/heal/find 读取一条自愈记录（含策略判定与证据快照）。
func (h *HealRouter) HealFind(c *gin.Context) {
	tag := "[agent/heal/find]"
	var req request.ReqJobHealFind
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	svc, ok := requireHealService(c, tag)
	if !ok {
		return
	}
	row, err := svc.Find(req.HealID)
	if err != nil {
		if errors.Is(err, service.ErrHealNotFound) {
			resp.FailWithMessage(resp.ErrorHealNotFound,
				"自愈记录不存在（heal_id="+strings.TrimSpace(req.HealID)+"）", c)
			return
		}
		resp.FailWithMessage(resp.ERROR, tag+" 查询失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{"heal": row}, "ok", c)
}

// HealByRun POST /agent/heal/by_run 查某次失败执行的自愈记录（从执行详情跳转用）。
func (h *HealRouter) HealByRun(c *gin.Context) {
	tag := "[agent/heal/by_run]"
	var req struct {
		RunID string `json:"run_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" run_id 不能为空", c)
		return
	}
	svc, ok := requireHealService(c, tag)
	if !ok {
		return
	}
	rows, err := svc.HealBySourceRun(runID)
	if err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 查询失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(gin.H{"list": rows, "total": len(rows)}, "ok", c)
}

// HealPolicy POST /agent/heal/policy 读当前生效的自愈策略（只读）。
//
// 为什么要暴露它：运维在打开总开关之前必须先看得见边界（能重试几次、冷却多久、
// 超时最多放大到多少、写能力是否已开）。让边界可读，是"敢于打开它"的前提。
func (h *HealRouter) HealPolicy(c *gin.Context) {
	tag := "[agent/heal/policy]"
	svc, ok := requireHealService(c, tag)
	if !ok {
		return
	}
	writes := false
	if rt := service.DefaultAgentRuntime(); rt != nil {
		writes = rt.WritesEnabled()
	}
	resp.OkWithDetailed(gin.H{
		"policy":         svc.Policy(),
		"writes_enabled": writes,
		"status_names":   healStatusNames(),
		"classes": []gin.H{
			{"class": string(selfheal.ClassNodeUnreachable), "desc": selfheal.ClassNodeUnreachable.String(), "actions": []string{string(models.HealActionEscalate)}},
			{"class": string(selfheal.ClassTimeout), "desc": selfheal.ClassTimeout.String(), "actions": []string{string(models.HealActionRetryExtendTimeout), string(models.HealActionRetrySameNode)}},
			{"class": string(selfheal.ClassCommandFailed), "desc": selfheal.ClassCommandFailed.String(), "actions": []string{string(models.HealActionRetrySameNode)}},
			{"class": string(selfheal.ClassDeliveryFailed), "desc": selfheal.ClassDeliveryFailed.String(), "actions": []string{string(models.HealActionRetrySameNode)}},
			{"class": string(selfheal.ClassOrchestrationInvalid), "desc": selfheal.ClassOrchestrationInvalid.String(), "actions": []string{string(models.HealActionEscalate)}},
			{"class": string(selfheal.ClassUnknown), "desc": selfheal.ClassUnknown.String(), "actions": []string{string(models.HealActionEscalate)}},
		},
		"action_names": models.HealActionNames,
	}, "ok", c)
}

// HealScan POST /agent/heal/scan 手动触发一轮扫描（或只处置指定的一次失败执行）。
//
// 手动触发**不绕过闸门**：它只是"现在就跑一轮"，所有判定与自动轮次完全一致。
// 这一点决定了"手动点一下"不能成为绕过策略的后门。
func (h *HealRouter) HealScan(c *gin.Context) {
	tag := "[agent/heal/scan]"
	var req request.ReqJobHealScan
	if err := c.ShouldBindJSON(&req); err != nil && !isEOF(err) {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	svc, ok := requireHealService(c, tag)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	by := "manual"
	if u := middlerware.CurrentUserName(c); u != "" {
		by = "manual:" + u
	}
	if runID := strings.TrimSpace(req.RunID); runID != "" {
		run := &models.JobRun{RunID: runID}
		if err := run.FindByRunID(); err != nil {
			resp.FailWithMessage(resp.ErrorHealInvalid, "指定的执行记录不存在（run_id="+runID+"）", c)
			return
		}
		if models.RunStatus(run.Status) != models.RunStatusFailed &&
			models.RunStatus(run.Status) != models.RunStatusTimeout {
			resp.FailWithMessage(resp.ErrorHealInvalid,
				"指定的执行不是失败状态（status="+models.RunStatus(run.Status).String()+"），自愈只处置失败执行", c)
			return
		}
		heal, err := svc.HealRun(ctx, run, by)
		if err != nil {
			resp.FailWithMessage(resp.ERROR, tag+" 处置失败: "+err.Error(), c)
			return
		}
		resp.OkWithDetailed(gin.H{"heal": heal}, "已处置该次失败（结论见返回）", c)
		return
	}
	rep, err := svc.ScanOnce(ctx)
	if err != nil {
		resp.FailWithMessage(resp.ERROR, tag+" 扫描失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(rep, "扫描完成", c)
}

// healStatusNames 状态中文名（接口回执直接给人看）。
func healStatusNames() gin.H {
	return gin.H{
		string(models.HealStatusPlanned):   models.HealStatusPlanned.String(),
		string(models.HealStatusDenied):    models.HealStatusDenied.String(),
		string(models.HealStatusExecuted):  models.HealStatusExecuted.String(),
		string(models.HealStatusSucceeded): models.HealStatusSucceeded.String(),
		string(models.HealStatusFailed):    models.HealStatusFailed.String(),
		string(models.HealStatusEscalated): models.HealStatusEscalated.String(),
	}
}

// isEOF 空请求体不算参数错误（查询接口允许无过滤条件）。
func isEOF(err error) bool {
	return err != nil && err.Error() == "EOF"
}
