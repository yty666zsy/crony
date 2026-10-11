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
