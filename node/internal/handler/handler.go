package handler

import (
	"github.com/tmnhs/crony/common/models"
)

// Handler 旧版"单次执行"抽象。
//
// Deprecated: 阶段 1 起执行统一走 executor.Executor + RunCoordinator（见 runner.go）：
// 新路径补齐了输出上限、进程树取消、退出码/状态码分类（可重试 vs 不可重试）、
// 结构化结果与 job_run/job_run_step 落库。本接口仅为兼容旧脚本相关代码保留。
type Handler interface {
	Run(job *Job) (string, error)
}

// CreateHandler 按任务类型返回旧版执行器。
//
// Deprecated: 见 Handler 说明；新代码请使用 executor.DefaultRegistry()。
func CreateHandler(j *Job) Handler {
	var handler Handler = nil
	switch j.Type {
	case models.JobTypeCmd:
		handler = new(CMDHandler)
	case models.JobTypeHttp:
		handler = new(HTTPHandler)
	}
	return handler
}
