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
	)
}
