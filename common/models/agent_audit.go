package models

import (
	"strconv"
	"time"

	"gorm.io/gorm"
)

// CronyAgentToolAuditTableName 工具调用审计表名。
const CronyAgentToolAuditTableName = "agent_tool_audit"

// 审计决策取值。
//
// 为什么要区分到这么细：这四个值回答的是**完全不同的复盘问题**——
//   - allowed：这次调用真的执行了；
//   - denied：被哪一道闸拦下的（reason 里必须写明闸名，否则复盘时只能说"它被拒了"）；
//   - approval_required：**没有副作用**，只是生成了一张待人工批准的审批单；
//   - approval_executed：人工批准后真的执行了（approval_id 能追到是谁批的）。
//
// 如果只记一个 bool，事后无法回答"当时到底动没动手"——这正是阶段 3
// `expired` 行不带原因吃过的亏。
const (
	AuditDecisionAllowed          = "allowed"
	AuditDecisionDenied           = "denied"
	AuditDecisionApprovalRequired = "approval_required"
	AuditDecisionApprovalExecuted = "approval_executed"
)

// AgentToolAudit 一次工具调用的审计行（**append-only：只插入，不更新，不删除**）。
//
// 设计取舍：
//   - 与 `agent_session.tool_stats` 的区别：那份是"会话视角的汇总"，本表是
//     "每次调用一行的事实"。汇总可以重算，事实不能重来；
//   - 与 `agent_step` 的区别：轨迹面向"模型想了什么"，本表面向"系统放行了什么"，
//     两条链路互相独立——只改轨迹不影响审计，这是有意的；
//   - 拒绝（denied）也要落行：**没被执行的尝试同样是需要复盘的事实**
//     （模型在反复试探什么？哪道闸在拦它？）。
type AgentToolAudit struct {
	ID int `json:"id" gorm:"column:id;primary_key;auto_increment"`
	// AuditID 外部引用用（形如 audit-<nano>-<n>），不暴露自增 id。
	AuditID string `json:"audit_id" gorm:"size:64;column:audit_id;not null;uniqueIndex:uk_agent_tool_audit_id"`
	// SessionID 哪个会话（诊断/NL 起草/巡检）。
	SessionID string `json:"session_id" gorm:"size:128;column:session_id;not null;index:idx_agent_tool_audit_session"`
	// AgentKind 会话类型（diagnose/nljob/inspect/…），便于按类型统计工具使用。
	AgentKind string `json:"agent_kind" gorm:"size:32;column:agent_kind;default:''"`
	// RunID 关联的执行记录（诊断类会话有值）。
	RunID string `json:"run_id" gorm:"size:64;column:run_id;default:'';index:idx_agent_tool_audit_run"`
	// Tool 工具名；Risk 调用时的风险级别（read/write/destructive）。
	Tool string `json:"tool" gorm:"size:64;column:tool;not null;index:idx_agent_tool_audit_tool"`
	Risk string `json:"risk" gorm:"size:16;column:risk;not null;default:'read'"`
	// ArgsJSON **脱敏后**的参数（口令/token 已由 llm.Redact 抹掉，且有长度上限）。
	ArgsJSON string `json:"args_json" gorm:"type:text;column:args_json"`
	// Decision 见上面的 AuditDecision* 常量。
	Decision string `json:"decision" gorm:"size:32;column:decision;not null;index:idx_agent_tool_audit_decision"`
	// Reason 人类可读的原因：被拒时**必须**写明触发了哪道闸（会话配额/速率/并发/爆炸半径/自治级别）。
	Reason string `json:"reason" gorm:"size:512;column:reason;default:''"`
	// ApprovalID 关联审批单（转审批或审批后执行时有值）。
	ApprovalID string `json:"approval_id" gorm:"size:64;column:approval_id;default:'';index:idx_agent_tool_audit_approval"`
	// TargetNode 本次调用影响/观测的目标节点（写动作必填）。
	TargetNode string `json:"target_node" gorm:"size:128;column:target_node;default:''"`
	// OK/Err/DurationMS/OutputBytes/EvidenceCount 结果事实（denied 行保持零值 + OK=false）。
	OK            bool   `json:"ok" gorm:"column:ok;not null;default:false"`
	Err           string `json:"err" gorm:"size:512;column:err;default:''"`
	DurationMS    int64  `json:"duration_ms" gorm:"column:duration_ms;default:0"`
	OutputBytes   int    `json:"output_bytes" gorm:"column:output_bytes;default:0"`
	EvidenceCount int    `json:"evidence_count" gorm:"column:evidence_count;default:0"`
	// Created/CreatedMS 双口径时间：秒给"人看/对账"，毫秒给"排序/去重"。
	// 阶段 3 吃过单位不一致的亏（node_metric 是秒、node_probe_request 是毫秒），
	// 这里两个都存，避免再猜。
	Created   int64 `json:"created" gorm:"column:created;not null;default:0;index:idx_agent_tool_audit_created"`
	CreatedMS int64 `json:"created_ms" gorm:"column:created_ms;not null;default:0"`
}

// TableName 实现 gorm 表名约定。
func (a *AgentToolAudit) TableName() string { return CronyAgentToolAuditTableName }

// NewToolAuditID 生成审计行号（形如 audit-<nano>）。
//
// 为什么不暴露自增 id 给外部引用：自增 id 会泄露"系统一共发生过多少次工具调用"
// 这种不该顺手暴露的规模信息，而且跨环境（测试/生产）不可比。
func NewToolAuditID() string {
	return "audit-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// AppendAgentToolAudit 追加一条审计行。
//
// 只提供"追加"这一个写入口（不提供 Update/Delete）：审计表的价值在于不可篡改，
// 一旦在代码里开了更新口子，"审计"就退化成"另一份日志"。
func AppendAgentToolAudit(db *gorm.DB, a *AgentToolAudit) error {
	return db.Table(CronyAgentToolAuditTableName).Create(a).Error
}

// ListAgentToolAudits 按会话/工具/决策过滤的只读查询（控制台审计面板用）。
//
// 分页与排序固定为 created_ms 倒序 + audit_id 兜底：毫秒同值的情况下靠 id 保证顺序稳定，
// 否则翻页会出现"同一条记录看两次/漏一条"。
func ListAgentToolAudits(db *gorm.DB, sessionID, tool, decision string, page, pageSize int) ([]AgentToolAudit, int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}
	q := db.Table(CronyAgentToolAuditTableName)
	if sessionID != "" {
		q = q.Where("session_id = ?", sessionID)
	}
	if tool != "" {
		q = q.Where("tool = ?", tool)
	}
	if decision != "" {
		q = q.Where("decision = ?", decision)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []AgentToolAudit
	err := q.Order("created_ms desc, id desc").
		Offset((page - 1) * pageSize).Limit(pageSize).Find(&out).Error
	return out, total, err
}
