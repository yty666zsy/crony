package handler

import (
	"testing"

	"github.com/tmnhs/crony/common/models"
)

// TestDecideRegisterNeverGrantsAdminAfterBootstrap 是本阶段最关键的一条安全断言：
// 只要系统里已经存在用户，注册接口就永远不能产生新的管理员账号，
// 哪怕调用方是管理员或显式开启了公开注册（管理员身份必须经 /user/update 授予）。
func TestDecideRegisterNeverGrantsAdminAfterBootstrap(t *testing.T) {
	cases := []struct {
		name         string
		userCount    int64
		isAdmin      bool
		allowPublic  bool
		wantAllowed  bool
		wantRole     int
		wantRejected bool
	}{
		{"空库引导注册", 0, false, false, true, models.RoleAdmin, false},
		{"空库即管理员列表", 0, true, false, true, models.RoleAdmin, false},
		{"有用户+管理员", 5, true, false, true, models.RoleNormal, false},
		{"有用户+公开注册", 5, false, true, true, models.RoleNormal, false},
		{"有用户+匿名+关闭注册", 5, false, false, false, 0, true},
		{"有用户+匿名+关闭注册（大量用户）", 9999, false, false, false, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideRegister(tc.userCount, tc.isAdmin, tc.allowPublic)
			if got.Allowed != tc.wantAllowed {
				t.Fatalf("Allowed = %v, want %v (reason=%s)", got.Allowed, tc.wantAllowed, got.Reason)
			}
			if tc.wantRejected && got.Reason == "" {
				t.Fatal("拒绝时必须给出原因，便于前端提示与审计")
			}
			if got.Allowed && got.Role != tc.wantRole {
				t.Fatalf("Role = %d, want %d", got.Role, tc.wantRole)
			}
			if tc.userCount > 0 && got.Allowed && got.Role == models.RoleAdmin {
				t.Fatal("已存在用户时注册接口不得产生管理员账号（提权漏洞回归）")
			}
		})
	}
}
