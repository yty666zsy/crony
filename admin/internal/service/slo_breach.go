package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agentmemory"
	"github.com/tmnhs/crony/common/pkg/agentruntime"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/notify"
	"github.com/tmnhs/crony/common/pkg/slo"
	"github.com/tmnhs/crony/common/pkg/utils"
	"gorm.io/gorm"
)

// 阶段 5 补全：SLO 违约后的**动作接线**。
//
// 接线前的状态（PHASE8 §12.6 / 验收报告 §4.5）：worker 评估到违约只打一行日志，
// 契约里的 `breach_action=diagnose` 是个没人读的预留字段——"对结果负责"这件事
// 到"发现不达标"就断了，没有下一步。
//
// 本服务补的就是这一步，且严格遵守三条纪律：
//  1. **默认不动手**：`slo.breach-action-enabled=false`（默认）时，周期循环只评估 + 打日志，
//     与接线前**完全一致**（零回归）。动作只在显式打开后才会发生。
//  2. **动作是闭合白名单**：none / notify / diagnose，三选一，不存在"让模型现编一个动作"。
//  3. **每一次动作都是一行事实**：落 `slo_breach`，并同时充当**跨进程冷却**
//     （worker 与 admin 都跑时靠"最近一行的时间"退避，不靠各自内存里的 map）。
//
// diagnose 需要 Agent 运行时（LLM）。不可用时**降级为通知并如实写明**，
// 绝不假装"诊断过了"——没有 LLM 的部署仍然是诚实可用的。

// SloBreachCooldownDefault 默认冷却：同一契约 30 分钟内不重复动作。
const SloBreachCooldownDefault = 30 * time.Minute

// DiagnoseOutcome 一次违约诊断的发起结果。
type DiagnoseOutcome struct {
	// Available 诊断通道是否可用（Agent 运行时是否装配）。
	Available bool
	// OK 诊断是否真的成功发起。
	OK bool
	// SessionID 诊断会话号（可回放）。
	SessionID string
	// Detail 人可读说明（成功/降级/失败的原因）。
	Detail string
}

// SloBreachDeps 违约处置的全部外部依赖（显式注入 = 可测 + 可替换）。
type SloBreachDeps struct {
	Now  func() time.Time
	Logf func(format string, args ...interface{})
	// AutoEnabled 自动处置是否开启（`slo.breach-action-enabled`）。
	AutoEnabled func() bool
	// Cooldown 同一契约两次动作之间的最小间隔。
	Cooldown func() time.Duration
	// LastActionAt 读该契约最近一次处置的时间（unix 秒）；0 表示没有。
	LastActionAt func(policyID int) (int64, error)
	// SaveBreach 落一行处置记录。
	SaveBreach func(b *models.SloBreach) error
	// FindJob 取任务（拿通知方式与收件人）。
	FindJob func(jobID int) (*models.Job, error)
	// Notify 发送违约告警，返回实际收件人数（0 = 没配收件人，未发送）。
	Notify func(job *models.Job, subject, body string) int
	// LatestFailedRun 取该任务最近一次失败执行（诊断的输入）；没有返回 nil, nil。
	LatestFailedRun func(jobID int) (*models.JobRun, error)
	// Diagnose 触发一次诊断。
	Diagnose func(ctx context.Context, runID string) DiagnoseOutcome
}

// SloBreachOptions 一次处置的选项。
type SloBreachOptions struct {
	// Allowed 是否允许执行动作。周期循环传配置开关；人工触发接口恒为 true。
	Allowed bool
	// BypassCooldown 忽略冷却（人工"强制"）。
	BypassCooldown bool
	// ExecutedBy 触发方标记：auto / manual。
	ExecutedBy string
}

// SloBreachReport 一次处置的汇总（接口回执与日志都用它）。
type SloBreachReport struct {
	Evaluated   int                `json:"evaluated"`
	Breached    int                `json:"breached"`
	Acted       int                `json:"acted"`
	Observed    int                `json:"observed"`
	Skipped     int                `json:"skipped"`
	Failed      int                `json:"failed"`
	AutoEnabled bool               `json:"auto_enabled"`
	ExecutedBy  string             `json:"executed_by"`
	Rows        []models.SloBreach `json:"rows"`
	Errors      []string           `json:"errors,omitempty"`
}

// SloBreachService 违约处置服务。
type SloBreachService struct {
	deps SloBreachDeps
}

// NewSloBreachService 创建服务并补齐缺省依赖。
func NewSloBreachService(deps SloBreachDeps) *SloBreachService {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...interface{}) {}
	}
	if deps.AutoEnabled == nil {
		deps.AutoEnabled = func() bool { return false }
	}
	if deps.Cooldown == nil {
		deps.Cooldown = func() time.Duration { return SloBreachCooldownDefault }
	}
	if deps.Notify == nil {
		deps.Notify = func(*models.Job, string, string) int { return 0 }
	}
	if deps.Diagnose == nil {
		deps.Diagnose = func(context.Context, string) DiagnoseOutcome {
			return DiagnoseOutcome{Available: false, Detail: "未装配诊断通道"}
		}
	}
	return &SloBreachService{deps: deps}
}

// AutoEnabled 自动处置开关当前值。
func (s *SloBreachService) AutoEnabled() bool { return s.deps.AutoEnabled() }

// normalizeBreachAction 归一化契约里的动作。未知/空值一律**保守地**当作 none，
// 并说明原因——绝不因为"看不懂用户写的动作"就顺手做了别的动作。
func normalizeBreachAction(raw string) (models.SloBreachAction, string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "none", "log":
		return models.SloBreachActionNone, ""
	case "notify", "alert":
		return models.SloBreachActionNotify, ""
	case "diagnose":
		return models.SloBreachActionDiagnose, ""
	default:
		return models.SloBreachActionNone,
			fmt.Sprintf("未知的违约动作 %q（按 none 处理：只记录，不动手）", raw)
	}
}

// HandleBreaches 处理一批评估结果里**违约**的部分。
//
// 只处理 Breached=true 的项；未违约/无样本一律不动（口径与 slo.Evaluate 一致）。
func (s *SloBreachService) HandleBreaches(ctx context.Context, evals []SloEvaluation, opts SloBreachOptions) SloBreachReport {
	rep := SloBreachReport{
		Evaluated:   len(evals),
		AutoEnabled: s.deps.AutoEnabled(),
		ExecutedBy:  opts.ExecutedBy,
		Rows:        make([]models.SloBreach, 0),
	}
	for i := range evals {
		e := &evals[i]
		if !e.Report.Breached {
			continue
		}
		rep.Breached++

		action, note := normalizeBreachAction(e.Policy.BreachAction)
		if !opts.Allowed {
			// 关闭状态：不动手，也不落库（与接线前"只打日志"完全一致）。
			rep.Skipped++
			s.deps.Logf("[slo_breach] 契约 %q 违约，但违约动作未开启（slo.breach-action-enabled=false）：仅记录", e.Policy.Name)
			continue
		}

		now := s.deps.Now()
		if !opts.BypassCooldown {
			cd := s.deps.Cooldown()
			if cd > 0 && s.deps.LastActionAt != nil {
				last, err := s.deps.LastActionAt(e.Policy.ID)
				if err != nil {
					rep.Failed++
					rep.Errors = append(rep.Errors, fmt.Sprintf("读取契约 %d 冷却失败: %v", e.Policy.ID, err))
					continue
				}
				if last > 0 && now.Unix()-last < int64(cd.Seconds()) {
					rep.Skipped++
					s.deps.Logf("[slo_breach] 契约 %q 违约，但仍在冷却期（%s）内：跳过", e.Policy.Name, cd)
					continue
				}
			}
		}

		row := models.SloBreach{
			PolicyID:   e.Policy.ID,
			JobID:      e.Policy.JobID,
			PolicyName: e.Policy.Name,
			JobName:    e.JobName,
			Action:     action,
			Reason:     strings.Join(e.Report.Reasons, "；"),
			ReportJSON: sloReportJSON(e.Report),
			ExecutedBy: opts.ExecutedBy,
			Created:    now.Unix(),
			CreatedMS:  now.UnixMilli(),
		}
		if note != "" {
			row.Detail = note
		}

		switch action {
		case models.SloBreachActionNotify:
			row.Status, row.Detail = s.doNotify(e, row.Detail)
		case models.SloBreachActionDiagnose:
			row.Status, row.Detail = s.doDiagnose(ctx, e, row.Detail)
		default:
			row.Status = models.SloBreachObserved
			if row.Detail == "" {
				row.Detail = "动作=none：仅留痕，不发送通知、不触发诊断"
			}
		}

		if err := s.save(&row); err != nil {
			rep.Failed++
			rep.Errors = append(rep.Errors, fmt.Sprintf("契约 %d 处置留痕失败: %v", e.Policy.ID, err))
			continue
		}
		rep.Rows = append(rep.Rows, row)
		switch row.Status {
		case models.SloBreachActed:
			rep.Acted++
		case models.SloBreachFailed:
			rep.Failed++
		default:
			rep.Observed++
		}
		s.deps.Logf("[slo_breach] 契约 %q（任务 %s）违约动作=%s 结果=%s：%s",
			e.Policy.Name, e.JobName, action, row.Status, row.Detail)
	}
	return rep
}

// doNotify 发送违约告警。返回状态与说明。
func (s *SloBreachService) doNotify(e *SloEvaluation, prefix string) (models.SloBreachStatus, string) {
	subject := fmt.Sprintf("[Crony SLO 违约] 契约 %q", e.Policy.Name)
	if e.JobName != "" {
		subject += "（任务 " + e.JobName + "）"
	}
	body := "指标未达标：" + strings.Join(e.Report.Reasons, "；")

	if e.Policy.JobID <= 0 || s.deps.FindJob == nil {
		detail := "全局契约未绑定任务，无法解析收件人（未发送）"
		if prefix != "" {
			detail = prefix + "；" + detail
		}
		return models.SloBreachObserved, detail
	}
	job, err := s.deps.FindJob(e.Policy.JobID)
	if err != nil || job == nil {
		detail := "读取任务失败，无法解析收件人（未发送）"
		if prefix != "" {
			detail = prefix + "；" + detail
		}
		return models.SloBreachFailed, detail
	}
	n := s.deps.Notify(job, subject, body)
	if n <= 0 {
		detail := "任务未配置有效通知收件人（未发送）"
		if prefix != "" {
			detail = prefix + "；" + detail
		}
		return models.SloBreachObserved, detail
	}
	detail := fmt.Sprintf("已向 %d 个收件人发送违约告警", n)
	if prefix != "" {
		detail = prefix + "；" + detail
	}
	return models.SloBreachActed, detail
}

// doDiagnose 触发诊断；通道不可用时降级为通知（并如实写明）。
func (s *SloBreachService) doDiagnose(ctx context.Context, e *SloEvaluation, prefix string) (models.SloBreachStatus, string) {
	if e.Policy.JobID <= 0 {
		detail := "全局契约未绑定任务，没有可诊断的具体执行（未触发）"
		if prefix != "" {
			detail = prefix + "；" + detail
		}
		return models.SloBreachObserved, detail
	}
	if s.deps.LatestFailedRun == nil {
		return s.doNotify(e, joinPrefix(prefix, "无失败执行可取（未触发诊断），降级为通知"))
	}
	run, err := s.deps.LatestFailedRun(e.Policy.JobID)
	if err != nil {
		return models.SloBreachFailed, joinPrefix(prefix, "读取失败执行出错（未触发诊断）: "+err.Error())
	}
	if run == nil || strings.TrimSpace(run.RunID) == "" {
		return s.doNotify(e, joinPrefix(prefix, "窗口内没有失败执行可诊断，降级为通知"))
	}

	out := s.deps.Diagnose(ctx, run.RunID)
	if !out.Available {
		// 没有 LLM / 未装配诊断通道：诚实降级为通知，不假装诊断过。
		return s.doNotify(e, joinPrefix(prefix, "诊断通道不可用（"+out.Detail+"），降级为通知"))
	}
	if !out.OK {
		return models.SloBreachFailed, joinPrefix(prefix, "诊断触发失败: "+out.Detail)
	}
	detail := "已触发诊断"
	if out.SessionID != "" {
		detail += "（会话 " + out.SessionID + "，可回放）"
	}
	if out.Detail != "" {
		detail += "：" + out.Detail
	}
	return models.SloBreachActed, joinPrefix(prefix, detail)
}

// save 落库一行处置记录（失败不影响后续契约的处理，由上层计入 Failed）。
func (s *SloBreachService) save(b *models.SloBreach) error {
	if s.deps.SaveBreach == nil {
		return nil
	}
	return s.deps.SaveBreach(b)
}

func joinPrefix(prefix, detail string) string {
	if strings.TrimSpace(prefix) == "" {
		return detail
	}
	if strings.TrimSpace(detail) == "" {
		return prefix
	}
	return prefix + "；" + detail
}

// sloReportJSON 违约快照（runes 安全截断到列宽，避免超长或多字节截断导致写库失败）。
func sloReportJSON(r slo.Report) string {
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return agentmemory.ClipRunes(string(b), 1000)
}

// ---------- 默认装配 ----------

// sloBreachService 进程内单例（与 DefaultSelfHealService 同一模式）。
var sloBreachService *SloBreachService

// NewDefaultSloBreachService 生产装配：接到真实数据库、通知渠道与 Agent 运行时。
func NewDefaultSloBreachService() *SloBreachService {
	return NewSloBreachService(SloBreachDeps{
		Now:  time.Now,
		Logf: func(format string, args ...interface{}) { logger.GetLogger().Info(fmt.Sprintf(format, args...)) },
		AutoEnabled: func() bool {
			cfg := config.GetConfigModels()
			return cfg != nil && cfg.Slo.BreachActionEnabled
		},
		Cooldown: func() time.Duration {
			cfg := config.GetConfigModels()
			if cfg == nil || cfg.Slo.CooldownSeconds <= 0 {
				return SloBreachCooldownDefault
			}
			return time.Duration(cfg.Slo.CooldownSeconds) * time.Second
		},
		LastActionAt: func(policyID int) (int64, error) {
			var last int64
			err := dbclient.GetMysqlDB().Table(models.CronySloBreachTableName).
				Select("coalesce(max(created),0)").Where("policy_id = ?", policyID).
				Scan(&last).Error
			return last, err
		},
		SaveBreach: func(b *models.SloBreach) error {
			return dbclient.GetMysqlDB().Table(models.CronySloBreachTableName).Create(b).Error
		},
		FindJob: func(jobID int) (*models.Job, error) {
			job := &models.Job{ID: jobID}
			if err := job.FindById(); err != nil {
				return nil, err
			}
			return job, nil
		},
		Notify: notifySloBreach,
		LatestFailedRun: func(jobID int) (*models.JobRun, error) {
			run := &models.JobRun{}
			err := dbclient.GetMysqlDB().Table(models.CronyJobRunTableName).
				Where("job_id = ? and status in ?", jobID,
					[]int{int(models.RunStatusFailed), int(models.RunStatusTimeout)}).
				Order("id desc").First(run).Error
			if err != nil {
				if err == gorm.ErrRecordNotFound {
					return nil, nil
				}
				return nil, err
			}
			return run, nil
		},
		Diagnose: defaultBreachDiagnose,
	})
}

// notifySloBreach 发送违约告警，返回实际收件人数。
//
// 收件人解析与节点侧失败通知（node/internal/handler/job.go notifyFailure）**同一口径**：
// job.notify_to 存的是用户 id 数组，按通知方式解析成邮箱（邮件）或用户名（飞书）。
// 口径不一致会造成"任务失败能收到、SLO 违约收不到"这种查不出的差异。
func notifySloBreach(job *models.Job, subject, body string) int {
	if job == nil || len(job.NotifyTo) == 0 {
		return 0
	}
	var ids []int
	if err := json.Unmarshal(job.NotifyTo, &ids); err != nil {
		return 0
	}
	kind := ""
	if cfg := config.GetConfigModels(); cfg != nil {
		kind = cfg.WebHook.Kind
	}
	to := make([]string, 0, len(ids))
	for _, uid := range ids {
		u := &models.User{ID: uid}
		if err := u.FindById(); err != nil {
			continue
		}
		switch {
		case job.NotifyType == notify.NotifyTypeMail:
			if strings.TrimSpace(u.Email) != "" {
				to = append(to, u.Email)
			}
		case job.NotifyType == notify.NotifyTypeWebHook && kind == "feishu":
			to = append(to, u.UserName)
		}
	}
	if len(to) == 0 {
		return 0
	}
	notify.Send(&notify.Message{
		Type:      job.NotifyType,
		Subject:   subject,
		Body:      "[Crony SLO 违约] " + body,
		To:        to,
		OccurTime: time.Now().Format(utils.TimeFormatSecond),
	})
	return len(to)
}

// defaultBreachDiagnose 用 Agent 运行时触发一次失败诊断。
func defaultBreachDiagnose(ctx context.Context, runID string) DiagnoseOutcome {
	rt := DefaultAgentRuntime()
	if rt == nil {
		return DiagnoseOutcome{Available: false, Detail: "Agent 运行时未启用（未配置 llm）"}
	}
	res, err := rt.RunDiagnosis(ctx, agentruntime.DiagnosisRequest{RunID: runID})
	if err != nil {
		return DiagnoseOutcome{Available: true, OK: false, Detail: err.Error()}
	}
	if res == nil {
		return DiagnoseOutcome{Available: true, OK: false, Detail: "诊断没有返回结果"}
	}
	return DiagnoseOutcome{Available: true, OK: true, SessionID: res.SessionID, Detail: "诊断完成"}
}

// DefaultSloBreachService 取进程内违约处置服务（可能为 nil，调用方必须处理）。
func DefaultSloBreachService() *SloBreachService { return sloBreachService }

// SetSloBreachService 装配违约处置服务（启动流程与测试用）。
func SetSloBreachService(s *SloBreachService) { sloBreachService = s }

// InitSloBreach 启动时装配违约处置服务（admin 与 worker 共用）。
//
// 这里只装配服务、不启动循环：周期评估在 worker 的 runSloLoop 里已有，
// admin 侧仅通过 `/ops/slo/act` 提供人工触发。
func InitSloBreach() *SloBreachService {
	svc := NewDefaultSloBreachService()
	SetSloBreachService(svc)
	cfg := config.GetConfigModels()
	enabled := cfg != nil && cfg.Slo.BreachActionEnabled
	cd := SloBreachCooldownDefault
	if cfg != nil && cfg.Slo.CooldownSeconds > 0 {
		cd = time.Duration(cfg.Slo.CooldownSeconds) * time.Second
	}
	logger.GetLogger().Info(fmt.Sprintf(
		"[slo_breach] SLO 违约处置已就绪：自动动作=%v 冷却=%s（关闭时周期循环只评估与打日志，零回归）",
		enabled, cd))
	return svc
}
