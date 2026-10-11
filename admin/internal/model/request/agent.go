package request

// 阶段 2（Agent 内核）请求体。
//
// 约定：全部走 POST + JSON；字段名与 PHASE2-接口契约.md 一一对应，
// 前端不得传额外的"权威字段"——服务端一律以自己查到/复验的结果为准。

// AgentSessionSearch 会话列表（契约 §1）。
type AgentSessionSearch struct {
	JobID    int    `json:"job_id" form:"job_id"`
	RunID    string `json:"run_id" form:"run_id"`
	Kind     string `json:"kind" form:"kind"`
	Status   string `json:"status" form:"status"`
	NodeUUID string `json:"node_uuid" form:"node_uuid"`
	Page     int    `json:"page" form:"page"`
	PageSize int    `json:"page_size" form:"page_size"`
}

// AgentSessionFind 会话详情/轨迹回放（契约 §2）。
type AgentSessionFind struct {
	SessionID string `json:"session_id" form:"session_id"`
}

// AgentDiagnosisFind 读取已存诊断（契约 §3.1）。
type AgentDiagnosisFind struct {
	RunID string `json:"run_id" form:"run_id"`
}

// AgentDiagnosisRun 触发一次诊断（契约 §3.2）。
type AgentDiagnosisRun struct {
	RunID string `json:"run_id" form:"run_id"`
	// Force=false 时已有诊断直接复用（幂等、不烧 token）。
	Force bool `json:"force" form:"force"`
}

// AgentNLJobDraft 自然语言起草任务（契约 §5.1）。
type AgentNLJobDraft struct {
	Text     string `json:"text" form:"text"`
	NodeUUID string `json:"node_uuid" form:"node_uuid"`
	Timezone string `json:"timezone" form:"timezone"`
}

// AgentNLJobOverrides 确认时可改的字段（**只允许这四个**）。
//
// 白名单而不是"整体覆盖"：草案里的 command/spec 等字段是模型产物，
// 放开覆盖等于把"人工确认"变成"人工定义任务"，一旦改错就是新的攻击面。
type AgentNLJobOverrides struct {
	Name           string `json:"name" form:"name"`
	Spec           string `json:"spec" form:"spec"`
	TimeoutSeconds int64  `json:"timeout_seconds" form:"timeout_seconds"`
	// RetryTimes 用 -1 表示"未修改"（0 是合法值，不能用零值判空）。
	RetryTimes *int `json:"retry_times" form:"retry_times"`
}

// AgentNLJobConfirm 确认创建（契约 §5.2）。
type AgentNLJobConfirm struct {
	SessionID string              `json:"session_id" form:"session_id"`
	Confirmed bool                `json:"confirmed" form:"confirmed"`
	Overrides AgentNLJobOverrides `json:"overrides" form:"overrides"`
}
