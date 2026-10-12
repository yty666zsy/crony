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
