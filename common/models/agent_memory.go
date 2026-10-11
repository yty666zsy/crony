package models

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CronyAgentMemoryTableName 经验库（Agent 记忆）表名。
//
// 这是阶段 4「记忆、编排与调度智能化」的第一块地基：自愈调度要回答"上次这种情况怎么处理的、
// 管用吗"，回放评测要回答"当时 Agent 建议 vs 真实处置"，两者都要先有"症状→动作→结果"的沉淀。
const CronyAgentMemoryTableName = "agent_memory"

// 记忆条目的结果取值。
//
// 注意与审批终态（executed/execution_failed/blocked）**不是同一套**：
// 这里描述的是"这条经验对现场是否有效"，一条 from 审批被拦截的动作，其经验仍然是
// "此路不通"（blocked），这对后来的决策同样是有效信息。
const (
	// MemoryOutcomeSuccess 动作执行成功（工具返回 OK，或人工批准后执行成功）。
	MemoryOutcomeSuccess = "success"
	// MemoryOutcomeFailed 动作执行了但失败。
	MemoryOutcomeFailed = "failed"
	// MemoryOutcomeBlocked 动作**从未执行**（复核未通过 / 策略收紧 / 工具下线）。
	MemoryOutcomeBlocked = "blocked"
	// MemoryOutcomeDenied 动作被闸门拒绝（配额/速率/自治级别/爆炸半径）。
	MemoryOutcomeDenied = "denied"
	// MemoryOutcomeAdvised 会话没有动手，只给了建议（含"证据不足"）。
	MemoryOutcomeAdvised = "advised"
)

// MemoryAgentMemory 语料里的动作种类。
const (
	// MemoryActionTool 通过工具网关的一次调用（有审计行）。
	MemoryActionTool = "tool"
	// MemoryActionAdvice 只出建议、未执行任何工具（含证据不足的结论）。
	MemoryActionAdvice = "advice"
)

// AgentMemory 一条沉淀下来的经验。
//
// 设计取舍（为什么不是"直接存会话摘要"）：
//   - 召回需要**跨会话比较相似度**，会话摘要里混着任务名、时间戳、模型自述，
//     相似度会被噪声淹没；所以这里只留"症状 / 动作 / 结果"三段可比较的文本；
//   - 同一个症状重复出现应当**累加计数**而不是新增行（否则高频故障会把低频故障
//     挤出召回窗口），所以有 dedupe_key 唯一索引 + 计数列；
//   - 记忆是**提示词的输入**，任何能写它的人都能影响模型的行为（等价于提示词注入）。
//     因此本轮**不提供自由编辑接口**，只提供"停用/启用 + 原因"，且停用不删数据。
type AgentMemory struct {
	ID int64 `json:"id" gorm:"column:id;primary_key;auto_increment"`
	// MemoryID 外部引用号（形如 mem-<nano>-<n>），不暴露自增 id。
	MemoryID string `json:"memory_id" gorm:"size:64;column:memory_id;not null;uniqueIndex:uk_agent_memory_id"`
	// DedupeKey 去重键 = hash(归一化症状 | 动作种类 | 动作文本 | 目标节点)。
	// 同一症状重复出现时累加计数，而不是让库无限膨胀。
	DedupeKey string `json:"dedupe_key" gorm:"size:64;column:dedupe_key;not null;uniqueIndex:uk_agent_memory_dedupe"`

	SessionID string `json:"session_id" gorm:"size:128;column:session_id;not null;index:idx_agent_memory_session"`
	RunID     string `json:"run_id" gorm:"size:128;column:run_id;default:'';index:idx_agent_memory_run"`
	JobID     int    `json:"job_id" gorm:"column:job_id;default:0;index:idx_agent_memory_job"`
	AgentKind string `json:"agent_kind" gorm:"size:32;column:agent_kind;default:'';index:idx_agent_memory_kind"`

	// Symptom 症状：人可读的一句话（任务 + 失败现象/诊断目标）。
	Symptom string `json:"symptom" gorm:"size:512;column:symptom;not null"`
	// SymptomTokens 归一化后的词元（空格分隔，含中文 2-gram），召回时用它算相似度。
	//
	// 为什么落库而不是每次现算：召回发生在每次诊断之前，是热路径；
	// 而且落库后**可以人工复盘"为什么这两条被判为相似"**——黑盒相似度没法解释。
	SymptomTokens string `json:"symptom_tokens" gorm:"size:2000;column:symptom_tokens;default:''"`

	ActionKind string `json:"action_kind" gorm:"size:16;column:action_kind;default:'';index:idx_agent_memory_action"`
	// ActionText 动作的人可读描述（工具名 + 脱敏后的关键参数）。
	ActionText string `json:"action_text" gorm:"size:512;column:action_text;default:''"`
	Tool       string `json:"tool" gorm:"size:64;column:tool;default:'';index:idx_agent_memory_tool"`
	// TargetNode 该动作作用于哪台节点（跨节点不一定适用，召回时要能按节点过滤）。
	TargetNode string `json:"target_node" gorm:"size:128;column:target_node;default:''"`
	// Risk 风险级别（read/write/destructive）：只读经验与写动作经验的可复用性完全不同。
	Risk string `json:"risk" gorm:"size:16;column:risk;default:''"`

	// Outcome 最近一次的结果；计数列给出历史全貌。
	Outcome string `json:"outcome" gorm:"size:16;column:outcome;not null;index:idx_agent_memory_outcome"`
	// ResultText 结果的人可读摘要（**脱敏后**，长度受限）。
	ResultText string `json:"result_text" gorm:"size:1024;column:result_text;default:''"`
	// ResultDetail 更长的事实（观测输出尾部 / 错误信息），**脱敏后**。
	ResultDetail string `json:"result_detail" gorm:"type:text;column:result_detail"`

	OccurrenceCount int `json:"occurrence_count" gorm:"column:occurrence_count;not null;default:1"`
	SuccessCount    int `json:"success_count" gorm:"column:success_count;not null;default:0"`
	FailCount       int `json:"fail_count" gorm:"column:fail_count;not null;default:0"`
	BlockedCount    int `json:"blocked_count" gorm:"column:blocked_count;not null;default:0"`
	DeniedCount     int `json:"denied_count" gorm:"column:denied_count;not null;default:0"`
	AdvisedCount    int `json:"advised_count" gorm:"column:advised_count;not null;default:0"`

	// Enabled 是否参与召回。停用**不删行**：历史事实不能被抹掉，只让它不再影响模型。
	Enabled bool `json:"enabled" gorm:"column:enabled;not null;default:true;index:idx_agent_memory_enabled"`
	// DisabledBy / DisabledReason / DisabledAt 停用必须可追责（谁、为什么、什么时候）。
	DisabledBy     string `json:"disabled_by" gorm:"size:64;column:disabled_by;default:''"`
	DisabledReason string `json:"disabled_reason" gorm:"size:512;column:disabled_reason;default:''"`
	DisabledAt     int64  `json:"disabled_at" gorm:"column:disabled_at;default:0"`

	// CreatedMS / UpdatedMS / LastSeenMS 毫秒口径；Created/Updated/LastSeen 秒口径。
	// 阶段 3 吃过单位不一致的亏：这里两套都给，避免调用方猜。
	Created    int64 `json:"created" gorm:"column:created;not null;default:0;index:idx_agent_memory_created"`
	CreatedMS  int64 `json:"created_ms" gorm:"column:created_ms;not null;default:0"`
	Updated    int64 `json:"updated" gorm:"column:updated;not null;default:0"`
	UpdatedMS  int64 `json:"updated_ms" gorm:"column:updated_ms;not null;default:0"`
	LastSeen   int64 `json:"last_seen" gorm:"column:last_seen;not null;default:0"`
	LastSeenMS int64 `json:"last_seen_ms" gorm:"column:last_seen_ms;not null;default:0"`
}

// TableName 实现 gorm 表名约定。
func (m *AgentMemory) TableName() string { return CronyAgentMemoryTableName }

// memorySeq 进程内自增序号，兜住"同纳秒并发"下的 id 碰撞。
var memorySeq uint64

// NewMemoryID 生成记忆条目号：`mem-<纳秒>-<自增序号>`。
func NewMemoryID(now time.Time) string {
	seq := atomic.AddUint64(&memorySeq, 1)
	return fmt.Sprintf("mem-%d-%d", now.UnixNano(), seq)
}

// MemoryDedupeKey 计算去重键。
//
// 归一化规则（必须稳定且可解释）：
//   - 全部转小写、压缩空白；
//   - 症状里常见的"时间/数字噪声"不清除——过强的归一化会把不同故障合并成一条经验，
//     那比重复沉淀更危险（会给出错误的"历史成功率"）。
func MemoryDedupeKey(symptom, actionKind, actionText, targetNode string) string {
	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
	}
	raw := strings.Join([]string{
		norm(symptom), norm(actionKind), norm(actionText), norm(targetNode),
	}, "|")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// MemorySuccessRate 历史成功率 = 成功 / (成功 + 失败)。
//
// 为什么分母不含 blocked/denied/advised：这三类**根本没有执行**，
// 把它们算进成功率会得出"这个动作成功率很低"的假象，而事实是"它没被允许跑过"。
// 分母为 0 时返回 (0, false)，让调用方能区分"0% 成功"与"没有数据"。
func (m *AgentMemory) MemorySuccessRate() (float64, bool) {
	denom := m.SuccessCount + m.FailCount
	if denom <= 0 {
		return 0, false
	}
	return float64(m.SuccessCount) / float64(denom), true
}

// MemoryOutcomeCounts 按结果汇总各计数，供统计接口自洽校验。
func (m *AgentMemory) MemoryOutcomeCounts() map[string]int {
	return map[string]int{
		MemoryOutcomeSuccess: m.SuccessCount,
		MemoryOutcomeFailed:  m.FailCount,
		MemoryOutcomeBlocked: m.BlockedCount,
		MemoryOutcomeDenied:  m.DeniedCount,
		MemoryOutcomeAdvised: m.AdvisedCount,
	}
}

// ApplyOutcome 把一次结果计入对应的计数列，并把 Outcome 更新为最近一次的结果。
//
// 导出是为了让**非 SQL 的实现**（内存实现 / 测试替身）与真库 upsert 的口径一致：
// 真库走 `ON DUPLICATE KEY UPDATE` 的 SQL 表达式；两边各写一套计数逻辑必然漂移，
// 而"计数漂移"在经验库里的后果是**成功率算错**，那会直接误导下一次决策。
func (m *AgentMemory) ApplyOutcome(outcome string) {
	m.bumpOutcome(outcome)
	m.Outcome = outcome
	m.OccurrenceCount++
}

// bumpOutcome 把一次结果计入对应的计数列。
func (m *AgentMemory) bumpOutcome(outcome string) {
	switch outcome {
	case MemoryOutcomeSuccess:
		m.SuccessCount++
	case MemoryOutcomeFailed:
		m.FailCount++
	case MemoryOutcomeBlocked:
		m.BlockedCount++
	case MemoryOutcomeDenied:
		m.DeniedCount++
	default:
		m.AdvisedCount++
	}
}

// MemoryObservation 一次"症状→动作→结果"的观测（由会话轨迹抽取而来）。
type MemoryObservation struct {
	SessionID string
	RunID     string
	JobID     int
	AgentKind string

	Symptom       string
	SymptomTokens string

	ActionKind string
	ActionText string
	Tool       string
	TargetNode string
	Risk       string

	Outcome      string
	ResultText   string
	ResultDetail string
}

// ToAgentMemory 把观测转成待落库的记忆行（计数列按本次结果置 1）。
func (o MemoryObservation) ToAgentMemory(now time.Time) *AgentMemory {
	m := &AgentMemory{
		MemoryID:      NewMemoryID(now),
		DedupeKey:     MemoryDedupeKey(o.Symptom, o.ActionKind, o.ActionText, o.TargetNode),
		SessionID:     o.SessionID,
		RunID:         o.RunID,
		JobID:         o.JobID,
		AgentKind:     o.AgentKind,
		Symptom:       o.Symptom,
		SymptomTokens: o.SymptomTokens,
		ActionKind:    o.ActionKind,
		ActionText:    o.ActionText,
		Tool:          o.Tool,
		TargetNode:    o.TargetNode,
		Risk:          o.Risk,
		Outcome:       o.Outcome,
		ResultText:    o.ResultText,
		ResultDetail:  o.ResultDetail,
		Enabled:       true,
		Created:       now.Unix(),
		CreatedMS:     now.UnixMilli(),
		Updated:       now.Unix(),
		UpdatedMS:     now.UnixMilli(),
		LastSeen:      now.Unix(),
		LastSeenMS:    now.UnixMilli(),
	}
	m.bumpOutcome(o.Outcome)
	if m.OccurrenceCount == 0 {
		m.OccurrenceCount = 1
	}
	return m
}

// UpsertAgentMemory 落一条经验：同一 dedupe_key 存在则**累加计数**并更新最近一次的结果。
//
// 为什么用 INSERT ... ON DUPLICATE KEY UPDATE 而不是"先查后写"：
// 诊断会话可能并发落库（同一次故障被多个入口触发），先查后写会出现两条同 dedupe_key 的行，
// 唯一索引会让其中一条直接报 1062——那时"沉淀经验"就变成了"偶发落库失败"。
//
// 去重命中时会把 mem.MemoryID **改写成已存在的那个 id**：调用方（沉淀日志、
// 验收脚本、控制台"这条经验从哪来"）拿到的单号必须能真的查到行。否则日志里
// 会出现一批"查不到的 memory_id"——那比不打印还坏。
//
// 返回 (是否新建, error)。
func UpsertAgentMemory(db *gorm.DB, mem *AgentMemory) (bool, error) {
	if mem == nil {
		return false, fmt.Errorf("记忆条目为空")
	}
	if err := mem.Validate(); err != nil {
		return false, err
	}
	// 先探测是否已存在（用于返回"是否是新建"以及回填真实单号），再走 upsert。
	// 这一步是**只读**的，即使并发下有偏差也不影响数据的正确性（只影响返回值）。
	var existingID string
	if err := db.Table(CronyAgentMemoryTableName).
		Select("memory_id").Where("dedupe_key = ?", mem.DedupeKey).
		Limit(1).Scan(&existingID).Error; err != nil {
		return false, err
	}
	if strings.TrimSpace(existingID) != "" {
		mem.MemoryID = existingID
	}

	onConflict := clause.OnConflict{
		Columns: []clause.Column{{Name: "dedupe_key"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"occurrence_count": gorm.Expr("occurrence_count + 1"),
			"success_count":    gorm.Expr(incFor(mem.Outcome, MemoryOutcomeSuccess)),
			"fail_count":       gorm.Expr(incFor(mem.Outcome, MemoryOutcomeFailed)),
			"blocked_count":    gorm.Expr(incFor(mem.Outcome, MemoryOutcomeBlocked)),
			"denied_count":     gorm.Expr(incFor(mem.Outcome, MemoryOutcomeDenied)),
			"advised_count":    gorm.Expr(incFor(mem.Outcome, MemoryOutcomeAdvised)),
			"outcome":          mem.Outcome,
			"result_text":      mem.ResultText,
			"result_detail":    mem.ResultDetail,
			"session_id":       mem.SessionID,
			"run_id":           mem.RunID,
			"last_seen":        mem.LastSeen,
			"last_seen_ms":     mem.LastSeenMS,
			"updated":          mem.Updated,
			"updated_ms":       mem.UpdatedMS,
		}),
	}
	if err := db.Table(CronyAgentMemoryTableName).Clauses(onConflict).Create(mem).Error; err != nil {
		return false, err
	}
	return strings.TrimSpace(existingID) == "", nil
}

// incFor 生成"某计数列 +1"的 SQL 片段（用于 ON DUPLICATE KEY UPDATE）。
//
// 故意写成字符串而不是参数：这些列名是代码里的常量，不存在注入面；
// 用参数占位符反而会让 MySQL 把 `success_count + 1` 当成值而不是表达式。
func incFor(outcome, target string) string {
	col := map[string]string{
		MemoryOutcomeSuccess: "success_count",
		MemoryOutcomeFailed:  "fail_count",
		MemoryOutcomeBlocked: "blocked_count",
		MemoryOutcomeDenied:  "denied_count",
		MemoryOutcomeAdvised: "advised_count",
	}[target]
	if col == "" {
		col = "advised_count"
	}
	if outcome != target {
		return col + " + 0"
	}
	return col + " + 1"
}

// Validate 校验列宽（插入前调用，理由同 AgentSession.Validate）。
func (m *AgentMemory) Validate() error {
	if m == nil {
		return fmt.Errorf("记忆条目为空")
	}
	if strings.TrimSpace(m.Symptom) == "" {
		return fmt.Errorf("记忆的 symptom 不能为空")
	}
	if strings.TrimSpace(m.DedupeKey) == "" {
		return fmt.Errorf("记忆的 dedupe_key 不能为空")
	}
	short := []struct {
		field string
		value string
		max   int
	}{
		{"memory_id", m.MemoryID, 64},
		{"dedupe_key", m.DedupeKey, 64},
		{"session_id", m.SessionID, 128},
		{"run_id", m.RunID, 128},
		{"agent_kind", m.AgentKind, 32},
		{"symptom", m.Symptom, 512},
		{"symptom_tokens", m.SymptomTokens, 2000},
		{"action_kind", m.ActionKind, 16},
		{"action_text", m.ActionText, 512},
		{"tool", m.Tool, 64},
		{"target_node", m.TargetNode, 128},
		{"risk", m.Risk, 16},
		{"outcome", m.Outcome, 16},
		{"result_text", m.ResultText, 1024},
		{"disabled_by", m.DisabledBy, 64},
		{"disabled_reason", m.DisabledReason, 512},
	}
	for _, f := range short {
		if n := len([]rune(f.value)); n > f.max {
			return fmt.Errorf("记忆字段 %s 超长：%d 字符 > 列宽 %d", f.field, n, f.max)
		}
	}
	return nil
}

// MemoryQuery 经验列表查询条件（接口 /agent/memory/search）。
type MemoryQuery struct {
	AgentKind  string
	Outcome    string
	Tool       string
	TargetNode string
	SessionID  string
	Keyword    string
	// Enabled 三态：nil=全部、true=仅启用、false=仅停用。
	Enabled  *bool
	Page     int
	PageSize int
}

// normalize 归一化分页参数。
func (q *MemoryQuery) normalize() {
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

// ListAgentMemories 分页查询经验（新的在前）。
func ListAgentMemories(db *gorm.DB, q MemoryQuery) ([]AgentMemory, int64, error) {
	q.normalize()
	tx := db.Table(CronyAgentMemoryTableName)
	if v := strings.TrimSpace(q.AgentKind); v != "" {
		tx = tx.Where("agent_kind = ?", v)
	}
	if v := strings.TrimSpace(q.Outcome); v != "" {
		tx = tx.Where("outcome = ?", v)
	}
	if v := strings.TrimSpace(q.Tool); v != "" {
		tx = tx.Where("tool = ?", v)
	}
	if v := strings.TrimSpace(q.TargetNode); v != "" {
		tx = tx.Where("target_node = ?", v)
	}
	if v := strings.TrimSpace(q.SessionID); v != "" {
		tx = tx.Where("session_id = ?", v)
	}
	if v := strings.TrimSpace(q.Keyword); v != "" {
		like := "%" + v + "%"
		tx = tx.Where("symptom like ? or action_text like ? or result_text like ?", like, like, like)
	}
	if q.Enabled != nil {
		tx = tx.Where("enabled = ?", *q.Enabled)
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []AgentMemory
	err := tx.Order("last_seen_ms desc, id desc").
		Offset((q.Page - 1) * q.PageSize).Limit(q.PageSize).Find(&out).Error
	return out, total, err
}

// FindAgentMemory 按 memory_id 读取一条。
func FindAgentMemory(db *gorm.DB, memoryID string) (*AgentMemory, error) {
	row := &AgentMemory{}
	err := db.Table(CronyAgentMemoryTableName).Where("memory_id = ?", memoryID).First(row).Error
	if err != nil {
		if IsRecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return row, nil
}

// LoadRecallCandidates 取参与召回的候选集（**只取启用的**）。
//
// 召回是"每次诊断之前"的热路径，所以：
//   - 停用过滤**必须在 SQL 侧**：停用的经验绝不允许进入候选集；
//   - 有上限（默认 500 条），防止库大了以后把全表拉进内存；
//   - 不做 kind 硬过滤：诊断经验可以跨 kind 复用（都是"故障现象"），
//     同 kind 的优先级交给 Go 侧打分加成处理（见 agentmemory.Recall）。
//
// 关键词匹配也交给 Go 做：中文没有空格，`LIKE '%词%'` 对多词查询既不能排序，
// 也无法解释"为什么算相似"。
func LoadRecallCandidates(db *gorm.DB, limit int) ([]AgentMemory, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	var out []AgentMemory
	err := db.Table(CronyAgentMemoryTableName).
		Where("enabled = ?", true).
		Order("last_seen_ms desc, id desc").Limit(limit).Find(&out).Error
	return out, err
}

// MemoryStats 经验库统计口径。
type MemoryStats struct {
	Total     int64            `json:"total"`
	Enabled   int64            `json:"enabled"`
	Disabled  int64            `json:"disabled"`
	ByOutcome map[string]int64 `json:"by_outcome"`
	ByTool    []MemoryStatRow  `json:"by_tool"`
	ByKind    []MemoryStatRow  `json:"by_kind"`
	// SuccessRate 全库成功率 = 成功 / (成功 + 失败)，没有可执行样本时为 nil。
	SuccessRate *float64 `json:"success_rate"`
	SuccessSum  int64    `json:"success_sum"`
	FailSum     int64    `json:"fail_sum"`
	// LastSeen 最近一条经验的时间（秒），0 表示库为空。
	LastSeen int64 `json:"last_seen"`
}

// MemoryStatRow 分组统计行。
type MemoryStatRow struct {
	Key   string `json:"key"`
	Total int64  `json:"total"`
}

// QueryMemoryStats 计算经验库统计。
//
// 用 SQL 聚合而不是把行拉出来算：这个接口会被看板反复调用，
// 且"统计口径"必须与列表页看到的行数一致（同一张表、同一套 where）。
func QueryMemoryStats(db *gorm.DB) (*MemoryStats, error) {
	st := &MemoryStats{ByOutcome: map[string]int64{}, ByTool: []MemoryStatRow{}, ByKind: []MemoryStatRow{}}
	if err := db.Table(CronyAgentMemoryTableName).Count(&st.Total).Error; err != nil {
		return nil, err
	}
	if err := db.Table(CronyAgentMemoryTableName).Where("enabled = ?", true).Count(&st.Enabled).Error; err != nil {
		return nil, err
	}
	st.Disabled = st.Total - st.Enabled

	var rows []struct {
		Outcome string
		Total   int64
	}
	if err := db.Table(CronyAgentMemoryTableName).
		Select("outcome, count(*) as total").Group("outcome").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		st.ByOutcome[r.Outcome] = r.Total
	}

	if err := db.Table(CronyAgentMemoryTableName).
		Select("tool as `key`, count(*) as total").Where("tool <> ''").
		Group("tool").Order("total desc").Limit(10).Scan(&st.ByTool).Error; err != nil {
		return nil, err
	}
	if err := db.Table(CronyAgentMemoryTableName).
		Select("agent_kind as `key`, count(*) as total").Where("agent_kind <> ''").
		Group("agent_kind").Order("total desc").Limit(10).Scan(&st.ByKind).Error; err != nil {
		return nil, err
	}

	var sums struct {
		Success int64
		Fail    int64
	}
	if err := db.Table(CronyAgentMemoryTableName).
		Select("coalesce(sum(success_count),0) as success, coalesce(sum(fail_count),0) as fail").
		Scan(&sums).Error; err != nil {
		return nil, err
	}
	st.SuccessSum, st.FailSum = sums.Success, sums.Fail
	if sums.Success+sums.Fail > 0 {
		rate := float64(sums.Success) / float64(sums.Success+sums.Fail)
		st.SuccessRate = &rate
	}

	var last struct {
		V int64
	}
	if err := db.Table(CronyAgentMemoryTableName).
		Select("coalesce(max(last_seen),0) as v").Scan(&last).Error; err != nil {
		return nil, err
	}
	st.LastSeen = last.V
	return st, nil
}

// SetAgentMemoryEnabled 停用/启用一条经验（CAS：按 memory_id 更新，返回是否生效）。
type MemoryStateChange struct {
	MemoryID string
	Enabled  bool
	By       string
	Reason   string
	Now      time.Time
}

// SetAgentMemoryEnabled 停用或启用一条经验。
//
// 停用是**治理动作**，必须留下三件事：谁、为什么、什么时候。
// 未停用（enabled=true）时清空这三个字段，避免"上次的停用原因"残留在启用的行上——
// 那种残留会让人误以为这条经验现在仍被限制。
func SetAgentMemoryEnabled(db *gorm.DB, c MemoryStateChange) (bool, error) {
	if strings.TrimSpace(c.MemoryID) == "" {
		return false, fmt.Errorf("memory_id 不能为空")
	}
	if !c.Enabled && strings.TrimSpace(c.Reason) == "" {
		return false, fmt.Errorf("停用经验必须写明原因")
	}
	updates := map[string]interface{}{
		"enabled":         c.Enabled,
		"disabled_by":     "",
		"disabled_reason": "",
		"disabled_at":     int64(0),
		"updated":         c.Now.Unix(),
		"updated_ms":      c.Now.UnixMilli(),
	}
	if !c.Enabled {
		updates["disabled_by"] = c.By
		updates["disabled_reason"] = c.Reason
		updates["disabled_at"] = c.Now.Unix()
	}
	// CAS：被停用的行不能再被停用一次（两次停用说明有人在重复操作，
	// 与审批的"一次决策只能生效一次"同理）。
	res := db.Table(CronyAgentMemoryTableName).
		Where("memory_id = ? and enabled <> ?", c.MemoryID, c.Enabled).
		Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}
