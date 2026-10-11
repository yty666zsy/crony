package handler

import (
	"encoding/json"
	"fmt"
	"github.com/coreos/etcd/clientv3"
	"github.com/jakecoffman/cron"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/etcdclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/notify"
	"github.com/tmnhs/crony/common/pkg/utils"
	"github.com/tmnhs/crony/common/pkg/utils/errors"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Job struct {
	*models.Job
}
type Jobs map[int]*Job

func JobKey(nodeUUID string, jobId int) string {
	return fmt.Sprintf(etcdclient.KeyEtcdJob, nodeUUID, jobId)
}

func GetJobAndRev(nodeUUID string, jobId int) (job *Job, rev int64, err error) {
	resp, err := etcdclient.Get(JobKey(nodeUUID, jobId))
	if err != nil {
		return
	}

	if resp.Count == 0 {
		err = errors.ErrNotFound
		return
	}

	rev = resp.Kvs[0].ModRevision
	if err = json.Unmarshal(resp.Kvs[0].Value, &job); err != nil {
		return
	}

	job.SplitCmd()
	return
}

func GetJobs(nodeUUID string) (jobs Jobs, err error) {
	resp, err := etcdclient.Get(fmt.Sprintf(etcdclient.KeyEtcdJobProfile, nodeUUID), clientv3.WithPrefix())
	if err != nil {
		return
	}

	count := len(resp.Kvs)
	jobs = make(Jobs, count)
	if count == 0 {
		return
	}

	for _, j := range resp.Kvs {
		job := new(Job)
		if e := json.Unmarshal(j.Value, job); e != nil {
			logger.GetLogger().Warn(fmt.Sprintf("job[%s] umarshal err: %s", string(j.Key), e.Error()))
			continue
		}
		if err := job.Check(); err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("job[%s] is invalid: %s", string(j.Key), err.Error()))
			continue
		}
		jobs[job.ID] = job
	}
	return
}

// RunWithRecovery 立即执行一次任务（/job/once 手动触发路径）。
//
// 阶段 1 起，真正的执行交给 RunCoordinator（并发闸门 + 幂等 + 步骤编排 + 取消传播），
// 这里只负责：兜住 panic、写兼容用的 job_log、失败时发通知。
func (j *Job) RunWithRecovery() {
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			logger.GetLogger().Warn(fmt.Sprintf("panic running job: %v\n%s", r, buf))
		}
	}()
	j.runWithLog(models.TriggerManual, "任务[%s]立即执行失败")
}

func CreateJob(j *Job) cron.FuncJob {
	taskFunc := func() {
		defer func() {
			if r := recover(); r != nil {
				const size = 64 << 10
				buf := make([]byte, size)
				buf = buf[:runtime.Stack(buf, false)]
				logger.GetLogger().Warn(fmt.Sprintf("panic running scheduled job: %v\n%s", r, buf))
			}
		}()
		logger.GetLogger().Info(fmt.Sprintf("start the job#%s#command-%s", j.Name, j.Command))
		j.runWithLog(models.TriggerScheduler, "任务[%s]执行失败")
	}
	return taskFunc
}

// runWithLog 执行一次任务并维护旧接口依赖的 job_log 记录。
//
// 记录分工（阶段 1）：
//   - job_run / job_run_step：新模型，一次执行 + 每个步骤（结构化、含幂等键与策略快照）；
//   - job_log：旧模型，保持写入以兼容旧接口 /job/log 与旧前端。
//
// 被并发策略跳过（skip）或幂等去重的执行**不写 job_log**：
// 旧表语义是"执行结果"，把"没执行"混进去会让历史成功率统计失真；
// 这类事件在 job_run 里以 status=skipped 记录，可审计。
func (j *Job) runWithLog(source models.TriggerSource, subjectFmt string) {
	t := time.Now()
	jobLogId, err := j.CreateJobLog()
	if err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("Failed to write to job log with jobID:%d nodeUUID: %s error:%s", j.ID, j.RunOn, err.Error()))
	}

	outcome := DefaultRunCoordinator().RunJob(j, source, t)
	if outcome.Skipped {
		logger.GetLogger().Warn(fmt.Sprintf("job[%d] 本次未执行：%s", j.ID, outcome.SkipReason))
		// 排队中的执行还没有 run 记录（真正开始执行时才落库），此时不写 job_log
		return
	}

	if outcome.Err != nil {
		if err = j.Fail(jobLogId, t, outcome.ErrorText(), outcome.RetryCount); err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("Failed to write to job log with jobID:%d nodeUUID: %s error:%s", j.ID, j.RunOn, err.Error()))
		}
		j.notifyFailure(fmt.Sprintf(subjectFmt, j.Name), outcome)
		return
	}
	if err = j.Success(jobLogId, t, outcome.Output, outcome.RetryCount); err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("Failed to write to job log with jobID:%d nodeUUID: %s error:%s", j.ID, j.RunOn, err.Error()))
	}
}

// notifyFailure 发送失败通知（保持与旧实现一致的通知渠道与收件人解析逻辑）。
func (j *Job) notifyFailure(subject string, outcome *RunOutcome) {
	node := &models.Node{UUID: j.RunOn}
	if err := node.FindByUUID(); err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("Failed to find node with jobID:%d nodeUUID: %s error:%s", j.ID, j.RunOn, err.Error()))
	}
	var to []string
	for _, userId := range j.NotifyToArray {
		userModel := &models.User{ID: userId}
		if err := userModel.FindById(); err != nil {
			continue
		}
		if j.NotifyType == notify.NotifyTypeMail {
			to = append(to, userModel.Email)
		} else if j.NotifyType == notify.NotifyTypeWebHook && config.GetConfigModels().WebHook.Kind == "feishu" {
			to = append(to, userModel.UserName)
		}
	}
	runID := ""
	if outcome.Run != nil {
		runID = outcome.Run.RunID
	}
	msg := &notify.Message{
		Type:    j.NotifyType,
		IP:      fmt.Sprintf("%s:%s", node.IP, node.PID),
		Subject: subject,
		Body: fmt.Sprintf("job[%d] run on node[%s] failed ,retry %d times ,run_id:%s ,output:%s ,error:%s",
			j.ID, j.RunOn, outcome.RetryCount, runID, truncateText(outcome.Output, 500), outcome.ErrorText()),
		To:        to,
		OccurTime: time.Now().Format(utils.TimeFormatSecond),
	}
	go notify.Send(msg)
}

func WatchJobs(nodeUUID string) clientv3.WatchChan {
	return etcdclient.Watch(fmt.Sprintf(etcdclient.KeyEtcdJobProfile, nodeUUID), clientv3.WithPrefix())
}

func GetJobIDFromKey(key string) int {
	index := strings.LastIndex(key, "/")
	if index < 0 {
		return 0
	}
	jobId, err := strconv.Atoi(key[index+1:])
	if err != nil {
		return 0
	}
	return jobId
}

func (j *Job) CreateJobLog() (int, error) {
	start := time.Now()
	jobLog := &models.JobLog{
		Name:      j.Name,
		JobId:     j.ID,
		Command:   j.Command,
		IP:        j.Ip,
		Hostname:  j.Hostname,
		NodeUUID:  j.RunOn,
		Spec:      j.Spec,
		StartTime: start.Unix(),
	}
	return jobLog.Insert()
}

func UpdateJobLog(jobLogId int, start time.Time, output string, retry int, success bool) error {
	end := time.Now()
	jobLog := &models.JobLog{
		ID:         jobLogId,
		StartTime:  start.Unix(),
		RetryTimes: retry,
		Success:    success,
		Output:     output,
		EndTime:    end.Unix(),
	}
	return jobLog.Update()
}
func (j *Job) Success(jobLogId int, start time.Time, output string, retry int) error {
	return UpdateJobLog(jobLogId, start, output, retry, true)
}

func (j *Job) Fail(jobLogId int, start time.Time, errMsg string, retry int) error {
	return UpdateJobLog(jobLogId, start, errMsg, retry, false)
}
