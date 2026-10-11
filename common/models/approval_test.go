package models

import (
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// 这一组用例守的是"并发点批准时，输的那个不该看到死锁"这条体验/正确性底线。
//
// 背景：真库集成测试实测到 4 个并发批准会有约 1/4 概率让一条 CAS UPDATE 报
// Error 1213 Deadlock。我们在 CAS 上加了一次重试（见 casUpdate 的注释），
// 前提就是"能识别出这是锁竞争而不是真的写失败"。
func TestIsRetryableLockErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"普通错误", errors.New("connection refused"), false},
		{"业务错误", errors.New("审批单状态已变更，本次决策未生效"), false},
		{"死锁 1213（驱动类型）", &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"}, true},
		{"锁等待超时 1205（驱动类型）", &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}, true},
		{"重复键 1062 不重试", &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}, false},
		{"死锁（被包装成字符串）", errors.New("Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction"), true},
		{"锁等待（字符串）", errors.New("Error 1205: Lock wait timeout exceeded; try restarting transaction"), true},
	}
	for _, c := range cases {
		if got := isRetryableLockErr(c.err); got != c.want {
			t.Errorf("%s: isRetryableLockErr = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// casUpdate 的重试必须是"只在锁竞争时重试、且次数有上限"：
// 否则要么把一个确定的失败重试成 30 秒卡死，要么把死锁直接抛给运维。
func TestCasUpdateRetriesOnlyOnLockContention(t *testing.T) {
	// 1) 锁竞争：重试到上限后仍然返回最后一次的错误（不吞错）。
	attempts := 0
	lockErr := &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}
	res := casUpdate(3, func() *gorm.DB {
		attempts++
		return &gorm.DB{Error: lockErr}
	})
	if attempts != 3 {
		t.Fatalf("锁竞争应重试到上限 3 次，实际 %d 次", attempts)
	}
	if res.Error == nil {
		t.Fatal("重试耗尽后必须把错误交回调用方，不能静默当作成功")
	}

	// 2) 非锁竞争：只执行一次，错误原样返回。
	attempts2 := 0
	plain := errors.New("表不存在")
	res2 := casUpdate(3, func() *gorm.DB {
		attempts2++
		return &gorm.DB{Error: plain}
	})
	if attempts2 != 1 {
		t.Fatalf("非锁竞争不得重试，实际 %d 次", attempts2)
	}
	if !errors.Is(res2.Error, plain) {
		t.Fatalf("错误必须原样返回，实际 %v", res2.Error)
	}

	// 3) 成功一次就够：不得因为"看着不放心"而重复执行 CAS（重复执行会多一次行锁竞争）。
	attempts3 := 0
	res3 := casUpdate(3, func() *gorm.DB {
		attempts3++
		return &gorm.DB{Error: nil, RowsAffected: 1}
	})
	if attempts3 != 1 || res3.RowsAffected != 1 {
		t.Fatalf("成功路径必须只执行一次，实际 %d 次 RowsAffected=%d", attempts3, res3.RowsAffected)
	}
}
