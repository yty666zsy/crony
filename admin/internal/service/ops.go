package service

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/autonomy"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/eval"
	"github.com/tmnhs/crony/common/pkg/slo"
	"gorm.io/gorm"
)

// 阶段 4-D / 阶段 5：运维智能化的读取与评估服务（多目标调度见 scheduler.go）。
//
// 这一层的原则：**只读 + 纯计算**为主，唯一的写是 SLO 契约维护与自治信用状态迁移。
// 它们都不"直接改生产"，而是把"系统现在健康吗、调度为什么这么选、Agent 这轮该不该放权"
// 变成可解释的事实。

// ---------- SLO 契约 ----------

type SloService struct{}

var DefaultSloService = new(SloService)

// SloEvaluation 一条契约的评估结果（契约 + 实际报告 + 任务名）。
type SloEvaluation struct {
	Policy  models.SloPolicy `json:"policy"`
	JobName string           `json:"job_name,omitempty"`
	Report  slo.Report       `json:"report"`
}

// SearchSlo 列出契约（可按 job_id 过滤）。
func (s *SloService) SearchSlo(jobID int) ([]models.SloPolicy, error) {
	db := dbclient.GetMysqlDB().Table(models.CronySloPolicyTableName)
	if jobID > 0 {
		db = db.Where("job_id = ?", jobID)
	}
	list := make([]models.SloPolicy, 0)
	if err := db.Order("id desc").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// SaveSlo 新建或更新一条契约（id>0 更新，否则新建）。
func (s *SloService) SaveSlo(p *models.SloPolicy) error {
	if p == nil || strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("SLO 契约必须有名字")
	}
	now := time.Now()
	db := dbclient.GetMysqlDB().Table(models.CronySloPolicyTableName)
	if p.WindowSeconds <= 0 {
		p.WindowSeconds = 86400
	}
	if p.GraceSeconds <= 0 {
		p.GraceSeconds = int64(slo.DefaultGrace.Seconds())
	}
	if p.BreachAction == "" {
		p.BreachAction = "diagnose"
	}
	if p.ID > 0 {
		p.Updated = now.Unix()
		return db.Where("id = ?", p.ID).Updates(map[string]interface{}{
			"name": p.Name, "job_id": p.JobID,
			"target_success_rate": p.TargetSuccessRate, "target_on_time_rate": p.TargetOnTimeRate,
			"window_seconds": p.WindowSeconds, "grace_seconds": p.GraceSeconds,
			"enabled": p.Enabled, "breach_action": p.BreachAction, "updated": p.Updated,
		}).Error
	}
	p.Created = now.Unix()
	return db.Create(p).Error
}

// EvaluateSlo 评估契约：jobID>0 只评估该任务的契约，否则评估所有启用的契约。
//
// 数据来源：job_run（已结束的执行）映射成 slo.Observation。口径与 run.go 的状态机一致：
// success=2；未结束（finished_at=0）不进统计。
func (s *SloService) EvaluateSlo(jobID int) ([]SloEvaluation, error) {
	policies, err := s.SearchSlo(jobID)
	if err != nil {
		return nil, err
	}
	out := make([]SloEvaluation, 0, len(policies))
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		obs, err := loadObservations(dbclient.GetMysqlDB(), p.JobID, time.Now().Add(-time.Duration(p.WindowSeconds)*time.Second).Unix())
		if err != nil {
			return nil, err
		}
		rep := slo.Evaluate(slo.Targets{
			TargetSuccessRate: p.TargetSuccessRate,
			TargetOnTimeRate:  p.TargetOnTimeRate,
			Window:            time.Duration(p.WindowSeconds) * time.Second,
			Grace:             time.Duration(p.GraceSeconds) * time.Second,
		}, obs)
		jobName := ""
		if p.JobID > 0 {
			job := &models.Job{}
			if err := dbclient.GetMysqlDB().Table(models.CronyJobTableName).Where("id = ?", p.JobID).First(job).Error; err == nil {
				jobName = job.Name
			}
		}
		out = append(out, SloEvaluation{Policy: p, JobName: jobName, Report: rep})
	}
	return out, nil
}

// loadObservations 把 job_run 映射成 SLO 观测。jobID=0 表示全部任务。
func loadObservations(db *gorm.DB, jobID int, since int64) ([]slo.Observation, error) {
	runs := make([]models.JobRun, 0)
	q := db.Table(models.CronyJobRunTableName).Where("finished_at > 0 and finished_at >= ?", since)
	if jobID > 0 {
		q = q.Where("job_id = ?", jobID)
	}
	if err := q.Order("id desc").Limit(5000).Find(&runs).Error; err != nil {
		return nil, err
	}
	obs := make([]slo.Observation, 0, len(runs))
	for i := range runs {
		r := &runs[i]
		obs = append(obs, slo.Observation{
			RunID:         r.RunID,
			Status:        r.Status,
			ScheduledTime: r.ScheduledTime,
			StartedAt:     r.StartedAt,
			FinishedAt:    r.FinishedAt,
		})
	}
	return obs, nil
}

// ---------- 指标看板 ----------

type MetricsService struct{}

var DefaultMetricsService = new(MetricsService)

// Overview 汇总运维关心的核心指标（阶段 5 指标看板的后端）。
//
// 数据全部来自已有表，不引入新的埋点：任务数、今日执行、成功率、执行耗时、
// 节点在线、审批待办、自愈分布、经验库规模。任何一项拿不到都以 0/空呈现，
// 但会在 blocked 里说明（不静默吞掉）。
func (s *MetricsService) Overview() map[string]interface{} {
	db := dbclient.GetMysqlDB()
	out := map[string]interface{}{}
	todayStart := time.Now().Truncate(24 * time.Hour).Unix()

	type runAgg struct {
		Total   int64
		Success int64
	}
	var ra runAgg
	_ = db.Table(models.CronyJobRunTableName).
		Select("count(*) as total, sum(case when status = ? then 1 else 0 end) as success", int(models.RunStatusSuccess)).
		Where("finished_at >= ?", todayStart).Scan(&ra).Error
	successRate := -1.0
	if ra.Total > 0 {
		successRate = float64(ra.Success) / float64(ra.Total)
	}
	out["runs_today"] = ra.Total
	out["runs_success_today"] = ra.Success
	out["success_rate_today"] = successRate

	var avgDur float64
	_ = db.Table(models.CronyJobRunTableName).
		Select("coalesce(avg(duration_ms),0)").Where("finished_at >= ?", todayStart).Scan(&avgDur).Error
	out["avg_duration_ms_today"] = avgDur

	var jobTotal, jobAssigned int64
	_ = db.Table(models.CronyJobTableName).Count(&jobTotal).Error
	_ = db.Table(models.CronyJobTableName).Where("run_on <> '' and status = ?", 1).Count(&jobAssigned).Error
	out["jobs_total"] = jobTotal
	out["jobs_assigned"] = jobAssigned

	var nodeTotal, nodeUp int64
	_ = db.Table(models.CronyNodeTableName).Count(&nodeTotal).Error
	_ = db.Table(models.CronyNodeTableName).Where("status <> ?", models.NodeConnFail).Count(&nodeUp).Error
	out["nodes_total"] = nodeTotal
	out["nodes_up"] = nodeUp

	var pendingApprovals int64
	_ = db.Table(models.CronyApprovalRequestTableName).Where("status = ?", "pending").Count(&pendingApprovals).Error
	out["approvals_pending"] = pendingApprovals

	heals := make([]struct {
		Status string
		N      int64
	}, 0)
	_ = db.Table(models.CronyJobHealTableName).Select("status, count(*) as n").Group("status").Scan(&heals).Error
	healMap := map[string]int64{}
	for _, h := range heals {
		healMap[h.Status] = h.N
	}
	out["heals_by_status"] = healMap

	var memTotal int64
	_ = db.Table(models.CronyAgentMemoryTableName).Where("enabled = 1").Count(&memTotal).Error
	out["memory_enabled"] = memTotal

	out["generated_at"] = time.Now().Unix()
	return out
}

// ---------- 轨迹评测（Trace → Eval） ----------

type EvalService struct{}

var DefaultEvalService = new(EvalService)

// ScoreSession 把一次 Agent 会话的轨迹映射成评测样本并打分。
//
// 输入是会话号 + 期望（工具序列 / 步数上限）；期望由人工标注的历史评测集提供（阶段 5 的种子）。
// 成本未计量时跳过成本断言（见 common/pkg/eval 的口径）。
func (s *EvalService) ScoreSession(sessionID string, expected []string, maxSteps int) (eval.Result, error) {
	db := dbclient.GetMysqlDB()
	steps := make([]models.AgentStep, 0)
	if err := db.Table(models.CronyAgentStepTableName).
		Where("session_id = ?", sessionID).Order("seq asc").Find(&steps).Error; err != nil {
		return eval.Result{}, err
	}
	if len(steps) == 0 {
		return eval.Result{CaseID: sessionID, Checks: []eval.Check{{Name: "load", Passed: false, Detail: "会话没有轨迹步骤"}}}, nil
	}
	es := make([]eval.Step, 0, len(steps))
	var latency int64
	for i := range steps {
		st := &steps[i]
		es = append(es, eval.Step{
			Kind:      st.Kind,
			Tool:      st.ToolName,
			ArgsHash:  hashString(st.ToolArgsJSON),
			LatencyMS: st.LatencyMs,
		})
		latency += st.LatencyMs
	}
	return eval.Score(eval.Case{ID: sessionID, ExpectedToolSequence: expected, MaxSteps: maxSteps},
		eval.Actual{Steps: es, LatencyMS: latency}), nil
}

func hashString(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:8])
}

// ---------- 自治信用（灰度与自动降级） ----------

type AutonomyService struct{}

var DefaultAutonomyService = new(AutonomyService)

// ListAutonomy 列出全部信用状态。
func (s *AutonomyService) ListAutonomy() ([]models.AutonomyCredit, error) {
	list := make([]models.AutonomyCredit, 0)
	err := dbclient.GetMysqlDB().Table(models.CronyAutonomyCreditTableName).Order("scope asc").Find(&list).Error
	return list, err
}

// ObserveAutonomy 记录一次动作结果并推进信用状态（升级/降级/冻结）。
func (s *AutonomyService) ObserveAutonomy(scope string, success bool) (models.AutonomyCredit, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return models.AutonomyCredit{}, fmt.Errorf("自治信用必须有 scope（形如 tool@env）")
	}
	db := dbclient.GetMysqlDB()
	row := models.AutonomyCredit{}
	err := db.Table(models.CronyAutonomyCreditTableName).Where("scope = ?", scope).First(&row).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return models.AutonomyCredit{}, err
	}
	credit := autonomy.Credit{
		Level: autonomy.Level(row.Level), Streak: row.Streak, TotalUp: row.TotalUp,
		TotalDown: row.TotalDown, Frozen: row.Frozen, FrozenReason: row.FrozenReason,
	}
	credit = autonomy.Observe(credit, success, autonomy.DefaultConfig())

	now := time.Now()
	row.Scope, row.Level, row.Streak = scope, int(credit.Level), credit.Streak
	row.TotalUp, row.TotalDown, row.Frozen, row.FrozenReason = credit.TotalUp, credit.TotalDown, credit.Frozen, credit.FrozenReason
	row.Updated, row.UpdatedMS = now.Unix(), now.UnixMilli()
	if row.ID == 0 {
		row.Created = now.Unix()
		if err := db.Table(models.CronyAutonomyCreditTableName).Create(&row).Error; err != nil {
			return models.AutonomyCredit{}, err
		}
		return row, nil
	}
	if err := db.Table(models.CronyAutonomyCreditTableName).Where("scope = ?", scope).Updates(map[string]interface{}{
		"level": row.Level, "streak": row.Streak, "total_up": row.TotalUp, "total_down": row.TotalDown,
		"frozen": row.Frozen, "frozen_reason": row.FrozenReason, "updated": row.Updated, "updated_ms": row.UpdatedMS,
	}).Error; err != nil {
		return models.AutonomyCredit{}, err
	}
	return row, nil
}

// UnfreezeAutonomy 人工解冻（显式动作）。
func (s *AutonomyService) UnfreezeAutonomy(scope string) (models.AutonomyCredit, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return models.AutonomyCredit{}, fmt.Errorf("自治信用必须有 scope")
	}
	db := dbclient.GetMysqlDB()
	row := models.AutonomyCredit{}
	if err := db.Table(models.CronyAutonomyCreditTableName).Where("scope = ?", scope).First(&row).Error; err != nil {
		return models.AutonomyCredit{}, err
	}
	credit := autonomy.Unfreeze(autonomy.Credit{Level: autonomy.Level(row.Level), Streak: row.Streak, Frozen: row.Frozen})
	now := time.Now()
	if err := db.Table(models.CronyAutonomyCreditTableName).Where("scope = ?", scope).Updates(map[string]interface{}{
		"frozen": credit.Frozen, "frozen_reason": credit.FrozenReason, "streak": credit.Streak,
		"level": int(credit.Level), "updated": now.Unix(), "updated_ms": now.UnixMilli(),
	}).Error; err != nil {
		return models.AutonomyCredit{}, err
	}
	row.Frozen, row.FrozenReason, row.Streak, row.Level = credit.Frozen, credit.FrozenReason, credit.Streak, int(credit.Level)
	return row, nil
}
