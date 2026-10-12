package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
)

// 阶段 5 补全：控制台概览 / 依赖健康度 / 配置快照（全部只读，登录即可）。
//
// 这三个接口在控制台里此前是"规划中"空态；补齐后页面能显示真实数据。
// 权限与其它 /statis 接口一致：登录即可（它们是"看"，不改任何东西）。
type InsightRouter struct{}

var defaultInsightRouter = new(InsightRouter)

// GetOverview GET /statis/overview
func (s *InsightRouter) GetOverview(c *gin.Context) {
	resp.OkWithDetailed(service.DefaultOverviewService.Report(), "ok", c)
}

// GetHealth GET /statis/health
func (s *InsightRouter) GetHealth(c *gin.Context) {
	resp.OkWithDetailed(service.DefaultHealthService.Report(), "ok", c)
}

// GetSettings GET /settings
func (s *InsightRouter) GetSettings(c *gin.Context) {
	resp.OkWithDetailed(service.DefaultSettingsService.Get(), "ok", c)
}

// PostAuditSearch POST /audit/search
//
// 合并两个**真实**事实源（工具调用审计 + 写动作审批），而不是返回样例流水。
func (s *InsightRouter) PostAuditSearch(c *gin.Context) {
	var req struct {
		Operator string `json:"operator"`
		Action   string `json:"action"`
		Target   string `json:"target"`
		Result   string `json:"result"`
		IP       string `json:"ip"`
		Page     int    `json:"page"`
		PageSize int    `json:"page_size"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[audit/search] 请求参数错误: "+err.Error(), c)
		return
	}
	page, err := service.DefaultAuditService.Search(service.AuditQuery{
		Operator: req.Operator, Action: req.Action, Target: req.Target,
		Result: req.Result, IP: req.IP, Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		resp.FailWithMessage(resp.ERROR, "[audit/search] 查询失败: "+err.Error(), c)
		return
	}
	resp.OkWithDetailed(page, "ok", c)
}
