package models

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tmnhs/crony/common/pkg/dbclient"
)

// 阶段 2（Agent 内核）新增的两张表。
//
// 为什么轨迹要单独建表而不是塞进 job_run.output：
//   - 一次 Agent 会话有"模型调用/工具调用/修复提示"多个步骤，且每一步都要能单独回放；
//   - 轨迹是后续阶段（审批、评测、知识沉淀）的原料，必须可查询、可按会话聚合；
//   - job_run 的 output 只有 2048 字节，装不下一次真实诊断（3 步 + 证据链）。
//
// 分工：
//   - agent_session：一次会话的概要 + 完整轨迹 JSON（用于逐字节回放）；
//   - agent_step：同一轨迹的**展开行**（用于列表、筛选、统计与后续的评测抽样）。
//
// 冗余是刻意的：JSON 保证"回放不走样"，展开行保证"查询不用解析 JSON"。
const (
	CronyAgentSessionTableName = "agent_session"
	CronyAgentStepTableName    = "agent_step"
)

// Agent 会话类型。
const (
	// AgentKindDiagnose 失败诊断（阶段 2 场景一，只读、只给建议）。
	AgentKindDiagnose = "diagnose"
	// AgentKindNLJob 自然语言 → 任务草案（阶段 2 场景二，产出草稿不落库）。
	AgentKindNLJob = "nljob"
	// AgentKindJob 任务类型为 Agent 的定时巡检总结（阶段 2 场景三）。
	AgentKindJob = "job"
)

// Agent 会话状态（与 agent.StopReason 的取值对齐，另加 failed/canceled 便于筛选）。
const (
	AgentStatusCompleted = "completed"
	AgentStatusFailed    = "failed"
	AgentStatusCanceled  = "canceled"
)

// AgentSession 一次 Agent 会话。
type AgentSession struct {
	ID            int64  `json:"id" gorm:"column:id;primary_key;auto_increment"`
	SessionID     string `json:"session_id" gorm:"size:128;column:session_id;not null;uniqueIndex:uk_agent_session_id"`
	Kind          string `json:"kind" gorm:"size:32;column:kind;index:idx_agent_session_kind"`
	JobID         int    `json:"job_id" gorm:"column:job_id;default:0;index:idx_agent_session_run,priority:1"`
	RunID         string `json:"run_id" gorm:"size:128;column:run_id;index:idx_agent_session_run,priority:2"`
	NodeUUID      string `json:"node_uuid" gorm:"size:128;column:node_uuid;index:idx_agent_session_node"`
	Goal          string `json:"goal" gorm:"size:1024;column:goal"`
	Status        string `json:"status" gorm:"size:32;column:status;index:idx_agent_session_status"`
	StopReason    string `json:"stop_reason" gorm:"size:32;column:stop_reason"`
	Provider      string `json:"provider" gorm:"size:64;column:provider"`
	Model         string `json:"model" gorm:"size:64;column:model"`
	PromptVersion string `json:"prompt_version" gorm:"size:64;column:prompt_version"`
	ModelCalls    int    `json:"model_calls" gorm:"column:model_calls;default:0"`
	ToolCalls     int    `json:"tool_calls" gorm:"column:tool_calls;default:0"`
	TotalTokens   int    `json:"total_tokens" gorm:"column:total_tokens;default:0"`
	DurationMs    int64  `json:"duration_ms" gorm:"column:duration_ms;default:0"`
	WarnCount     int    `json:"warn_count" gorm:"column:warn_count;default:0"`
	EvidenceCount int    `json:"evidence_count" gorm:"column:evidence_count;default:0"`
	// SystemPrompt / ContextJSON 保存"当时给模型看了什么"：
	// Agent 结论出问题时，第一件要查的就是输入，而不是输出。
	SystemPrompt string `json:"system_prompt" gorm:"size:4096;column:system_prompt"`
	ContextJSON  string `json:"context_json" gorm:"type:text;column:context_json"`
	WarningsJSON string `json:"warnings_json" gorm:"type:text;column:warnings_json"`
	// TraceJSON 完整轨迹（含每一步的原文与用量），回放的唯一真相来源。
	TraceJSON string `json:"trace_json" gorm:"type:longtext;column:trace_json"`
	// ResultJSON 结构化结论（诊断报告 / 任务草案）。
	ResultJSON string `json:"result_json" gorm:"type:longtext;column:result_json"`
	Created    int64  `json:"created" gorm:"column:created;not null;index:idx_agent_session_created"`
	Updated    int64  `json:"updated" gorm:"column:updated;default:0"`
}

func (s *AgentSession) TableName() string { return CronyAgentSessionTableName }

// AgentStep Agent 会话中的一个步骤（模型调用 / 工具调用 / 修复提示）。
type AgentStep struct {
	ID        int64  `json:"id" gorm:"column:id;primary_key;auto_increment"`
	SessionID string `json:"session_id" gorm:"size:128;column:session_id;not null;index:idx_agent_step_session,priority:1"`
	// Seq 与内核 agent.Step.Index 一致，**从 0 开始**（与 job_run_step.step_index 同口径）。
	// 不要在这里改成 1 起：两处口径不一致会让"第几步失败"对不上。
	Seq      int    `json:"seq" gorm:"column:seq;not null;index:idx_agent_step_session,priority:2"`
	Kind     string `json:"kind" gorm:"size:16;column:kind;not null"`
	Provider string `json:"provider" gorm:"size:64;column:provider"`
	Model    string `json:"model" gorm:"size:64;column:model"`
	// PromptPreview 提示词预览（截断），完整内容在 agent_session.trace_json。
	PromptPreview string `json:"prompt_preview" gorm:"type:text;column:prompt_preview"`
	PromptBytes   int    `json:"prompt_bytes" gorm:"column:prompt_bytes;default:0"`
	Content       string `json:"content" gorm:"type:mediumtext;column:content"`
	ToolCallsJSON string `json:"tool_calls_json" gorm:"type:text;column:tool_calls_json"`
	ToolName      string `json:"tool_name" gorm:"size:64;column:tool_name;index:idx_agent_step_tool"`
	ToolArgsJSON  string `json:"tool_args_json" gorm:"type:text;column:tool_args_json"`
	Observation   string `json:"observation" gorm:"type:mediumtext;column:observation"`
	EvidenceJSON  string `json:"evidence_json" gorm:"type:text;column:evidence_json"`
	DurationMs    int64  `json:"duration_ms" gorm:"column:duration_ms;default:0"`
	LatencyMs     int64  `json:"latency_ms" gorm:"column:latency_ms;default:0"`
	TotalTokens   int    `json:"total_tokens" gorm:"column:total_tokens;default:0"`
	FinishReason  string `json:"finish_reason" gorm:"size:32;column:finish_reason"`
	ErrorMsg      string `json:"error_msg" gorm:"type:text;column:error_msg"`
	Notice        string `json:"notice" gorm:"type:text;column:notice"`
	Truncated     bool   `json:"truncated" gorm:"column:truncated;default:false"`
	Created       int64  `json:"created" gorm:"column:created;not null"`
}

func (s *AgentStep) TableName() string { return CronyAgentStepTableName }

// AgentSessionQuery 会话列表查询条件（阶段 2 接口 /agent/session/search）。
type AgentSessionQuery struct {
	JobID    int
	RunID    string
	Kind     string
	Status   string
	NodeUUID string
	Page     int
	PageSize int
}

// normalize 归一化分页参数，避免把 10 万条会话一次拉进内存。
func (q *AgentSessionQuery) normalize() {
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.PageSize <= 0 {
		q.PageSize = 20
	}
	if q.PageSize > 200 {
		q.PageSize = 200
	}
}

// Insert 落库一条会话。
//
// 先做长度校验再写库：gorm 的 size tag 只在建表时生效，**插入时不校验**，
// 超出长度的值要等 MySQL 严格模式返回 `Error 1406 Data too long for column 'xxx'` 才知道，
// 而那条错误既看不出是哪一步写错的，也只在有真库时才会出现。
// 真实事故：调用方把 131 字符的系统提示词全文当成 `prompt_version`（varchar(64)）传入，
// 在没有数据库的单元测试里全绿，一上真库则**每次诊断都落库失败**。
// 这里把它变成一条可读的、无需数据库就能触发的错误。
func (s *AgentSession) Insert() (int64, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	err := dbclient.GetMysqlDB().Table(CronyAgentSessionTableName).Create(s).Error
	return s.ID, err
}

// MaxSystemPromptRunes 是 system_prompt（varchar(4096)）的容量上限，
// 留出余量避免"刚好等于列宽"时因字符集差异被拒。
const MaxSystemPromptRunes = 4000

// agentSessionLimits 列宽（字符数）与字段名，与 struct tag 一一对应。
// 只列短列：长文本列（text/longtext）容量足够，不在这里挡。
var agentSessionLimits = []struct {
	Field string
	Value string
	Max   int
}{
	{"session_id", "", 128},
	{"kind", "", 32},
	{"run_id", "", 128},
	{"node_uuid", "", 128},
	{"goal", "", 1024},
	{"status", "", 32},
	{"stop_reason", "", 32},
	{"provider", "", 64},
	{"model", "", 64},
	{"prompt_version", "", 64},
	{"system_prompt", "", 4096},
}

// Validate 校验短列长度（插入前调用）。
func (s *AgentSession) Validate() error {
	if s == nil {
		return fmt.Errorf("agent 会话为空")
	}
	if strings.TrimSpace(s.SessionID) == "" {
		return fmt.Errorf("session_id 不能为空")
	}
	values := map[string]string{
		"session_id": s.SessionID, "kind": s.Kind, "run_id": s.RunID,
		"node_uuid": s.NodeUUID, "goal": s.Goal, "status": s.Status,
		"stop_reason": s.StopReason, "provider": s.Provider, "model": s.Model,
		"prompt_version": s.PromptVersion, "system_prompt": s.SystemPrompt,
	}
	for _, lim := range agentSessionLimits {
		v := values[lim.Field]
		if n := len([]rune(v)); n > lim.Max {
			return fmt.Errorf("字段 %s 超长：%d 字符 > 列宽 %d（这类值应存到长文本列或先截断）",
				lim.Field, n, lim.Max)
		}
	}
	return nil
}

// Insert 落库一个步骤。
func (s *AgentStep) Insert() (int64, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	err := dbclient.GetMysqlDB().Table(CronyAgentStepTableName).Create(s).Error
	return s.ID, err
}

// Validate 校验步骤短列长度与序号。
func (s *AgentStep) Validate() error {
	if s == nil {
		return fmt.Errorf("agent 步骤为空")
	}
	if strings.TrimSpace(s.SessionID) == "" {
		return fmt.Errorf("步骤的 session_id 不能为空")
	}
	if s.Seq < 0 {
		return fmt.Errorf("步骤序号不能为负，实际 %d", s.Seq)
	}
	short := []struct {
		field string
		value string
		max   int
	}{
		{"session_id", s.SessionID, 128},
		{"kind", s.Kind, 16},
		{"provider", s.Provider, 64},
		{"model", s.Model, 64},
		{"tool_name", s.ToolName, 64},
		{"finish_reason", s.FinishReason, 32},
	}
	for _, f := range short {
		if n := len([]rune(f.value)); n > f.max {
			return fmt.Errorf("步骤字段 %s 超长：%d 字符 > 列宽 %d", f.field, n, f.max)
		}
	}
	return nil
}

// FindBySessionID 按 session_id 读取会话。
func (s *AgentSession) FindBySessionID() error {
	return dbclient.GetMysqlDB().Table(CronyAgentSessionTableName).
		Where("session_id = ?", s.SessionID).First(s).Error
}

// ListAgentSessions 分页查询会话（新的在前）。
func ListAgentSessions(q AgentSessionQuery) (list []*AgentSession, total int64, err error) {
	q.normalize()
	db := dbclient.GetMysqlDB().Table(CronyAgentSessionTableName)
	if q.JobID > 0 {
		db = db.Where("job_id = ?", q.JobID)
	}
	if r := strings.TrimSpace(q.RunID); r != "" {
		db = db.Where("run_id = ?", r)
	}
	if k := strings.TrimSpace(q.Kind); k != "" {
		db = db.Where("kind = ?", k)
	}
	if st := strings.TrimSpace(q.Status); st != "" {
		db = db.Where("status = ?", st)
	}
	if n := strings.TrimSpace(q.NodeUUID); n != "" {
		db = db.Where("node_uuid = ?", n)
	}
	if err = db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err = db.Order("id DESC").Offset((q.Page - 1) * q.PageSize).Limit(q.PageSize).Find(&list).Error
	return list, total, err
}

// ListAgentSteps 读取一次会话的全部步骤（按发生顺序）。
func ListAgentSteps(sessionID string) ([]*AgentStep, error) {
	steps := make([]*AgentStep, 0, 8)
	err := dbclient.GetMysqlDB().Table(CronyAgentStepTableName).
		Where("session_id = ?", sessionID).Order("seq ASC").Find(&steps).Error
	return steps, err
}

// FindLatestAgentSession 取某次运行的最新一条某类会话（诊断"看最新一次"的语义）。
//
// 未找到时返回 (nil, nil)：**"还没诊断过"不是错误**，调用方要能区分它与数据库故障。
func FindLatestAgentSession(runID, kind string) (*AgentSession, error) {
	session := &AgentSession{}
	err := dbclient.GetMysqlDB().Table(CronyAgentSessionTableName).
		Where("run_id = ? AND kind = ?", runID, kind).Order("id DESC").First(session).Error
	if err != nil {
		if IsRecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

// NewAgentSessionID 生成会话 ID：`<前缀>-<纳秒>-<自增序号>`。
//
// 与 run_id 同样用进程内自增序号兜住"同纳秒并发"的碰撞。
func NewAgentSessionID(prefix string, now time.Time) string {
	seq := atomic.AddUint64(&manualRunSeq, 1)
	if prefix == "" {
		prefix = "agent"
	}
	return fmt.Sprintf("%s-%d-%d", prefix, now.UnixNano(), seq)
}
