package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/crypt"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/etcdclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 阶段 5 补全：控制台概览 / 依赖健康度 / 配置快照（只读）。
//
// 这三个接口此前在控制台里被标成"规划中"（`planned:true`），页面只能显示空态。
// 现在按**真实来源**补齐，且遵守两条纪律：
//  1. **不编数据**：拿不到就如实说（degraded / 0 / "未配置"），绝不填样例值；
//  2. **只读**：不触碰任何写路径，权限与其它统计接口一致（登录即可）。
//
// 为什么不用 Mock 里那份富样例做契约：样例里有平台并不存在的子系统
// （如独立告警中心），照抄会把"没有"说成"有"。这里只暴露**能从真实状态算出来**的字段。

// processStartedAt 进程启动时刻（用于健康度里的 uptime）。
var processStartedAt = time.Now()

// 健康度状态取值（与前端约定的三档 + planned）。
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
	HealthDown     = "down"
	HealthPlanned  = "planned"
)

// HealthComponent 一个依赖的健康度。
type HealthComponent struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail"`
}

// HealthReport GET /statis/health 的响应体。
type HealthReport struct {
	Status     string            `json:"status"`
	CheckedAt  string            `json:"checked_at"`
	Components []HealthComponent `json:"components"`
}

// HealthService 依赖健康度（只读）。
type HealthService struct{}

// DefaultHealthService 进程内单例。
var DefaultHealthService = new(HealthService)

// Report 汇总各依赖的健康度。整体状态取所有组件里最差的一档
// （down > degraded > ok；planned 不参与降级，因为"规划中"不是故障）。
func (s *HealthService) Report() HealthReport {
	comps := []HealthComponent{
		probeAdmin(),
		probeMySQL(),
		probeNodes(),
		probeEtcd(),
		probeNotify(),
		probeAgentRuntime(),
	}
	return HealthReport{
		Status:     aggregateHealthStatus(comps),
		CheckedAt:  time.Now().Format(time.RFC3339),
		Components: comps,
	}
}

// aggregateHealthStatus 取最差档；planned 视为中性（不影响整体）。
func aggregateHealthStatus(comps []HealthComponent) string {
	rank := map[string]int{HealthOK: 0, HealthPlanned: 0, HealthDegraded: 1, HealthDown: 2}
	worst, worstRank := HealthOK, 0
	for _, c := range comps {
		if r, ok := rank[c.Status]; ok && r > worstRank {
			worst, worstRank = c.Status, r
		}
	}
	return worst
}

func probeAdmin() HealthComponent {
	up := time.Since(processStartedAt)
	return HealthComponent{
		Name: HealthAdminName, Status: HealthOK,
		Detail: fmt.Sprintf("crony-admin 已运行 %s", uptimeText(up)),
	}
}

// HealthAdminName 组件名常量（避免字符串在各处漂移）。
const HealthAdminName = "crony-admin (API)"

func uptimeText(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%dm%ds", m, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func probeMySQL() HealthComponent {
	c := HealthComponent{Name: "MySQL"}
	db := dbclient.GetMysqlDB()
	if db == nil {
		c.Status, c.Detail = HealthDown, "数据库未初始化"
		return c
	}
	sqlDB, err := db.DB()
	if err != nil {
		c.Status, c.Detail = HealthDown, "取连接池失败: "+err.Error()
		return c
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = sqlDB.PingContext(ctx)
	c.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		c.Status, c.Detail = HealthDown, "连接失败: "+err.Error()
		return c
	}
	st := sqlDB.Stats()
	cfg := config.GetConfigModels()
	addr := "MySQL"
	if cfg != nil {
		addr = fmt.Sprintf("%s:%s/%s", cfg.Mysql.Path, cfg.Mysql.Port, cfg.Mysql.Dbname)
	}
	c.Status = HealthOK
	c.Detail = fmt.Sprintf("%s · 打开 %d 使用中 %d 空闲 %d", addr, st.OpenConnections, st.InUse, st.Idle)
	return c
}

func probeNodes() HealthComponent {
	c := HealthComponent{Name: "crony-node 集群"}
	total, err := countRows(models.CronyNodeTableName, "")
	if err != nil {
		c.Status, c.Detail = HealthDegraded, "读取节点失败: "+err.Error()
		return c
	}
	online, _ := countRows(models.CronyNodeTableName, "status = ?", models.NodeConnSuccess)
	c.Detail = fmt.Sprintf("%d/%d 在线", online, total)
	switch {
	case total == 0:
		c.Status = HealthDegraded
		c.Detail = "暂无已注册节点"
	case online == total:
		c.Status = HealthOK
	case online == 0:
		c.Status = HealthDown
	default:
		c.Status = HealthDegraded
	}
	return c
}

func probeEtcd() HealthComponent {
	c := HealthComponent{Name: "调度协同（etcd）"}
	if etcdclient.GetEtcdClient() == nil {
		c.Status = HealthDegraded
		c.Detail = "etcd 未初始化：节点注册/调度协同不可用（控制面 API 仍可用）"
		return c
	}
	start := time.Now()
	_, err := etcdclient.Get("__crony_health_probe__")
	c.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		c.Status, c.Detail = HealthDown, "etcd 不可达: "+err.Error()
		return c
	}
	c.Status, c.Detail = HealthOK, "etcd 可达"
	return c
}

func probeNotify() HealthComponent {
	c := HealthComponent{Name: "通知渠道"}
	cfg := config.GetConfigModels()
	if cfg == nil {
		c.Status, c.Detail = HealthDegraded, "配置未加载"
		return c
	}
	parts := make([]string, 0, 2)
	if strings.TrimSpace(cfg.WebHook.Url) != "" {
		kind := strings.TrimSpace(cfg.WebHook.Kind)
		if kind == "" {
			kind = "webhook"
		}
		parts = append(parts, kind+" 已配置")
	}
	if strings.TrimSpace(cfg.Email.Host) != "" {
		parts = append(parts, "邮件 SMTP 已配置")
	}
	if len(parts) == 0 {
		// 没有渠道不是故障（可以纯控制台使用），但值得提示。
		c.Status, c.Detail = HealthDegraded, "未配置任何通知渠道（失败通知与违约告警不会送达）"
		return c
	}
	c.Status, c.Detail = HealthOK, strings.Join(parts, " · ")
	return c
}

func probeAgentRuntime() HealthComponent {
	c := HealthComponent{Name: "Agent 运行时"}
	rt := DefaultAgentRuntime()
	if rt == nil {
		c.Status = HealthPlanned
		c.Detail = "未启用（llm.enabled=false 或未配置模型）：诊断/自然语言建任务不可用"
		return c
	}
	c.Status = HealthOK
	c.Detail = fmt.Sprintf("provider=%s model=%s offline=%v 工具=%d",
		rt.ProviderName(), rt.Model(), rt.Offline(), len(rt.ToolNames()))
	return c
}

// countRows 统计行数（table 由调用方给出常量，不接受外部输入）。
func countRows(table string, where string, args ...interface{}) (int64, error) {
	db := dbclient.GetMysqlDB()
	if db == nil {
		return 0, fmt.Errorf("数据库未初始化")
	}
	var n int64
	q := db.Table(table)
	if strings.TrimSpace(where) != "" {
		q = q.Where(where, args...)
	}
	if err := q.Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// ---------- 控制台概览 ----------

// OverviewKPI 概览指标。
type OverviewKPI struct {
	TotalJobs        int64   `json:"total_jobs"`
	EnabledJobs      int64   `json:"enabled_jobs"`
	OnlineNodes      int64   `json:"online_nodes"`
	TotalNodes       int64   `json:"total_nodes"`
	RunsToday        int64   `json:"runs_today"`
	SuccessToday     int64   `json:"success_today"`
	SuccessRateToday float64 `json:"success_rate_today"`
	FailedToday      int64   `json:"failed_today"`
	TimeoutToday     int64   `json:"timeout_today"`
	SkippedToday     int64   `json:"skipped_today"`
	RunningNow       int64   `json:"running_now"`
	AvgDurationMs    int64   `json:"avg_duration_ms"`
	KBDocs           int64   `json:"kb_docs"`
	PendingApprovals int64   `json:"pending_approvals"`
	// Agent 会话（**真实**计数）。平台没有"Agent 在线"这种状态——agent_session 只有终态
	// completed/failed/canceled，所以这里给的是会话事实，而不是编一个"在线数"。
	AgentSessionsTotal       int64 `json:"agent_sessions_total"`
	AgentSessionsToday       int64 `json:"agent_sessions_today"`
	AgentSessionsFailedToday int64 `json:"agent_sessions_failed_today"`
}

// OverviewAlert 概览告警（从健康度与 SLO 违约**真实推导**，没有独立告警子系统就不假装有）。
type OverviewAlert struct {
	ID     string `json:"id"`
	Level  string `json:"level"` // critical | warning | info
	Title  string `json:"title"`
	Source string `json:"source"`
	Time   string `json:"time"`
	Status string `json:"status"`
}

// OverviewReport GET /statis/overview 的响应体。
type OverviewReport struct {
	KPI    OverviewKPI     `json:"kpi"`
	Alerts []OverviewAlert `json:"alerts"`
	// Trends 是 KPI 卡的迷你趋势（12 个 2 小时片段，最近的最后）——控制台 StatCard 的
	// Sparkline 直接吃数组，因此这里给数组而不是"环比百分比"。
	Trends OverviewTrends `json:"trends"`
	// RecentTasks 来自 job_run 的真实最近执行（不含样例数据）。
	RecentTasks []OverviewTask `json:"recent_tasks"`
}

// OverviewTrends KPI 迷你趋势（各 12 段，最近的排在最后）。某段没有数据就是 0。
type OverviewTrends struct {
	RunsToday        []int64   `json:"runs_today"`
	RunsFailed       []int64   `json:"runs_failed"`
	SuccessRateToday []float64 `json:"success_rate_today"`
	AvgDuration      []int64   `json:"avg_duration"`
}

// OverviewTask 「最近任务」一行（字段与控制台表格一一对应）。
type OverviewTask struct {
	RunID         string `json:"run_id"`
	JobID         int    `json:"job_id"`
	JobName       string `json:"job_name"`
	Hostname      string `json:"hostname"`
	IP            string `json:"ip"`
	Status        int    `json:"status"`
	DurationMs    int64  `json:"duration_ms"`
	TriggerSource int    `json:"trigger_source"`
	StartedAt     int64  `json:"started_at"`
	ScheduledTime int64  `json:"scheduled_time"`
}

// 趋势分桶：12 段 × 2 小时 = 24 小时。
const (
	trendBucketWidth = 2 * 60 * 60
	trendBucketCount = 12
)

// OverviewService 控制台概览（只读聚合）。
type OverviewService struct{}

// DefaultOverviewService 进程内单例。
var DefaultOverviewService = new(OverviewService)

// Report 计算概览。任何一项取不到都用 0 + 不加告警的方式如实呈现，不报错中断。
func (s *OverviewService) Report() OverviewReport {
	rep := OverviewReport{Alerts: make([]OverviewAlert, 0)}
	k := &rep.KPI
	k.TotalJobs, _ = countRows(models.CronyJobTableName, "")
	k.EnabledJobs, _ = countRows(models.CronyJobTableName, "status = ?", models.JobStatusAssigned)
	k.TotalNodes, _ = countRows(models.CronyNodeTableName, "")
	k.OnlineNodes, _ = countRows(models.CronyNodeTableName, "status = ?", models.NodeConnSuccess)
	k.KBDocs, _ = countRows(models.CronyAgentMemoryTableName, "enabled = ?", true)
	k.PendingApprovals, _ = countRows(models.CronyApprovalRequestTableName, "status = ?", models.ApprovalStatusPending)

	since := startOfTodayUnix()
	k.RunsToday, _ = countRows(models.CronyJobRunTableName, "finished_at >= ?", since)
	k.FailedToday, _ = countRows(models.CronyJobRunTableName, "finished_at >= ? and status = ?", since, int(models.RunStatusFailed))
	k.TimeoutToday, _ = countRows(models.CronyJobRunTableName, "finished_at >= ? and status = ?", since, int(models.RunStatusTimeout))
	k.SkippedToday, _ = countRows(models.CronyJobRunTableName, "finished_at >= ? and status = ?", since, int(models.RunStatusSkipped))
	k.SuccessToday, _ = countRows(models.CronyJobRunTableName, "finished_at >= ? and status = ?", since, int(models.RunStatusSuccess))
	if k.RunsToday > 0 {
		k.SuccessRateToday = round1(float64(k.SuccessToday) * 100 / float64(k.RunsToday))
	}
	k.AvgDurationMs = avgDurationSince(since)
	// 正在执行的次数（真实来源：etcd 里在跑的进程档案），取不到就是 0。
	k.RunningNow, _ = DefaultJobService.GetRunningJobCount()
	rep.Trends = buildOverviewTrends()
	rep.RecentTasks = recentOverviewTasks(8)

	// Agent 会话（真实计数；只有终态，故不报"在线数"）。
	k.AgentSessionsTotal, _ = countRows(models.CronyAgentSessionTableName, "")
	k.AgentSessionsToday, _ = countRows(models.CronyAgentSessionTableName, "created >= ?", since)
	k.AgentSessionsFailedToday, _ = countRows(models.CronyAgentSessionTableName,
		"created >= ? and status = ?", since, models.AgentStatusFailed)

	rep.Alerts = append(rep.Alerts, alertsFromHealth(s.health())...)
	rep.Alerts = append(rep.Alerts, alertsFromSlo()...)
	return rep
}

// health 允许测试替换（默认走真实健康度）。
func (s *OverviewService) health() HealthReport {
	return DefaultHealthService.Report()
}

func mustCount(table, where string, args ...interface{}) int64 {
	n, _ := countRows(table, where, args...)
	return n
}

func avgDurationSince(since int64) int64 {
	db := dbclient.GetMysqlDB()
	if db == nil {
		return 0
	}
	var avg float64
	err := db.Table(models.CronyJobRunTableName).
		Select("coalesce(avg(duration_ms),0)").
		Where("finished_at >= ? and duration_ms > 0", since).
		Scan(&avg).Error
	if err != nil {
		return 0
	}
	return int64(avg)
}

// buildOverviewTrends 聚合最近 24 小时的 job_run，切成 12 个 2 小时片段（最近的最后）。
//
// 全部来自真实执行记录：某段没有数据就是 0，不做插值、不用样例。
// 起点对齐到分桶边界（now 往前 12 个整桶），避免首尾半桶被算错位置。
func buildOverviewTrends() OverviewTrends {
	t := OverviewTrends{
		RunsToday:        make([]int64, trendBucketCount),
		RunsFailed:       make([]int64, trendBucketCount),
		SuccessRateToday: make([]float64, trendBucketCount),
		AvgDuration:      make([]int64, trendBucketCount),
	}
	db := dbclient.GetMysqlDB()
	if db == nil {
		return t
	}
	now := time.Now().Unix()
	lastBucket := now / trendBucketWidth
	firstBucket := lastBucket - trendBucketCount + 1
	start := firstBucket * trendBucketWidth

	type agg struct {
		Bucket int64
		Runs   int64
		Succ   int64
		DurSum int64
		DurCnt int64
	}
	var rows []agg
	// 常量全部来自代码（不是外部输入），拼接安全。
	sql := fmt.Sprintf(
		"select (finished_at div %d) as bucket, count(*) as runs, "+
			"sum(case when status = %d then 1 else 0 end) as succ, "+
			"coalesce(sum(duration_ms),0) as dur_sum, "+
			"sum(case when duration_ms > 0 then 1 else 0 end) as dur_cnt "+
			"from %s where finished_at >= ? group by bucket",
		trendBucketWidth, int(models.RunStatusSuccess), models.CronyJobRunTableName)
	if err := db.Raw(sql, start).Scan(&rows).Error; err != nil {
		logger.GetLogger().Warn("[overview] 趋势聚合失败，返回零值趋势：" + err.Error())
		return t
	}
	for _, r := range rows {
		idx := int(r.Bucket - firstBucket)
		if idx < 0 || idx >= trendBucketCount {
			continue
		}
		t.RunsToday[idx] = r.Runs
		t.RunsFailed[idx] = r.Runs - r.Succ
		if r.Runs > 0 {
			t.SuccessRateToday[idx] = round1(float64(r.Succ) * 100 / float64(r.Runs))
		}
		if r.DurCnt > 0 {
			t.AvgDuration[idx] = r.DurSum / r.DurCnt
		}
	}
	return t
}

// recentOverviewTasks 取最近 N 次执行（真实 job_run）。查询失败返回空列表，不返回 nil。
func recentOverviewTasks(limit int) []OverviewTask {
	out := make([]OverviewTask, 0, limit)
	db := dbclient.GetMysqlDB()
	if db == nil {
		return out
	}
	err := db.Table(models.CronyJobRunTableName).
		Select("run_id, job_id, job_name, hostname, ip, status, duration_ms, trigger_source, started_at, scheduled_time").
		Order("id desc").Limit(limit).Find(&out).Error
	if err != nil {
		logger.GetLogger().Warn("[overview] 最近任务查询失败：" + err.Error())
		return make([]OverviewTask, 0)
	}
	return out
}

func alertsFromHealth(h HealthReport) []OverviewAlert {
	out := make([]OverviewAlert, 0)
	now := time.Now().Format(time.RFC3339)
	for _, c := range h.Components {
		if c.Status != HealthDegraded && c.Status != HealthDown {
			continue
		}
		level := "warning"
		if c.Status == HealthDown {
			level = "critical"
		}
		out = append(out, OverviewAlert{
			ID: "health:" + c.Name, Level: level,
			Title: c.Name + " 异常：" + c.Detail, Source: "依赖健康度",
			Time: now, Status: "firing",
		})
	}
	return out
}

func alertsFromSlo() []OverviewAlert {
	out := make([]OverviewAlert, 0)
	evals, err := DefaultSloService.EvaluateSlo(0)
	if err != nil {
		return out
	}
	now := time.Now().Format(time.RFC3339)
	for _, e := range evals {
		if !e.Report.Breached {
			continue
		}
		title := fmt.Sprintf("SLO 契约 %q 违约", e.Policy.Name)
		if e.JobName != "" {
			title += "（任务 " + e.JobName + "）"
		}
		out = append(out, OverviewAlert{
			ID: fmt.Sprintf("slo:%d", e.Policy.ID), Level: "warning", Title: title,
			Source: "SLO 契约", Time: now, Status: "firing",
		})
	}
	return out
}

func startOfTodayUnix() int64 {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).Unix()
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

// ---------- 配置快照 ----------

// SettingsSnapshot GET /settings 的响应体（只读，永不含密钥/口令）。
type SettingsSnapshot struct {
	Auth      map[string]interface{} `json:"auth"`
	Scheduler map[string]interface{} `json:"scheduler"`
	Notify    map[string]interface{} `json:"notify"`
	Agent     map[string]interface{} `json:"agent"`
}

// SettingsService 配置快照（只读）。
type SettingsService struct{}

// DefaultSettingsService 进程内单例。
var DefaultSettingsService = new(SettingsService)

// Get 组装只读配置快照。**只暴露可公开的字段**：口令、API Key、JWT 密钥一律不出现在响应里。
func (s *SettingsService) Get() SettingsSnapshot {
	cfg := config.GetConfigModels()
	snap := SettingsSnapshot{
		Auth:      map[string]interface{}{},
		Scheduler: map[string]interface{}{},
		Notify:    map[string]interface{}{},
		Agent:     map[string]interface{}{},
	}
	if cfg == nil {
		return snap
	}
	snap.Auth = authSnapshot(cfg)
	snap.Scheduler = schedulerSnapshot(cfg)
	snap.Notify = map[string]interface{}{
		"channels": notifyChannels(cfg),
	}
	snap.Agent = s.agentSnapshot(cfg)
	return snap
}

// authSnapshot 鉴权配置只读快照。
//
// 字段名与前端 SettingsView 的读取口径对齐（`jwt_issuer`/`jwt_ttl_hours`/`jwt_secret_source`/
// `force_password_change`/`bootstrap_admin`），否则页面会显示空白。
// 只放**可公开**的事实：口令与密钥原文永不出现在这里。
func authSnapshot(cfg *models.Config) map[string]interface{} {
	secretSource := "未配置（auth.jwt-secret 与 CRONY_AUTH_JWT_SECRET 均为空）"
	if strings.TrimSpace(cfg.Auth.JwtSecret) != "" {
		secretSource = "已配置（admin/conf/app.ini 或环境变量 CRONY_AUTH_JWT_SECRET，环境变量优先）"
	}
	bootstrap := "未配置：用户表为空时由首个 /register 成为管理员（引导注册）"
	if u := strings.TrimSpace(cfg.Auth.BootstrapAdminUser); u != "" && strings.TrimSpace(cfg.Auth.BootstrapAdminPassword) != "" {
		bootstrap = "已配置初始管理员 " + u + "（仅在用户表为空时创建；口令来自配置/环境变量）"
	}
	return map[string]interface{}{
		"jwt_issuer":            models.JwtIssuer,
		"jwt_ttl_hours":         cfg.Auth.TokenExpireHours,
		"jwt_secret_source":     secretSource,
		"password_min_length":   crypt.MinPasswordLength,
		"force_password_change": false, // 现状：本系统没有强制改密机制（如实回答"是否强制"= 否）
		"rbac":                  "已启用：JWTAuth + RequireAdmin",
		"bootstrap_admin":       bootstrap,
		// 兼容/补充字段（不影响渲染）。
		"token_expire_hours":         cfg.Auth.TokenExpireHours,
		"allow_public_register":      cfg.Auth.AllowPublicRegister,
		"bootstrap_admin_configured": strings.TrimSpace(cfg.Auth.BootstrapAdminUser) != "",
		"jwt_secret_configured":      strings.TrimSpace(cfg.Auth.JwtSecret) != "",
	}
}

// schedulerSnapshot 调度默认值（只读）。
//
// 重试/超时是 models.Job 的真实零值默认；并发策略与分配方式是代码固定口径。
func schedulerSnapshot(cfg *models.Config) map[string]interface{} {
	return map[string]interface{}{
		"default_concurrency_policy": "skip",
		"default_retry_times":        0,
		"default_retry_interval":     0,
		"max_retry_times":            models.MaxRetryTimes,
		"default_timeout_seconds":    0,
		"default_allocation":         "auto",
		"self_heal_enabled":          cfg.SelfHeal.Enabled,
		"slo_breach_action_enabled":  cfg.Slo.BreachActionEnabled,
	}
}

func notifyChannels(cfg *models.Config) []map[string]interface{} {
	ch := make([]map[string]interface{}, 0, 4)
	kind := strings.ToLower(strings.TrimSpace(cfg.WebHook.Kind))
	hookLabel := "自定义 Webhook"
	switch kind {
	case "feishu":
		hookLabel = "飞书机器人"
	case "dingtalk":
		hookLabel = "钉钉机器人"
	}
	ch = append(ch, map[string]interface{}{
		"key": "webhook", "label": hookLabel, "supported": true,
		"enabled": strings.TrimSpace(cfg.WebHook.Url) != "",
	})
	ch = append(ch, map[string]interface{}{
		"key": "email", "label": "邮件（SMTP）", "supported": true,
		"enabled": strings.TrimSpace(cfg.Email.Host) != "",
	})
	return ch
}

func (s *SettingsService) agentSnapshot(cfg *models.Config) map[string]interface{} {
	out := map[string]interface{}{
		"enabled":                        cfg.LLM.Enabled,
		"provider":                       cfg.LLM.Provider,
		"model":                          cfg.LLM.Model,
		"autonomy_level":                 cfg.Tools.Autonomy,
		"allow_write":                    cfg.Tools.AllowWrite,
		"allow_destructive":              cfg.Tools.AllowDestructive,
		"autonomy_credit":                cfg.Tools.AutonomyCredit,
		"autonomy_env":                   ConfiguredAutonomyEnv(),
		"max_tool_calls_per_session":     cfg.Tools.MaxCallsPerSession,
		"max_write_actions_per_session":  cfg.Tools.MaxWriteActionsPerSession,
		"max_affected_hosts_per_session": cfg.Tools.MaxAffectedHostsPerSession,
		"require_approval_for":           "所有写动作（除非命中 auto-approve 白名单且生效自治级别 ≥ L2）",
	}
	if rt := DefaultAgentRuntime(); rt != nil {
		out["readonly_tools"] = len(rt.ToolNames())
		out["write_tools"] = rt.WritesEnabled()
		pol := rt.Policy()
		out["autonomy_level"] = pol.Autonomy.String()
		out["auto_approve"] = append([]string(nil), pol.AutoApprove...)
	} else {
		out["readonly_tools"] = 0
		out["write_tools"] = false
	}
	if strings.TrimSpace(cfg.Tools.Autonomy) == "" {
		out["autonomy_level"] = tools.AutonomyL1.String()
	}
	return out
}
