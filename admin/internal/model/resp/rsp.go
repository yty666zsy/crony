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
