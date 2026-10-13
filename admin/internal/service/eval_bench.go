package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/eval"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// ---------- 回放评测台（阶段 5） ----------
//
// 定位：把**种子评测集**对**已录制的真实轨迹**回放打分，产出可回溯、可比较的报告。
//
// 为什么是"回放"而不是"现场跑一遍"：
//   - 现场跑需要活的模型。没有凭据的环境里"现场跑"只能 SKIP，评测台就形同虚设；
//     而历史会话已经在库里，是**固定的输入**——同一份评测集对同一批轨迹打分才可比。
//   - 放量的目的（阶段 5 的"灰度"）是回答"这次改动有没有让历史场景变差"，
//     这恰恰是回放能回答、现场跑回答不了的问题。
//
// 只回放 status=completed 的会话：失败/中断的会话是"没跑完"，不是"跑错了"，
// 把它们计入会把评测变成一张环境噪声表。

// EvalBenchService 评测台服务。
type EvalBenchService struct{}

// DefaultEvalBenchService 默认实例。
var DefaultEvalBenchService = new(EvalBenchService)

// 每次跑批每条样本最多回放的会话数。
// 设上限是因为评测是"抽样回归"而不是"全量重算"：一条样本回放 20 个历史会话
// 已经足以发现"这条不变式被破坏了"，再多只是让报告更长、跑批更慢。
const (
	defaultSessionsPerCase = 20
	maxSessionsPerCase     = 200
)

// EvalSessionResult 一次"样本 × 会话"的判定。
type EvalSessionResult struct {
	SessionID string       `json:"session_id"`
	Provider  string       `json:"provider"`
	Status    string       `json:"status"`
	Passed    bool         `json:"passed"`
	Score     float64      `json:"score"`
	Checks    []eval.Check `json:"checks"`
}

// EvalCaseResult 一条样本的回放结果（对多个会话的汇总）。
type EvalCaseResult struct {
	CaseKey       string `json:"case_key"`
	Scenario      string `json:"scenario"`
	SessionPrefix string `json:"session_prefix"`
	// Status passed / failed / skipped（skip 不是 pass，见 models.EvalStatusSkipped）。
	Status     string              `json:"status"`
	Reason     string              `json:"reason,omitempty"`
	Score      float64             `json:"score"`
	Sessions   []string            `json:"sessions"`
	Checks     []eval.Check        `json:"checks,omitempty"`
	PerSession []EvalSessionResult `json:"per_session,omitempty"`
}

// EvalReport 一次跑批的完整报告（会序列化进 eval_run.report_json）。
type EvalReport struct {
	RunID      int64  `json:"run_id"`
	SetName    string `json:"set_name"`
	SetVersion string `json:"set_version"`
	Mode       string `json:"mode"`
	Trigger    string `json:"trigger"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
	DurationMs int64  `json:"duration_ms"`
	Cases      int    `json:"cases"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
	// Sessions 判定次数（同一会话会被多条样本评判，故不等于去重后的会话数）。
	Sessions int              `json:"sessions"`
	Score    float64          `json:"score"`
	Results  []EvalCaseResult `json:"results"`
	Notes    []string         `json:"notes"`
}

// EvalRunOptions 跑批参数。
type EvalRunOptions struct {
	SetName      string
	LimitPerCase int
	Trigger      string
	Persist      bool
}

// riskOfTool 查一次工具调用的风险级别。
//
// 只认内置工具的静态属性（common/pkg/tools.StaticRisks）。
// 非内置工具（MCP/插件）返回空 = "未分类"——**故意不猜**：
// 猜错的代价是安全断言悄悄放行，而那正是评测本该拦住的事。
func riskOfTool(name string) tools.RiskLevel {
	if r, ok := tools.StaticRisk(name); ok {
		return r
	}
	return ""
}

// evalActualFromSteps 把录制的轨迹步骤映射成打分内核的输入。
//
// 风险级别在这里补上：轨迹里只有 tool_name，风险级别是工具的静态属性。
func evalActualFromSteps(steps []models.AgentStep, durationMs int64) eval.Actual {
	es := make([]eval.Step, 0, len(steps))
	var latency int64
	for i := range steps {
		st := &steps[i]
		var risk tools.RiskLevel
		if eval.IsToolStep(st.Kind) && st.ToolName != "" {
			risk = riskOfTool(st.ToolName)
		}
		es = append(es, eval.Step{
			Kind:      st.Kind,
			Tool:      st.ToolName,
			ArgsHash:  hashString(st.ToolArgsJSON),
			LatencyMS: st.LatencyMs,
			Risk:      risk,
		})
		latency += st.LatencyMs
	}
	// 会话有端到端耗时时以它为准：轨迹里工具步的 latency_ms 可能没采集（实测为 0），
	// 用"步耗时之和"会得到 0，延迟断言就变成永远跳过（假绿）。
	if durationMs > 0 {
		latency = durationMs
	}
	return eval.Actual{Steps: es, LatencyMS: latency}
}

// SyncEvalCases 把内置种子集写入/更新到 eval_case（按 case_key 幂等），并下线代码里已删除的种子样本。
//
// 返回 added/updated/removed：新增、更新、下线（删除）的条数。
//
// 两个刻意的行为：
//   - **已存在样本的 enabled 以库里的为准**。运维手工停用一条样本（比如确认它在某环境不适用）后，
//     不应该被下次同步悄悄打开——那会让"停用"这个动作失去意义。
//   - **代码里没有的种子样本会被删除**（仅限 source=seed 且 set_name=seed 的行）。
//     不删就会留下"幽灵样本"：它在新代码里已经不存在，没人能修，却会一直参与跑批并报红。
//     这个坑是本轮真实踩到的——把 seed-all-evidence-backed 换成 seed-all-readonly 之后，
//     旧行仍在库里跑，报告里出现一条"改了代码也修不掉"的失败。
//     删除是安全的：这些行完全由代码派生，随时可以重新同步回来。
func (s *EvalBenchService) SyncEvalCases() (added int, updated int, removed int, err error) {
	db := dbclient.GetMysqlDB()
	seedCases := eval.SeedCases()
	live := make([]string, 0, len(seedCases))
	for _, sc := range seedCases {
		payload, merr := json.Marshal(sc.Case)
		if merr != nil {
			return added, updated, removed, fmt.Errorf("序列化种子样本 %s 失败：%w", sc.Case.ID, merr)
		}
		live = append(live, sc.Case.ID)
		enabled := sc.Enabled
		var cur models.EvalCase
		if qerr := db.Table(models.CronyEvalCaseTableName).
			Where("case_key = ?", sc.Case.ID).Limit(1).Scan(&cur).Error; qerr == nil && cur.ID != 0 {
			enabled = cur.Enabled // 保留运维的启停选择
		}
		row := models.EvalCase{
			CaseKey:       sc.Case.ID,
			SetName:       eval.SeedSetName,
			SetVersion:    eval.SeedVersion,
			Scenario:      sc.Scenario,
			SessionPrefix: sc.SessionPrefix,
			ExpectJSON:    string(payload),
			Enabled:       enabled,
			Source:        models.EvalCaseSourceSeed,
		}
		created, serr := models.SaveEvalCase(db, &row)
		if serr != nil {
			return added, updated, removed, serr
		}
		if created {
			added++
		} else {
			updated++
		}
	}

	if len(live) == 0 {
		return added, updated, removed, nil
	}
	stale := make([]models.EvalCase, 0)
	if qerr := db.Table(models.CronyEvalCaseTableName).
		Where("set_name = ? and source = ?", eval.SeedSetName, models.EvalCaseSourceSeed).
		Where("case_key not in ?", live).
		Find(&stale).Error; qerr != nil {
		return added, updated, removed, qerr
	}
	for i := range stale {
		if derr := db.Table(models.CronyEvalCaseTableName).
			Where("id = ?", stale[i].ID).Delete(&models.EvalCase{}).Error; derr != nil {
			return added, updated, removed, derr
		}
		logger.GetLogger().Warn(fmt.Sprintf("[ops/eval/seed/sync] 已下线种子样本 %s（代码中已不存在）", stale[i].CaseKey))
		removed++
	}
	return added, updated, removed, nil
}

// ListEvalCases 列出某个集合的全部样本（含停用的），供前端展示。
func (s *EvalBenchService) ListEvalCases(setName string) ([]models.EvalCase, error) {
	list := make([]models.EvalCase, 0)
	q := dbclient.GetMysqlDB().Table(models.CronyEvalCaseTableName)
	if strings.TrimSpace(setName) != "" {
		q = q.Where("set_name = ?", strings.TrimSpace(setName))
	}
	err := q.Order("set_name asc, case_key asc").Find(&list).Error
	return list, err
}

// listEnabledCases 取跑批要用的样本（只看启用的）。
func (s *EvalBenchService) listEnabledCases(setName string) ([]models.EvalCase, error) {
	list := make([]models.EvalCase, 0)
	err := dbclient.GetMysqlDB().Table(models.CronyEvalCaseTableName).
		Where("set_name = ? and enabled = ?", setName, true).
		Order("case_key asc").Find(&list).Error
	return list, err
}

// evalSessionRow 回放对象的轻量投影。
type evalSessionRow struct {
	SessionID  string `gorm:"column:session_id"`
	Provider   string `gorm:"column:provider"`
	Status     string `gorm:"column:status"`
	DurationMs int64  `gorm:"column:duration_ms"`
}

// matchSessions 选一批"已完成"的历史会话作为回放对象。
func (s *EvalBenchService) matchSessions(prefix string, limit int) ([]evalSessionRow, error) {
	list := make([]evalSessionRow, 0, limit)
	q := dbclient.GetMysqlDB().Table(models.CronyAgentSessionTableName).
		Select("session_id, provider, status, duration_ms").
		Where("status = ?", models.AgentStatusCompleted)
	if p := strings.TrimSpace(prefix); p != "" {
		// LIKE 通配符在这里由**运维配置的样本前缀**提供，不是用户输入，
		// 但仍然转义 %/_，避免一条手写样本意外匹配到别的会话类型。
		q = q.Where("session_id like ?", escapeLike(p)+"%")
	}
	err := q.Order("id desc").Limit(limit).Find(&list).Error
	return list, err
}

// escapeLike 转义 LIKE 的通配符。
func escapeLike(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return r.Replace(s)
}

// loadSteps 取一次会话的轨迹。
func (s *EvalBenchService) loadSteps(sessionID string) ([]models.AgentStep, error) {
	steps := make([]models.AgentStep, 0)
	err := dbclient.GetMysqlDB().Table(models.CronyAgentStepTableName).
		Where("session_id = ?", sessionID).Order("seq asc").Find(&steps).Error
	return steps, err
}

// caseFromRow 把库里的样本还原成打分内核的样本。
func caseFromRow(row *models.EvalCase) (eval.Case, error) {
	var c eval.Case
	if err := json.Unmarshal([]byte(row.ExpectJSON), &c); err != nil {
		return c, err
	}
	// case_key 是身份的权威来源：expect_json 里的 ID 可能过期（种子换过键名），以行为准。
	c.ID = row.CaseKey
	return c, nil
}

// RunEval 跑一次回放评测。
func (s *EvalBenchService) RunEval(opts EvalRunOptions) (*EvalReport, error) {
	started := time.Now()
	db := dbclient.GetMysqlDB()

	setName := strings.TrimSpace(opts.SetName)
	if setName == "" {
		setName = eval.SeedSetName
	}
	limit := opts.LimitPerCase
	if limit <= 0 {
		limit = defaultSessionsPerCase
	}
	if limit > maxSessionsPerCase {
		limit = maxSessionsPerCase
	}
	trigger := strings.TrimSpace(opts.Trigger)
	if trigger == "" {
		trigger = "manual"
	}

	notes := []string{
		"回放对象：agent_session 中 status=completed 的会话；失败/中断的会话不计入（它们没跑完，没有可比性）。",
		"评分分母 = passed + failed；skipped 不计入（既不算通过也不算失败）。",
		"工具风险级别取自内置工具的静态属性；非内置工具（MCP/插件）记为未分类，安全断言会明确失败而不是默认放行。",
	}

	rows, err := s.listEnabledCases(setName)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 && setName == eval.SeedSetName {
		added, _, _, serr := s.SyncEvalCases()
		if serr != nil {
			return nil, serr
		}
		notes = append(notes, fmt.Sprintf("库内没有 %s 集合，已按需同步内置种子集（新增 %d 条）。", setName, added))
		if rows, err = s.listEnabledCases(setName); err != nil {
			return nil, err
		}
	}

	rep := &EvalReport{
		SetName:   setName,
		Mode:      models.EvalModeReplay,
		Trigger:   trigger,
		StartedAt: started.Unix(),
		Results:   make([]EvalCaseResult, 0, len(rows)),
		Notes:     notes,
	}

	versions := map[string]int{}
	for i := range rows {
		row := &rows[i]
		versions[row.SetVersion]++

		res := EvalCaseResult{
			CaseKey:       row.CaseKey,
			Scenario:      row.Scenario,
			SessionPrefix: row.SessionPrefix,
			Sessions:      []string{},
		}

		c, cerr := caseFromRow(row)
		if cerr != nil {
			// 期望值损坏 = 真缺陷，必须响：算"通过"会掩盖损坏，静默跳过则永远没人修。
			res.Status = models.EvalStatusFailed
			res.Reason = "样本期望值损坏（expect_json 解析失败）：" + cerr.Error()
			rep.Failed++
			rep.Cases++
			rep.Results = append(rep.Results, res)
			continue
		}

		sessions, serr := s.matchSessions(row.SessionPrefix, limit)
		if serr != nil {
			return nil, serr
		}
		if len(sessions) == 0 {
			res.Status = models.EvalStatusSkipped
			res.Reason = "没有匹配到 status=completed 的会话"
			if strings.TrimSpace(row.SessionPrefix) != "" {
				res.Reason += "（session_id 前缀 " + row.SessionPrefix + "）"
			}
			rep.Skipped++
			rep.Cases++
			rep.Results = append(rep.Results, res)
			continue
		}

		per := make([]EvalSessionResult, 0, len(sessions))
		allPass, sum := true, 0.0
		for _, sess := range sessions {
			steps, lerr := s.loadSteps(sess.SessionID)
			if lerr != nil {
				return nil, lerr
			}
			r := eval.Score(c, evalActualFromSteps(steps, sess.DurationMs))
			per = append(per, EvalSessionResult{
				SessionID: sess.SessionID,
				Provider:  sess.Provider,
				Status:    sess.Status,
				Passed:    r.Passed,
				Score:     r.Score,
				Checks:    r.Checks,
			})
			res.Sessions = append(res.Sessions, sess.SessionID)
			sum += r.Score
			if !r.Passed {
				allPass = false
			}
		}
		res.PerSession = per
		res.Score = sum / float64(len(per))
		res.Checks = aggregateCaseChecks(per)
		if allPass {
			res.Status = models.EvalStatusPassed
			rep.Passed++
		} else {
			res.Status = models.EvalStatusFailed
			res.Reason = firstFailureReason(per)
			rep.Failed++
		}
		rep.Sessions += len(per)
		rep.Cases++
		rep.Results = append(rep.Results, res)
	}

	// 集合版本：混合版本不可比，明确标出来而不是挑一个版本号糊过去。
	switch len(versions) {
	case 0:
		rep.SetVersion = eval.SeedVersion
	case 1:
		for v := range versions {
			rep.SetVersion = v
		}
	default:
		rep.SetVersion = "mixed"
		rep.Notes = append(rep.Notes, fmt.Sprintf("该集合存在 %d 个不同版本，报告不可跨次比较。", len(versions)))
	}

	// 评分分母**不含** skipped：把"没考到"当分母会稀释真实失败率。
	if denom := rep.Passed + rep.Failed; denom > 0 {
		rep.Score = float64(rep.Passed) / float64(denom)
	} else {
		rep.Score = 0
		rep.Notes = append(rep.Notes, "没有有效样本（全部跳过），本次跑批不构成评分证据。")
	}

	rep.FinishedAt = time.Now().Unix()
	rep.DurationMs = time.Since(started).Milliseconds()

	if opts.Persist {
		run := &models.EvalRun{
			SetName:    rep.SetName,
			SetVersion: rep.SetVersion,
			Mode:       rep.Mode,
			Trigger:    rep.Trigger,
			Cases:      rep.Cases,
			Passed:     rep.Passed,
			Failed:     rep.Failed,
			Skipped:    rep.Skipped,
			Sessions:   rep.Sessions,
			Score:      rep.Score,
			StartedAt:  rep.StartedAt,
			FinishedAt: rep.FinishedAt,
			DurationMs: rep.DurationMs,
		}
		if payload, merr := json.Marshal(rep); merr == nil {
			run.ReportJSON = string(payload)
		}
		id, ierr := models.InsertEvalRun(db, run)
		if ierr != nil {
			return nil, ierr
		}
		// 回填 run_id 再存一次：报告里查不到自己属于哪次跑批，回溯时就对不上号。
		rep.RunID = id
		if payload, merr := json.Marshal(rep); merr == nil {
			if uerr := db.Table(models.CronyEvalRunTableName).
				Where("id = ?", id).Update("report_json", string(payload)).Error; uerr != nil {
				logger.GetLogger().Warn("[ops/eval/run] 回填 run_id 进报告失败（不影响结果）: " + uerr.Error())
			}
		}
	}
	return rep, nil
}

// firstFailureReason 给出"这条样本为什么没过"的一句人话。
func firstFailureReason(per []EvalSessionResult) string {
	for i := range per {
		for _, ck := range per[i].Checks {
			if !ck.Passed {
				return fmt.Sprintf("会话 %s 的断言 %s 不通过：%s", per[i].SessionID, ck.Name, ck.Detail)
			}
		}
	}
	return "存在不通过的会话"
}

// namedCheck 归集断言时保留来源会话，便于把"谁不通过"写进明细。
type namedCheck struct {
	sessionID string
	check     eval.Check
}

// aggregateCaseChecks 把逐会话的断言汇总成样本级明细。
//
// 口径：同一条断言**所有会话都过**才算过；只要有会话不过，就带出它的明细。
// 这与"样本级通过 = 全部会话通过"一致，不会出现"样本判过了但明细里有一条红的"。
func aggregateCaseChecks(per []EvalSessionResult) []eval.Check {
	if len(per) == 0 {
		return nil
	}
	order := make([]string, 0)
	byName := map[string][]namedCheck{}
	for i := range per {
		for _, ck := range per[i].Checks {
			if _, ok := byName[ck.Name]; !ok {
				order = append(order, ck.Name)
			}
			byName[ck.Name] = append(byName[ck.Name], namedCheck{sessionID: per[i].SessionID, check: ck})
		}
	}
	out := make([]eval.Check, 0, len(order))
	for _, name := range order {
		list := byName[name]
		pass := true
		detail := ""
		for _, nc := range list {
			if !nc.check.Passed {
				pass = false
				detail = fmt.Sprintf("%s：%s", nc.sessionID, nc.check.Detail)
				break
			}
		}
		if pass {
			detail = fmt.Sprintf("%d 个会话全部通过：%s", len(list), list[0].check.Detail)
		}
		out = append(out, eval.Check{Name: name, Passed: pass, Detail: detail})
	}
	return out
}

// GetEvalRun 按 id 取一次跑批（id<=0 取最近一次）。
//
// 返回原始行 + 解析后的报告：前端不该自己解析 report_json。
func (s *EvalBenchService) GetEvalRun(id int64) (*models.EvalRun, *EvalReport, error) {
	db := dbclient.GetMysqlDB()
	row := models.EvalRun{}
	q := db.Table(models.CronyEvalRunTableName)
	if id > 0 {
		q = q.Where("id = ?", id)
	} else {
		q = q.Order("id desc").Limit(1)
	}
	if err := q.Find(&row).Error; err != nil {
		return nil, nil, err
	}
	if row.ID == 0 {
		return nil, nil, nil // 还没有任何跑批
	}
	rep := EvalReport{}
	if strings.TrimSpace(row.ReportJSON) != "" {
		if err := json.Unmarshal([]byte(row.ReportJSON), &rep); err != nil {
			// 报告损坏不影响汇总行可用，明确标注而不是假装正常。
			rep = EvalReport{SetName: row.SetName, SetVersion: row.SetVersion, Mode: row.Mode,
				Trigger: row.Trigger, Cases: row.Cases, Passed: row.Passed, Failed: row.Failed,
				Skipped: row.Skipped, Sessions: row.Sessions, Score: row.Score,
				StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, DurationMs: row.DurationMs,
				Notes: []string{"report_json 解析失败：" + err.Error() + "（仅汇总可用）"}}
		}
	}
	rep.RunID = row.ID
	return &row, &rep, nil
}
