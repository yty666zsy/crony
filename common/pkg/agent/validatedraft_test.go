package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// baseDraft 一份"已经通过起草、等待人工确认"的合法草案。
func baseDraft() JobDraft {
	return JobDraft{
		Name:              "每日备份",
		Description:       "每天凌晨 2 点备份数据库",
		JobType:           "cmd",
		Spec:              "0 0 2 * * *",
		Command:           []string{"/usr/local/bin/backup.sh", "--full"},
		RawArgv:           true,
		TimeoutSeconds:    1800,
		RetryTimes:        2,
		RetryInterval:     60,
		ConcurrencyPolicy: "skip",
		ConcurrencyValue:  0,
		Timezone:          "Asia/Shanghai",
	}
}

// TestValidateDraftRecomputesTriggers 确认阶段必须由**代码**重算触发时间：
// 前端回传的触发时间一律不采信（它可能被改过，或已经过期）。
func TestValidateDraftRecomputesTriggers(t *testing.T) {
	now := time.Date(2026, 3, 2, 1, 0, 0, 0, time.UTC)
	d := baseDraft()
	d.NextTriggerText = []string{"假的触发时间"}
	d.TriggerEcho = "假的回显"
	d.NextTriggers = []time.Time{now.Add(99 * time.Hour)}

	out, err := ValidateDraft(d, now, "Asia/Shanghai")
	if err != nil {
		t.Fatalf("合法草案不应失败: %v", err)
	}
	if len(out.NextTriggers) != NextTriggerCount {
		t.Fatalf("应重算 %d 次触发，实际 %d", NextTriggerCount, len(out.NextTriggers))
	}
	if len(out.NextTriggerText) != NextTriggerCount || strings.Contains(strings.Join(out.NextTriggerText, " "), "假的") {
		t.Fatalf("触发时间必须由代码重算，实际 %v", out.NextTriggerText)
	}
	if strings.Contains(out.TriggerEcho, "假的") {
		t.Fatalf("触发回显必须由代码重算，实际 %q", out.TriggerEcho)
	}
	// 每天 02:00（Asia/Shanghai）→ 首次触发应是次日 02:00 本地时间
	first := out.NextTriggers[0].In(time.FixedZone("CST", 8*3600))
	if first.Hour() != 2 || first.Minute() != 0 {
		t.Fatalf("首次触发应在 02:00，实际 %s", first)
	}
	if !out.RequiresConfirmation || out.Confirmed {
		t.Fatalf("复验结果必须要求确认且未确认（requires=%v confirmed=%v）", out.RequiresConfirmation, out.Confirmed)
	}
}

// TestValidateDraftRejectsConfirmedFlag 外部传入 confirmed=true 不被采信：
// 确认闸门必须由接口层在复验通过后决定，不能被请求体绕过。
func TestValidateDraftRejectsConfirmedFlag(t *testing.T) {
	d := baseDraft()
	d.Confirmed = true
	out, err := ValidateDraft(d, time.Now(), "")
	if err != nil {
		t.Fatalf("不应失败: %v", err)
	}
	if out.Confirmed {
		t.Fatal("复验必须把 confirmed 归零")
	}
}

func TestValidateDraftRejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*JobDraft)
		wantSub string
	}{
		{"非法 cron", func(d *JobDraft) { d.Spec = "0 2 * *" }, "cron"},
		{"任务名为空", func(d *JobDraft) { d.Name = "  " }, "任务名"},
		{"命令为空", func(d *JobDraft) { d.Command = nil }, "命令为空"},
		{"空参数无法表达", func(d *JobDraft) { d.Command = []string{"cmd", ""} }, "空字符串"},
		{"超时越界", func(d *JobDraft) { d.TimeoutSeconds = MaxTimeoutSeconds + 1 }, "timeout_seconds"},
		{"超时为 0", func(d *JobDraft) { d.TimeoutSeconds = 0 }, "timeout_seconds"},
		{"重试次数越界", func(d *JobDraft) { d.RetryTimes = MaxRetryTimesAllowed + 1 }, "retry_times"},
		{"重试间隔为 0", func(d *JobDraft) { d.RetryInterval = 0 }, "retry_interval"},
		{"并发策略非法", func(d *JobDraft) { d.ConcurrencyPolicy = "whatever" }, "并发策略"},
		{"未知类型", func(d *JobDraft) { d.JobType = "docker" }, "job_type"},
		{"http 缺 url", func(d *JobDraft) { d.JobType = "http"; d.HTTP = nil }, "http"},
		{"http 协议非法", func(d *JobDraft) { d.JobType = "http"; d.HTTP = &HTTPDraft{Method: "GET", URL: "ftp://x"} }, "http/https"},
		{"http 方法不支持", func(d *JobDraft) { d.JobType = "http"; d.HTTP = &HTTPDraft{Method: "DELETE", URL: "http://x"} }, "方法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := baseDraft()
			tc.mutate(&d)
			_, err := ValidateDraft(d, time.Now(), "Asia/Shanghai")
			if err == nil {
				t.Fatal("必须拒绝")
			}
			if !errors.Is(err, ErrDraftInvalid) {
				t.Fatalf("错误应包装 ErrDraftInvalid，实际 %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息应指出问题字段（含 %q），实际 %v", tc.wantSub, err)
			}
		})
	}
}

// TestValidateDraftDoesNotSilentlyClamp 人工确认阶段**不裁剪**越界数值：
// 静默裁剪会让"界面看到的值"与"落库的值"不一致。
func TestValidateDraftDoesNotSilentlyClamp(t *testing.T) {
	d := baseDraft()
	d.TimeoutSeconds = MaxTimeoutSeconds + 12345
	if _, err := ValidateDraft(d, time.Now(), ""); err == nil {
		t.Fatal("越界必须报错而不是裁剪")
	}
}

// TestValidateDraftWarnsFiveFieldSpec 5 段表达式：本库语义与 Unix cron 不同，
// 必须给出显式告警（否则"每天 2 点"会变成"每小时第 2 分钟"）。
func TestValidateDraftWarnsFiveFieldSpec(t *testing.T) {
	d := baseDraft()
	d.Spec = "*/5 * * * *"
	out, err := ValidateDraft(d, time.Now(), "Asia/Shanghai")
	if err != nil {
		t.Fatalf("5 段表达式在本库是可解析的（语义不同），不应直接拒绝: %v", err)
	}
	joined := strings.Join(out.Warnings, " ")
	if !strings.Contains(joined, "5 段") {
		t.Fatalf("必须告警 5 段语义差异，实际 %v", out.Warnings)
	}
	// 本库把 5 段解释为（秒 分 时 日 月）："*/5 * * * *" 是**每 5 秒**，
	// 而不是 Unix cron 的"每 5 分钟"——这正是必须告警的原因。
	if len(out.NextTriggers) != NextTriggerCount {
		t.Fatalf("应算出 %d 次触发，实际 %d", NextTriggerCount, len(out.NextTriggers))
	}
	if got := out.NextTriggers[1].Sub(out.NextTriggers[0]); got != 5*time.Second {
		t.Fatalf("5 段语义应为每 5 秒触发一次（本库把第 1 段当秒），实际间隔 %s", got)
	}
}

func TestValidateDraftHTTP(t *testing.T) {
	d := baseDraft()
	d.JobType = "http"
	d.Command = nil
	d.HTTP = &HTTPDraft{Method: "post", URL: "https://example.com/health"}
	out, err := ValidateDraft(d, time.Now(), "")
	if err != nil {
		t.Fatalf("合法 http 草案不应失败: %v", err)
	}
	if out.HTTP.Method != "POST" {
		t.Fatalf("方法应归一化为大写，实际 %q", out.HTTP.Method)
	}
	if len(out.Command) != 0 {
		t.Fatalf("http 类型不应保留 command 数组，实际 %#v", out.Command)
	}
}

// TestValidateDraftKeepsLocalTimezone 确认时不传时区 → 沿用草案时区，
// 否则会"确认后触发时间整体偏移"。
func TestValidateDraftKeepsLocalTimezone(t *testing.T) {
	d := baseDraft()
	d.Timezone = "Asia/Shanghai"
	out, err := ValidateDraft(d, time.Now(), "")
	if err != nil {
		t.Fatalf("不应失败: %v", err)
	}
	if out.Timezone != "Asia/Shanghai" {
		t.Fatalf("应沿用草案时区，实际 %q", out.Timezone)
	}
	// 时区非法 → 回退本地时区并告警（不静默）
	out2, err2 := ValidateDraft(d, time.Now(), "Mars/Olympus")
	if err2 != nil {
		t.Fatalf("时区无法解析应回退而不是失败: %v", err2)
	}
	if !strings.Contains(strings.Join(out2.Warnings, " "), "无法解析") {
		t.Fatalf("时区回退必须告警，实际 %v", out2.Warnings)
	}
}
