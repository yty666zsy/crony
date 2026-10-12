package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/agentmemory"
	"github.com/tmnhs/crony/common/pkg/selfheal"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 阶段 4-C：自愈调度。
//
// 它在整条链路里的位置：**执行失败之后、人工介入之前**。
//
//	调度/执行 → job_run 落库（失败）→[本服务] 分类 → 决策 → 策略闸门 → 受限动作 → 观测结果 → 沉淀经验
//	                                                              └→ 拒绝/超出能力 → 转人工（留痕 + 通知）
//
// 三条不可动摇的设计原则（写在代码入口，避免后来者按"更智能"的方向改写它）：
//  1. **规则决定候选，历史只改排序**：动作来自闭合白名单（common/pkg/selfheal），
//     经验库（阶段 4-A）只影响同一失败类型下候选的先后。模型不参与"发明动作"。
//  2. **默认不动手**：总开关 + 任务级 opt-in + 写能力（allow_write）三道闸门，
//     任何一道没开都只记录不执行。升级系统时行为不变是第一要求。
//  3. **每一次尝试都是一行事实**：动手/没动手/为什么/依据什么/结果如何，
//     全部落 job_heal；并且把"症状→动作→结果"沉淀进 agent_memory，
//     让下一次决策能参考"上次这么干管不管用"。
type SelfHealService struct {
	mu     sync.Mutex
	deps   HealDeps
	policy selfheal.Policy
}

// HealDeps 自愈服务的全部外部依赖（显式注入 = 可测 + 可替换）。
//
// 为什么把依赖做成函数字段而不是直接调包级函数：自愈是**写动作**，
// 它碰到的每一样东西（查失败、读节点、投递重跑、改超时、写经验）都必须在测试里可控，
// 否则"验证自愈不会乱动"这句话就没有证据。
type HealDeps struct {
	// FindFailedRuns 取回看窗口内已结束的失败执行（倒序）。
	FindFailedRuns func(since time.Time, limit int) ([]*models.JobRun, error)
	// FindSteps 取某次执行的步骤明细（可为空）。
	FindSteps func(runID string) ([]models.JobRunStep, error)
	// FindJob 取任务（读自愈开关、超时、目标节点）。
	FindJob func(jobID int) (*models.Job, error)
	// FindNode 取节点（判可达性）。
	FindNode func(uuid string) (*models.Node, error)
	// HealsForSource / HealsForJob 读取同一次失败 / 同一任务窗口内的历史自愈记录。
	HealsForSource func(sourceRunID string) ([]*models.JobHeal, error)
	HealsForJob    func(jobID int, since time.Time) ([]*models.JobHeal, error)
	// ActiveHeals 处于"已执行待观测"的自愈（并发闸门用）。
	ActiveHeals func() ([]*models.JobHeal, error)
	// SaveHeal 写入/更新一行自愈记录。
	SaveHeal func(h *models.JobHeal) error
	// FindHeal 按单号读详情。
	FindHeal func(healID string) (*models.JobHeal, error)
	// SearchHeals 分页查询。
	SearchHeals func(req *request.ReqJobHealSearch) ([]models.JobHeal, int64, error)
	// FollowRun 找"自愈之后该任务新出现的那次执行"（观测结果用）；没有返回 nil。
	FollowRun func(h *models.JobHeal) (*models.JobRun, error)
	// UpsertMemory 把结果沉淀进经验库，返回经验条目号。
	UpsertMemory func(obs models.MemoryObservation, now time.Time) ([]string, error)
	// RecallStats 召回同类症状的历史经验并按动作聚合出统计。
	RecallStats func(symptom, nodeUUID string) (map[models.HealAction]selfheal.HistoryStats, []string, string)
	// Retry 投递一次重跑（复用 job_retry 的执行通道）。
	Retry func(ctx context.Context, req tools.RetryRequest) (tools.Result, error)
	// UpdateTimeout 受控地修改任务的超时上限（只在闸门放行后被调用）。
	UpdateTimeout func(jobID int, timeout int64) error
	// WritesEnabled 写能力是否已开启（与工具网关同一判据）。
	WritesEnabled func() bool
	// Notify 转人工时的通知（可为空实现）。
	Notify func(job *models.Job, subject, body string)
	// Now / Logf 时钟与日志（可注入）。
	Now  func() time.Time
	Logf func(format string, args ...interface{})
}

// HealReport 一次扫描的结果（接口回执与日志都用它，避免"扫了但没人知道扫到什么"）。
type HealReport struct {
	Scanned   int      `json:"scanned"`   // 扫描到的失败执行数
	Skipped   int      `json:"skipped"`   // 已被处置过（幂等跳过）
	Denied    int      `json:"denied"`    // 被策略拒绝
	Executed  int      `json:"executed"`  // 已执行待观测
	Escalated int      `json:"escalated"` // 转人工
	Failed    int      `json:"failed"`    // 动作执行失败
	Observed  int      `json:"observed"`  // 本次判定完成的观测数
	Succeeded int      `json:"succeeded"` // 观测到自愈成功
	Errors    []string `json:"errors,omitempty"`
}

// NewSelfHealService 创建自愈服务（policy 会做一次 Normalize，非法值由默认值兜住）。
func NewSelfHealService(deps HealDeps, policy selfheal.Policy) *SelfHealService {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logf == nil {
		deps.Logf = func(format string, args ...interface{}) {}
	}
	if deps.WritesEnabled == nil {
		deps.WritesEnabled = func() bool { return false }
	}
	return &SelfHealService{deps: deps, policy: policy.Normalize()}
}

// Policy 返回当前生效策略（副本）。
func (s *SelfHealService) Policy() selfheal.Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

// SetPolicy 更新策略（配置热更新与测试用）。
func (s *SelfHealService) SetPolicy(p selfheal.Policy) {
	s.mu.Lock()
	s.policy = p.Normalize()
	s.mu.Unlock()
}

// ScanOnce 执行一轮自愈：先观测上一轮的待观测记录，再处置新出现的失败。
//
// 全程串行（mu 保护）：同一进程内两轮扫描并发会让"次数/并发"闸门失效——
// 两个 goroutine 各自读到"次数还够"然后同时动手，正是最危险的一类竞态。
// 跨进程并发由 max_concurrent + 幂等跳过共同兜住（多副本部署时仍可能重复，
// 这一点写在文档的未验证清单里，而不是假装解决了）。
func (s *SelfHealService) ScanOnce(ctx context.Context) (HealReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rep := HealReport{}
	now := s.deps.Now()

	// justObserved：本轮刚刚判出结果的失败执行。
	// 它们不再参与本轮的"要不要处置"判定 —— 否则会出现
	// "刚观测到自愈失败 → 同一轮立刻再试 → 撞冷却 → 多一条拒绝记录"这种同轮噪音。
	// 下一轮起它是可再试的（受次数与冷却约束），语义上更清楚。
	justObserved := map[string]bool{}
	if err := s.observePending(ctx, now, &rep, justObserved); err != nil {
		rep.Errors = append(rep.Errors, "观测待定自愈失败："+err.Error())
	}

	runs, err := s.deps.FindFailedRuns(now.Add(-s.policy.Lookback()), s.policy.ScanLimit)
	if err != nil {
		return rep, fmt.Errorf("读取失败执行失败：%w", err)
	}
	rep.Scanned = len(runs)
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if run == nil {
			continue
		}
		if justObserved[run.RunID] {
			rep.Skipped++
			s.deps.Logf("[self_heal] 跳过 run=%s：本轮刚观测完，下一轮再决定是否再试", run.RunID)
			continue
		}
		if skip, why := s.alreadyHandled(run); skip {
			rep.Skipped++
			s.deps.Logf("[self_heal] 跳过 run=%s：%s", run.RunID, why)
			continue
		}
		if _, err := s.healLocked(ctx, run, "self_heal_loop", now, &rep); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("处置 run=%s 失败：%v", run.RunID, err))
		}
	}
	return rep, nil
}

// HealRun 处置指定的某一次失败执行（人工触发入口，同样过闸门）。
//
// by 会落进 executed_by，让"这次自愈是谁让它动的"永远可查。
func (s *SelfHealService) HealRun(ctx context.Context, run *models.JobRun, by string) (*models.JobHeal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run == nil || strings.TrimSpace(run.RunID) == "" {
		return nil, fmt.Errorf("必须指定要处置的执行记录（run_id）")
	}
	rep := HealReport{}
	return s.healLocked(ctx, run, by, s.deps.Now(), &rep)
}

// observePending 观测所有"已执行待观测"的自愈。
// justObserved 收集本轮判出终态的失败执行单号（供调用方避免同轮重复判定）。
func (s *SelfHealService) observePending(ctx context.Context, now time.Time, rep *HealReport, justObserved map[string]bool) error {
	active, err := s.deps.ActiveHeals()
	if err != nil {
		return err
	}
	for _, h := range active {
		if err := ctx.Err(); err != nil {
			return err
		}
		if h == nil {
			continue
		}
		done, err := s.observeOne(h, now, rep)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("观测 heal=%s 失败：%v", h.HealID, err))
			continue
		}
		if done {
			rep.Observed++
			if h.SourceRunID != "" && justObserved != nil {
				justObserved[h.SourceRunID] = true
			}
		}
	}
	return nil
}

// observeOne 判定一条待观测自愈的最终结果。返回 true 表示本次进入终态。
func (s *SelfHealService) observeOne(h *models.JobHeal, now time.Time, rep *HealReport) (bool, error) {
	follow, err := s.deps.FollowRun(h)
	if err != nil {
		return false, err
	}
	if follow == nil {
		// 窗口内没有出现新执行：**不能算成功**（也不立刻算失败，给足观测时间）。
		if h.Deadline > 0 && now.Unix() >= h.Deadline {
			h.NewRunID = ""
			s.finish(h, models.HealStatusFailed,
				"观测窗口内没有出现新的执行：自愈动作没有产生可观测的效果（可能投递未生效或任务已被停用）",
				now)
			rep.Failed++
			return true, s.saveAndSink(h, models.MemoryOutcomeFailed, now)
		}
		return false, nil
	}
	status := models.RunStatus(follow.Status)
	if !status.IsFinal() {
		// 还在跑：继续等（不提前判定，避免把"慢成功"记成失败）。
		return false, nil
	}
	h.NewRunID = follow.RunID
	if status == models.RunStatusSuccess {
		s.finish(h, models.HealStatusSucceeded,
			fmt.Sprintf("自愈后观测到新执行 %s 成功", follow.RunID),
			now)
		rep.Succeeded++
		return true, s.saveAndSink(h, models.MemoryOutcomeSuccess, now)
	}
	s.finish(h, models.HealStatusFailed,
		fmt.Sprintf("自愈后新执行 %s 仍然失败（status=%s，exit=%d）：%s",
			follow.RunID, status.String(), follow.ExitCode, agentmemory.ClipRunes(follow.ErrorMsg, 200)),
		now)
	rep.Failed++
	return true, s.saveAndSink(h, models.MemoryOutcomeFailed, now)
}

// alreadyHandled 幂等判定：这一次失败是否已经处置过。
//
// 规则只有一条：**只要这一次失败已经有自愈记录，且记录不是 `failed`，就不再重复判定。**
//
//   - succeeded/escalated/executed/planned/denied → 跳过；
//   - failed → 允许再试（受窗口次数与冷却约束，这正是"再试一两次"的来源）。
//
// 为什么这么定：
//   - `denied` 也必须跳过。否则冷却/并发这类临时原因会让同一次失败**每轮**都产生一条
//     "又被拒了"的记录——同一条事实被写几十遍，运维只会把这一页当噪音关掉。
//     而"拒绝不消耗次数预算"（见 selfheal.EvalCaps）意味着：不允许重复判定就必须跳过，
//     否则得不到刷屏的中点，只会在"刷屏"和"把预估算错"之间二选一。
//   - 新的失败（新的 run_id）是新的机会：配置改了、节点修好了之后，下一个失败就会重新走流程。
//     这正是我们要的行为——**不追着同一次历史失败反复动手**。
//   - `failed` 允许再试，是因为它代表"系统真的动过手但没用"，此时再试一次（不同动作或
//     同一动作）是有信息价值的；次数上限与冷却负责兜住它。
func (s *SelfHealService) alreadyHandled(run *models.JobRun) (bool, string) {
	hist, err := s.deps.HealsForSource(run.RunID)
	if err != nil {
		// 读不到历史时**保守跳过**：宁可漏处置，不可重复动手。
		return true, "读取历史自愈记录失败，保守跳过：" + err.Error()
	}
	for _, h := range hist {
		if h == nil {
			continue
		}
		if h.Status == models.HealStatusFailed {
			return false, ""
		}
		return true, "该次失败已有自愈记录（" + string(h.Status) + "，单号 " + h.HealID + "）"
	}
	return false, ""
}

// healLocked 单次失败的完整处置流程（调用方必须已持锁）。
func (s *SelfHealService) healLocked(ctx context.Context, run *models.JobRun, by string, now time.Time, rep *HealReport) (*models.JobHeal, error) {
	policy := s.policy
	job, err := s.deps.FindJob(run.JobID)
	if err != nil || job == nil {
		return nil, fmt.Errorf("任务 %d 不存在或读取失败：%v", run.JobID, err)
	}
	steps, err := s.deps.FindSteps(run.RunID)
	if err != nil {
		// 步骤明细读不到不影响分类：run 自身已经带了状态/退出码/错误文本。
		s.deps.Logf("[self_heal] run=%s 步骤明细读取失败（不影响分类）：%v", run.RunID, err)
	}
	reachable := s.nodeReachable(run.NodeUUID)

	cls := selfheal.Classify(selfheal.ClassifyInput{Run: run, Steps: steps, NodeReachable: reachable})
	symptom := selfheal.Symptom(job.Name, run.JobID, cls, run)
	stats, memIDs, hint := s.recallStats(symptom, run.NodeUUID)

	plan := selfheal.PlanFor(selfheal.PlanInput{
		Classification: cls, Run: run, Job: job, Policy: policy,
		History: stats, MemoryIDs: memIDs, MemoryHint: hint,
	})

	heal := &models.JobHeal{
		HealID:       models.NewHealID(now),
		SourceRunID:  run.RunID,
		JobID:        run.JobID,
		JobName:      job.Name,
		Status:       models.HealStatusPlanned,
		FailureClass: string(cls.Class),
		Symptom:      symptom,
		Action:       plan.Action,
		ActionJSON:   plan.ActionArgs.JSON(),
		Reason:       plan.Reason,
		EvidenceJSON: mustJSON(cls.Evidence),
		MemoryJSON:   mustJSON(map[string]interface{}{"stats": stats, "hint": hint, "memory_ids": memIDs}),
		NodeUUID:     strings.TrimSpace(run.NodeUUID),
		ExecutedBy:   by,
		Created:      now.Unix(),
		CreatedMS:    now.UnixMilli(),
		Updated:      now.Unix(),
		UpdatedMS:    now.UnixMilli(),
	}

	// 转人工不需要闸门：它不做任何动作，只把结论交给人。
	if plan.Action == models.HealActionEscalate {
		heal.Deadline = now.Unix()
		s.finish(heal, models.HealStatusEscalated, plan.Reason, now)
		rep.Escalated++
		s.notifyEscalate(job, heal)
		return heal, s.saveAndSink(heal, models.MemoryOutcomeAdvised, now)
	}

	// 闸门：能不能真动手。
	hist, err := s.deps.HealsForJob(job.ID, now.Add(-policy.Window()))
	if err != nil {
		return nil, fmt.Errorf("读取任务 %d 的自愈历史失败：%w", job.ID, err)
	}
	active, err := s.deps.ActiveHeals()
	if err != nil {
		return nil, fmt.Errorf("读取并发自愈数失败：%w", err)
	}
	// 并发数直接取"数据库里处于待观测状态的行数"。
	//
	// 这里**刻意不加"本轮已执行数"**：每一轮决策结束时自愈记录已经落库，
	// 下一次 ActiveHeals() 查询就会把它读回来。再加一次就会重复计数——
	// 集成测试正是这样抓到"max_concurrent=2 实际只允许 1 条"的。
	// 换句话说：并发的唯一事实来源是库里的行，不是内存里的计数器。
	activeCount := len(active)
	decision := selfheal.EvalCaps(policy, selfheal.CapInput{
		JobSelfHeal:   job.SelfHeal == 1,
		WritesEnabled: s.deps.WritesEnabled(),
		History:       hist,
		ActiveCount:   activeCount,
		Now:           now,
	})
	heal.Attempt = decision.Attempts
	heal.PolicyJSON = decision.JSON()
	if !decision.Allowed {
		status := models.HealStatusDenied
		outcome := models.MemoryOutcomeDenied
		// 次数用尽意味着"不再自动尝试"，语义上是转人工而不是"这次不许"——
		// 两者对运维是不同的事情：前者要人接手，后者等着就行。
		if decision.Rule == selfheal.CapAttempts {
			status = models.HealStatusEscalated
			outcome = models.MemoryOutcomeAdvised
			rep.Escalated++
			s.notifyEscalate(job, heal)
		} else {
			rep.Denied++
		}
		heal.Deadline = now.Unix()
		s.finish(heal, status, plan.Reason+"；"+"策略闸门："+decision.Reason, now)
		return heal, s.saveAndSink(heal, outcome, now)
	}

	// 闸门放行：执行白名单动作。
	s.execute(ctx, job, heal, plan, now, rep)
	if heal.Status == models.HealStatusExecuted {
		// 已投递但尚未观测出结果：**先落库、不沉淀经验**。
		// 现在写"成功"是撒谎，写"失败"也不对；经验必须等在观测之后。
		if err := s.deps.SaveHeal(heal); err != nil {
			return heal, fmt.Errorf("保存自愈记录失败（heal=%s）：%w", heal.HealID, err)
		}
		return heal, nil
	}
	return heal, s.saveAndSink(heal, healMemoryOutcome(heal.Status), now)
}

// execute 执行动作（重跑 / 调超时后重跑）。
func (s *SelfHealService) execute(ctx context.Context, job *models.Job, heal *models.JobHeal,
	plan selfheal.Plan, now time.Time, rep *HealReport) {
	// 调超时：先改配置，改不成就终止（**不允许"改配置失败还照样重跑"**——
	// 那会让记录里写着"已调大超时"，而实际配置没变）。
	if plan.Action == models.HealActionRetryExtendTimeout {
		if err := s.deps.UpdateTimeout(job.ID, plan.ActionArgs.ToTimeout); err != nil {
			s.finish(heal, models.HealStatusFailed,
				fmt.Sprintf("调大超时失败（%d→%d）：%v；未执行重跑",
					plan.ActionArgs.FromTimeout, plan.ActionArgs.ToTimeout, err),
				now)
			rep.Failed++
			return
		}
	}

	res, err := s.deps.Retry(ctx, tools.RetryRequest{
		JobID:       job.ID,
		RunID:       heal.SourceRunID,
		Reason:      "自愈调度：" + heal.Reason,
		MaxAttempts: 1,
	})
	if err != nil {
		s.finish(heal, models.HealStatusFailed, "投递重跑失败："+err.Error(), now)
		rep.Failed++
		return
	}
	if !res.OK {
		s.finish(heal, models.HealStatusFailed, "投递重跑未成功："+res.Summary+" "+res.Err, now)
		rep.Failed++
		return
	}
	heal.Deadline = now.Add(s.policy.ObserveWindow()).Unix()
	note := res.Summary
	if plan.Action == models.HealActionRetryExtendTimeout {
		note = fmt.Sprintf("已把超时上限 %d→%d 并投递重跑（%s）",
			plan.ActionArgs.FromTimeout, plan.ActionArgs.ToTimeout, res.Summary)
	}
	s.finish(heal, models.HealStatusExecuted, note, now)
	rep.Executed++
}

// finish 设置终态字段（不落库；落库统一走 saveAndSink）。
func (s *SelfHealService) finish(h *models.JobHeal, status models.HealStatus, note string, now time.Time) {
	h.Status = status
	h.OutcomeNote = agentmemory.ClipRunes(note, 500)
	h.Updated = now.Unix()
	h.UpdatedMS = now.UnixMilli()
	if status.IsFinal() {
		h.FinishedAt = now.Unix()
	}
}

// saveAndSink 落库 + 沉淀经验（经验沉淀失败不影响自愈记录本身，但会被记进日志）。
func (s *SelfHealService) saveAndSink(h *models.JobHeal, outcome string, now time.Time) error {
	if err := s.deps.SaveHeal(h); err != nil {
		return fmt.Errorf("保存自愈记录失败（heal=%s）：%w", h.HealID, err)
	}
	if s.deps.UpsertMemory == nil {
		return nil
	}
	obs := models.MemoryObservation{
		SessionID:     "self_heal:" + h.HealID,
		RunID:         h.SourceRunID,
		JobID:         h.JobID,
		AgentKind:     AgentKindSelfHeal,
		Symptom:       h.Symptom,
		SymptomTokens: agentmemory.TokenizeString(h.Symptom),
		ActionKind:    memoryActionKind(h.Action),
		ActionText:    selfheal.MemoryActionText(h.Action, h.JobID, healActionArgs(h)),
		Tool:          ToolNameSelfHeal,
		TargetNode:    h.NodeUUID,
		Risk:          memoryRisk(h.Action),
		Outcome:       outcome,
		ResultText:    h.OutcomeNote,
		ResultDetail:  h.OutcomeNote,
	}
	ids, err := s.deps.UpsertMemory(obs, now)
	if err != nil {
		s.deps.Logf("[self_heal] heal=%s 经验沉淀失败（不影响处置结论）：%v", h.HealID, err)
		return nil
	}
	h.MemoryIDs = mustJSON(ids)
	// 补写一次经验条目号（失败不影响主流程）。
	if err := s.deps.SaveHeal(h); err != nil {
		s.deps.Logf("[self_heal] heal=%s 补写经验条目号失败：%v", h.HealID, err)
	}
	return nil
}

// nodeReachable 判节点可达性；节点未知（uuid 为空/查不到）返回 nil（不据此分类）。
func (s *SelfHealService) nodeReachable(uuid string) *bool {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" || s.deps.FindNode == nil {
		return nil
	}
	node, err := s.deps.FindNode(uuid)
	if err != nil || node == nil {
		// 节点记录不存在：这本身是事实，但"查不到"不等于"不可达"，
		// 因此不据此判成 node_unreachable（否则一个脏 uuid 会让所有失败都被判成节点问题）。
		return nil
	}
	ok := node.Status != models.NodeConnFail
	return &ok
}

// recallStats 召回历史经验并按动作聚合（失败不报错：召回只是排序线索）。
func (s *SelfHealService) recallStats(symptom, node string) (map[models.HealAction]selfheal.HistoryStats, []string, string) {
	if s.deps.RecallStats == nil {
		return nil, nil, ""
	}
	stats, ids, hint := s.deps.RecallStats(symptom, node)
	return stats, ids, hint
}

// notifyEscalate 转人工时通知（失败只记日志：通知不到位不该影响自愈记录落库）。
func (s *SelfHealService) notifyEscalate(job *models.Job, h *models.JobHeal) {
	if s.deps.Notify == nil || job == nil {
		return
	}
	subject := fmt.Sprintf("[Crony 自愈] 任务 %s 需要人工介入", job.Name)
	body := fmt.Sprintf("任务: %s(id=%d)\n失败执行: %s\n失败类型: %s\n自愈结论: %s\n依据: %s\n",
		job.Name, job.ID, h.SourceRunID, h.FailureClass, h.Reason, h.OutcomeNote)
	s.deps.Notify(job, subject, body)
}

// Search / Find 供接口层使用。
func (s *SelfHealService) Search(req *request.ReqJobHealSearch) ([]models.JobHeal, int64, error) {
	return s.deps.SearchHeals(req)
}

// Find 读取一条自愈记录。
func (s *SelfHealService) Find(healID string) (*models.JobHeal, error) {
	healID = strings.TrimSpace(healID)
	if healID == "" {
		return nil, ErrHealNotFound
	}
	h, err := s.deps.FindHeal(healID)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, ErrHealNotFound
	}
	return h, nil
}

// HealBySourceRun 读某次失败执行的自愈记录（控制台"这次失败被怎么处置了"）。
func (s *SelfHealService) HealBySourceRun(runID string) ([]*models.JobHeal, error) {
	return s.deps.HealsForSource(strings.TrimSpace(runID))
}

// 常量：经验库中自愈经验的归属标识。
const (
	// AgentKindSelfHeal 自愈经验在经验库里的 agent_kind 取值（召回按它过滤）。
	AgentKindSelfHeal = "self_heal"
	// ToolNameSelfHeal 自愈经验在经验库里的 tool 取值。
	ToolNameSelfHeal = "self_heal"
)

// memoryActionKind 动作种类：真正动手的是 tool，只给结论（转人工）的是 advice。
//
// 为什么沿用 4-A 的两种取值而不新增：动作种类是经验库的**跨模块契约**，
// 新增取值会让"审批/诊断/自愈"三类经验无法用同一套召回口径比较。
func memoryActionKind(a models.HealAction) string {
	if a == models.HealActionEscalate {
		return models.MemoryActionAdvice
	}
	return models.MemoryActionTool
}

// memoryRisk 风险级别：自愈动作会改状态/配置，因此是 write；转人工没动手。
func memoryRisk(a models.HealAction) string {
	if a.IsWrite() {
		return "write"
	}
	return "read"
}

// healMemoryOutcome 把自愈状态映射成经验库的结果口径。
func healMemoryOutcome(status models.HealStatus) string {
	switch status {
	case models.HealStatusSucceeded:
		return models.MemoryOutcomeSuccess
	case models.HealStatusFailed:
		return models.MemoryOutcomeFailed
	case models.HealStatusDenied:
		return models.MemoryOutcomeDenied
	case models.HealStatusEscalated:
		return models.MemoryOutcomeAdvised
	default:
		return models.MemoryOutcomeSuccess
	}
}

// healActionArgs 解析一行自愈记录的动作参数（解析失败按零值处理，不影响处置结论）。
func healActionArgs(h *models.JobHeal) selfheal.ActionJSON {
	if h == nil {
		return selfheal.ActionJSON{}
	}
	args, _ := selfheal.ParseActionJSON(h.ActionJSON)
	return args
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
