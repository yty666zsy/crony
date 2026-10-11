package main

import (
	"context"
	"fmt"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/notify"
	"github.com/tmnhs/crony/common/pkg/server"
	"github.com/tmnhs/crony/common/pkg/utils/event"
	"github.com/tmnhs/crony/node/internal/service"
	"os"
)

const ServerName = "node"

func main() {
	if _, err := server.InitNodeServer(ServerName); err != nil {
		fmt.Println("init node server error:", err.Error())
		os.Exit(1)
	}
	nodeServer, err := service.NewNodeServer()
	if err != nil {
		fmt.Println("init node server error:", err.Error())
		os.Exit(1)
	}
	service.RegisterTables(dbclient.GetMysqlDB())
	if err = nodeServer.Register(); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("register node into etcd error:%s", err.Error()))
		os.Exit(1)
	}
	if err = nodeServer.Run(); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("node run error: %s", err.Error()))
		os.Exit(1)
	}
	//notification operation
	go notify.Serve()
	logger.GetLogger().Info(fmt.Sprintf("crony node %s service started, Ctrl+C or send kill sign to exit", nodeServer.String()))
	// 阶段 3（观测闭环）：admin→node 只读探测通道。
	//
	// 为什么在节点侧跑：端口/进程/服务这些事实**只有本机能观测**，admin 查库永远查不到，
	// 于是模型只能猜（实测它就是这么猜的）。这条通道把它变成"可观测 + 可审计"。
	// 边界：白名单只读命令、不用 shell、有超时与输出上限、每次都落库一条审计记录。
	startProbeWorker(nodeServer.UUID)
	// Register the logout event
	event.OnEvent(event.EXIT, nodeServer.Stop)
	// Listen for exit signals
	event.WaitEvent()
	event.EmitEvent(event.EXIT, nil)
	logger.GetLogger().Info("exit success")
}

// startProbeWorker 启动节点侧只读探测执行器。
//
// 失败时只告警**不退出**：探测通道不可用不应让节点无法接任务
// （但也不会静默——日志里必须有明确一行，且 admin 侧探测会拿到超时）。
func startProbeWorker(nodeUUID string) {
	host, _ := os.Hostname()
	worker, err := service.NewProbeWorker(dbclient.GetMysqlDB(), nodeUUID, host)
	if err != nil {
		logger.GetLogger().Warn("[probe] 节点侧只读探测未启用: " + err.Error())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	event.OnEvent(event.EXIT, func(interface{}) { cancel() })
	go worker.Run(ctx)
}
