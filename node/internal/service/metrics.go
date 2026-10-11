package service

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/nodestatus"
)

// metricIntervalEnv 上报周期的环境变量名。
//
// 为什么用环境变量而不是 conf 里的字段：conf 是 admin/node 各自一份 JSON
// （production/testing 各一套），加一个字段要同步改 4 个文件；而这条参数
// 只有"调试时想调快一点"的用途，环境变量足够，且与既有的
// CRONY_LLM_API_KEY / CRONY_AUTH_BOOTSTRAP_ADMIN_PASSWORD 风格一致。
const metricIntervalEnv = "CRONY_NODE_METRIC_INTERVAL_SECONDS"

// metricInterval 解析上报周期：默认 30s，可覆盖为 5s~3600s。
func metricInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv(metricIntervalEnv))
	if raw == "" {
		return nodestatus.DefaultInterval
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec <= 0 {
		logger.GetLogger().Warn(fmt.Sprintf("[nodestatus] %s=%q 不是正整数，按默认 %s 上报",
			metricIntervalEnv, raw, nodestatus.DefaultInterval))
		return nodestatus.DefaultInterval
	}
	d := time.Duration(sec) * time.Second
	if d < 5*time.Second || d > time.Hour {
		logger.GetLogger().Warn(fmt.Sprintf("[nodestatus] %s=%d 超出 5~3600 秒，按默认 %s 上报",
			metricIntervalEnv, sec, nodestatus.DefaultInterval))
		return nodestatus.DefaultInterval
	}
	return d
}

// StartMetricPublisher 装配并启动资源水位发布器（阻塞，调用方用 go 起）。
//
// 这是阶段 3 的收尾件：admin 侧 `node_status` 工具早就等着 `node_metric` 有行，
// 但此前没有任何节点侧代码往里写，工具只能一直输出"未上报"。
func (srv *NodeServer) StartMetricPublisher(ctx context.Context) {
	pub := nodestatus.NewPublisher(dbclient.GetMysqlDB(), srv.UUID, srv.Hostname, srv.Version).
		SetInterval(metricInterval()).
		// srv.UpTime 是**本进程**启动时刻（NewNodeServer 里写入），
		// 不是 OS uptime：模型关心的是"节点进程是不是刚重启过"。
		SetStartedAt(time.Unix(srv.UpTime, 0)).
		SetRunningJobs(func() int { return countRunningJobs(srv.UUID) }).
		SetLogger(func(format string, args ...any) { logger.GetLogger().Warn(fmt.Sprintf(format, args...)) })
	logger.GetLogger().Info(fmt.Sprintf("[nodestatus] 资源水位上报已启用：node=%s 周期=%s（可用 %s 覆盖）",
		srv.UUID, metricInterval(), metricIntervalEnv))
	pub.Run(ctx)
}

// countRunningJobs 统计本节点当前在跑的任务数。
//
// 读不到时返回 0 并告警：这个值只是线索（admin 侧的 node_status 会自己再算一遍），
// 但 CPU/内存/磁盘水位遇到采集失败是**跳过上报**——两者的区别在于
// "任务数算不出来"不会被误读成"节点空闲"，而"CPU=0"会被。
func countRunningJobs(nodeUUID string) int {
	var n int64
	err := dbclient.GetMysqlDB().Table(models.CronyJobRunTableName).
		Where("node_uuid = ? AND status = ?", nodeUUID, models.RunStatusRunning).
		Count(&n).Error
	if err != nil {
		logger.GetLogger().Warn("[nodestatus] 统计本节点在跑任务数失败（本次按 0 上报）: " + err.Error())
		return 0
	}
	return int(n)
}
