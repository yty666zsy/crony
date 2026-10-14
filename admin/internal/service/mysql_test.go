package service

import (
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/pkg/errors"
)

// TestIsConcurrentDDLConflict 覆盖「admin 与 node 并发建表」这一竞态的错误识别。
//
// 背景（2026-10-14 本机 docker compose 实测 3/3 命中）：同一条 `docker compose up`
// 同时拉起 admin 与 node，两边都跑 AutoMigrate，慢的一方拿到 MySQL 1050。
// 如果这种错误被当成"真正的建表失败"直接返回，RegisterTables 会提前 return，
// **初始管理员引导被整段跳过**，登录时只会看到"用户名或密码不正确"。
// 所以这里必须钉死：哪些错误该重试，哪些必须原样抛出去。
func TestIsConcurrentDDLConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"1050 表已存在（并发抢建）", &mysql.MySQLError{Number: 1050, Message: "Table 'node' already exists"}, true},
		{"1051 表不存在（并发 DROP）", &mysql.MySQLError{Number: 1051, Message: "Unknown table 'crony.job'"}, true},
		{"1061 索引名重复", &mysql.MySQLError{Number: 1061, Message: "Duplicate key name 'idx_run_id'"}, true},
		{"被 pkg/errors 包装后仍可识别", errors.Wrap(&mysql.MySQLError{Number: 1050, Message: "Table 'node' already exists"}, "register table failed"), true},
		{"1146 表不存在（真的没建起来，必须抛出去）", &mysql.MySQLError{Number: 1146, Message: "Table 'crony.job' doesn't exist"}, false},
		{"1045 权限不足（必须抛出去）", &mysql.MySQLError{Number: 1045, Message: "Access denied for user 'crony'@'%'"}, false},
		{"认得类型但不是 DDL 冲突（必须抛出去）", &mysql.MySQLError{Number: 1054, Message: "Unknown column 'xx' in 'field list'"}, false},
		{"类型丢失时按关键字兜底（1050 文案）", fmt.Errorf("register table failed: Error 1050: Table 'node' already exists"), true},
		{"类型丢失时按关键字兜底（DUPLICATE KEY NAME）", errors.New("Error 1061: Duplicate key name 'idx_run_id'"), true},
		{"类型丢失时按关键字兜底：连接被拒不算冲突", fmt.Errorf("dial tcp 127.0.0.1:3306: connect: connection refused"), false},
		{"nil 不算冲突", nil, false},
	}
	for _, c := range cases {
		if got := isConcurrentDDLConflict(c.err); got != c.want {
			t.Errorf("%s：isConcurrentDDLConflict=%v，期望 %v（err=%v）", c.name, got, c.want, c.err)
		}
	}
}
