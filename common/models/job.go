package models

import (
	"encoding/json"
	"fmt"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/utils"
	"github.com/tmnhs/crony/common/pkg/utils/errors"
	"strings"
)

type JobType int

const (
	JobTypeCmd  = JobType(1)
	JobTypeHttp = JobType(2)
	// JobTypeAgent 阶段 2 新增：任务体是一段"自然语言目标"，由节点上的 Agent 运行时
	// 用只读工具去收集证据并给出结论（例如每日巡检总结）。
	//
	// 与 type=1 的关键区别（也是为什么它是新类型而不是给 cmd 加参数）：
	//   - 执行体不是命令而是目标 + 输出 Schema，执行过程不确定（工具调用顺序由模型决定），
	//     因此"命令字符串"不是可审计的执行凭据，轨迹才是；
	//   - 因此 Agent 任务的所有副作用被限制在"只读工具 + 落迹"，
	//     这也是它能直接排进现有调度器的前提（不需要新的权限模型）。
	JobTypeAgent = JobType(3)

	HTTPMethodGet  = 1
	HTTPMethodPost = 2

	JobExcSuccess = 1
	JobExcFail    = 0

	JobStatusNotAssigned = 0
	JobStatusAssigned    = 1

	ManualAllocation = 1
	AutoAllocation   = 2

	// MaxRetryTimes 单次任务失败后的最大重试次数上限。
	// 旧实现只透传 retry_times，一个很大的值会造成重试风暴（每次失败都重试 N 次），
	// 阶段 0 先在入口做上限约束。
	MaxRetryTimes = 100
)

// register to  /crony/job/<node_uuid>/<job_id>
type Job struct {
	ID      int    `json:"id" gorm:"column:id;primary_key;auto_increment"`
	Name    string `json:"name" gorm:"size:64;column:name;not null;index:idx_job_name" binding:"required"`
	Command string `json:"command" gorm:"type:text;column:command;not null" binding:"required"`
	//preset script ID
	ScriptID      []byte `json:"-"  gorm:"size:256;column:script_id;default:null"`
	ScriptIDArray []int  `json:"script_id" gorm:"-"`
	//Timeout setting of job execution time, which is effective when it is greater than 0.
	Timeout int64 `json:"timeout" gorm:"size:13;column:timeout;default:0"`
	// Retry times of task execution failures
	// The default value is 0
	RetryTimes int `json:"retry_times" gorm:"size:4,column:retry_times;default:0"`
	// Retry interval for task execution failure
	// in seconds. If the value is less than 0, try again immediately
	RetryInterval int64   `json:"retry_interval" gorm:"size:10;column:retry_interval;default:0"`
	Type          JobType `json:"job_type" gorm:"size:1;column:type;not null;" binding:"required"`
	HttpMethod    int     `json:"http_method" gorm:"size:1;column:http_method"`
	NotifyType    int     `json:"notify_type" gorm:"size:1;column:notify_type;not null"`
	// Whether to allocate nodes
	Status        int    `json:"status" gorm:"size:1;column:status;not null;default:0;index:idx_job_status"`
	NotifyTo      []byte `json:"-" gorm:"size:256;column:notify_to;default:null"`
	NotifyToArray []int  `json:"notify_to" gorm:"-"`
	Spec          string `json:"spec" gorm:"size:64;column:spec;not null"`
	RunOn         string `json:"run_on" gorm:"size:128;column:run_on;index:idx_job_run_on;"`
	// ConcurrencyPolicy 同一任务的并发策略（阶段 1 引入）：
	// 0=skip（默认，与旧行为最接近：上一次没跑完就跳过本次）
	// 1=buffer_one / 2=buffer_all / 3=cancel_other / 4=terminate_other
	// 取值见 common/pkg/runpolicy。老数据默认为 0，升级后行为可预期。
	ConcurrencyPolicy int    `json:"concurrency_policy" gorm:"size:1;column:concurrency_policy;not null;default:0"`
	Note              string `json:"note" gorm:"size:512;column:note;default:''"`
	Created           int64  `json:"created" gorm:"column:created;not null"`
	Updated           int64  `json:"updated" gorm:"column:updated;default:0"`

	Hostname string   `json:"host_name" gorm:"-"`
	Ip       string   `json:"ip" gorm:"-"`
	Cmd      []string `json:"cmd" gorm:"-"`
}

func (j *Job) InitNodeInfo(status int, nodeUUID, hostname, ip string) {
	j.Status, j.RunOn, j.Hostname, j.Ip = status, nodeUUID, hostname, ip
}

func (j *Job) Insert() (insertId int, err error) {
	err = dbclient.GetMysqlDB().Table(CronyJobTableName).Create(j).Error
	if err == nil {
		insertId = j.ID
	}
	return
}

func (j *Job) Update() error {
	return dbclient.GetMysqlDB().Table(CronyJobTableName).Updates(j).Error
}

func (j *Job) Delete() error {
	return dbclient.GetMysqlDB().Exec(fmt.Sprintf("delete from %s where id = ?", CronyJobTableName), j.ID).Error
}

func (j *Job) FindById() error {
	return dbclient.GetMysqlDB().Table(CronyJobTableName).Where("id = ? ", j.ID).First(j).Error
}
func (j *Job) Check() error {
	j.Name = strings.TrimSpace(j.Name)
	if len(j.Name) == 0 {
		return errors.ErrEmptyJobName
	}
	// 旧实现写成 `if j.RetryInterval == 0 { j.RetryTimes = 1 }`：
	// 只要没填重试间隔就把"不重试(0)"改成"重试 1 次"，属于静默改写用户意图。
	// 现在只做输入合法化，不改变语义：
	//   - RetryTimes < 0 → 0，过大 → 上限 100（防止一次失败触发长时间重试风暴）；
	//   - RetryInterval < 0 → 0（0 表示"立即重试"，执行侧对 >0 才按秒休眠）。
	if j.RetryTimes < 0 {
		j.RetryTimes = 0
	}
	if j.RetryTimes > MaxRetryTimes {
		j.RetryTimes = MaxRetryTimes
	}
	if j.RetryInterval < 0 {
		j.RetryInterval = 0
	}
	if len(strings.TrimSpace(j.Command)) == 0 {
		return errors.ErrEmptyJobCommand
	}
	if len(j.Cmd) == 0 && j.Type == JobTypeCmd {
		j.SplitCmd()
	}
	return nil
}

func (j *Job) SplitCmd() {
	ps := strings.SplitN(j.Command, " ", 2)
	if len(ps) == 1 {
		j.Cmd = ps
		return
	}
	j.Cmd = make([]string, 0, 2)
	j.Cmd = append(j.Cmd, ps[0])
	j.Cmd = append(j.Cmd, utils.ParseCmdArguments(ps[1])...)
}

func (j *Job) Val() string {
	data, err := json.Marshal(j)
	if err != nil {
		return err.Error()
	}
	return string(data)
}

func (j *Job) TableName() string {
	return CronyJobTableName
}

func (j *Job) Unmarshal() (err error) {
	err = json.Unmarshal(j.NotifyTo, &j.NotifyToArray)
	err = json.Unmarshal(j.ScriptID, &j.ScriptIDArray)
	return
}
