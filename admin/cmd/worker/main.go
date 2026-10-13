// Command cronyworker 是阶段 4-D 的「独立 Worker」。
//
// 为什么要把它拆出来：admin 是**控制面**（HTTP API + 交互式诊断），它不该背着
// "周期扫描失败执行、评估 SLO"这类常驻循环；把这些循环放进独立进程后：
//   - admin 更接近无状态，可多副本扩容而不互相抢跑（配合 self-heal 的幂等与并发闸门）；
//   - 循环进程可以单独扩容、单独重启、单独部署到离数据库更近的机器；
//   - 长任务（将来的重推理 Agent 会话）迁移到 worker 时，不影响控制面可用性。
//
// 语义边界（诚实说明）：
//   - worker **不做任何写动作**，只做"扫描 → 分类 → 决策 → 闸门 → （放行的白名单动作）"，
//     放行动作仍受 self-heal 的三道闸门约束，与 admin 内嵌循环**完全一致**；
//   - worker 与 admin 内嵌循环**可同时运行**：self-heal 靠 `alreadyHandled` 幂等 +
//     `max-concurrent` 兜住重复处置（跨进程并发仍未强锁，见 PHASE7 §11）。
//     生产部署建议二选一：要么 admin 内嵌（默认），要么用 worker 并把 admin 的自愈循环关掉。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/notify"
)

const (
	serverName      = "admin" // 复用 admin 的配置目录 admin/conf/<env>/main.json
	defaultInterval = 60 * time.Second
)

func main() {
	env := flag.String("e", envOr("CRONY_ENV", "testing"), "运行环境（testing/production）")
	flag.Parse()

	cfg, err := config.LoadConfig(*env, serverName, "main")
	if err != nil || cfg == nil {
		fmt.Fprintf(os.Stderr, "cronyworker: 加载配置失败: %v\n", err)
		os.Exit(1)
	}
	// 与 api.go 同一套初始化顺序：logger → notify → mysql。
	logger.Init(serverName, cfg.Log.Level, cfg.Log.Format, cfg.Log.Prefix, cfg.Log.Director,
		cfg.Log.ShowLine, cfg.Log.EncodeLevel, cfg.Log.StacktraceKey, cfg.Log.LogInConsole)
	notify.Init(&notify.Mail{
		Port: cfg.Email.Port, From: cfg.Email.From, Host: cfg.Email.Host,
		Secret: cfg.Email.Secret, Nickname: cfg.Email.Nickname,
	}, &notify.WebHook{Url: cfg.WebHook.Url, Kind: cfg.WebHook.Kind})

	if _, err := dbclient.Init(cfg.Mysql.Dsn(), cfg.Mysql.LogMode, cfg.Mysql.MaxIdleConns, cfg.Mysql.MaxOpenConns); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("cronyworker: 初始化 MySQL 失败: %v", err))
		os.Exit(1)
	}
	if err := service.RegisterTables(dbclient.GetMysqlDB()); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("cronyworker: 建表失败（继续，表多半已存在）: %v", err))
	}

	// 自愈循环（复用与 admin 完全相同的服务装配与判定）。
	var selfHealClose chan struct{}
	service.InitSelfHeal(&selfHealClose)

	// 阶段 5 补全（SLO 违约处置）：装配违约处置服务。
	// 是否**自动动手**由 `slo.breach-action-enabled` 决定（默认 false：
	// 周期循环只评估 + 打日志，与接线前完全一致）。
	service.InitSloBreach()

	// SLO 评估循环：评估所有启用契约；违约时按配置决定是否执行 breach_action。
	sloClose := runSloLoop(time.Duration(cfg.SelfHeal.IntervalSeconds) * time.Second)

	// 阶段 5 补全（放量自动降级）：与自愈 / SLO 循环同进程常驻。
	// 默认关闭（tools.rollout-auto-eval=false）；开启后也只收回已放量的环境。
	var rolloutClose chan struct{}
	service.InitRolloutAutoEval(&rolloutClose)

	logger.GetLogger().Info(fmt.Sprintf("[cronyworker] 已启动：env=%s mysql=%s:%s/%s",
		*env, cfg.Mysql.Path, cfg.Mysql.Port, cfg.Mysql.Dbname))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	if selfHealClose != nil {
		close(selfHealClose)
	}
	if sloClose != nil {
		close(sloClose)
	}
	if rolloutClose != nil {
		close(rolloutClose)
	}
	logger.GetLogger().Info("[cronyworker] 已停止")
}

// runSloLoop 周期评估所有启用契约。
//
// 违约时：默认（`slo.breach-action-enabled=false`）只打日志，与接线前完全一致；
// 打开后按契约的 breach_action 执行（通知/诊断），并按冷却退避。
func runSloLoop(interval time.Duration) chan struct{} {
	if interval <= 0 {
		interval = defaultInterval
	}
	t := time.NewTicker(interval)
	closed := make(chan struct{})
	go func() {
		for {
			select {
			case <-t.C:
				evals, err := service.DefaultSloService.EvaluateSlo(0)
				if err != nil {
					logger.GetLogger().Error(fmt.Sprintf("[cronyworker] SLO 评估失败: %v", err))
					continue
				}
				breached := 0
				for _, e := range evals {
					if e.Report.Breached {
						breached++
						logger.GetLogger().Warn(fmt.Sprintf("[cronyworker] SLO 违约：契约 %q（任务 %d %s）原因=%v",
							e.Policy.Name, e.Policy.JobID, e.JobName, e.Report.Reasons))
					}
				}
				if breached == 0 {
					continue
				}
				svc := service.DefaultSloBreachService()
				if svc == nil || !svc.AutoEnabled() {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), interval)
				rep := svc.HandleBreaches(ctx, evals, service.SloBreachOptions{
					Allowed:    true,
					ExecutedBy: "auto",
				})
				cancel()
				if rep.Breached > 0 {
					logger.GetLogger().Info(fmt.Sprintf(
						"[cronyworker] SLO 违约处置：违约=%d 已动作=%d 仅观测=%d 跳过=%d 失败=%d 错误=%v",
						rep.Breached, rep.Acted, rep.Observed, rep.Skipped, rep.Failed, rep.Errors))
				}
			case <-closed:
				t.Stop()
				return
			}
		}
	}()
	return closed
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
