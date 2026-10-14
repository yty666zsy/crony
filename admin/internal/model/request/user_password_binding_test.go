package request

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gin-gonic/gin/binding"
)

// 口令字段的绑定口径：binding 只保证"有值且不超 bcrypt 上限"，强度由 `crypt` 一处判定。
//
// 旧写法 `min=4,max=20,alphanum` 会在接口层另立一套策略，直接制造"能建出来、却用不了"的账号：
//
//   - 引导管理员口令走 `crypt.HashPassword`（允许符号），可 `change-me-strong-password`
//     这种口令登录时会被判成参数错误（code=1001），排查方向被彻底带偏；
//   - 21 位以上的合规口令同样过不了 `max=20`。
//
// 这组用例钉的就是这个回归：**登录/改密不该施加注册策略**。
func TestPasswordBindingsDoNotImposePolicy(t *testing.T) {
	// bindJSON 走的是与 handler 里 c.ShouldBindJSON 完全相同的绑定路径（gin 的 binding.JSON），
	// 所以这里测到的拒绝/放行，就是线上接口的拒绝/放行。
	bindJSON := func(t *testing.T, body any, out any) error {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("构造请求体失败: %v", err)
		}
		return binding.JSON.BindBody(raw, out)
	}

	cases := []struct {
		name     string
		password string
		wantOK   bool
	}{
		{"含符号的引导口令（旧写法会 1001）", "change-me-strong-password", true},
		{"超过 20 位（旧写法会 1001）", "CronyAdminPassword2026", true},
		{"含空格与中文的口令", "运维 口令 2026", true},
		{"单字符口令：策略问题交给 crypt，不该由 binding 判", "x", true},
		{"空口令必须拒绝", "", false},
		{"超 bcrypt 72 字节上限必须拒绝", strings.Repeat("a", 73), false},
	}
	for _, c := range cases {
		var loginReq ReqUserLogin
		err := bindJSON(t, map[string]any{"username": "admin", "password": c.password}, &loginReq)
		if c.wantOK && err != nil {
			t.Fatalf("[登录] %s：应当放行，却被 binding 拒绝（%v）", c.name, err)
		}
		if !c.wantOK && err == nil {
			t.Fatalf("[登录] %s：应当拒绝，却被放行", c.name)
		}

		var regReq ReqUserRegister
		err = bindJSON(t, map[string]any{"username": "ops", "password": c.password}, &regReq)
		if c.wantOK && err != nil {
			t.Fatalf("[注册] %s：应当放行（长度下限与弱口令由 crypt.ValidatePasswordStrength 判定），却被 binding 拒绝（%v）", c.name, err)
		}
		if !c.wantOK && err == nil {
			t.Fatalf("[注册] %s：应当拒绝，却被放行", c.name)
		}
	}

	// 改密：旧口令可能是任意字符集（历史账号），新口令的强度由 service 侧 HashPassword 判定。
	var changeReq ReqChangePassword
	if err := bindJSON(t, map[string]any{
		"password":     "change-me-strong-password",
		"new_password": "CronyAdmin2026",
	}, &changeReq); err != nil {
		t.Fatalf("[改密] 含符号的旧口令应当放行，否则历史账号永远改不了口令：%v", err)
	}
	var emptyNew ReqChangePassword
	if err := bindJSON(t, map[string]any{
		"password":     "CronyAdmin2026",
		"new_password": "",
	}, &emptyNew); err == nil {
		t.Fatal("[改密] 空的新口令必须拒绝")
	}
}
