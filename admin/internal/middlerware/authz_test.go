package middlerware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/common/models"
)

// newAuthzRouter 构造一个只用于测试的引擎：先注入 claims（模拟 JWTAuth 的结果），
// 再挂 RequireAdmin()。claims 为 nil 时模拟"未登录/令牌无效"。
func newAuthzRouter(claims *CustomClaims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/admin")
	g.Use(func(c *gin.Context) {
		if claims != nil {
			c.Set("claims", claims)
		}
		c.Next()
	}, RequireAdmin())
	g.GET("", func(c *gin.Context) { resp.OkWithMessage("ok", c) })
	return r
}

func doRequest(r *gin.Engine) (int, int) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin", nil)
	r.ServeHTTP(w, req)
	var body resp.Response
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		return w.Code, -1
	}
	return w.Code, body.Code
}

func TestRequireAdminAllowsAdmin(t *testing.T) {
	r := newAuthzRouter(&CustomClaims{BaseClaims: BaseClaims{ID: 1, UserName: "root", Role: models.RoleAdmin}})
	status, code := doRequest(r)
	if status != http.StatusOK || code != resp.SUCCESS {
		t.Fatalf("admin request = (status %d, code %d), want (%d, %d)", status, code, http.StatusOK, resp.SUCCESS)
	}
}

func TestRequireAdminRejectsNormalUser(t *testing.T) {
	r := newAuthzRouter(&CustomClaims{BaseClaims: BaseClaims{ID: 2, UserName: "operator", Role: models.RoleNormal}})
	status, code := doRequest(r)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (业务 code 表达错误，保持既有响应约定)", status)
	}
	if code != resp.ErrorForbidden {
		t.Fatalf("code = %d, want %d (禁止非管理员)", code, resp.ErrorForbidden)
	}
}

func TestRequireAdminRejectsMissingClaims(t *testing.T) {
	r := newAuthzRouter(nil)
	status, code := doRequest(r)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if code != resp.ERROR {
		t.Fatalf("code = %d, want %d (未登录)", code, resp.ERROR)
	}
}
