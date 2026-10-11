package service

import (
	"github.com/tmnhs/crony/common/models"
	"gorm.io/gorm"
)

func RegisterTables(db *gorm.DB) {
	// 阶段 1（任务模型 2.0）新增 job_run / job_run_step：
	// 节点侧也要注册（节点可能先于 admin 启动），AutoMigrate 幂等。
	_ = db.AutoMigrate(
		models.User{},
		models.Node{},
		models.Job{},
		models.JobLog{},
		models.JobRun{},
		models.JobRunStep{},
		// 阶段 2（Agent 内核）：节点侧执行 Agent 型任务时也要落会话与轨迹，
		// 否则"节点侧跑了什么"在控制台上看不到。
		models.AgentSession{},
		models.AgentStep{},
		// 阶段 3（观测闭环）：node 自己上报的资源水位。
		// 节点侧必须注册：节点往往是先启动的一方，且这张表由**节点**写入。
		models.NodeMetric{},
		// 阶段 3（观测闭环）：admin→node 的只读探测请求表。
		// 节点侧必须注册：节点先启动时这张表还不存在，探测通道会整条不可用。
		models.NodeProbeRequest{},
	)
}
