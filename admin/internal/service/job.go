package service

import (
	"fmt"
	"github.com/coreos/etcd/clientv3"
	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/etcdclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/utils"
	"time"
)

type JobService struct {
}

var DefaultJobService = new(JobService)

func (j *JobService) Search(s *request.ReqJobSearch) ([]models.Job, int64, error) {
	db := dbclient.GetMysqlDB().Table(models.CronyJobTableName)
	if s.ID > 0 {
		db = db.Where("id = ?", s.ID)
	}
	if len(s.Name) > 0 {
		db = db.Where("name like ?", s.Name+"%")
	}
	if len(s.RunOn) > 0 {
		db = db.Where("run_on = ?", s.RunOn)
	}
	if s.Type > 0 {
		db = db.Where("type = ?", s.Type)
	}
	if s.Status != nil {
		db = db.Where("status = ? ", *s.Status)
	}
	// make([]T, 0) 的原因同 node.go：非空切片会让 GORM 追加而不是覆盖。
	jobs := make([]models.Job, 0)
	var total int64
	err := db.Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	err = db.Limit(s.PageSize).Offset((s.Page - 1) * s.PageSize).Find(&jobs).Error
	if err != nil {
		return nil, 0, err
	}
	return jobs, total, nil
}

func (j *JobService) SearchJobLog(s *request.ReqJobLogSearch) ([]models.JobLog, int64, error) {
	db := dbclient.GetMysqlDB().Table(models.CronyJobLogTableName)
	if len(s.Name) > 0 {
		db = db.Where("name like ?", s.Name+"%")
	}
	if s.JobId > 0 {
		db = db.Where("job_id = ?", s.JobId)
	}
	if len(s.NodeUUID) > 0 {
		db = db.Where("node_uuid = ?", s.NodeUUID)
	}
	if s.Success != nil {
		db = db.Where("success = ? ", *s.Success)
	}
	jobLogs := make([]models.JobLog, 0)
	var total int64
	err := db.Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	err = db.Limit(s.PageSize).Offset((s.Page - 1) * s.PageSize).Order("start_time desc").Find(&jobLogs).Error
	if err != nil {
		return nil, 0, err
	}

	return jobLogs, total, nil
}

// Get the total number of tasks executed today 1 indicates success 0 indicates failure
func (j *JobService) GetTodayJobExcCount(success int) (int64, error) {
	db := dbclient.GetMysqlDB().Table(models.CronyJobLogTableName).Where("start_time > ? and end_time!=0 and success = ?", utils.GetTodayUnix(), success)
	var total int64
	err := db.Count(&total).Error
	if err != nil {
		return 0, err
	}
	return total, nil
}

// The number of tasks per day in a certain period of time
func (j *JobService) GetJobExcCount(start, end int64, success int) ([]resp.DateCount, error) {
	var dateCount []resp.DateCount
	db := dbclient.GetMysqlDB().Table(models.CronyJobLogTableName).Select("FROM_UNIXTIME( start_time, '%Y-%m-%d' ) AS date", "COUNT( * ) AS count ").Group("date").Order("date ASC").Where("start_time > ? and start_time<?  and end_time!=0 and success = ?", start, end, success)
	err := db.Find(&dateCount).Error
	if err != nil {
		return nil, err
	}
	return dateCount, nil
}

func (j *JobService) GetNotAssignedJob() (jobs []models.Job, err error) {
	err = dbclient.GetMysqlDB().Table(models.CronyJobTableName).Where("status = ?", models.JobStatusNotAssigned).Find(&jobs).Error
	return
}

func (j *JobService) GetRunningJobCount() (int64, error) {
	wresp, err := etcdclient.Get(fmt.Sprintf(etcdclient.KeyEtcdProcProfile), clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		return 0, err
	}

	return wresp.Count, nil
}

const MaxJobCount = 10000

// AutoAllocateNodeFor 阶段 4-D：带任务的分配——多目标加权打分（scheduling 内核）。
//
// 与旧的 AutoAllocateNode 的关系：**打分优先，旧策略兜底，但兜底不越过硬约束**。
//   - 调度器能在"满足任务硬约束"的候选里选出节点 → 用它；
//   - 任务**没有**硬约束（未声明 require_labels/require_tools）→ 退化为旧策略
//     （任务数最少），对老任务保持零回归；
//   - 任务**有**硬约束但当前一个节点都不满足 → 返回 ""（宁可留空，也不乱派）。
//
// 为什么必须有第三条（20 节点舰队实测，F3）：兜底若不分青红皂白就会**绕过硬约束**——
// 一个 require_labels 指向不存在标签的任务会被兜底随手派给任意节点，
// 于是"硬约束"名存实亡，任务在一台装不了它的机器上失败。
func (j *JobService) AutoAllocateNodeFor(job *models.Job) string {
	if chosen := DefaultSchedulerService.Allocate(job); chosen != "" {
		return chosen
	}
	if DefaultSchedulerService.HasHardRequirement(job) {
		return ""
	}
	return j.AutoAllocateNode()
}

// Give priority to the node with the least number of tasks
func (j *JobService) AutoAllocateNode() string {
	//Get all the living nodes
	nodeList := DefaultNodeWatcher.List2Array()
	resultCount, resultNodeUUID := MaxJobCount, ""
	for _, nodeUUID := range nodeList {
		//Check the database to see if it is alive
		node := &models.Node{UUID: nodeUUID}
		err := node.FindByUUID()
		if err != nil {
			continue
		}
		if node.Status == models.NodeConnFail {
			//The node has failed
			// 必须走加锁的 delNodeList：直接 delete(map) 与 watcher 的写入并发会触发
			// Go 运行时的 "concurrent map writes" 致命错误（20 节点高频上下线时概率显著上升）。
			DefaultNodeWatcher.delNodeList(nodeUUID)
			continue
		}
		count, err := DefaultNodeWatcher.GetJobCount(nodeUUID)
		if err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("node[%s] get job conut error:%s", nodeUUID, err.Error()))
			continue
		}
		if resultCount > count {
			resultCount, resultNodeUUID = count, nodeUUID
		}
	}
	return resultNodeUUID
}

func (j *JobService) Once(once *request.ReqJobOnce) (err error) {
	//The default existence time is 60 seconds
	_, err = etcdclient.PutWithTtl(fmt.Sprintf(etcdclient.KeyEtcdOnce, once.JobId), once.NodeUUID, 60)
	return
}

func RunLogCleaner(cleanPeriod time.Duration, expiration int64) (close chan struct{}) {
	t := time.NewTicker(cleanPeriod)
	close = make(chan struct{})
	go func() {
		for {
			select {
			case <-t.C:
				err := cleanupLogs(expiration)
				if err != nil {
					logger.GetLogger().Error(fmt.Sprintf("clean up logs at time:%v error:%s", time.Now(), err.Error()))
				}
			case <-close:
				t.Stop()
				return
			}
		}
	}()
	return
}

func cleanupLogs(expirationTime int64) error {
	sql := fmt.Sprintf("delete from %s where start_time < ?", models.CronyJobLogTableName)
	if err := dbclient.GetMysqlDB().Exec(sql, time.Now().Unix()-expirationTime).Error; err != nil {
		return err
	}
	// 阶段 1：新模型（job_run / job_run_step）跟随同一过期策略清理，
	// 否则新表会无上限增长（旧表有清理、新表没有，是最典型的数据膨胀事故）。
	return CleanupRuns(expirationTime)
}
