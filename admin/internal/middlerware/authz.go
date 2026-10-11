package middlerware

import (
	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/common/models"
)

// RequireRole 角色校验中间件。
//
// 必须挂在 JWTAuth() 之后：JWTAuth 负责解析令牌并把 claims 写入 gin.Context，
// RequireRole 只做授权判断，不重复解析令牌。
//
// 与本项目既有响应约定保持一致：HTTP 状态码固定 200，用业务 code 表达结果
// （未登录 1000 + reload 标记，权限不足 1005），避免破坏既有前端拦截器。
func RequireRole(roles ...int) gin.HandlerFunc {
	allowed := make(map[int]struct{}, len(roles))
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	return func(c *gin.Context) {
		claims := GetUserInfo(c)
		if claims == nil {
			resp.FailWithDetailed(resp.ERROR, gin.H{"reload": true}, "未登录或非法访问", c)
			c.Abort()
			return
		}
		if _, ok := allowed[claims.Role]; !ok {
			resp.FailWithMessage(resp.ErrorForbidden, "权限不足：该操作需要管理员角色", c)
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireAdmin 仅允许管理员（models.RoleAdmin）。
func RequireAdmin() gin.HandlerFunc {
	return RequireRole(models.RoleAdmin)
}

// IsAdmin 判断当前请求的登录用户是否为管理员。
// 用于"是否允许其执行管理动作"的分支判断（例如注册接口）。
func IsAdmin(c *gin.Context) bool {
	return CurrentRole(c) == models.RoleAdmin
}

// CurrentRole 返回当前登录用户角色，未登录返回 0。
func CurrentRole(c *gin.Context) int {
	if claims := GetUserInfo(c); claims != nil {
		return claims.Role
	}
	return 0
}

// CurrentUserID 返回当前登录用户 ID，未登录返回 0。
//
// 注意：CustomClaims 同时内嵌 BaseClaims 与 jwt.RegisteredClaims，
// 两者都有 ID 字段（int 用户 ID / string 的 jti），因此必须写 claims.BaseClaims.ID。
func CurrentUserID(c *gin.Context) int {
	if claims := GetUserInfo(c); claims != nil {
		return claims.BaseClaims.ID
	}
	return 0
}

// CurrentUserName 返回当前登录用户名，未登录返回空字符串。
func CurrentUserName(c *gin.Context) string {
	if claims := GetUserInfo(c); claims != nil {
		return claims.BaseClaims.UserName
	}
	return ""
}
