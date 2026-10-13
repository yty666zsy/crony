package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/rollout"
	"github.com/tmnhs/crony/common/pkg/tools"
	"gorm.io/gorm"
)

// 阶段 5 补全：放量档位（rollout gate）。
//
// 判定内核在 common/pkg/rollout（纯函数、可穷举测试）；本文件负责三件事：
//  1. **证据从哪来**：最近一次评测跑批 + 最近一轮 SLO 评估；
//  2. **状态存哪**：rollout_state（按环境唯一，重启不丢）；
//  3. **接进闸门**：作为第三个"只能收紧"的上限（配置 ∩ 信用 ∩ 放量）。
//
// 一条硬规矩：**加宽要证据，收紧不要**。收紧永远是安全的，所以自动降级
// 不需要任何人的批准；而加宽是在扩大爆炸半径，必须带着可复查的事实。

// RolloutService 放量档位服务。
type RolloutService struct{}

// DefaultRolloutService 默认实例。
var DefaultRolloutService = new(RolloutService)

// RolloutEnvDefault 未配置环境段时的默认值（与自治信用同一个取值空间）。
const RolloutEnvDefault = "default"

// RolloutEnv 归一化环境段：留空 = 用进程配置的 `tools.autonomy-env`。
//
// 为什么与信用共用同一个来源：放量与信用是同一枚硬币的两面，
// "放行按 acc、放量按 prod"这种两套口径会让闸门形同虚设。
func RolloutEnv(env string) string {
	if e := strings.TrimSpace(env); e != "" {
		return e
	}
	return ConfiguredAutonomyEnv()
}

// RolloutEnabled 进程是否把放量档位接进了放行判定（`tools.rollout-gate`，默认 false）。
func RolloutEnabled() bool {
	if cfg := config.GetConfigModels(); cfg != nil {
		return cfg.Tools.RolloutGate
	}
	return false
}

// GetRolloutState 读某环境的放量状态；不存在返回 (nil, nil)——"没有记录"不是错误。
//
// 为什么不存在要返回 nil 而不是造一个 L0 行：**没有记录 = 不表态**。
// 造 L0 会让"开启开关"这一个动作立刻改写全平台自治级别（行为突变），
// 而不表态 = 沿用配置 = 开启开关本身零回归（与自治信用的"没有历史沿用配置"一致）。
func (s *RolloutService) GetRolloutState(env string) (*models.RolloutState, error) {
	db := dbclient.GetMysqlDB()
	if db == nil {
		return nil, nil
	}
	row := models.RolloutState{}
	err := db.Table(models.CronyRolloutStateTableName).Where("env = ?", RolloutEnv(env)).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// RolloutCapFor 取某环境的放量上限，供闸门使用。
//
// ok=false 表示"不表态"（没有记录 / 数据库不可用）：调用方沿用配置。
// **故意不把"查询出错"当成 L0**：那会让一次数据库抖动变成全平台静默降级，
// 而运维在日志里只看到一堆 L0，查不到原因。宁可沿用配置（与信用同样的取舍）。
func (s *RolloutService) RolloutCapFor(env string) (tools.AutonomyLevel, bool, string) {
	row, err := s.GetRolloutState(env)
	if err != nil || row == nil {
		return "", false, ""
	}
	stage, perr := tools.ParseAutonomyLevel(row.Stage)
	if perr != nil {
		// 库里存了非法档位：按最严处理（这是数据损坏，不是配置选择）。
		return tools.AutonomyL0, true, "库里档位非法（" + row.Stage + "），按 L0 处理"
	}
	if row.Frozen {
		if stage != tools.AutonomyL0 {
			return tools.AutonomyL0, true, "放量已冻结：" + row.Reason
		}
	}
	return stage, true, row.Reason
}

// RolloutEvidence 采集"放行依据"：最近一次评测跑批 + 最近一轮 SLO 评估。
//
// 两处都**只取最近一次/这一轮**：放量看的是"现在有没有证据"，
// 不是"历史上最好的那一次"。任何一处取不到就如实留空，由内核判定为"缺证据"。
func (s *RolloutService) RolloutEvidence(now time.Time) (rollout.Evidence, error) {
	ev := rollout.Evidence{}

	// 评测证据：最近一次跑批（复用评测台的汇总行，避免第二套口径）。
	runs, err := DefaultEvalBenchService.ListEvalRuns(1)
	if err != nil {
		return ev, fmt.Errorf("读取评测跑批失败: %w", err)
	}
	if len(runs) > 0 {
		r := runs[0]
		ev.EvalRunID, ev.EvalPassed, ev.EvalFailed = r.RunID, r.Passed, r.Failed
		ev.EvalSkipped, ev.EvalScore, ev.EvalAt = r.Skipped, r.Score, r.FinishedAt
	}

	// SLO 证据：现算一轮（不读历史违约表——放量要的是"此刻有没有违约"，
	// 而不是"一个月前违约过没有"）。
	evals, err := DefaultSloService.EvaluateSlo(0)
	if err != nil {
		return ev, fmt.Errorf("评估 SLO 契约失败: %w", err)
	}
	ev.SloTotal = len(evals)
	for i := range evals {
		if evals[i].Report.Breached {
			ev.SloBreaches++
		}
	}
	ev.SloAt = now.Unix()
	return ev, nil
}

// RolloutWiden 加宽到目标档位。返回 (状态, 拒绝原因)；原因非空表示**没有写库**。
//
// 加宽必须先过证据这一关：这不是"多一层审批"，而是"放量本来就得拿事实说话"。
func (s *RolloutService) RolloutWiden(env, stage string) (*models.RolloutState, []string, rollout.Evidence, error) {
	target, err := tools.ParseAutonomyLevel(stage)
	if err != nil {
		return nil, []string{err.Error()}, rollout.Evidence{}, nil
	}
	now := time.Now()
	ev, err := s.RolloutEvidence(now)
	if err != nil {
		return nil, nil, ev, err
	}
	cfg := rollout.DefaultConfig()
	reasons := rollout.WidenReasons(target, ev, cfg, now)
	if len(reasons) > 0 {
		return nil, reasons, ev, nil
	}

	evJSON, _ := json.Marshal(ev)
	reason := fmt.Sprintf("证据放行：评测 run_id=%d（%d 通过 / %d 失败 / %d 跳过，得分 %.3f）+ SLO %d 个契约无违约",
		ev.EvalRunID, ev.EvalPassed, ev.EvalFailed, ev.EvalSkipped, ev.EvalScore, ev.SloTotal)
	row, err := s.saveStage(env, target, false, reason, string(evJSON), true)
	if err != nil {
		return nil, nil, ev, err
	}
	logger.GetLogger().Info(fmt.Sprintf("[rollout] 放量加宽 env=%s -> %s（%s）", row.Env, row.Stage, reason))
	return row, nil, ev, nil
}

// RolloutTighten 收紧到目标档位（默认 L0）。收紧**不需要**任何证据，也永不拒绝。
func (s *RolloutService) RolloutTighten(env, stage, reason string) (*models.RolloutState, error) {
	target := tools.AutonomyL0
	if strings.TrimSpace(stage) != "" {
		parsed, err := tools.ParseAutonomyLevel(stage)
		if err != nil {
			return nil, err
		}
		target = parsed
	}
	if strings.TrimSpace(reason) == "" {
		reason = "人工收紧"
	}
	// 收紧保留上一次加宽的证据快照：复盘要能同时看到"凭什么放的"和"为什么收的"。
	prev, _ := s.GetRolloutState(env)
	evJSON := ""
	if prev != nil {
		evJSON = prev.EvidenceJSON
	}
	row, err := s.saveStage(env, target, true, reason, evJSON, false)
	if err != nil {
		return nil, err
	}
	logger.GetLogger().Warn(fmt.Sprintf("[rollout] 放量收紧 env=%s -> %s（%s）", row.Env, row.Stage, reason))
	return row, nil
}

// RolloutEvaluate 自动降级：采集证据，命中坏信号就把档位收回 L0。
//
// 这是"不需要人拍板"的那一半：返回 tightened=false 表示没有坏信号、什么都没改。
// 幂等：连着调用两次，第二次不会重复写库，也不会把已经 L0 的状态再改一遍。
func (s *RolloutService) RolloutEvaluate(env string) (row *models.RolloutState, ev rollout.Evidence, reasons []string, tightened bool, err error) {
	now := time.Now()
	ev, err = s.RolloutEvidence(now)
	if err != nil {
		return nil, ev, nil, false, err
	}
	target, reasons, hit := rollout.AutoTarget(ev)
	if !hit {
		row, err = s.GetRolloutState(env)
		return row, ev, nil, false, err
	}
	if row, err = s.saveStage(env, target, true, "自动降级："+strings.Join(reasons, "；"), "", false); err != nil {
		return nil, ev, reasons, false, err
	}
	return row, ev, reasons, true, nil
}

// saveStage 写入档位状态（不存在则创建）。widen=true 时刷新 WidenedAt，否则刷新 TightenedAt。
func (s *RolloutService) saveStage(env string, stage tools.AutonomyLevel, frozen bool, reason, evidenceJSON string, widen bool) (*models.RolloutState, error) {
	db := dbclient.GetMysqlDB()
	if db == nil {
		return nil, fmt.Errorf("数据库不可用，无法写入放量状态")
	}
	env = RolloutEnv(env)
	now := time.Now()
	row, err := s.GetRolloutState(env)
	if err != nil {
		return nil, err
	}
	if row == nil {
		row = &models.RolloutState{Env: env, Created: now.Unix()}
	}
	row.Stage, row.Frozen, row.Reason = stage.String(), frozen, reason
	if evidenceJSON != "" {
		row.EvidenceJSON = evidenceJSON
	}
	if widen {
		row.WidenedAt, row.Frozen = now.Unix(), false // 加宽成功即解除冻结（加宽本身就是人工确认）
	} else {
		row.TightenedAt = now.Unix()
	}
	row.Updated = now.Unix()

	if row.ID == 0 {
		if err := db.Table(models.CronyRolloutStateTableName).Create(row).Error; err != nil {
			return nil, err
		}
		return row, nil
	}
	if err := db.Table(models.CronyRolloutStateTableName).Where("id = ?", row.ID).Updates(map[string]interface{}{
		"stage": row.Stage, "frozen": row.Frozen, "reason": row.Reason,
		"evidence_json": row.EvidenceJSON, "widened_at": row.WidenedAt,
		"tightened_at": row.TightenedAt, "updated": row.Updated,
	}).Error; err != nil {
		return nil, err
	}
	return row, nil
}

// foldCaps 把「信用侧等级」与「放量档位」折成一个 want 级别（纯函数，便于穷举测试）。
//
// 语义与网关的 Tighten 一致：两者都只会**更严**，谁更严听谁的；
// 两者都没表态（ok 都为 false）时返回 known=false，由网关回落到配置级别。
func foldCaps(credit tools.AutonomyLevel, creditOK bool, cap tools.AutonomyLevel, capOK bool) (tools.AutonomyLevel, bool) {
	switch {
	case creditOK && capOK:
		return tools.Tighten(credit, cap), true
	case creditOK:
		return credit, true
	case capOK:
		return cap, true
	}
	return "", false
}

// nextStage 给出"在当前档位之上再放一档"的目标，用于回答
// "现在还能不能再放、为什么不能"。已经是最高档时返回自身。
func nextStage(cur tools.AutonomyLevel) tools.AutonomyLevel {
	switch cur {
	case tools.AutonomyL0:
		return tools.AutonomyL1
	case tools.AutonomyL1:
		return tools.AutonomyL2
	default:
		return tools.AutonomyL3
	}
}

// RolloutView 放量状态的可读快照（读接口与控制台面板共用）。
//
// 为什么要一口气返回"状态 + 证据 + 再放一档为什么不行"：
// 放量页面上运维真正要问的只有一个问题——"现在能不能再放一档，不能的话卡在哪"，
// 把这三样拆成三个接口只会让人来回拼。
type RolloutView struct {
	// Env 实际生效的环境段（留空入参会被归一化成配置的环境段）。
	Env string `json:"env"`
	// Enabled 进程是否把放量档位接进了放行判定（`tools.rollout-gate`）。
	Enabled bool `json:"enabled"`
	// State 当前档位状态；nil = 还没有记录（不表态，沿用配置）。
	State *models.RolloutState `json:"state"`
	// Evidence 本次采集到的放行依据（读接口顺带给出，避免"看不到证据只能猜"）。
	Evidence rollout.Evidence `json:"evidence"`
	// EvidenceError 证据采集失败的原因（有值时 Evidence 不完整）。
	EvidenceError string `json:"evidence_error,omitempty"`
	// NextStage 当前档位的上一档（解释用）。
	NextStage string `json:"next_stage"`
	// WidenReasons 加宽到 NextStage 的拒绝原因；空 = 现在就能放。
	WidenReasons []string `json:"widen_reasons"`
	// MaxStage 放量允许的最高档（内核默认 L2；L3 要改配置）。
	MaxStage string `json:"max_stage"`
	// Note 人话说明。
	Note string `json:"note"`
}

// RolloutViewOf 组装某环境的状态快照（**只读**：不改库、不自动降级）。
func (s *RolloutService) RolloutViewOf(env string) RolloutView {
	env = RolloutEnv(env)
	view := RolloutView{Env: env, Enabled: RolloutEnabled(), MaxStage: rollout.DefaultConfig().MaxStage.String()}
	cur := tools.AutonomyL0
	row, err := s.GetRolloutState(env)
	if err != nil {
		view.Note = "读取放量状态失败：" + err.Error()
		return view
	}
	view.State = row
	if row != nil {
		if parsed, perr := tools.ParseAutonomyLevel(row.Stage); perr == nil {
			cur = parsed
		}
	}
	view.NextStage = nextStage(cur).String()

	now := time.Now()
	ev, eerr := s.RolloutEvidence(now)
	view.Evidence = ev
	if eerr != nil {
		view.EvidenceError = eerr.Error()
	}
	view.WidenReasons = rollout.WidenReasons(nextStage(cur), ev, rollout.DefaultConfig(), now)
	if row == nil {
		view.Note = "还没有放量记录：放行判定沿用配置（开开关本身不改行为）"
	} else if row.Frozen {
		view.Note = "放量已冻结：" + row.Reason
	} else {
		view.Note = "当前档位 " + row.Stage + "：" + row.Reason
	}
	return view
}

// RollingGateResolver 构造写入工具网关的"双层"解析器：配置为上限，信用与放量都只能收紧。
//
// creditOn / rolloutOn 来自配置：两个都关时调用方**不应该**装配解析器
// （与阶段 4 行为完全一致），这里只负责把开关都打开时的组合语义做对。
func RollingGateResolver(env string, creditOn, rolloutOn bool) tools.AutonomyResolver {
	env = RolloutEnv(env)
	return func(tool string) (tools.AutonomyLevel, bool) {
		creditLevel, creditOK := tools.AutonomyLevel(""), false
		if creditOn {
			if _, enforced, ok, _, _ := resolveCredit(dbclient.GetMysqlDB(), AutonomyScope(tool, env)); ok {
				creditLevel, creditOK = toolsLevelOf(enforced), true
			}
		}
		capLevel, capOK := tools.AutonomyLevel(""), false
		if rolloutOn {
			lvl, ok, _ := DefaultRolloutService.RolloutCapFor(env)
			capLevel, capOK = lvl, ok
		}
		return foldCaps(creditLevel, creditOK, capLevel, capOK)
	}
}
