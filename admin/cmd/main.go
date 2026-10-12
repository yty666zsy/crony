package main

import (
	"fmt"
	"github.com/tmnhs/crony/admin/internal/handler"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/notify"
	"github.com/tmnhs/crony/common/pkg/server"
	"os"
	"time"
)

const (
	ServerName = "admin"
)

func main() {
	srv, err := server.NewApiServer(ServerName)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("new api server error:%s", err.Error()))
		os.Exit(1)
	}
	// Register the API routing service
	srv.RegisterRouters(handler.RegisterRouters)
	service.DefaultNodeWatcher = service.NewNodeWatcherService()
	if err = service.DefaultNodeWatcher.Watch(); err != nil {
		// 降级而非崩溃：节点列表/调度协同不可用，控制面 HTTP API 继续服务
		// （Agent 诊断、执行记录查询与订阅都不依赖 etcd）。
		// 依赖 etcd 的接口会返回明确错误（errors.ErrEtcdNotInit），不会静默失败。
		logger.GetLogger().Error(fmt.Sprintf("node watcher 未启动，进入降级运行（节点/调度协同不可用）:%v", err))
	}
	//init db table
	err = service.RegisterTables(dbclient.GetMysqlDB())
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("init db table error:%#v", err))
	}
	// 阶段 2（Agent 内核）：装配 Agent 运行时。
	// 未配置 llm 时只告警、不影响启动（相关接口返回 1007/1011），
	// 这样"没配模型"的部署仍是一个可用的传统运维平台。
	if _, err := service.InitAgentRuntime(); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("init agent runtime error:%#v", err))
	}
	// Notify operation
	go notify.Serve()
	// log cleaner
	var closeChan chan struct{}
	period := config.GetConfigModels().System.LogCleanPeriod
	if period > 0 {
		closeChan = service.RunLogCleaner(time.Duration(period)*time.Minute, config.GetConfigModels().System.LogCleanExpiration)
	}
	// 阶段 4-C（自愈调度）：周期扫描失败执行 → 分类 → 白名单动作 → 闸门 → 观测 → 沉淀。
	// 循环无条件启动（enabled=false 时只分类与留痕，不动手），
	// 这样"打开自愈到底会发生什么"可以先用真实数据预演，而不是靠承诺。
	var selfHealClose chan struct{}
	service.InitSelfHeal(&selfHealClose)
	// 阶段 5 补全（SLO 违约处置）：装配服务（周期评估在 worker；admin 侧提供
	// `/ops/slo/act` 人工触发）。默认关闭动作 = 与接线前一致，零回归。
	service.InitSloBreach()
	err = srv.ListenAndServe()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("startup api server error:%v", err.Error()))
		if closeChan != nil {
			close(closeChan)
		}
		if selfHealClose != nil {
			close(selfHealClose)
		}
		os.Exit(1)
	}
	os.Exit(0)
}
