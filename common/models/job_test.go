package models

import "testing"

// TestJobCheckDoesNotRewriteRetryTimes 回归测试：
// 旧实现会在 RetryInterval == 0 时把 RetryTimes 强制改成 1，
// 用户选择"不重试"会被静默改写。Check() 现在只做合法化，不改变重试语义。
func TestJobCheckDoesNotRewriteRetryTimes(t *testing.T) {
	job := &Job{Name: "no-retry", Command: "echo ok", Type: JobTypeCmd, RetryTimes: 0, RetryInterval: 0}
	if err := job.Check(); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if job.RetryTimes != 0 {
		t.Fatalf("RetryTimes = %d, want 0（不得把「不重试」改写为「重试 1 次」）", job.RetryTimes)
	}
	if job.RetryInterval != 0 {
		t.Fatalf("RetryInterval = %d, want 0", job.RetryInterval)
	}
}

func TestJobCheckClampsRetryValues(t *testing.T) {
	cases := []struct {
		name         string
		retryTimes   int
		retrySeconds int64
		wantTimes    int
		wantSeconds  int64
	}{
		{"negative times", -5, 10, 0, 10},
		{"negative interval", 3, -1, 3, 0},
		{"over limit", MaxRetryTimes + 50, 5, MaxRetryTimes, 5},
		{"passthrough", 2, 30, 2, 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &Job{Name: "j", Command: "echo ok", Type: JobTypeCmd, RetryTimes: tc.retryTimes, RetryInterval: tc.retrySeconds}
			if err := job.Check(); err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if job.RetryTimes != tc.wantTimes || job.RetryInterval != tc.wantSeconds {
				t.Fatalf("Check() = (times %d, interval %d), want (%d, %d)",
					job.RetryTimes, job.RetryInterval, tc.wantTimes, tc.wantSeconds)
			}
		})
	}
}

func TestJobCheckRejectsEmptyFields(t *testing.T) {
	if err := (&Job{Name: "  ", Command: "echo"}).Check(); err == nil {
		t.Fatal("empty job name must be rejected")
	}
	if err := (&Job{Name: "j", Command: "  "}).Check(); err == nil {
		t.Fatal("empty job command must be rejected")
	}
}

// TestJobSplitCmd 保障命令解析行为在改造中不被破坏。
func TestJobSplitCmd(t *testing.T) {
	job := &Job{Name: "j", Command: `echo "hello world"`, Type: JobTypeCmd}
	if err := job.Check(); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(job.Cmd) != 2 || job.Cmd[0] != "echo" || job.Cmd[1] != "hello world" {
		t.Fatalf("SplitCmd() = %#v, want [echo, hello world]", job.Cmd)
	}
}
