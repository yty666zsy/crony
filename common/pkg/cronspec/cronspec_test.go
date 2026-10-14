package cronspec

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestCountSegments 段数统计必须区分"描述符"与"普通表达式"。
func TestCountSegments(t *testing.T) {
	cases := []struct {
		spec string
		want int
	}{
		{"", 0},
		{"   ", 0},
		{"@daily", 0},
		{"@every 1h30m", 0},
		{"0 0 3 * * *", 6},
		{"0 3 * * *", 5},
		{"*/5 * * * *", 5},
		{"0 0 3 * * * *", 7},
	}
	for _, tc := range cases {
		if got := Count(tc.spec); got != tc.want {
			t.Errorf("Count(%q) = %d，期望 %d", tc.spec, got, tc.want)
		}
	}
}

// TestValidateRejectsFiveSegments 是本包存在的理由：
// 5 段在底层库里是合法表达式（不报错），但语义与 Unix crontab 不同。
// 写入路径必须拒绝，并且把"真实语义 + 正确写法"说清楚。
func TestValidateRejectsFiveSegments(t *testing.T) {
	err := Validate("0 3 * * *")
	if err == nil {
		t.Fatal("5 段表达式必须被拒绝（否则会静默按每小时运行）")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("应可用 errors.Is(err, ErrInvalid) 判断，实际 %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"5 段", "6 段", "每小时第 3 分钟", Example} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错必须包含 %q，实际：%s", want, msg)
		}
	}
	// 真实语义必须与底层库一致：不是"每天 03:00"，而是每小时第 3 分钟。
	sched, perr := Parse("0 3 * * *")
	if perr != nil {
		t.Fatalf("底层库应能解析 5 段（这正是危险之处）: %v", perr)
	}
	from := time.Date(2026, 3, 2, 10, 30, 0, 0, time.Local)
	next := sched.Next(from)
	if next.Hour() != 11 || next.Minute() != 3 {
		t.Fatalf("底层库语义应把 \"0 3 * * *\" 当作每小时第 3 分钟，实际下一次=%s", next)
	}
}

// TestValidateAcceptsSixSegmentsAndDescriptors 6 段与描述符必须放行，
// 否则会误伤正常任务（升级不能把好配置挡在门外）。
func TestValidateAcceptsSixSegmentsAndDescriptors(t *testing.T) {
	ok := []string{
		"0 0 3 * * *",
		"*/30 * * * * *",
		"0 0 0 1 1 *",
		"@daily",
		"@midnight",
		"@every 1h30m",
	}
	for _, spec := range ok {
		if err := Validate(spec); err != nil {
			t.Errorf("Validate(%q) 应通过，实际 %v", spec, err)
		}
	}
}

// TestValidateRejectsGarbage 不可解析/为空/超长必须报错而不是 panic。
func TestValidateRejectsGarbage(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"not-a-cron",
		"0 0 3 * * * *",                 // 7 段
		strings.Repeat("1 ", 100) + "1", // 过长
		"99 99 99 * * *",                // 越界
	}
	for _, spec := range bad {
		err := Validate(spec)
		if err == nil {
			t.Errorf("Validate(%q) 应被拒绝", spec)
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%q) 的错误应可用 errors.Is(ErrInvalid) 识别，实际 %v", spec, err)
		}
	}
}

// TestParseNeverPanics 空串/垃圾表达式不能打挂进程（安全性底线）。
func TestParseNeverPanics(t *testing.T) {
	for _, spec := range []string{"", " ", "abc", "@", "@nope", "1 2 3"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) 应返回错误", spec)
		}
	}
	// 解析成功时返回的调度器必须给出真实触发时间（语义与调度器一致）。
	sched, err := Parse("0 0 3 * * *")
	if err != nil {
		t.Fatalf("6 段标准表达式应可解析: %v", err)
	}
	next := sched.Next(time.Date(2026, 3, 2, 10, 30, 0, 0, time.Local))
	if next.Hour() != 3 || next.Minute() != 0 || next.Day() != 3 {
		t.Fatalf("\"0 0 3 * * *\" 下次触发应为次日 03:00，实际 %s", next)
	}
}

// TestFiveSegmentsWarning 调度侧的告警：5 段要能"看见"，其它情况不打扰。
func TestFiveSegmentsWarning(t *testing.T) {
	w := FiveSegmentsWarning("0 4 * * *")
	if w == "" {
		t.Fatal("5 段表达式必须产生告警（否则存量错配无人发现）")
	}
	for _, want := range []string{"5 段", "0 4 * * *", "6 段"} {
		if !strings.Contains(w, want) {
			t.Errorf("告警应包含 %q，实际：%s", want, w)
		}
	}
	for _, spec := range []string{"0 0 4 * * *", "@daily", ""} {
		if got := FiveSegmentsWarning(spec); got != "" {
			t.Errorf("FiveSegmentsWarning(%q) 应返回空串，实际 %q", spec, got)
		}
	}
}
