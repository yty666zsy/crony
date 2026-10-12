package request

// 阶段 4-C（自愈调度）接口入参。
type (
	// ReqJobHealSearch 自愈记录查询。
	ReqJobHealSearch struct {
		PageInfo
		JobID        int    `json:"job_id" form:"job_id"`
		Status       string `json:"status" form:"status"`
		FailureClass string `json:"failure_class" form:"failure_class"`
		Action       string `json:"action" form:"action"`
		SourceRunID  string `json:"source_run_id" form:"source_run_id"`
		NodeUUID     string `json:"node_uuid" form:"node_uuid"`
	}
	// ReqJobHealFind 自愈记录详情。
	ReqJobHealFind struct {
		HealID string `json:"heal_id" form:"heal_id" binding:"required"`
	}
	// ReqJobHealScan 手动触发一次自愈扫描（人工排查用；同一次失败不会被重复处置）。
	ReqJobHealScan struct {
		// RunID 指定只处置这一次失败执行；为空则按策略扫描最近的失败。
		RunID string `json:"run_id" form:"run_id"`
	}
	// ReqJobHealPolicy 读取当前生效的自愈策略（只读，不含修改入口）。
	ReqJobHealPolicy struct{}
)

// Check 分页兜底 + 上限（防止把自愈记录当数据源拖库）。
func (r *ReqJobHealSearch) Check() {
	r.PageInfo.Check()
	if r.PageSize > 200 {
		r.PageSize = 200
	}
}
