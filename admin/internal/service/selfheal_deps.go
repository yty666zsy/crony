package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agentmemory"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/notify"
	"github.com/tmnhs/crony/common/pkg/selfheal"
	"github.com/tmnhs/crony/common/pkg/tools"
	"github.com/tmnhs/crony/common/pkg/utils"
	"gorm.io/gorm"
)

// ErrHealNotFound 自愈记录不存在（接口层映射成 1023）。
var ErrHealNotFound = errors.New("自愈记录不存在")

// selfHealService：进程内单例（与 DefaultMemoryService 同一模式）。
var selfHealService *SelfHealService

// NewDefaultSelfHealService 生产装配：把自愈服务接到真实数据库、经验库与工具通道上。
//
// 装配要点（每一处都有非它不可的理由）：
//   - Retry 走 `NewJobRetryExecutor()`：与审批执行**同一条通道**。
//     两条投递路径意味着两种行为，而"自愈后到底会发生什么"必须唯一（与 4.2 同一取舍）。
//   - WritesEnabled 读运行时能力面（写工具是否真的注册上了），而不是另读一份配置：
//     配置与能力面不一致时，"能力面"才是事实。
//   - UpdateTimeout 只改 `timeout` 一列：不整行覆盖（整行覆盖会把并发读取到的
//     其它字段一起写回去，是典型的数据丢失事故）。
//   - Notify 只发给任务自己配置的收件人（沿用既有失败通知口径），没有配置就静默。
func NewDefaultSelfHealService(policy selfheal.Policy) *SelfHealService {
	retryExec := NewJobRetryExecutor()
	deps := HealDeps{
		FindFailedRuns: func(since time.Time, limit int) ([]*models.JobRun, error) {
			runs := make([]*models.JobRun, 0)
			err := dbclient.GetMysqlDB().Table(models.CronyJobRunTableName).
				Where("status in ?", []int{int(models.RunStatusFailed), int(models.RunStatusTimeout)}).
				Where("finished_at >= ?", since.Unix()).
				Order("id desc").Limit(limit).Find(&runs).Error
			if err != nil {
				return nil, err
			}
			return runs, nil
		},
		FindSteps: func(runID string) ([]models.JobRunStep, error) {
			steps := make([]models.JobRunStep, 0)
			err := dbclient.GetMysqlDB().Table(models.CronyJobRunStepTableName).
				Where("run_id = ?", runID).Order("step_index asc").Find(&steps).Error
			if err != nil {
				return nil, err
			}
			return steps, nil
		},
		FindJob: func(jobID int) (*models.Job, error) {
			job := &models.Job{ID: jobID}
			if err := job.FindById(); err != nil {
				return nil, err
			}
			return job, nil
		},
		FindNode: func(uuid string) (*models.Node, error) {
			node := &models.Node{UUID: uuid}
			if err := node.FindByUUID(); err != nil {
				return nil, err
			}
			return node, nil
		},
		HealsForSource: func(sourceRunID string) ([]*models.JobHeal, error) {
			rows := make([]models.JobHeal, 0)
			err := dbclient.GetMysqlDB().Table(models.CronyJobHealTableName).
				Where("source_run_id = ?", sourceRunID).Order("id asc").Find(&rows).Error
			if err != nil {
				return nil, err
			}
			return ptrHeals(rows), nil
		},
		HealsForJob: func(jobID int, since time.Time) ([]*models.JobHeal, error) {
			rows := make([]models.JobHeal, 0)
			err := dbclient.GetMysqlDB().Table(models.CronyJobHealTableName).
				Where("job_id = ? and created >= ?", jobID, since.Unix()).
				Order("id asc").Find(&rows).Error
			if err != nil {
				return nil, err
			}
			return ptrHeals(rows), nil
		},
		ActiveHeals: func() ([]*models.JobHeal, error) {
			rows := make([]models.JobHeal, 0)
			err := dbclient.GetMysqlDB().Table(models.CronyJobHealTableName).
				Where("status = ?", string(models.HealStatusExecuted)).
				Order("id asc").Find(&rows).Error
			if err != nil {
				return nil, err
			}
			return ptrHeals(rows), nil
		},
		SaveHeal: func(h *models.JobHeal) error {
			return saveHealRow(h)
		},
		FindHeal: func(healID string) (*models.JobHeal, error) {
			row := &models.JobHeal{}
			err := dbclient.GetMysqlDB().Table(models.CronyJobHealTableName).
				Where("heal_id = ?", healID).First(row).Error
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil, nil
				}
				return nil, err
			}
			return row, nil
		},
		SearchHeals: searchHeals,
		FollowRun:   followRunAfter,
		UpsertMemory: func(obs models.MemoryObservation, now time.Time) ([]string, error) {
			mem := obs.ToAgentMemory(now)
			created, err := models.UpsertAgentMemory(dbclient.GetMysqlDB(), mem)
			if err != nil {
				return nil, err
			}
			_ = created
			return []string{mem.MemoryID}, nil
		},
		RecallStats: recallHealStats,
		Retry: func(ctx context.Context, req tools.RetryRequest) (tools.Result, error) {
			return retryExec.RetryJob(ctx, req)
		},
		UpdateTimeout: func(jobID int, timeout int64) error {
			return dbclient.GetMysqlDB().Table(models.CronyJobTableName).
				Where("id = ?", jobID).Update("timeout", timeout).Error
		},
		WritesEnabled: func() bool {
			rt := DefaultAgentRuntime()
			return rt != nil && rt.WritesEnabled()
		},
		Notify: func(job *models.Job, subject, body string) {
			if job == nil || len(job.NotifyTo) == 0 {
				return
			}
			to := make([]string, 0)
			_ = json.Unmarshal(job.NotifyTo, &to)
			msg := &notify.Message{
				Type:      notify.NotifyTypeMail,
				Subject:   subject,
				Body:      "[Crony 自愈调度] " + body,
				To:        to,
				OccurTime: time.Now().Format(utils.TimeFormatSecond),
			}
			go notify.Send(msg)
		},
		Now:  time.Now,
		Logf: func(format string, args ...interface{}) { logger.GetLogger().Info(fmt.Sprintf(format, args...)) },
	}
	return NewSelfHealService(deps, policy)
}

// ptrHeals 把值切片转成指针切片（便于闸门里做 nil 判定）。
func ptrHeals(rows []models.JobHeal) []*models.JobHeal {
	out := make([]*models.JobHeal, 0, len(rows))
	for i := range rows {
		out = append(out, &rows[i])
	}
	return out
}

// saveHealRow 落库一行自愈记录（存在则更新，不存在则插入）。
func saveHealRow(h *models.JobHeal) error {
	if h == nil {
		return errors.New("自愈记录为空")
	}
	db := dbclient.GetMysqlDB().Table(models.CronyJobHealTableName)
	var exists int64
	if err := db.Where("heal_id = ?", h.HealID).Count(&exists).Error; err != nil {
		return err
	}
	if exists == 0 {
		return db.Create(h).Error
	}
	return db.Where("heal_id = ?", h.HealID).Updates(map[string]interface{}{
		"status":        string(h.Status),
		"action":        string(h.Action),
		"action_json":   h.ActionJSON,
		"reason":        h.Reason,
		"policy_json":   h.PolicyJSON,
		"evidence_json": h.EvidenceJSON,
		"memory_json":   h.MemoryJSON,
		"memory_ids":    h.MemoryIDs,
		"new_run_id":    h.NewRunID,
		"outcome_note":  h.OutcomeNote,
		"deadline":      h.Deadline,
		"attempt":       h.Attempt,
		"finished_at":   h.FinishedAt,
		"updated":       h.Updated,
		"updated_ms":    h.UpdatedMS,
	}).Error
}

// searchHeals 分页查询自愈记录。
func searchHeals(req *request.ReqJobHealSearch) ([]models.JobHeal, int64, error) {
	if req == nil {
		return nil, 0, errors.New("查询条件为空")
	}
	req.Check()
	db := dbclient.GetMysqlDB().Table(models.CronyJobHealTableName)
	if req.JobID > 0 {
		db = db.Where("job_id = ?", req.JobID)
	}
	if s := strings.TrimSpace(req.Status); s != "" {
		db = db.Where("status = ?", s)
	}
	if s := strings.TrimSpace(req.FailureClass); s != "" {
		db = db.Where("failure_class = ?", s)
	}
	if s := strings.TrimSpace(req.Action); s != "" {
		db = db.Where("action = ?", s)
	}
	if s := strings.TrimSpace(req.SourceRunID); s != "" {
		db = db.Where("source_run_id = ?", s)
	}
	if s := strings.TrimSpace(req.NodeUUID); s != "" {
		db = db.Where("node_uuid = ?", s)
	}
	rows := make([]models.JobHeal, 0)
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := db.Order("id desc").Limit(req.PageSize).Offset((req.Page - 1) * req.PageSize).Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// followRunAfter 找"自愈之后该任务新出现的那次执行"。
//
// 判定口径：source run 的 id 之后、同一个 job_id 的第一条 job_run（按 id 升序）。
// 为什么用 id 而不是时间：scheduled_time 对"立即执行一次"是触发时刻，精度只到秒，
// 同秒内多次触发会分不出先后；自增 id 才是真正的写入顺序。
func followRunAfter(h *models.JobHeal) (*models.JobRun, error) {
	if h == nil || strings.TrimSpace(h.SourceRunID) == "" {
		return nil, nil
	}
	db := dbclient.GetMysqlDB()
	src := &models.JobRun{}
	if err := db.Table(models.CronyJobRunTableName).
		Select("id, run_id, job_id").Where("run_id = ?", h.SourceRunID).First(src).Error; err != nil {
		// 源执行查不到：不能假装"没有后续执行"，直接把错误交上去（观测失败会被记录）。
		return nil, fmt.Errorf("读取源执行 %s 失败：%w", h.SourceRunID, err)
	}
	next := &models.JobRun{}
	err := db.Table(models.CronyJobRunTableName).
		Where("job_id = ? and id > ?", src.JobID, src.ID).
		Order("id asc").First(next).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return next, nil
}

// recallHealStats 召回同类症状的自愈经验并按动作聚合。
//
// 口径（与 4-A 一致）：经验库只作**排序线索**，绝不当证据。
// 返回的 hint 是"参考了哪些历史、成功率如何"的可读说明，会落进 job_heal.memory_json，
// 让"这次为什么选了重跑而不是改超时"事后能查。
func recallHealStats(symptom, nodeUUID string) (map[models.HealAction]selfheal.HistoryStats, []string, string) {
	svc := DefaultMemoryService()
	if svc == nil {
		return nil, nil, ""
	}
	res := svc.RecallForDiagnose(symptom, AgentKindSelfHeal, nodeUUID, 8)
	if len(res.Hits) == 0 {
		return nil, nil, ""
	}
	stats := map[models.HealAction]selfheal.HistoryStats{}
	ids := make([]string, 0, len(res.Hits))
	for _, hit := range res.Hits {
		action, ok := selfheal.ActionFromMemoryText(hit.ActionText)
		if !ok {
			// 认不出动作的经验不进统计（宁可少算，不可算错）。
			continue
		}
		st := stats[action]
		switch hit.Outcome {
		case models.MemoryOutcomeSuccess:
			st.Success++
		case models.MemoryOutcomeFailed:
			st.Fail++
		case models.MemoryOutcomeBlocked:
			st.Blocked++
		case models.MemoryOutcomeDenied:
			st.Denied++
		}
		stats[action] = st
		ids = append(ids, hit.MemoryID)
	}
	if len(stats) == 0 {
		return nil, nil, ""
	}
	hint := "参考 " + fmt.Sprintf("%d", len(ids)) + " 条同类历史经验："
	for action, st := range stats {
		if rate, ok := st.Rate(); ok {
			hint += fmt.Sprintf("%s 成功率 %.0f%%（%d/%d）；", action, rate*100, st.Success, st.Success+st.Fail)
		} else {
			hint += fmt.Sprintf("%s 无执行记录（%d 次被挡）；", action, st.Samples())
		}
	}
	return stats, ids, agentmemory.ClipRunes(hint, 400)
}

// SelfHealPolicyFromConfig 读取配置里的自愈策略（缺省即默认：关着）。
func SelfHealPolicyFromConfig() selfheal.Policy {
	cfg := config.GetConfigModels()
	if cfg == nil {
		return selfheal.DefaultPolicy()
	}
	return selfheal.Policy{
		Enabled:                 cfg.SelfHeal.Enabled,
		MaxAttemptsPerJobWindow: cfg.SelfHeal.MaxAttemptsPerJobWindow,
		WindowSeconds:           cfg.SelfHeal.WindowSeconds,
		CooldownSeconds:         cfg.SelfHeal.CooldownSeconds,
		ExtendTimeoutFactor:     cfg.SelfHeal.ExtendTimeoutFactor,
		MaxExtendTimeoutSeconds: cfg.SelfHeal.MaxExtendTimeoutSeconds,
		MaxConcurrent:           cfg.SelfHeal.MaxConcurrent,
		ObserveWindowSeconds:    cfg.SelfHeal.ObserveWindowSeconds,
		ScanLimit:               cfg.SelfHeal.ScanLimit,
		LookbackSeconds:         cfg.SelfHeal.LookbackSeconds,
	}.Normalize()
}

// DefaultSelfHealService 取进程内自愈服务（可能为 nil，调用方必须处理）。
func DefaultSelfHealService() *SelfHealService {
	if selfHealService == nil {
		return nil
	}
	return selfHealService
}

// SetSelfHealService 装配自愈服务（启动流程与测试用）。
func SetSelfHealService(s *SelfHealService) {
	selfHealService = s
}

// InitSelfHeal 启动时装配：注册自愈表由 RegisterTables 负责，这里只建服务与循环。
//
// 循环**始终启动**（即使 enabled=false）：关着的时候扫描只做分类与留痕，
// 不做任何动作。这样运维能先看到"如果我打开，它会怎么判"——把开关从 0 到 1
// 之前有真实依据，而不是靠承诺。
func InitSelfHeal(closeRef *chan struct{}) *SelfHealService {
	svc := NewDefaultSelfHealService(SelfHealPolicyFromConfig())
	SetSelfHealService(svc)
	interval := time.Duration(config.GetConfigModels().SelfHeal.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if closeRef != nil {
		ch := RunSelfHealLoop(svc, interval)
		*closeRef = ch
	}
	logger.GetLogger().Info(fmt.Sprintf("[self_heal] 自愈调度已就绪：enabled=%v 间隔=%s 窗口内上限=%d 冷却=%ds 并发上限=%d",
		svc.Policy().Enabled, interval, svc.Policy().MaxAttemptsPerJobWindow,
		svc.Policy().CooldownSeconds, svc.Policy().MaxConcurrent))
	return svc
}

// RunSelfHealLoop 周期扫描（与 RunLogCleaner 同一模式：ticker + close 通道）。
//
// 每轮都重新读一次配置：策略是安全边界，改了配置应当**在不重启的情况下生效**，
// 否则"先把自愈关掉"这件事会变成一次重启流程。
func RunSelfHealLoop(svc *SelfHealService, interval time.Duration) chan struct{} {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	closed := make(chan struct{})
	go func() {
		for {
			select {
			case <-t.C:
				svc.SetPolicy(SelfHealPolicyFromConfig())
				ctx, cancel := context.WithTimeout(context.Background(), interval)
				rep, err := svc.ScanOnce(ctx)
				cancel()
				if err != nil {
					logger.GetLogger().Error(fmt.Sprintf("[self_heal] 扫描失败：%v", err))
					continue
				}
				if rep.Scanned > 0 || rep.Observed > 0 || len(rep.Errors) > 0 {
					logger.GetLogger().Info(fmt.Sprintf(
						"[self_heal] 本轮：扫描=%d 跳过=%d 拒绝=%d 已执行=%d 转人工=%d 失败=%d 观测=%d 成功=%d 错误=%v",
						rep.Scanned, rep.Skipped, rep.Denied, rep.Executed, rep.Escalated,
						rep.Failed, rep.Observed, rep.Succeeded, rep.Errors))
				}
			case <-closed:
				t.Stop()
				return
			}
		}
	}()
	return closed
}
