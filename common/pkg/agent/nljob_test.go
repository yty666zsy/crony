package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/runpolicy"
)

// shanghai 返回 Asia/Shanghai；拿不到时跳过相关断言（环境缺少 tzdata 不是代码缺陷）。
func shanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("当前环境没有时区数据: %v", err)
	}
	return loc
}

// validDraft 合规的起草输出。
func validDraft() map[string]interface{} {
	return map[string]interface{}{
		"name":               "清理临时文件",
		"description":        "删除 /tmp 下 7 天前的文件",
		"job_type":           "cmd",
		"spec":               "0 0 2 * * *",
		"command":            []interface{}{"/usr/bin/find", "/tmp", "-mtime", "+7", "-delete"},
		"timeout_seconds":    300,
		"retry_times":        2,
		"retry_interval":     60,
		"concurrency_policy": "skip",
		"rationale":          "用户要求每天凌晨 2 点执行，超时 5 分钟",
		"warnings":           []interface{}{"删除操作不可撤销，建议先 dry-run"},
	}
}

// draftRequest 起草请求。
func draftRequest(loc *time.Location) NLJobRequest {
	return NLJobRequest{
		Text:     "每天凌晨 2 点清理 /tmp 下 7 天前的文件，超时 5 分钟，失败重试 2 次",
		NodeUUID: "node-1",
		Timezone: loc.String(),
		Now:      time.Date(2026, 3, 2, 10, 30, 0, 0, loc),
	}
}

func TestDraftJobHappyPath(t *testing.T) {
	loc := shanghai(t)
	gateway, fp := newGateway()
	fp.WithResponder(func(llm.Request) (llm.Completion, error) { return llm.FakeJSON(validDraft()), nil })

	a := New(gateway, nil, DefaultConfig())
	draft, err := DraftJob(context.Background(), a, draftRequest(loc))
	if err != nil {
		t.Fatalf("应起草成功: %v", err)
	}
	if !draft.RequiresConfirmation || draft.Confirmed {
		t.Fatalf("必须要求人工确认且不得自动确认: %+v", draft)
	}
	if draft.JobType != "cmd" || len(draft.Command) != 5 || !draft.RawArgv {
		t.Fatalf("命令解析不符: %+v", draft)
	}
	if draft.TimeoutSeconds != 300 || draft.RetryTimes != 2 || draft.RetryInterval != 60 {
		t.Fatalf("参数不符: %+v", draft)
	}
	if draft.ConcurrencyPolicy != "skip" || draft.ConcurrencyValue != int(runpolicy.ConcurrencySkip) {
		t.Fatalf("并发策略映射不符: %+v", draft)
	}

	// 接下来 5 次触发必须是**代码算的**（不采信模型自述）
	if len(draft.NextTriggers) != NextTriggerCount || len(draft.NextTriggerText) != NextTriggerCount {
		t.Fatalf("应回显 %d 次触发: %+v", NextTriggerCount, draft.NextTriggerText)
	}
	for _, ts := range draft.NextTriggers {
		if ts.Hour() != 2 || ts.Minute() != 0 || ts.Second() != 0 {
			t.Fatalf("触发时间不符（应每天 02:00）: %s", ts)
		}
		if _, offset := ts.Zone(); offset != 8*3600 {
			t.Fatalf("触发时间应按请求时区计算: %s", ts)
		}
	}
	// 首次触发必须在 now 之后
	if !draft.NextTriggers[0].After(time.Date(2026, 3, 2, 10, 30, 0, 0, loc)) {
		t.Fatalf("首次触发应晚于当前时间: %s", draft.NextTriggers[0])
	}
	if !strings.Contains(draft.TriggerEcho, "接下来 5 次触发") || !strings.Contains(draft.TriggerEcho, "确认前不会写入调度器") {
		t.Fatalf("确认文本不符:\n%s", draft.TriggerEcho)
	}
	if !strings.Contains(draft.Explain(), "需要你注意") || !strings.Contains(draft.Explain(), "dry-run") {
		t.Fatalf("说明应包含模型告警:\n%s", draft.Explain())
	}
	if draft.Trace == nil || draft.Trace.ModelCalls != 1 {
		t.Fatalf("轨迹不符: %+v", draft.Trace)
	}
}

// TestDraftJobWarnsOnFiveFieldSpec 五段表达式的语义陷阱必须显式提示。
//
// 底层库（jakecoffman/cron，兼容 robfig v1）把 5 段解释为「秒 分 时 日 月」，
// 于是 "0 2 * * *" 是"每小时第 2 分钟第 0 秒"，而不是 Unix 的"每天 02:00"。
// 这是把自然语言翻译成 cron 时最容易出错的地方——必须告警而不是默默接受。
func TestDraftJobWarnsOnFiveFieldSpec(t *testing.T) {
	loc := shanghai(t)
	payload := validDraft()
	payload["spec"] = "0 2 * * *"
	gateway, fp := newGateway()
	fp.WithResponder(func(llm.Request) (llm.Completion, error) { return llm.FakeJSON(payload), nil })

	a := New(gateway, nil, DefaultConfig())
	draft, err := DraftJob(context.Background(), a, draftRequest(loc))
	if err != nil {
		t.Fatalf("应起草成功: %v", err)
	}
	joined := strings.Join(draft.Warnings, " | ")
	if !strings.Contains(joined, "5 段") || !strings.Contains(joined, "6 段") {
		t.Fatalf("应提示五段表达式的语义陷阱: %v", draft.Warnings)
	}
	// 观测到的真实语义：每小时的第 2 分钟
	if len(draft.NextTriggers) != NextTriggerCount {
		t.Fatalf("触发次数不符: %+v", draft.NextTriggers)
	}
	for _, ts := range draft.NextTriggers {
		if ts.Minute() != 2 {
			t.Fatalf("按本库语义 5 段 \"0 2 * * *\" 应为每小时第 2 分钟，实际 %s", ts)
		}
	}
	gap := draft.NextTriggers[1].Sub(draft.NextTriggers[0])
	if gap != time.Hour {
		t.Fatalf("相邻间隔应为 1 小时，实际 %s", gap)
	}
}

func TestDraftJobRepairsInvalidCronSpec(t *testing.T) {
	loc := shanghai(t)
	gateway, fp := newGateway()
	calls := 0
	fp.WithResponder(func(req llm.Request) (llm.Completion, error) {
		calls++
		if calls == 1 {
			bad := validDraft()
			bad["spec"] = "99 99 * * *"
			return llm.FakeJSON(bad), nil
		}
		// 第二次请求里必须带上"上次表达式非法"的提示
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "无法解析") {
				return llm.FakeJSON(validDraft()), nil
			}
		}
		bad := validDraft()
		bad["spec"] = "99 99 * * *"
		return llm.FakeJSON(bad), nil
	})

	a := New(gateway, nil, DefaultConfig())
	draft, err := DraftJob(context.Background(), a, draftRequest(loc))
	if err != nil {
		t.Fatalf("修复后应成功: %v", err)
	}
	if draft.Repairs != 1 || draft.Spec != "0 0 2 * * *" {
		t.Fatalf("修复轮不符: repairs=%d spec=%q", draft.Repairs, draft.Spec)
	}
	joined := strings.Join(draft.Warnings, " | ")
	if !strings.Contains(joined, "已自动重试") {
		t.Fatalf("应保留「第一次不可用」的痕迹: %v", draft.Warnings)
	}
	if len(draft.NextTriggers) != NextTriggerCount {
		t.Fatalf("修复后仍必须回显触发时间: %+v", draft.NextTriggers)
	}
}

func TestDraftJobRejectsAfterRepairExhausted(t *testing.T) {
	loc := shanghai(t)
	gateway, fp := newGateway()
	bad := validDraft()
	bad["spec"] = "not-a-cron"
	fp.WithResponder(func(llm.Request) (llm.Completion, error) { return llm.FakeJSON(bad), nil })

	a := New(gateway, nil, DefaultConfig())
	draft, err := DraftJob(context.Background(), a, draftRequest(loc))
	if !errors.Is(err, ErrDraftInvalid) {
		t.Fatalf("应报起草不可用: %v", err)
	}
	if draft.Spec != "not-a-cron" {
		t.Fatalf("应保留模型原值便于人工修正: %q", draft.Spec)
	}
	if len(draft.Warnings) == 0 {
		t.Fatal("应说明为何不可用")
	}
	if fp.CallCount() != 2 {
		t.Fatalf("应为 1 次起草 + 1 次修复: %d", fp.CallCount())
	}
}

// TestDraftJobSchemaRangeRepairedByAgent 参数越界由输出 Schema 拦住，并由 Agent 的修复轮
// 纠正（不是草案层重试）——所以 draft.Repairs 仍为 0，但轨迹里必须有 1 条修复提示。
func TestDraftJobSchemaRangeRepairedByAgent(t *testing.T) {
	loc := shanghai(t)
	gateway, fp := newGateway()
	calls := 0
	fp.WithResponder(func(llm.Request) (llm.Completion, error) {
		calls++
		if calls == 1 {
			bad := validDraft()
			bad["timeout_seconds"] = 999999 // 超出 24 小时上限
			return llm.FakeJSON(bad), nil
		}
		return llm.FakeJSON(validDraft()), nil
	})
	a := New(gateway, nil, DefaultConfig())
	draft, err := DraftJob(context.Background(), a, draftRequest(loc))
	if err != nil {
		t.Fatalf("修复后应成功: %v", err)
	}
	if draft.TimeoutSeconds != 300 {
		t.Fatalf("最终应采用修复后的参数: %+v", draft)
	}
	if fp.CallCount() != 2 {
		t.Fatalf("应为 1 次起草 + 1 次修复: %d", fp.CallCount())
	}
	notices := 0
	for _, s := range draft.Trace.StepList() {
		if s.Kind == StepNotice {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("轨迹里应有 1 条修复提示，实际 %d", notices)
	}
}

func TestDraftJobShellCommandWarning(t *testing.T) {
	loc := shanghai(t)
	payload := validDraft()
	payload["command"] = []interface{}{"find /tmp -mtime +7 -delete | tee /var/log/clean.log"}
	gateway, fp := newGateway()
	fp.WithResponder(func(llm.Request) (llm.Completion, error) { return llm.FakeJSON(payload), nil })

	a := New(gateway, nil, DefaultConfig())
	draft, err := DraftJob(context.Background(), a, draftRequest(loc))
	if err != nil {
		t.Fatalf("应起草成功: %v", err)
	}
	if draft.RawArgv {
		t.Fatal("含管道的单元素命令不能按 argv 模式执行")
	}
	joined := strings.Join(draft.Warnings, " | ")
	if !strings.Contains(joined, "shell") {
		t.Fatalf("应提示需要 shell 解释: %v", draft.Warnings)
	}
}

func TestDraftJobHTTP(t *testing.T) {
	loc := shanghai(t)
	payload := validDraft()
	payload["job_type"] = "http"
	payload["command"] = []interface{}{}
	payload["http"] = map[string]interface{}{"method": "POST", "url": "https://api.example.com/health"}
	gateway, fp := newGateway()
	fp.WithResponder(func(llm.Request) (llm.Completion, error) { return llm.FakeJSON(payload), nil })

	a := New(gateway, nil, DefaultConfig())
	draft, err := DraftJob(context.Background(), a, draftRequest(loc))
	if err != nil {
		t.Fatalf("应起草成功: %v", err)
	}
	if draft.HTTP == nil || draft.HTTP.Method != "POST" || draft.HTTP.URL != "https://api.example.com/health" {
		t.Fatalf("HTTP 定义不符: %+v", draft.HTTP)
	}
	if len(draft.Command) != 0 || draft.RawArgv {
		t.Fatalf("HTTP 任务不应有命令: %+v", draft)
	}
}

func TestDraftJobRejectsEmptyText(t *testing.T) {
	gateway, _ := newGateway()
	a := New(gateway, nil, DefaultConfig())
	if _, err := DraftJob(context.Background(), a, NLJobRequest{}); !errors.Is(err, ErrDraftInvalid) {
		t.Fatalf("空描述应被拒绝: %v", err)
	}
}

// TestFillDraftLocalValidation 直接单测"最后一道防线"（不经模型）。
func TestFillDraftLocalValidation(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 3, 2, 10, 0, 0, 0, loc)

	t.Run("非法并发策略回退为 skip 并告警", func(t *testing.T) {
		obj := validDraft()
		obj["concurrency_policy"] = "whatever"
		d := JobDraft{}
		if err := fillDraft(&d, obj, loc, now); err != nil {
			t.Fatalf("应容忍并回退: %v", err)
		}
		if d.ConcurrencyPolicy != "skip" || d.ConcurrencyValue != int(runpolicy.ConcurrencySkip) {
			t.Fatalf("回退不符: %+v", d)
		}
		if !strings.Contains(strings.Join(d.Warnings, " "), "回退为 skip") {
			t.Fatalf("应告警: %v", d.Warnings)
		}
	})
	t.Run("数值越界裁剪", func(t *testing.T) {
		obj := validDraft()
		obj["timeout_seconds"] = 0
		obj["retry_times"] = 99
		obj["retry_interval"] = 0
		d := JobDraft{}
		if err := fillDraft(&d, obj, loc, now); err != nil {
			t.Fatalf("应裁剪而不是报错: %v", err)
		}
		if d.TimeoutSeconds != MinTimeoutSeconds || d.RetryTimes != MaxRetryTimesAllowed || d.RetryInterval != 1 {
			t.Fatalf("裁剪不符: %+v", d)
		}
	})
	t.Run("空命令被拒", func(t *testing.T) {
		obj := validDraft()
		obj["command"] = []interface{}{}
		d := JobDraft{}
		if err := fillDraft(&d, obj, loc, now); err == nil {
			t.Fatal("cmd 类型必须有命令")
		}
	})
	t.Run("命令含空参数被拒", func(t *testing.T) {
		obj := validDraft()
		obj["command"] = []interface{}{"/bin/echo", "  "}
		d := JobDraft{}
		if err := fillDraft(&d, obj, loc, now); err == nil {
			t.Fatal("空参数应被拒绝")
		}
	})
	t.Run("http 缺 url 被拒", func(t *testing.T) {
		obj := validDraft()
		obj["job_type"] = "http"
		d := JobDraft{}
		if err := fillDraft(&d, obj, loc, now); err == nil {
			t.Fatal("http 类型必须有 url")
		}
	})
	t.Run("http 非 http(s) 协议被拒", func(t *testing.T) {
		obj := validDraft()
		obj["job_type"] = "http"
		obj["http"] = map[string]interface{}{"method": "GET", "url": "ftp://example.com/x"}
		d := JobDraft{}
		if err := fillDraft(&d, obj, loc, now); err == nil {
			t.Fatal("非 http(s) 协议应被拒绝")
		}
	})
	t.Run("未知 job_type 被拒", func(t *testing.T) {
		obj := validDraft()
		obj["job_type"] = "magic"
		d := JobDraft{}
		if err := fillDraft(&d, obj, loc, now); err == nil {
			t.Fatal("未知类型应被拒绝")
		}
	})
	t.Run("缺任务名用占位名并告警", func(t *testing.T) {
		obj := validDraft()
		obj["name"] = ""
		d := JobDraft{}
		if err := fillDraft(&d, obj, loc, now); err != nil {
			t.Fatalf("应容忍: %v", err)
		}
		if d.Name == "" || !strings.Contains(strings.Join(d.Warnings, " "), "占位名") {
			t.Fatalf("应使用占位名并告警: %+v", d)
		}
	})
}

func TestNeedsShell(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"/usr/bin/find", "/tmp", "-mtime", "+7"}, false},
		{[]string{"/bin/echo", "|", "cat"}, false},    // 多元素：argv 直接执行，管道只是普通参数
		{[]string{"echo hello | cat"}, true},          // 单元素含管道
		{[]string{"echo $HOME"}, true},                // 单元素含变量展开
		{[]string{"rm -rf /tmp/x > /dev/null"}, true}, // 单元素含重定向
		{[]string{"/bin/true"}, false},                // 单元素纯路径
		{nil, false},
	}
	for _, tc := range cases {
		if got := needsShell(tc.argv); got != tc.want {
			t.Fatalf("needsShell(%v)=%v，期望 %v", tc.argv, got, tc.want)
		}
	}
}

// TestParseSpecNeverPanics 表达式非法时必须是错误，不能把进程打挂。
func TestParseSpecNeverPanics(t *testing.T) {
	cases := []struct {
		spec string
		ok   bool
		desc string
	}{
		{"", false, "空表达式（cron.Parse 内部会 panic）"},
		{"   ", false, "纯空格"},
		{"not a cron", false, "非法字段数且非数字"},
		{"99 99 * * *", false, "字段越界"},
		{"* * * *", false, "只有 4 段"},
		{"* * * * * * *", false, "7 段"},
		{"0 0 2 * * *", true, "6 段标准格式"},
		{"0 2 * * *", true, "5 段（本库语义为 秒 分 时 日 月）"},
		{"@daily", true, "描述符"},
		{"@every 1h30m", true, "间隔描述符"},
		{"@notexist", false, "未知描述符"},
		{strings.Repeat("1 ", 200), false, "超长表达式"},
	}
	for _, tc := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s: ParseSpec(%q) panic 了: %v", tc.desc, tc.spec, r)
				}
			}()
			_, err := ParseSpec(tc.spec)
			if tc.ok && err != nil {
				t.Fatalf("%s: %q 应可解析: %v", tc.desc, tc.spec, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("%s: %q 应报错", tc.desc, tc.spec)
			}
			if !tc.ok && err != nil && !errors.Is(err, ErrInvalidCronSpec) {
				t.Fatalf("%s: 错误类型不符: %v", tc.desc, err)
			}
		}()
	}
}

// TestValidateSpecReturnsRequestedCount 触发时间必须够数、且严格递增。
func TestValidateSpecReturnsRequestedCount(t *testing.T) {
	loc := shanghai(t)
	from := time.Date(2026, 3, 2, 10, 30, 0, 0, loc)
	times, err := ValidateSpec("0 0 2 * * *", from, 5)
	if err != nil {
		t.Fatalf("应可解析: %v", err)
	}
	if len(times) != 5 {
		t.Fatalf("应返回 5 次: %+v", times)
	}
	for i := 1; i < len(times); i++ {
		if !times[i].After(times[i-1]) {
			t.Fatalf("触发时间必须递增: %+v", times)
		}
	}
	if got := times[0].Format("2006-01-02 15:04:05"); got != "2026-03-03 02:00:00" {
		t.Fatalf("首次触发不符: %s", got)
	}
	if _, offset := times[0].Zone(); offset != 8*3600 {
		t.Fatalf("应按请求时区返回: %s", times[0])
	}
}

func TestResolveLocationFallbackWarns(t *testing.T) {
	loc, warn := resolveLocation("Mars/Olympus")
	if loc == nil || warn == "" || !strings.Contains(warn, "回退") {
		t.Fatalf("无效时区应回退并告警: %v %q", loc, warn)
	}
	loc2, warn2 := resolveLocation("")
	if loc2 == nil || warn2 != "" {
		t.Fatalf("空时区用本地时区且不告警: %v %q", loc2, warn2)
	}
}
