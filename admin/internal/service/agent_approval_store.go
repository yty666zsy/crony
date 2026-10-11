package service

import (
	"errors"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"gorm.io/gorm"
)

// errNoDB 数据库未初始化（配置缺失/启动早期）。审批是写动作的最后一道门，
// 这里绝不能"没有库就放过"——宁可报错，也不要在没有审计落点时执行写动作。
var errNoDB = errors.New("数据库未初始化，审批单无法读写")

// GormApprovalStore 审批单的 gorm 实现。
//
// 它只做"取 DB + 转调 models"这一件事：所有 CAS 语义都在 models 里（SQL 条件更新），
// 因为"一次批准一次执行"这种不变式必须落在**数据库**上——放在 service 里做"先查后写"，
// 多实例部署立刻失效。
type GormApprovalStore struct {
	// DB 可注入（测试用同库不同连接时传进来）；为空则用全局单例。
	DB *gorm.DB
}

// NewGormApprovalStore 默认实现（用全局 MySQL 单例）。
func NewGormApprovalStore() *GormApprovalStore { return &GormApprovalStore{} }

func (s *GormApprovalStore) db() *gorm.DB {
	if s != nil && s.DB != nil {
		return s.DB
	}
	return dbclient.GetMysqlDB()
}

// Create 落单。
func (s *GormApprovalStore) Create(req *models.ApprovalRequest) error {
	db := s.db()
	if db == nil {
		return errNoDB
	}
	return models.CreateApprovalRequest(db, req)
}

// Find 按单号查。
func (s *GormApprovalStore) Find(approvalID string) (*models.ApprovalRequest, error) {
	db := s.db()
	if db == nil {
		return nil, errNoDB
	}
	return models.FindApprovalRequest(db, approvalID)
}

// List 分页查询。
func (s *GormApprovalStore) List(status, tool, targetNode string, page, pageSize int) ([]models.ApprovalRequest, int64, error) {
	db := s.db()
	if db == nil {
		return nil, 0, errNoDB
	}
	return models.ListApprovalRequests(db, status, tool, targetNode, page, pageSize)
}

// Decide CAS 决策。
func (s *GormApprovalStore) Decide(approvalID, toStatus, decidedBy, note string, now int64) (bool, error) {
	db := s.db()
	if db == nil {
		return false, errNoDB
	}
	return models.DecideApprovalRequest(db, approvalID, toStatus, decidedBy, note, now)
}

// MarkExecuted CAS 回写执行结果。
func (s *GormApprovalStore) MarkExecuted(approvalID, status, result string, now int64) (bool, error) {
	db := s.db()
	if db == nil {
		return false, errNoDB
	}
	return models.MarkApprovalExecuted(db, approvalID, status, result, now)
}

// MarkBlocked CAS 回写"执行前复核未通过"（approved → blocked，不写 executed_at）。
func (s *GormApprovalStore) MarkBlocked(approvalID, result string) (bool, error) {
	db := s.db()
	if db == nil {
		return false, errNoDB
	}
	return models.MarkApprovalBlocked(db, approvalID, result)
}

// ExpirePending 惰性过期清理。
func (s *GormApprovalStore) ExpirePending(now int64) (int64, error) {
	db := s.db()
	if db == nil {
		return 0, errNoDB
	}
	return models.ExpirePendingApprovals(db, now)
}
