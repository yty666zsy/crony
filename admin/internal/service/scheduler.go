package service

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/scheduling"
	"gorm.io/gorm"
)

// 阶段 4-D：多目标调度服务。
//
// 它把"候选节点的观测值"从数据库里采出来，交给纯函数 scheduling.Decide 打分。
// 观测口径（都要能解释，否则打分就是玄学）：
//   - 负载：该节点上**已分配**的任务数（job.run_on = uuid）；
//   - 容量：节点自报的并发上限（node.capacity）；
//   - 健康度：近 N 小时该节点上已结束执行的**成功率**（job_run）；
//   - 亲和：任务声明的 prefer_node（默认取任务当前 RunOn，即"尽量别乱搬"）。
type SchedulerService struct{}

var DefaultSchedulerService = new(SchedulerService)

// healthWindow 健康度统计窗口：近 3 小时。
const healthWindow = 3 * time.Hour

// nodeSelector 是 job.node_selector 的解析形态（未知字段忽略，宽松解析）。
type nodeSelector struct {
	RequireLabels map[string]string `json:"require_labels"`
	RequireTools  []string          `json:"require_tools"`
	PreferNode    string            `json:"prefer_node"`
}

// BuildRequirement 从任务推导调度要求。
func (s *SchedulerService) BuildRequirement(job *models.Job) scheduling.Requirement {
	req := scheduling.Requirement{}
	if job == nil {
		return req
	}
	if sel := strings.TrimSpace(job.NodeSelector); sel != "" {
		var ns nodeSelector
		if json.Unmarshal([]byte(sel), &ns) == nil {
			req.RequireLabels = ns.RequireLabels
			req.RequireTools = ns.RequireTools
			req.PreferNode = strings.TrimSpace(ns.PreferNode)
		}
	}
	// 没有显式 prefer_node 时，用当前 RunOn 作为亲和（"能不搬就不搬"）。
	if req.PreferNode == "" {
		req.PreferNode = strings.TrimSpace(job.RunOn)
	}
	return req
}

// ExplainAllocation 采观测值 → 打分 → 返回可解释决策。
func (s *SchedulerService) ExplainAllocation(job *models.Job) scheduling.Decision {
	db := dbclient.GetMysqlDB()
	nodes := make([]models.Node, 0)
	if err := db.Table(models.CronyNodeTableName).Find(&nodes).Error; err != nil {
		return scheduling.Decision{Notes: []string{"读取节点失败：" + err.Error()}}
	}
	since := time.Now().Add(-healthWindow).Unix()
	cands := make([]scheduling.NodeCandidate, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		var load int64
		_ = db.Table(models.CronyJobTableName).Where("run_on = ?", n.UUID).Count(&load).Error
		c := scheduling.NodeCandidate{
			UUID:      n.UUID,
			Hostname:  n.Hostname,
			Labels:    n.LabelMap(),
			Capacity:  n.Capacity,
			Load:      int(load),
			Reachable: n.Status != models.NodeConnFail,
		}
		c.SuccessRate, c.Samples = nodeSuccessRate(db, n.UUID, since)
		cands = append(cands, c)
	}
	return scheduling.Decide(s.BuildRequirement(job), cands, scheduling.Weights{})
}

// Allocate 选出一个节点；没有可用节点时返回 ""。
func (s *SchedulerService) Allocate(job *models.Job) string {
	return s.ExplainAllocation(job).Chosen
}

// nodeSuccessRate 近期成功率（0~1）+ 样本数；无样本时 samples=0（调度按中性处理）。
// 分母只含**已结束**的执行（succeeded/failed/timeout），未结束的不进分母——
// 与项目里其它成功率口径保持一致，否则"很多任务还在跑"会被误读成成功率很低。
func nodeSuccessRate(db *gorm.DB, uuid string, since int64) (float64, int) {
	type agg struct {
		Total int64
		OK    int64
	}
	var a agg
	_ = db.Table(models.CronyJobRunTableName).
		Select("count(*) as total, sum(case when status = ? then 1 else 0 end) as ok", int(models.RunStatusSuccess)).
		Where("node_uuid = ? and finished_at >= ?", uuid, since).
		Scan(&a).Error
	if a.Total <= 0 {
		return 0, 0
	}
	return float64(a.OK) / float64(a.Total), int(a.Total)
}
