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
	err = service.DefaultNodeWatcher.Watch()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("resolver  error:%#v", err))
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
	err = srv.ListenAndServe()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("startup api server error:%v", err.Error()))
		close(closeChan)
		os.Exit(1)
	}
	os.Exit(0)
}
