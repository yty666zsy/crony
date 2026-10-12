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
	ConcurrencyPolicy int `json:"concurrency_policy" gorm:"size:1;column:concurrency_policy;not null;default:0"`
	// StepsJSON 阶段 4-B：多步骤编排定义（JSON 文本，见 job_steps.go 的形态说明）。
	//
	// 空串 = 没有编排 = 老的单步语义（执行时由 node 的 BuildPlan 决定），
	// 因此老任务升级后行为**逐字节不变**，不需要任何数据迁移。
	// 存成一列而不是拆表，理由见 job_steps.go 顶部注释。
	StepsJSON string `json:"steps_json" gorm:"type:text;column:steps_json"`
	// SelfHeal 阶段 4-C：该任务是否允许自愈调度自动处置失败（0=不允许，1=允许）。
	//
	// 为什么是**按任务开关且默认关闭**，而不是"全局一开就全自动"：
	// 自愈是写动作（会重跑任务、可能改超时配置）。默认关着意味着"升级后行为不变"，
	// 运维必须对具体任务显式点头——这和 allow_write 默认关是同一个原则：
	// 能力先存在、边界先收紧。
	SelfHeal int    `json:"self_heal" gorm:"size:1;column:self_heal;not null;default:0"`
	Note     string `json:"note" gorm:"size:512;column:note;default:''"`
	Created  int64  `json:"created" gorm:"column:created;not null"`
	Updated  int64  `json:"updated" gorm:"column:updated;default:0"`

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
	j.Cmd = SplitCommand(j.Command)
}

// SplitCommand 按与 Job.SplitCmd 完全相同的规则切分一条命令。
//
// 抽出来的理由：阶段 4-B 的多步骤编排也要切分命令。如果编排侧自己再写一份切分逻辑，
// 就会出现"单步任务与实际 argv 不一致"的经典事故（引号/转义的处理稍有不同就会改变参数个数），
// 而这种不一致不会报错，只会跑错命令。
func SplitCommand(command string) []string {
	ps := strings.SplitN(command, " ", 2)
	if len(ps) == 1 {
		return ps
	}
	out := make([]string, 0, 2)
	out = append(out, ps[0])
	out = append(out, utils.ParseCmdArguments(ps[1])...)
	return out
}

// SerializeArgv 把 argv 数组序列化成 Job.Command，保证 SplitCmd 能**无损还原**。
//
// 为什么需要它：阶段 2 的"自然语言建任务"拿到的是 argv 数组（`["find","/tmp","-mtime","+7"]`），
// 而 Job 只有一个字符串字段 Command，执行时由 SplitCmd 重新切分。
// 直接 strings.Join(argv, " ") 会在参数含空格/引号/反斜杠时改变语义
// （例如 `-name "my file"` 会被切成两个参数）——这类错误不会报错，只会让命令跑错。
// 因此含空白或转义字符的参数统一用双引号包裹，并按 parse.go 的规则转义。
func SerializeArgv(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		if needsQuoting(a) {
			escaped := strings.ReplaceAll(a, `\`, `\\`)
			escaped = strings.ReplaceAll(escaped, `"`, `\"`)
			parts = append(parts, `"`+escaped+`"`)
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// needsQuoting 判断单个参数是否必须加引号。
func needsQuoting(arg string) bool {
	if arg == "" {
		return true
	}
	return strings.ContainsAny(arg, " \t\n\r\"'\\")
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
