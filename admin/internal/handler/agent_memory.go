package handler

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/middlerware"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agentmemory"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/logger"
)

// 本文件是阶段 4-A（Agent 记忆/经验库）的接口层：
//   - /agent/memory/search：经验列表（按结果/工具/节点/关键词过滤）；
//   - /agent/memory/find：单条详情（含"要不要停用它"的判断材料）；
//   - /agent/memory/recall：**以症状试召回**（控制台/联调排查"为什么没召回"用）；
//   - /agent/memory/stats：统计口径（成功率怎么算的必须能看见）；
//   - /agent/memory/enable|disable：治理（停用/启用，停用必须写原因）。
//
// 两条硬约束：
//  1. **没有自由编辑接口**。经验库是提示词的输入：能改它的人就能影响模型行为
//     （等价于提示词注入）。所以本轮只提供"停用/启用 + 原因"，坏经验撤下来而不是改掉；
//  2. **经验不是证据**。recall 接口的返回值里写死 advisory_only=true /
//     usable_as_evidence=false，并在返回文本里明确这一点（见 recallView）。

// ---------- 视图 ----------

type agentMemoryView struct {
	MemoryID  string `json:"memory_id"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	JobID     int    `json:"job_id"`
	AgentKind string `json:"agent_kind"`

	Symptom string `json:"symptom"`
	Tool    string `json:"tool"`
	Risk    string `json:"risk"`
	// ActionText 动作描述（工具名 + 脱敏参数）。
	ActionText string `json:"action_text"`
	ActionKind string `json:"action_kind"`

	TargetNode string `json:"target_node"`
	Outcome    string `json:"outcome"`
	// OutcomeText 结果的中文解释（前端只渲染不推断）。
	OutcomeText string `json:"outcome_text"`
	ResultText  string `json:"result_text"`
	// ResultDetail 更长的事实（已脱敏）。
	ResultDetail string `json:"result_detail"`

	OccurrenceCount int `json:"occurrence_count"`
	SuccessCount    int `json:"success_count"`
	FailCount       int `json:"fail_count"`
	BlockedCount    int `json:"blocked_count"`
	DeniedCount     int `json:"denied_count"`
	AdvisedCount    int `json:"advised_count"`

	// SuccessRate 只有在有执行样本（成功+失败>0）时有效；
	// SuccessRateKnown=false 表示"没有执行样本"，前端必须显示"无数据"而不是 0%。
	SuccessRate      float64 `json:"success_rate"`
	SuccessRateKnown bool    `json:"success_rate_known"`
	SuccessRateText  string  `json:"success_rate_text"`

	Enabled        bool   `json:"enabled"`
	DisabledBy     string `json:"disabled_by"`
	DisabledReason string `json:"disabled_reason"`
	DisabledAt     string `json:"disabled_at"`

	Created    int64 `json:"created"`
	CreatedMS  int64 `json:"created_ms"`
	Updated    int64 `json:"updated"`
	UpdatedMS  int64 `json:"updated_ms"`
	LastSeen   int64 `json:"last_seen"`
	LastSeenMS int64 `json:"last_seen_ms"`

	// SymptomTokens 分词结果（可解释性：人能复盘"为什么这两条算相似"）。
	SymptomTokens string `json:"symptom_tokens"`
}

func memoryView(m *models.AgentMemory) agentMemoryView {
	if m == nil {
		return agentMemoryView{}
	}
	rate, known := m.MemorySuccessRate()
	return agentMemoryView{
		MemoryID:         m.MemoryID,
		SessionID:        m.SessionID,
		RunID:            m.RunID,
		JobID:            m.JobID,
		AgentKind:        m.AgentKind,
		Symptom:          m.Symptom,
		Tool:             m.Tool,
		Risk:             m.Risk,
		ActionText:       m.ActionText,
		ActionKind:       m.ActionKind,
		TargetNode:       m.TargetNode,
		Outcome:          m.Outcome,
		OutcomeText:      memoryOutcomeText(m.Outcome),
		ResultText:       llm.Redact(m.ResultText),
		ResultDetail:     llm.Truncate(llm.Redact(m.ResultDetail), 2000),
		OccurrenceCount:  m.OccurrenceCount,
		SuccessCount:     m.SuccessCount,
		FailCount:        m.FailCount,
		BlockedCount:     m.BlockedCount,
		DeniedCount:      m.DeniedCount,
		AdvisedCount:     m.AdvisedCount,
		SuccessRate:      rate,
		SuccessRateKnown: known,
		SuccessRateText:  memorySuccessRateText(rate, known, m),
		Enabled:          m.Enabled,
		DisabledBy:       m.DisabledBy,
		DisabledReason:   m.DisabledReason,
		DisabledAt:       unixText(m.DisabledAt),
		Created:          m.Created,
		CreatedMS:        m.CreatedMS,
		Updated:          m.Updated,
		UpdatedMS:        m.UpdatedMS,
		LastSeen:         m.LastSeen,
		LastSeenMS:       m.LastSeenMS,
		SymptomTokens:    m.SymptomTokens,
	}
}

// memoryOutcomeText 结果的中文解释。
//
// 每一个取值都必须说清"动作到底发生没发生"——这是阶段 3 `expired` 行吃过亏的地方：
// 把"没执行"读成"执行失败"，会让人去排查一个根本没跑过的动作。
func memoryOutcomeText(o string) string {
	switch o {
	case models.MemoryOutcomeSuccess:
		return "执行成功"
	case models.MemoryOutcomeFailed:
		return "执行了但失败"
	case models.MemoryOutcomeBlocked:
		return "未执行（转人工审批 / 复核未通过）"
	case models.MemoryOutcomeDenied:
		return "被闸门拒绝（未执行）"
	case models.MemoryOutcomeAdvised:
		return "只给了建议，本次没有动作"
	}
	return o
}

// memorySuccessRateText 成功率的**三态**文本：无数据 / 0% / x%。
//
// 为什么不能只给一个数字：把"没有执行样本"渲染成 0%，等于告诉运维
// "这个动作从来没成功过"——而事实是"它从来没被允许跑过"。这两句话会导致完全相反的决策。
func memorySuccessRateText(rate float64, known bool, m *models.AgentMemory) string {
	if !known {
		return "无执行样本（成功 0 / 失败 0；被拒/未执行不计入成功率）"
	}
	return fmt.Sprintf("%.1f%%（成功 %d / 失败 %d）", rate*100, m.SuccessCount, m.FailCount)
}

// ---------- 列表 / 详情 ----------

// MemorySearch 经验列表。
func (a *AgentRouter) MemorySearch(c *gin.Context) {
	var req struct {
		AgentKind  string `json:"agent_kind"`
		Outcome    string `json:"outcome"`
		Tool       string `json:"tool"`
		TargetNode string `json:"target_node"`
		SessionID  string `json:"session_id"`
		Keyword    string `json:"keyword"`
		// Enabled 三态：不传=全部，true=仅启用，false=仅停用。
		Enabled  *bool `json:"enabled"`
		Page     int   `json:"page"`
		PageSize int   `json:"page_size"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/memory/search] 请求参数错误: "+err.Error(), c)
		return
	}
	svc, ok := requireMemoryService(c, "[agent/memory/search]")
	if !ok {
		return
	}
	list, total, err := svc.Search(models.MemoryQuery{
		AgentKind:  strings.TrimSpace(req.AgentKind),
		Outcome:    strings.TrimSpace(req.Outcome),
		Tool:       strings.TrimSpace(req.Tool),
		TargetNode: strings.TrimSpace(req.TargetNode),
		SessionID:  strings.TrimSpace(req.SessionID),
		Keyword:    strings.TrimSpace(req.Keyword),
		Enabled:    req.Enabled,
		Page:       req.Page,
		PageSize:   req.PageSize,
	})
	if err != nil {
		logger.GetLogger().Error("[agent/memory/search] 查询经验库失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/memory/search] 查询经验库失败", c)
		return
	}
	views := make([]agentMemoryView, 0, len(list))
	for i := range list {
		views = append(views, memoryView(&list[i]))
	}
	resp.OkWithData(gin.H{
		"list": views, "total": total,
		"page": pageOr(req.Page), "page_size": pageSizeOr(req.PageSize),
		// 溯源自述：让前端能显示"经验从哪来、怎么用的"。
		"source": gin.H{
			"tables":                  []string{models.CronyAgentMemoryTableName},
			"extracted_from":          []string{models.CronyAgentSessionTableName, models.CronyAgentToolAuditTableName},
			"advisory_only":           true,
			"usable_as_evidence":      false,
			"editable":                false,
			"disable_requires_reason": true,
		},
	}, c)
}

// MemoryFind 单条详情。
func (a *AgentRouter) MemoryFind(c *gin.Context) {
	var req struct {
		MemoryID string `json:"memory_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/memory/find] 请求参数错误: "+err.Error(), c)
		return
	}
	svc, ok := requireMemoryService(c, "[agent/memory/find]")
	if !ok {
		return
	}
	row, err := svc.Find(req.MemoryID)
	if err != nil {
		code := memoryErrorCode(err)
		resp.Result(code, gin.H{}, "[agent/memory/find] "+memoryErrorMsg(err), c)
		return
	}
	resp.OkWithData(gin.H{"memory": memoryView(row)}, c)
}

// ---------- 召回 / 统计 ----------

// memoryRecallHitView 一条召回命中（含"凭什么像"与"历史成功率"）。
type memoryRecallHitView struct {
	MemoryID    string   `json:"memory_id"`
	Symptom     string   `json:"symptom"`
	ActionText  string   `json:"action_text"`
	Tool        string   `json:"tool"`
	TargetNode  string   `json:"target_node"`
	Outcome     string   `json:"outcome"`
	OutcomeText string   `json:"outcome_text"`
	ResultText  string   `json:"result_text"`
	Score       float64  `json:"score"`
	Matched     []string `json:"matched"`
	// Reasons 打分明细：不允许"只有分数没有理由"。
	Reasons          []string `json:"reasons"`
	SuccessRate      float64  `json:"success_rate"`
	SuccessRateKnown bool     `json:"success_rate_known"`
	Occurrences      int      `json:"occurrence_count"`
	// 结果构成：outcome 只是**最近一次**的结果，出现多次时前端要能显示全貌，
	// 否则"最近一次被拦下"会被读成"这个动作一直跑不通"。
	SuccessCount int   `json:"success_count"`
	FailCount    int   `json:"fail_count"`
	BlockedCount int   `json:"blocked_count"`
	DeniedCount  int   `json:"denied_count"`
	AdvisedCount int   `json:"advised_count"`
	LastSeen     int64 `json:"last_seen"`
	SameKind     bool  `json:"same_kind"`
	SameTarget   bool  `json:"same_target"`
	// IsFailed 该经验的结果是"失败/被拒/未执行"——展示时必须能一眼区分，
	// 否则"历史上这么干失败过"会被读成"历史上这么干成功了"。
	IsFailed bool `json:"is_failed"`
}

// MemoryRecall 以症状试召回（**只返回线索，不写库、不影响模型**）。
//
// 为什么这个接口重要：召回是"静默失败"最容易发生的地方（库里没相似经验、
// 相似度阈值过高、词元切分不合适，表现都只是"没注入"）。有了它，人和验收脚本
// 都能直接问"给这段症状，你会召回什么、为什么"。
func (a *AgentRouter) MemoryRecall(c *gin.Context) {
	var req struct {
		Symptom    string `json:"symptom"`
		AgentKind  string `json:"agent_kind"`
		TargetNode string `json:"target_node"`
		TopK       int    `json:"top_k"`
		// MinScore 可选：允许调低阈值观察"边缘相似"的条目（只影响本次查询）。
		MinScore float64 `json:"min_score"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/memory/recall] 请求参数错误: "+err.Error(), c)
		return
	}
	svc, ok := requireMemoryService(c, "[agent/memory/recall]")
	if !ok {
		return
	}
	if strings.TrimSpace(req.Symptom) == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[agent/memory/recall] symptom 不能为空（召回必须由具体症状发起）", c)
		return
	}
	res, err := svc.Recall(req.Symptom, strings.TrimSpace(req.AgentKind), strings.TrimSpace(req.TargetNode), req.TopK)
	if err != nil {
		logger.GetLogger().Error("[agent/memory/recall] 召回失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/memory/recall] 召回失败", c)
		return
	}
	if req.MinScore > 0 {
		// 重算一次（用调用方给的阈值）：models 层的默认阈值偏保守，
		// 排查"是不是阈值把相似条目挡住了"时需要能看到被挡住的那批。
		res = rescoreHits(res, req.MinScore, req.TopK)
	}
	hits := make([]memoryRecallHitView, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, memoryRecallHitView{
			MemoryID:         h.MemoryID,
			Symptom:          h.Symptom,
			ActionText:       h.ActionText,
			Tool:             h.Tool,
			TargetNode:       h.TargetNode,
			Outcome:          h.Outcome,
			OutcomeText:      memoryOutcomeText(h.Outcome),
			ResultText:       h.ResultText,
			Score:            h.Score,
			Matched:          h.Matched,
			Reasons:          h.Reasons,
			SuccessRate:      h.SuccessRate,
			SuccessRateKnown: h.SuccessRateKnown,
			Occurrences:      h.Occurrences,
			SuccessCount:     h.SuccessCount,
			FailCount:        h.FailCount,
			BlockedCount:     h.BlockedCount,
			DeniedCount:      h.DeniedCount,
			AdvisedCount:     h.AdvisedCount,
			LastSeen:         h.LastSeen,
			SameKind:         h.SameKind,
			SameTarget:       h.SameTarget,
			IsFailed:         h.Outcome != models.MemoryOutcomeSuccess,
		})
	}
	resp.OkWithData(gin.H{
		"query":        res.Query,
		"query_tokens": res.QueryTokens,
		"considered":   res.Considered,
		"hits":         hits,
		"top_k":        res.TopK,
		"min_score":    res.MinScore,
		// 这两个字段是**接口契约**（不是备注）：任何消费方都能直接看到
		// "召回结果只能当线索、不能当证据"。
		"advisory_only":      true,
		"usable_as_evidence": false,
		"notice":             "召回结果只作为排查线索：它不算本次证据，诊断结论仍必须由本次工具观测支撑。",
	}, c)
}

// rescoreHits 用调用方给的阈值重算召回（复用 agentmemory 的同一套打分，避免两套口径）。
func rescoreHits(res agentmemory.RecallResult, minScore float64, topK int) agentmemory.RecallResult {
	// 说明：agentmemory.Recall 只接受候选集，不接受"已有命中"。这里直接用返回的
	// Reasons/Score 无法重算覆盖率，所以采用**保守做法**：只按分数过滤 + 截断，
	// 并明确标注这是"放宽阈值后的观察"，不改变命中本身的理由。
	out := res
	kept := make([]agentmemory.Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		if h.Score >= minScore {
			kept = append(kept, h)
		}
	}
	if topK > 0 && len(kept) > topK {
		kept = kept[:topK]
	}
	out.Hits = kept
	out.MinScore = minScore
	if topK > 0 {
		out.TopK = topK
	}
	return out
}

// MemoryStats 统计口径。
func (a *AgentRouter) MemoryStats(c *gin.Context) {
	svc, ok := requireMemoryService(c, "[agent/memory/stats]")
	if !ok {
		return
	}
	st, err := svc.Stats()
	if err != nil {
		logger.GetLogger().Error("[agent/memory/stats] 统计失败: " + err.Error())
		resp.FailWithMessage(resp.ERROR, "[agent/memory/stats] 统计失败", c)
		return
	}
	data := gin.H{
		"total":       st.Total,
		"enabled":     st.Enabled,
		"disabled":    st.Disabled,
		"by_outcome":  st.ByOutcome,
		"by_tool":     st.ByTool,
		"by_kind":     st.ByKind,
		"success_sum": st.SuccessSum,
		"fail_sum":    st.FailSum,
		"last_seen":   st.LastSeen,
		// 口径说明：成功率的分母**只含真正执行过的动作**。
		"success_rate":       st.SuccessRate,
		"success_rate_known": st.SuccessRate != nil,
		"definition": gin.H{
			"success_rate": "成功 / (成功 + 失败)",
			"excluded":     []string{"blocked（未执行）", "denied（被拒）", "advised（只建议）"},
			"why":          "被拦下/未执行的动作从未跑过，把它们算进分母会得出虚假的低成功率",
		},
	}
	resp.OkWithData(data, c)
}

// ---------- 治理：停用 / 启用 ----------

// MemoryDisable 停用一条经验（必须写原因）。
//
// 停用是**撤下**而不是删除：历史事实不能被抹掉，但可以不再影响模型。
func (a *AgentRouter) MemoryDisable(c *gin.Context) {
	memoryStateChange(c, false)
}

// MemoryEnable 重新启用一条经验。
func (a *AgentRouter) MemoryEnable(c *gin.Context) {
	memoryStateChange(c, true)
}

func memoryStateChange(c *gin.Context, enabled bool) {
	action := "disable"
	if enabled {
		action = "enable"
	}
	tag := "[agent/memory/" + action + "]"
	var req struct {
		MemoryID string `json:"memory_id"`
		Reason   string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, tag+" 请求参数错误: "+err.Error(), c)
		return
	}
	// 操作人来自 token：请求体里的 by 一律忽略（谁停用的必须能追到人）。
	operator := middlerware.CurrentUserName(c)
	if operator == "" {
		resp.FailWithMessage(resp.ErrorForbidden, tag+" 无法从登录态取得操作人", c)
		return
	}
	svc, ok := requireMemoryService(c, tag)
	if !ok {
		return
	}
	row, err := svc.SetEnabled(req.MemoryID, enabled, operator, strings.TrimSpace(req.Reason))
	if err != nil {
		code := memoryErrorCode(err)
		data := gin.H{}
		if row != nil {
			data["memory"] = memoryView(row)
		}
		resp.Result(code, data, tag+" "+memoryErrorMsg(err), c)
		return
	}
	resp.OkWithData(gin.H{"memory": memoryView(row)}, c)
}

// ---------- 依赖与错误映射 ----------

// requireMemoryService 取经验库控制面（未装配时返回可解释的错误）。
func requireMemoryService(c *gin.Context, tag string) (*service.MemoryService, bool) {
	svc := service.DefaultMemoryService()
	if svc == nil {
		resp.FailWithMessage(resp.ErrorAgentDisabled,
			tag+" 经验库未装配（Agent 能力未启用或启动装配失败）", c)
		return nil, false
	}
	return svc, true
}

// memoryErrorCode 经验库错误 → 业务码。
func memoryErrorCode(err error) int {
	switch {
	case err == nil:
		return resp.SUCCESS
	case errors.Is(err, service.ErrMemoryNotFound):
		return resp.ErrorAgentMemoryNotFound
	case errors.Is(err, service.ErrMemoryConflict):
		return resp.ErrorAgentMemoryConflict
	default:
		return resp.ERROR
	}
}

func memoryErrorMsg(err error) string {
	if err == nil {
		return ""
	}
	return llm.Redact(err.Error())
}
