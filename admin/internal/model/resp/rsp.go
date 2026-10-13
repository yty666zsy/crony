package resp

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

type (
	PageResult struct {
		List     interface{} `json:"list"`
		Total    int64       `json:"total"`
		Page     int         `json:"page"`
		PageSize int         `json:"page_size"`
	}
	Response struct {
		Code int         `json:"code"`
		Data interface{} `json:"data"`
		Msg  string      `json:"msg"`
	}
)

const (
	SUCCESS = 200
	ERROR   = 1000

	ErrorRequestParameter = 1001
	ErrorJobFormat        = 1002
	ErrorTokenGenerate    = 1003
	ErrorUserNameExist    = 1004
	// ErrorForbidden 已登录但权限不足（阶段 0 新增 RBAC 后的返回码）
	ErrorForbidden = 1005
	// ErrorRegisterClosed 注册已关闭（非首个管理员引导场景必须由管理员操作）
	ErrorRegisterClosed = 1006

	// 阶段 2（Agent 内核）业务码。约定：HTTP 恒为 200，业务结果看 code。
	// ErrorAgentNotConfigured 本节点/本实例未配置模型（llm 段缺失或 provider 未知）。
	ErrorAgentNotConfigured = 1007
	// ErrorAgentSessionNotFound 会话不存在（session_id 无效或已被清理）。
	ErrorAgentSessionNotFound = 1008
	// ErrorAgentDraftInvalid 草案本地复验不通过（cron 非法、命令为空、数值越界等）。
	// msg 必须给出"哪个字段为什么不过"，不能只说校验失败。
	ErrorAgentDraftInvalid = 1009
	// ErrorAgentRunNotFound 指定的运行记录不存在。
	ErrorAgentRunNotFound = 1010
	// ErrorAgentDisabled Agent 能力被配置显式关闭（llm.enabled=false）。
	ErrorAgentDisabled = 1011
	// ErrorAgentUnavailable 目标节点不存在/未注册，无法在其上执行或取节点侧上下文。
	ErrorAgentUnavailable = 1012

	// 阶段 4.2（写动作审批）业务码。1013 起，按"哪一种拒绝"分类——
	// 写动作的失败必须可解释：只说"失败"会让运维只能猜是权限、过期还是并发。
	// ErrorAgentApprovalNotFound 审批单不存在（单号无效或已被清理）。
	ErrorAgentApprovalNotFound = 1013
	// ErrorAgentApprovalConflict 审批单状态已变更：已被别人决策/已执行/不可再撤。
	ErrorAgentApprovalConflict = 1014
	// ErrorAgentApprovalExpired 审批单已过期（写动作的合理性依赖当时的现场）。
	ErrorAgentApprovalExpired = 1015
	// ErrorAgentApprovalMismatch 参数与审批单不一致（批准 A 执行 B 的 TOCTOU 尝试）。
	ErrorAgentApprovalMismatch = 1016
	// ErrorAgentApprovalBlocked 执行前复核未通过：工具已下线/策略已收紧/执行器未装配。
	ErrorAgentApprovalBlocked = 1017
	// ErrorAgentApprovalExecFailed 已执行但失败（含业务失败 Result.OK=false）。
	ErrorAgentApprovalExecFailed = 1018

	// 阶段 4-A（Agent 记忆/经验库）业务码。
	//
	// 为什么"经验条目不存在"需要单独的码而不是复用 1010：1010 是"运行记录不存在"
	// （诊断入口），而经验条目是另一类对象；前端据此决定"是回列表还是提示单号失效"。
	// ErrorAgentMemoryNotFound 经验条目不存在（单号无效或已被清理）。
	ErrorAgentMemoryNotFound = 1019
	// ErrorAgentMemoryConflict 经验条目状态已变更（重复停用/启用，CAS 未生效）。
	ErrorAgentMemoryConflict = 1020

	// 阶段 4-B（多步骤编排）业务码。
	//
	// 为什么"编排非法"要单独一个码：它是**定义错误**（改定义可恢复），而不是
	// 参数格式错误（1001）或任务不存在（1002 附近的老码），前端据此在编排编辑器里
	// 标红具体步骤，而不是弹一句"参数错误"。
	// ErrorJobPlanInvalid 编排定义非法（含成环/依赖不存在/JSON 结构错误等，消息里带定位）。
	ErrorJobPlanInvalid = 1021
	// ErrorJobPlanNotFound 该任务没有编排定义（steps_json 为空）。
	ErrorJobPlanNotFound = 1022

	// 阶段 4-C（自愈调度）业务码。
	//
	// 为什么"自愈记录不存在"要单独一个码：控制台是从"这次失败执行"跳进自愈详情的，
	// 单号失效与"没有自愈记录"是两件事（后者在任务未开启自愈时很常见），
	// 前端据此显示"该次失败没有自愈记录"而不是报错。
	// ErrorHealNotFound 自愈记录不存在。
	ErrorHealNotFound = 1023
	// ErrorHealDenied 自愈被策略拒绝（消息里带命中的闸门规则与原因）。
	ErrorHealDenied = 1024
	// ErrorHealInvalid 自愈请求非法（例如指定的执行记录不存在、没有可处置的失败）。
	ErrorHealInvalid = 1025

	// 阶段 5 补全（放量档位）业务码。
	//
	// 为什么"放量被拒"要单独一个码而不是复用 1005/1000：它**不是权限问题**，
	// 也不是服务端故障，而是"证据不成立"——前端据此把逐条原因渲染成检查清单
	// （缺评测 / 评测判红 / SLO 违约 / 证据过期），而不是弹一句"操作失败"。
	// ErrorRolloutDenied 放量被拒（消息里带逐条原因）。
	ErrorRolloutDenied = 1026
)

func Result(code int, data interface{}, msg string, c *gin.Context) {
	c.JSON(http.StatusOK, Response{
		Code: code,
		Data: data,
		Msg:  msg,
	})
}

func Ok(c *gin.Context) {
	Result(SUCCESS, map[string]interface{}{}, "operation success", c)
}

func OkWithMessage(message string, c *gin.Context) {
	Result(SUCCESS, map[string]interface{}{}, message, c)
}

func OkWithData(data interface{}, c *gin.Context) {
	Result(SUCCESS, data, "operation success", c)
}

func OkWithDetailed(data interface{}, message string, c *gin.Context) {
	Result(SUCCESS, data, message, c)
}

func FailWithMessage(code int, message string, c *gin.Context) {
	Result(code, map[string]interface{}{}, message, c)
}
func FailWithCode(code int, c *gin.Context) {
	Result(code, map[string]interface{}{}, "operation failed", c)
}
func FailWithDetailed(code int, data interface{}, message string, c *gin.Context) {
	Result(code, data, message, c)
}
