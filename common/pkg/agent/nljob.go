package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/runpolicy"
)

// MaxDraftRepairs 起草失败（例如 cron 表达式非法）时的重试轮上限。
//
// 取值 1 而不是更多：模型第一次给错表达式后，通常能改对；连续两次错说明
// 用户描述本身有歧义，此时应该把问题抛回给人，而不是让模型继续猜。
const MaxDraftRepairs = 1

// 边界与默认值（与 models.Job 的语义保持一致）。
const (
	DefaultJobTimeoutSeconds   = 1800
	DefaultRetryIntervalSecond = 60
	MaxRetryTimesAllowed       = 10
	MinTimeoutSeconds          = 1
	MaxTimeoutSeconds          = 24 * 3600
)

// ErrDraftInvalid 起草结果不可用（表达式非法、命令缺失等）。
var ErrDraftInvalid = fmt.Errorf("agent: 起草的任务定义不可用")

// NLJobRequest 自然语言建任务请求。
type NLJobRequest struct {
	// Text 用户原话。
	Text string
	// NodeUUID 目标节点（仅作为材料提供给模型，不参与调度决策）。
	NodeUUID string
	// Timezone IANA 时区名（如 Asia/Shanghai）；空则用进程本地时区。
	Timezone string
	// Now 注入时钟（空则 time.Now），用于计算接下来 5 次触发。
	Now time.Time
	// Context 额外材料（如现有任务列表、节点可用命令白名单）。
	Context []string
}

// HTTPDraft HTTP 任务定义。
type HTTPDraft struct {
	Method string `json:"method"`
	URL    string `json:"url"`
}

// JobDraft 起草出的任务定义（**尚未落库、尚未进调度器**）。
type JobDraft struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	JobType     string `json:"job_type"`
	Spec        string `json:"spec"`

	Command []string   `json:"command,omitempty"`
	RawArgv bool       `json:"raw_argv"`
	HTTP    *HTTPDraft `json:"http,omitempty"`

	TimeoutSeconds    int64  `json:"timeout_seconds"`
	RetryTimes        int    `json:"retry_times"`
	RetryInterval     int    `json:"retry_interval"`
	ConcurrencyPolicy string `json:"concurrency_policy"`
	// ConcurrencyValue 落库用的数值（models.Job.ConcurrencyPolicy）。
	ConcurrencyValue int `json:"concurrency_value"`

	Rationale string   `json:"rationale"`
	Warnings  []string `json:"warnings"`

	// NextTriggers 接下来 5 次触发时间（人工确认的核心材料）。
	NextTriggers []time.Time `json:"next_triggers"`
	// NextTriggerText 人类可读的触发时间。
	NextTriggerText []string `json:"next_trigger_text"`
	// TriggerEcho 供界面/日志直接展示的确认文本。
	TriggerEcho string `json:"trigger_echo"`
	// RequiresConfirmation 恒为 true：**起草结果必须人工确认后才能落库**。
	RequiresConfirmation bool `json:"requires_confirmation"`
	// Confirmed 是否已人工确认（本包永不置 true）。
	Confirmed bool `json:"confirmed"`

	Timezone string `json:"timezone"`

	// 运行元信息。
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	PromptVersion string     `json:"prompt_version"`
	StopReason    StopReason `json:"stop_reason"`
	Repairs       int        `json:"repairs"`
	DurationMS    int64      `json:"duration_ms"`
	TotalTokens   int        `json:"total_tokens"`
	Trace         *Trace     `json:"-"`
}

// Explain 生成人类可读的起草说明。
func (d JobDraft) Explain() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("任务名: %s\n类型: %s\n调度: %s\n", d.Name, d.JobType, d.Spec))
	if d.JobType == "http" && d.HTTP != nil {
		b.WriteString(fmt.Sprintf("请求: %s %s\n", d.HTTP.Method, d.HTTP.URL))
	} else {
		b.WriteString(fmt.Sprintf("命令: %s（argv 模式=%v）\n", strings.Join(d.Command, " "), d.RawArgv))
	}
	b.WriteString(fmt.Sprintf("超时: %ds  重试: %d 次/间隔 %ds  并发策略: %s\n",
		d.TimeoutSeconds, d.RetryTimes, d.RetryInterval, d.ConcurrencyPolicy))
	if d.Rationale != "" {
		b.WriteString("翻译说明: " + d.Rationale + "\n")
	}
	b.WriteString("\n" + d.TriggerEcho + "\n")
	if len(d.Warnings) > 0 {
		b.WriteString("\n需要你注意:\n")
		for _, w := range d.Warnings {
			b.WriteString("  ⚠ " + w + "\n")
		}
	}
	b.WriteString(fmt.Sprintf("\n确认前不会创建任务（requires_confirmation=%v）\n", d.RequiresConfirmation))
	return b.String()
}

// NLJobSchema 起草输出的 JSON Schema。
func NLJobSchema() *jsonschema.Schema {
	httpObj := jsonschema.Object(map[string]*jsonschema.Schema{
		"method": jsonschema.String("HTTP 方法").WithEnum("GET", "POST"),
		"url":    jsonschema.String("请求地址").WithLength(1, 1024),
	}, "method", "url")

	return jsonschema.Object(map[string]*jsonschema.Schema{
		"name":            jsonschema.String("任务名").WithLength(1, 120),
		"description":     jsonschema.String("任务说明").WithLength(0, 500),
		"job_type":        jsonschema.String("任务类型").WithEnum("cmd", "http"),
		"spec":            jsonschema.String("cron 表达式").WithLength(1, 64),
		"command":         jsonschema.Array(jsonschema.String("参数").WithLength(0, 1024), "命令 argv").WithLength(0, 64),
		"http":            httpObj,
		"timeout_seconds": jsonschema.Integer("超时秒数").WithRange(MinTimeoutSeconds, MaxTimeoutSeconds),
		"retry_times":     jsonschema.Integer("重试次数").WithRange(0, MaxRetryTimesAllowed),
		"retry_interval":  jsonschema.Integer("重试间隔秒").WithRange(1, MaxTimeoutSeconds),
		"concurrency_policy": jsonschema.String("并发策略").
			WithEnum("skip", "buffer_one", "buffer_all", "cancel_other", "terminate_other"),
		"rationale": jsonschema.String("翻译依据").WithLength(1, 800),
		"warnings":  jsonschema.Array(jsonschema.String("提醒").WithLength(1, 300), "提醒事项").WithLength(0, 10),
	}, "name", "description", "job_type", "spec", "command", "timeout_seconds",
		"retry_times", "retry_interval", "concurrency_policy", "rationale", "warnings")
}

// DraftJob 把自然语言翻译成任务定义。
//
// **本函数不写任何存储、不进调度器**：它只产出草稿 + "接下来 5 次触发时间"，
// 由人在界面上确认后才由上层落库。这是阶段 2 人为确认闸门的技术落点。
func DraftJob(ctx context.Context, a *Agent, req NLJobRequest) (JobDraft, error) {
	start := time.Now()
	draft := JobDraft{
		PromptVersion:        PromptVersionNLJobV1,
		RequiresConfirmation: true,
		JobType:              "cmd",
		ConcurrencyPolicy:    runpolicy.ConcurrencySkip.String(),
	}
	if a == nil {
		return draft, fmt.Errorf("%w: agent 未初始化", ErrNoProvider)
	}
	if strings.TrimSpace(req.Text) == "" {
		return draft, fmt.Errorf("%w: 自然语言描述为空", ErrDraftInvalid)
	}

	loc, tzWarn := resolveLocation(req.Timezone)
	draft.Timezone = loc.String()
	if tzWarn != "" {
		draft.Warnings = append(draft.Warnings, tzWarn)
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.In(loc)

	baseContext := []string{"### 用户描述\n" + strings.TrimSpace(req.Text)}
	if req.NodeUUID != "" {
		baseContext = append(baseContext, "### 目标节点\n"+req.NodeUUID)
	}
	for i, extra := range req.Context {
		if strings.TrimSpace(extra) == "" {
			continue
		}
		baseContext = append(baseContext, fmt.Sprintf("### 补充材料 %d\n%s", i+1, extra))
	}

	var (
		out     Outcome
		err     error
		extra   string
		repairs int
	)
	// 1 + MaxDraftRepairs 次尝试：第一次用用户原话，之后带上"上次哪里不行"。
	for attempt := 0; attempt <= MaxDraftRepairs; attempt++ {
		in := Input{
			SessionID:     sessionIDFor("nljob", 0, fmt.Sprintf("%d-%d", now.Unix(), attempt)),
			Goal:          "把用户的自然语言描述翻译成定时任务定义",
			System:        PromptNLJobV1,
			Context:       append(append([]string(nil), baseContext...), extra),
			PromptVersion: PromptVersionNLJobV1,
		}
		out, err = a.Run(ctx, in, NLJobSchema())
		repairs = attempt
		if draft.Trace == nil {
			draft.Trace = out.Trace
		} else if out.Trace != nil {
			// 多轮尝试时保留最后一轮轨迹，并把前一轮摘要并入告警
			draft.Trace = out.Trace
		}
		if err != nil {
			draft.StopReason = out.StopReason
			draft.Repairs = repairs
			draft.DurationMS = time.Since(start).Milliseconds()
			return draft, err
		}
		if perr := fillDraft(&draft, out.Structured, loc, now); perr != nil {
			if attempt < MaxDraftRepairs {
				extra = fmt.Sprintf("### 上一次输出不可用\n%s", perr.Error())
				draft.Warnings = append(draft.Warnings, fmt.Sprintf("第 %d 次起草不可用：%v（已自动重试）", attempt+1, perr))
				continue
			}
			draft.StopReason = out.StopReason
			draft.Repairs = repairs
			draft.DurationMS = time.Since(start).Milliseconds()
			return draft, fmt.Errorf("%w: %v", ErrDraftInvalid, perr)
		}
		break
	}

	draft.StopReason = out.StopReason
	draft.Repairs = repairs
	draft.DurationMS = time.Since(start).Milliseconds()
	draft.Provider = providerOf(draft.Trace)
	draft.Model = modelOf(draft.Trace)
	if draft.Trace != nil {
		draft.TotalTokens = draft.Trace.Usage.TotalTokens
		// 触发时间必须本地计算，不能用模型给的——模型算日期不可靠
		times, terr := ValidateSpec(draft.Spec, now, NextTriggerCount)
		if terr != nil {
			return draft, fmt.Errorf("%w: %v", ErrDraftInvalid, terr)
		}
		draft.NextTriggers = times
		draft.NextTriggerText = FormatTriggers(times, loc)
		draft.TriggerEcho = TriggerEcho(draft.Spec, times, loc)
	}
	return draft, nil
}

// fillDraft 把模型输出填进草稿并做**本地校验与裁剪**。
//
// 这里承担"最后一道防线"的角色：模型给的任何取值都要经过本地规则
// （cron 可解析、命令非空、超时范围、并发策略枚举）才允许进入草稿。
func fillDraft(draft *JobDraft, obj map[string]interface{}, loc *time.Location, now time.Time) error {
	draft.Name = strings.TrimSpace(llm.StringOr(obj, "name", ""))
	if draft.Name == "" {
		draft.Name = "未命名任务"
		draft.Warnings = append(draft.Warnings, "模型未给出任务名，已使用占位名，请创建前修改")
	}
	draft.Description = llm.StringOr(obj, "description", "")
	draft.JobType = llm.StringOr(obj, "job_type", "cmd")
	draft.Spec = strings.TrimSpace(llm.StringOr(obj, "spec", ""))
	draft.Rationale = llm.StringOr(obj, "rationale", "")

	// cron 表达式必须是本地能解析的（模型给的表达式一律不信，先验后用）
	if _, err := ValidateSpec(draft.Spec, now, NextTriggerCount); err != nil {
		return RepairCronHintWrap(draft.Spec, err)
	}
	// 五段表达式的语义陷阱：底层库把 5 段解释为「秒 分 时 日 月」，
	// 于是 "0 2 * * *" 是"每小时第 2 分钟"，不是 Unix 的"每天 02:00"。
	// 这类错误不会报错、只会让任务按错误频率跑，必须显式告警。
	if !strings.HasPrefix(draft.Spec, "@") && len(strings.Fields(draft.Spec)) == 5 {
		draft.Warnings = append(draft.Warnings, fmt.Sprintf(
			"表达式 %q 只有 5 段：本系统底层库把 5 段解释为「秒 分 时 日 月」，不是 Unix 的「分 时 日 月 周」。"+
				"请改用 6 段格式（例如每天 02:00 应写 \"0 0 2 * * *\"）。下方触发时间按本库真实语义计算，请核对。",
			draft.Spec))
	}

	// 命令 / HTTP
	// argv 单独解析：**不能用通用的字符串列表助手**——那个助手会静默丢弃空字符串，
	// 而空参数会改变命令语义（"cmd ''" 与 "cmd" 不同），必须报错而不是悄悄改掉。
	cmd, cerr := argvList(obj, "command")
	if cerr != nil {
		return cerr
	}
	draft.Command = cmd
	if raw, ok := obj["http"].(map[string]interface{}); ok {
		draft.HTTP = &HTTPDraft{
			Method: strings.ToUpper(strings.TrimSpace(llm.StringOr(raw, "method", ""))),
			URL:    strings.TrimSpace(llm.StringOr(raw, "url", "")),
		}
	}
	switch draft.JobType {
	case "http":
		if draft.HTTP == nil || draft.HTTP.URL == "" {
			return fmt.Errorf("%w: job_type 为 http 但缺少 http.url", ErrDraftInvalid)
		}
		if !strings.HasPrefix(draft.HTTP.URL, "http://") && !strings.HasPrefix(draft.HTTP.URL, "https://") {
			return fmt.Errorf("%w: http.url 必须是 http/https 地址: %q", ErrDraftInvalid, draft.HTTP.URL)
		}
		if draft.HTTP.Method == "" {
			draft.HTTP.Method = "GET"
		}
		draft.Command = nil
		draft.RawArgv = false
	case "cmd":
		if len(draft.Command) == 0 {
			return fmt.Errorf("%w: 命令为空", ErrDraftInvalid)
		}
		for i, part := range draft.Command {
			if strings.TrimSpace(part) == "" {
				return fmt.Errorf("%w: 命令第 %d 个参数为空", ErrDraftInvalid, i+1)
			}
		}
		draft.RawArgv = !needsShell(draft.Command)
		if !draft.RawArgv {
			draft.Warnings = append(draft.Warnings,
				"该命令含管道/重定向/变量展开等 shell 语法，执行时会经过 shell 解释（存在注入面）。建议改写为 argv 数组形式。")
		}
	default:
		return fmt.Errorf("%w: 未知 job_type %q", ErrDraftInvalid, draft.JobType)
	}

	// 数值参数：越界裁剪 + 显式告警（不静默改用户意图）
	draft.TimeoutSeconds = clampInt64(llm.FloatOr(obj, "timeout_seconds", DefaultJobTimeoutSeconds), MinTimeoutSeconds, MaxTimeoutSeconds)
	draft.RetryTimes = int(clampInt64(llm.FloatOr(obj, "retry_times", 0), 0, MaxRetryTimesAllowed))
	draft.RetryInterval = int(clampInt64(llm.FloatOr(obj, "retry_interval", DefaultRetryIntervalSecond), 1, MaxTimeoutSeconds))

	policyRaw := llm.StringOr(obj, "concurrency_policy", runpolicy.ConcurrencySkip.String())
	policy, ok := runpolicy.ParseConcurrencyPolicy(policyRaw)
	if !ok {
		draft.Warnings = append(draft.Warnings, fmt.Sprintf("并发策略 %q 不是合法取值，已回退为 skip", policyRaw))
		policy = runpolicy.ConcurrencySkip
	}
	draft.ConcurrencyPolicy = policy.String()
	draft.ConcurrencyValue = int(policy)

	for _, w := range stringList(obj, "warnings") {
		if strings.TrimSpace(w) != "" {
			draft.Warnings = append(draft.Warnings, strings.TrimSpace(w))
		}
	}
	return nil
}

// RepairCronHintWrap 生成修复提示（包装 RepairCronHint，保持错误链）。
func RepairCronHintWrap(spec string, err error) error {
	return fmt.Errorf("%w: %s", ErrDraftInvalid, RepairCronHint(spec, err))
}

// needsShell 判断 argv 数组是否必须经过 shell 解释。
//
// 判定依据：单元素命令里含 shell 元字符。多元素数组按 argv 直接执行，
// **不做任何字符串拼接**，因此不存在注入面（阶段 1 的 argv 语义）。
func needsShell(argv []string) bool {
	if len(argv) != 1 {
		return false
	}
	return strings.ContainsAny(argv[0], "|&;<>$`*?(){}[]\n~")
}

// clampInt64 区间裁剪。
func clampInt64(v float64, min, max int64) int64 {
	n := int64(v)
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// resolveLocation 解析时区；失败时回退本地时区并给出告警（不静默）。
func resolveLocation(name string) (*time.Location, string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.Local, ""
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc, ""
	}
	return time.Local, fmt.Sprintf("时区 %q 无法解析，已回退为进程本地时区 %s；触发时间按本地时区计算", name, time.Local.String())
}
