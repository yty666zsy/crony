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
	SuccessRateToday float64 `json:"success_rate_today"`
	FailedToday      int64   `json:"failed_today"`
	TimeoutToday     int64   `json:"timeout_today"`
	SkippedToday     int64   `json:"skipped_today"`
	AvgDurationMs    int64   `json:"avg_duration_ms"`
	KBDocs           int64   `json:"kb_docs"`
	PendingApprovals int64   `json:"pending_approvals"`
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
}

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
	if k.RunsToday > 0 {
		k.SuccessRateToday = round1(float64(mustCount(models.CronyJobRunTableName,
			"finished_at >= ? and status = ?", since, int(models.RunStatusSuccess))) * 100 / float64(k.RunsToday))
	}
	k.AvgDurationMs = avgDurationSince(since)

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
	snap.Auth = map[string]interface{}{
		"token_expire_hours":         cfg.Auth.TokenExpireHours,
		"allow_public_register":      cfg.Auth.AllowPublicRegister,
		"password_min_length":        crypt.MinPasswordLength,
		"rbac":                       "已启用：JWTAuth + RequireAdmin",
		"bootstrap_admin_configured": strings.TrimSpace(cfg.Auth.BootstrapAdminUser) != "",
		"jwt_secret_configured":      strings.TrimSpace(cfg.Auth.JwtSecret) != "",
	}
	snap.Scheduler = map[string]interface{}{
		"default_concurrency_policy": "skip",
		"max_retry_times":            models.MaxRetryTimes,
		"default_allocation":         "auto",
		"self_heal_enabled":          cfg.SelfHeal.Enabled,
		"slo_breach_action_enabled":  cfg.Slo.BreachActionEnabled,
	}
	snap.Notify = map[string]interface{}{
		"channels": notifyChannels(cfg),
	}
	snap.Agent = s.agentSnapshot(cfg)
	return snap
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
