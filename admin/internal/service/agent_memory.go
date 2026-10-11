package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agentmemory"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"gorm.io/gorm"
)

// 经验库控制面的哨兵错误（handler 据此映射业务码）。
var (
	// ErrMemoryNotFound 经验条目不存在。
	ErrMemoryNotFound = errors.New("经验条目不存在")
	// ErrMemoryConflict 状态已变更：重复停用/启用（CAS 未生效）。
	ErrMemoryConflict = errors.New("经验条目状态已变更，本次操作未生效")
	// ErrMemoryReasonRequired 停用必须写明原因。
	ErrMemoryReasonRequired = errors.New("停用经验必须写明原因")
)

// MemoryStore 经验库持久化（真实现是 gorm，测试用内存替身）。
//
// 内嵌 agentmemory.Store：沉淀器只需要 Upsert，控制面还需要查询/统计/停用。
// 一个接口同时满足两条链路，避免出现"沉淀写一套、查询读另一套"的口径漂移。
type MemoryStore interface {
	agentmemory.Store
	List(q models.MemoryQuery) ([]models.AgentMemory, int64, error)
	Find(memoryID string) (*models.AgentMemory, error)
	Stats() (*models.MemoryStats, error)
	SetEnabled(c models.MemoryStateChange) (bool, error)
	Candidates(limit int) ([]models.AgentMemory, error)
}

// GormMemoryStore 基于 MySQL 的经验库实现。
//
// 与 GormApprovalStore 同一套写法：只做"取 DB + 转调 models"，
// 去重（ON DUPLICATE KEY UPDATE）、CAS 停用等语义全部落在 models 的 SQL 里——
// 放在 service 里做"先查后写"，多实例部署立刻失效。
type GormMemoryStore struct {
	// DB 可注入（测试用）；为空则用全局单例。
	DB *gorm.DB
}

// NewGormMemoryStore 创建真库实现。
func NewGormMemoryStore() *GormMemoryStore { return &GormMemoryStore{} }

func (s *GormMemoryStore) db() *gorm.DB {
	if s != nil && s.DB != nil {
		return s.DB
	}
	return dbclient.GetMysqlDB()
}

// Upsert 落一条经验（去重键命中时累加计数）。
func (s *GormMemoryStore) Upsert(mem *models.AgentMemory) (bool, error) {
	db := s.db()
	if db == nil {
		return false, errNoDB
	}
	return models.UpsertAgentMemory(db, mem)
}

// List 分页查询。
func (s *GormMemoryStore) List(q models.MemoryQuery) ([]models.AgentMemory, int64, error) {
	db := s.db()
	if db == nil {
		return nil, 0, errNoDB
	}
	return models.ListAgentMemories(db, q)
}

// Find 按 memory_id 读取。
func (s *GormMemoryStore) Find(memoryID string) (*models.AgentMemory, error) {
	db := s.db()
	if db == nil {
		return nil, errNoDB
	}
	return models.FindAgentMemory(db, memoryID)
}

// Stats 统计。
func (s *GormMemoryStore) Stats() (*models.MemoryStats, error) {
	db := s.db()
	if db == nil {
		return nil, errNoDB
	}
	return models.QueryMemoryStats(db)
}

// SetEnabled 停用/启用（CAS）。
func (s *GormMemoryStore) SetEnabled(c models.MemoryStateChange) (bool, error) {
	db := s.db()
	if db == nil {
		return false, errNoDB
	}
	return models.SetAgentMemoryEnabled(db, c)
}

// Candidates 召回候选集（只含启用条目）。
func (s *GormMemoryStore) Candidates(limit int) ([]models.AgentMemory, error) {
	db := s.db()
	if db == nil {
		return nil, errNoDB
	}
	return models.LoadRecallCandidates(db, limit)
}

// MemoryService 经验库控制面。
//
// 它承担四件事：
//  1. **召回**（RecallForDiagnose）：诊断前把相似历史作为线索给模型（**不是证据**）；
//  2. **落库**（Recorder）：会话结束后把"症状→动作→结果"沉淀下来（旁路，失败不影响会话）；
//  3. **查询/统计**：控制台要能看清"库里有什么、成功率怎么算的"；
//  4. **治理**（SetEnabled）：停用一条会误导模型的坏经验——**不提供自由编辑**，
//     因为经验库是提示词的输入，能改它的人就能影响模型行为（等价于提示词注入）。
type MemoryService struct {
	store    MemoryStore
	recorder *agentmemory.Recorder
	now      func() time.Time
}

// NewMemoryService 创建控制面。
func NewMemoryService(store MemoryStore) *MemoryService {
	if store == nil {
		store = NewGormMemoryStore()
	}
	return &MemoryService{
		store:    store,
		recorder: agentmemory.NewRecorder(store),
		now:      time.Now,
	}
}

// SetClock 注入时钟（测试观察 last_seen/停用时间用）。
func (s *MemoryService) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Recorder 暴露沉淀器给运行时装配层（agentruntime.MemoryRecorder）。
func (s *MemoryService) Recorder() *agentmemory.Recorder {
	if s == nil {
		return nil
	}
	return s.recorder
}

// RecallForDiagnose 实现 agentruntime.MemoryRecaller。
//
// **失败不报错**：经验库不可用/查询失败时返回空结果并记日志。
// 理由：召回是"锦上添花"，它绝不能让诊断整体失败——诊断的价值来自本次观测，
// 不来自历史；但失败必须可见（否则会变成"经验库悄悄不起作用"）。
func (s *MemoryService) RecallForDiagnose(symptom, agentKind, targetNode string, topK int) agentmemory.RecallResult {
	empty := agentmemory.RecallResult{
		Query: symptom, QueryTokens: []string{}, Hits: []agentmemory.Hit{},
		AdvisoryOnly: true, UsableAsEvidence: false,
	}
	if s == nil || s.store == nil || strings.TrimSpace(symptom) == "" {
		return empty
	}
	cands, err := s.store.Candidates(0)
	if err != nil {
		logger.GetLogger().Warn("[agent/memory] 召回候选集读取失败（本次诊断不使用历史经验）: " + err.Error())
		return empty
	}
	ptrs := make([]*models.AgentMemory, 0, len(cands))
	for i := range cands {
		ptrs = append(ptrs, &cands[i])
	}
	return agentmemory.Recall(ptrs, symptom, agentmemory.RecallOptions{
		TopK:       topK,
		AgentKind:  agentKind,
		TargetNode: targetNode,
	})
}

// Recall 供接口直接调用（控制台"以症状试召回"）。
//
// 与 RecallForDiagnose 的区别只是**参数来自请求**：它必须返回"比较了多少条候选"
// 这类信息，让人能判断"是没召回到，还是库里根本没人写过相似经验"。
func (s *MemoryService) Recall(symptom, agentKind, targetNode string, topK int) (agentmemory.RecallResult, error) {
	if strings.TrimSpace(symptom) == "" {
		return agentmemory.RecallResult{}, fmt.Errorf("symptom 不能为空")
	}
	if s == nil || s.store == nil {
		return agentmemory.RecallResult{}, ErrMemoryNotFound
	}
	cands, err := s.store.Candidates(0)
	if err != nil {
		return agentmemory.RecallResult{}, err
	}
	// 控制台查询允许临时放宽上限，但**不允许看到停用的条目**：
	// 停用意味着"不要再用它影响模型"，能查出来的只是它的存在（列表接口按 enabled 过滤）。
	ptrs := make([]*models.AgentMemory, 0, len(cands))
	for i := range cands {
		ptrs = append(ptrs, &cands[i])
	}
	return agentmemory.Recall(ptrs, symptom, agentmemory.RecallOptions{
		TopK:       topK,
		AgentKind:  agentKind,
		TargetNode: targetNode,
	}), nil
}

// Search 分页查询经验。
func (s *MemoryService) Search(q models.MemoryQuery) ([]models.AgentMemory, int64, error) {
	if s == nil || s.store == nil {
		return nil, 0, errors.New("经验库未装配")
	}
	return s.store.List(q)
}

// Find 读取单条。
func (s *MemoryService) Find(memoryID string) (*models.AgentMemory, error) {
	if strings.TrimSpace(memoryID) == "" {
		return nil, ErrMemoryNotFound
	}
	row, err := s.store.Find(strings.TrimSpace(memoryID))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrMemoryNotFound
	}
	return row, nil
}

// Stats 统计口径。
func (s *MemoryService) Stats() (*models.MemoryStats, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("经验库未装配")
	}
	return s.store.Stats()
}

// SetEnabled 停用/启用一条经验（停用必须给原因）。
//
// 顺序：先查存在性（决定 1019 还是 1020），再走 CAS。
// 为什么不直接依赖 CAS 的 RowsAffected：那样"单号不存在"与"状态已变更"会合并成同一个返回，
// 运维就无法区分"我点错了单子"与"别人刚改过"。
func (s *MemoryService) SetEnabled(memoryID string, enabled bool, by, reason string) (*models.AgentMemory, error) {
	row, err := s.Find(memoryID)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if !enabled && reason == "" {
		return nil, ErrMemoryReasonRequired
	}
	ok, err := s.store.SetEnabled(models.MemoryStateChange{
		MemoryID: row.MemoryID,
		Enabled:  enabled,
		By:       by,
		Reason:   reason,
		Now:      s.now(),
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return row, ErrMemoryConflict
	}
	// 回读一次，把"改完之后到底是什么状态"如实返回（不靠内存推断）。
	updated, err := s.store.Find(row.MemoryID)
	if err != nil || updated == nil {
		return row, nil
	}
	return updated, nil
}

// Record 把一次会话沉淀进经验库（实现 agentruntime.MemoryRecorder）。
//
// 方法名与 agentruntime.MemoryRecorder 保持一致（而不是 RecordSession）：
// 这样"控制面"可以直接当沉淀器接线，不需要再套一层适配器——
// 每多一层适配器就多一个"两边签名悄悄漂移"的位置。
//
// 注意返回值的语义：**Errors 非空不代表接口该失败**。沉淀是旁路，
// 调用方（运行时）只把它写进日志。
func (s *MemoryService) Record(session *models.AgentSession, audits []models.AgentToolAudit) agentmemory.RecordResult {
	if s == nil || s.recorder == nil {
		return agentmemory.RecordResult{MemoryIDs: []string{}}
	}
	return s.recorder.Record(session, audits)
}
