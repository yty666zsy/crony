package models

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tmnhs/crony/common/pkg/dbclient"
	"gorm.io/gorm"
)

// 阶段 1（任务模型 2.0）新增的两张运行时表。
//
// 旧模型 job_log 的问题：
//   - 一次执行只有一行，重试过程完全丢失（只能看到一个 retry_times 计数）；
//   - 没有 run_id，无法把"日志/进程/审批/Agent 轨迹"关联到同一次执行；
//   - 没有触发来源、没有取消传播、没有结构化产物。
//
// 新模型：job_run（一次执行）+ job_run_step（执行中的一个步骤），
// 并由 run_id 串起进程记录（/crony/proc）、取消信号与后续的 Agent 轨迹。
const (
	CronyJobRunTableName     = "job_run"
	CronyJobRunStepTableName = "job_run_step"
)

// RunStatus 执行状态。
//
// 状态机（CanTransition 固化）：
//
//	pending -> running -> {success, failed, canceled, timeout}
//
// 终态不可再变更，保证审计记录一旦落定就不会被覆盖。
type RunStatus int

const (
	RunStatusPending  = RunStatus(0)
	RunStatusRunning  = RunStatus(1)
	RunStatusSuccess  = RunStatus(2)
	RunStatusFailed   = RunStatus(3)
	RunStatusCanceled = RunStatus(4)
	RunStatusSkipped  = RunStatus(5)
	RunStatusTimeout  = RunStatus(6)
)

var runStatusNames = map[RunStatus]string{
	RunStatusPending:  "pending",
	RunStatusRunning:  "running",
	RunStatusSuccess:  "success",
	RunStatusFailed:   "failed",
	RunStatusCanceled: "canceled",
	RunStatusSkipped:  "skipped",
	RunStatusTimeout:  "timeout",
}

func (s RunStatus) String() string {
	if name, ok := runStatusNames[s]; ok {
		return name
	}
	return fmt.Sprintf("unknown(%d)", int(s))
}

// IsFinal 是否为终态（不可再变更）。
func (s RunStatus) IsFinal() bool {
	switch s {
	case RunStatusSuccess, RunStatusFailed, RunStatusCanceled, RunStatusSkipped, RunStatusTimeout:
		return true
	default:
		return false
	}
}

// IsSuccess 是否视为"成功"（供条件步骤 on_success/on_failure 判定）。
func (s RunStatus) IsSuccess() bool { return s == RunStatusSuccess }

// CanTransition 校验状态迁移是否合法，非法迁移必须被拒绝而不是静默覆盖。
func CanTransition(from, to RunStatus) bool {
	if from.IsFinal() {
		return false
	}
	switch from {
	case RunStatusPending:
		// 排队中的任务可以被跳过（并发策略 Skip）或直接取消
		return to == RunStatusRunning || to == RunStatusSkipped || to == RunStatusCanceled
	case RunStatusRunning:
		return to == RunStatusSuccess || to == RunStatusFailed || to == RunStatusCanceled || to == RunStatusTimeout
	default:
		return false
	}
}

// TriggerSource 触发来源：把"谁让这次执行发生"落库，
// 这是后续 Agent 自治分级（L0~L3）与审计的基础。
type TriggerSource int

const (
	TriggerScheduler = TriggerSource(1) // 定时调度
	TriggerManual    = TriggerSource(2) // 控制台/接口手动触发
	TriggerRetry     = TriggerSource(3) // 失败重试（由调度器发起）
	TriggerAPI       = TriggerSource(4) // 外部 API / WebHook 触发
	TriggerAgent     = TriggerSource(5) // AI Agent 发起（阶段 2 起使用）
	TriggerSelfHeal  = TriggerSource(6) // 自愈动作（阶段 4）
)

var triggerSourceNames = map[TriggerSource]string{
	TriggerScheduler: "scheduler",
	TriggerManual:    "manual",
	TriggerRetry:     "retry",
	TriggerAPI:       "api",
	TriggerAgent:     "agent",
	TriggerSelfHeal:  "self_heal",
}

func (t TriggerSource) String() string {
	if name, ok := triggerSourceNames[t]; ok {
		return name
	}
	return fmt.Sprintf("unknown(%d)", int(t))
}

// JobRun 一次任务执行。
type JobRun struct {
	ID      int64  `json:"id" gorm:"column:id;primary_key;auto_increment"`
	RunID   string `json:"run_id" gorm:"size:64;column:run_id;not null;uniqueIndex:uk_job_run_run_id"`
	JobID   int    `json:"job_id" gorm:"column:job_id;not null;index:idx_job_run_job_time,priority:1"`
	JobName string `json:"job_name" gorm:"size:64;column:job_name"`
	// JobVersion / Spec 快照：任务定义可以被修改，历史执行必须能还原"当时跑的是什么"
	Spec          string `json:"spec" gorm:"size:64;column:spec"`
	Command       string `json:"command" gorm:"size:1024;column:command"`
	JobType       int    `json:"job_type" gorm:"column:job_type"`
	NodeUUID      string `json:"node_uuid" gorm:"size:128;column:node_uuid;index:idx_job_run_node"`
	Hostname      string `json:"hostname" gorm:"size:64;column:hostname"`
	IP            string `json:"ip" gorm:"size:64;column:ip"`
	TriggerSource int    `json:"trigger_source" gorm:"column:trigger_source;not null;default:1"`
	TriggeredBy   string `json:"triggered_by" gorm:"size:64;column:triggered_by"` // 用户名 / Agent 会话 ID
	Status        int    `json:"status" gorm:"column:status;not null;default:0;index:idx_job_run_status"`
	Attempt       int    `json:"attempt" gorm:"column:attempt;not null;default:1"`
	// ScheduledTime 该次执行对应的调度槽位（Unix 秒），与 RunID 一起构成幂等键
	ScheduledTime int64  `json:"scheduled_time" gorm:"column:scheduled_time;default:0;index:idx_job_run_job_time,priority:2"`
	StartedAt     int64  `json:"started_at" gorm:"column:started_at;default:0"`
	FinishedAt    int64  `json:"finished_at" gorm:"column:finished_at;default:0"`
	DurationMs    int64  `json:"duration_ms" gorm:"column:duration_ms;default:0"`
	ExitCode      int    `json:"exit_code" gorm:"column:exit_code;default:0"`
	StepCount     int    `json:"step_count" gorm:"column:step_count;default:0"`
	SuccessSteps  int    `json:"success_steps" gorm:"column:success_steps;default:0"`
	FailedSteps   int    `json:"failed_steps" gorm:"column:failed_steps;default:0"`
	RetryCount    int    `json:"retry_count" gorm:"column:retry_count;default:0"`
	ErrorMsg      string `json:"error_msg" gorm:"size:1024;column:error_msg"`
	Output        string `json:"output" gorm:"size:2048;column:output"`
	// TraceID 贯穿日志/指标/Agent 轨迹
	TraceID string `json:"trace_id" gorm:"size:64;column:trace_id;index:idx_job_run_trace"`
	// PolicySnapshot 当时的并发/重试策略快照（JSON），用于复盘"为什么这次被跳过/重试了几次"
	PolicySnapshot string `json:"policy_snapshot" gorm:"size:512;column:policy_snapshot"`
	Created        int64  `json:"created" gorm:"column:created;not null"`
	Updated        int64  `json:"updated" gorm:"column:updated;default:0"`
}

func (r *JobRun) TableName() string { return CronyJobRunTableName }

// JobRunStep 一次执行中的一个步骤（线性编排的最小单元）。
type JobRunStep struct {
	ID        int64  `json:"id" gorm:"column:id;primary_key;auto_increment"`
	RunID     string `json:"run_id" gorm:"size:64;column:run_id;not null;index:idx_job_run_step_run,priority:1"`
	StepIndex int    `json:"step_index" gorm:"column:step_index;not null;index:idx_job_run_step_run,priority:2"`
	Name      string `json:"name" gorm:"size:64;column:name"`
	// Kind 执行器类型：shell / http / container / sql / python（阶段 1 先落地 shell、http）
	Kind string `json:"kind" gorm:"size:32;column:kind;not null"`
	// Condition 步骤执行条件：on_success / on_failure / always
	Condition  string `json:"condition" gorm:"size:16;column:condition;default:'on_success'"`
	Command    string `json:"command" gorm:"size:1024;column:command"`
	Status     int    `json:"status" gorm:"column:status;not null;default:0"`
	ExitCode   int    `json:"exit_code" gorm:"column:exit_code;default:0"`
	RetryCount int    `json:"retry_count" gorm:"column:retry_count;default:0"`
	StartedAt  int64  `json:"started_at" gorm:"column:started_at;default:0"`
	FinishedAt int64  `json:"finished_at" gorm:"column:finished_at;default:0"`
	DurationMs int64  `json:"duration_ms" gorm:"column:duration_ms;default:0"`
	Output     string `json:"output" gorm:"size:4096;column:output"`
	ErrorMsg   string `json:"error_msg" gorm:"size:1024;column:error_msg"`
	// Artifacts 结构化产物（JSON），例如 HTTP 状态码、容器 ID、解析出的指标
	Artifacts string `json:"artifacts" gorm:"size:2048;column:artifacts"`
	Created   int64  `json:"created" gorm:"column:created;not null"`
}

func (s *JobRunStep) TableName() string { return CronyJobRunStepTableName }

// RunIDForSlot 由 (job_id, 调度槽位) 生成确定性的 run_id。
//
// 这就是幂等键：同一个任务在同一个调度槽位上重复触发（重复投递、节点重启后重放、
// 多节点竞争）只会得到同一个 run_id，落库时被唯一索引挡下，从而"至多执行一次"。
func RunIDForSlot(jobID int, scheduledTime int64) string {
	return fmt.Sprintf("j%d-t%d", jobID, scheduledTime)
}

// NewManualRunID 手动/Agent 触发时使用：没有调度槽位，
// 用纳秒时间戳 + 进程内自增序号保证唯一（同一纳秒内并发触发也不会碰撞）。
func NewManualRunID(jobID int, now time.Time) string {
	seq := atomic.AddUint64(&manualRunSeq, 1)
	return fmt.Sprintf("j%d-m%d-%d", jobID, now.UnixNano(), seq)
}

// manualRunSeq 进程内自增序号，避免同一时间戳生成相同 run_id。
var manualRunSeq uint64

// IsSlotRunID 判断 run_id 是否来自调度槽位（幂等）而非手动触发。
func IsSlotRunID(runID string) bool {
	return len(runID) > 0 && runID[len(runID)-1] != 'm' && containsSlotMarker(runID)
}

func containsSlotMarker(runID string) bool {
	for i := 0; i+1 < len(runID); i++ {
		if runID[i] == '-' && runID[i+1] == 't' {
			return true
		}
	}
	return false
}

// Insert 落库（表不存在时由 AutoMigrate 创建）。
func (r *JobRun) Insert() (int64, error) {
	err := dbclient.GetMysqlDB().Table(CronyJobRunTableName).Create(r).Error
	return r.ID, err
}

// UpdateFields 只更新指定字段，避免把内存中的整行覆盖回库。
func (r *JobRun) UpdateFields(fields map[string]interface{}) error {
	fields["updated"] = time.Now().Unix()
	return dbclient.GetMysqlDB().Table(CronyJobRunTableName).Where("run_id = ?", r.RunID).Updates(fields).Error
}

// FindByRunID 按 run_id 读取执行记录（run_id 唯一，天然是幂等查询键）。
func (r *JobRun) FindByRunID() error {
	return dbclient.GetMysqlDB().Table(CronyJobRunTableName).Where("run_id = ?", r.RunID).First(r).Error
}

// InsertStep 落库一个步骤记录。
func (s *JobRunStep) Insert() (int64, error) {
	err := dbclient.GetMysqlDB().Table(CronyJobRunStepTableName).Create(s).Error
	return s.ID, err
}

// IsRecordNotFound 判断错误是否为"记录不存在"。
//
// 幂等检查必须能区分"没有历史记录"（可以执行）与"数据库查询失败"（必须放弃执行，
// 否则会在数据库抖动时重复执行任务）。gorm 的判定细节只暴露在 models 层。
func IsRecordNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// IsDuplicateKey 判断错误是否为唯一索引冲突（MySQL 1062）。
//
// 这是幂等链路的最后一道闸门：即使两个节点同时抢到同一个调度槽位，
// job_run.run_id 的唯一索引也只会放行一个。
//
// 注：gorm v1.23 只在开启 TranslateError 时才返回 gorm.ErrDuplicatedKey，
// 本项目未开启，因此这里按错误文本判定（MySQL 驱动的稳定报错格式）。
func IsDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate entry") || strings.Contains(msg, "error 1062")
}

// FindRecentRunBySlot 查询同一任务在给定槽位窗口内是否已有执行记录。
//
// 与 run_id 唯一索引互补：run_id 保证"同槽位只落一行"，
// 这里额外提供"去重窗口"（窗口内的相邻槽位也视作重复），用于节点时钟漂移场景。
func FindRecentRunBySlot(jobID int, slot int64, windowSeconds int64) (found bool, err error) {
	if windowSeconds < 0 {
		windowSeconds = -windowSeconds
	}
	var count int64
	err = dbclient.GetMysqlDB().Table(CronyJobRunTableName).
		Where("job_id = ? AND scheduled_time BETWEEN ? AND ?", jobID, slot-windowSeconds, slot+windowSeconds).
		Count(&count).Error
	return count > 0, err
}
