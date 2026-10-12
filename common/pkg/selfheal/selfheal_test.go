package selfheal

import (
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
)

func ptrBool(v bool) *bool { return &v }

func mkRun(status models.RunStatus, errMsg string, exitCode int) *models.JobRun {
	return &models.JobRun{
		RunID: "j21-t100", JobID: 21, JobName: "备份-订单库", NodeUUID: "node-a",
		Status: int(status), ErrorMsg: errMsg, ExitCode: exitCode, Attempt: 1,
	}
}

func TestClassifyPrecedence(t *testing.T) {
	cases := []struct {
		name string
		in   ClassifyInput
		want FailureClass
	}{
		{"空 run 归为未知", ClassifyInput{Run: nil}, ClassUnknown},
		{
			"编排非法优先于其它一切（错误文本里同时出现超时字样）",
			ClassifyInput{Run: mkRun(models.RunStatusFailed, "编排定义非法：存在环，且上一次是超时", 1)},
			ClassOrchestrationInvalid,
		},
		{
			"超时按 run 状态判定",
			ClassifyInput{Run: mkRun(models.RunStatusTimeout, "", 0)},
			ClassTimeout,
		},
		{
			"超时按错误文本判定",
			ClassifyInput{Run: mkRun(models.RunStatusFailed, "context deadline exceeded", 0)},
			ClassTimeout,
		},
		{
			"投递失败优先于节点不可达（事实是控制面没投出去）",
			ClassifyInput{Run: mkRun(models.RunStatusFailed, "投递重跑失败：etcd put error, node conn fail", 0), NodeReachable: ptrBool(false)},
			ClassDeliveryFailed,
		},
		{
			"节点不可达（显式观测）",
			ClassifyInput{Run: mkRun(models.RunStatusFailed, "exit status 1", 1), NodeReachable: ptrBool(false)},
			ClassNodeUnreachable,
		},
		{
			"节点可达性未知时不做节点判类",
			ClassifyInput{Run: mkRun(models.RunStatusFailed, "exit status 1", 1), NodeReachable: nil},
			ClassCommandFailed,
		},
		{
			"命令失败按退出码",
			ClassifyInput{Run: mkRun(models.RunStatusFailed, "script exit 3", 3)},
			ClassCommandFailed,
		},
		{
			"命令失败按失败步骤（退出码为 0 时也要能判出来）",
			ClassifyInput{
				Run:   mkRun(models.RunStatusFailed, "", 0),
				Steps: []models.JobRunStep{{StepIndex: 1, Name: "verify", Status: int(models.RunStatusFailed)}},
			},
			ClassCommandFailed,
		},
		{
			"既无状态也无退出码也无失败步骤 → 未知",
			ClassifyInput{Run: mkRun(models.RunStatusFailed, "", 0)},
			ClassUnknown,
		},
	}
	for _, c := range cases {
		got := Classify(c.in)
		if got.Class != c.want {
			t.Fatalf("%s: 期望 %s，实际 %s（理由：%s）", c.name, c.want, got.Class, got.Reason)
		}
		if got.Reason == "" {
			t.Fatalf("%s: 分类必须带可读理由", c.name)
		}
		// 有观测就必须带证据；没有观测（run 为空）时证据为空本身就是事实，
		// 但此时分类只允许是 unknown（不允许凭空归类）。
		if c.in.Run == nil {
			if got.Class != ClassUnknown {
				t.Fatalf("%s: 没有观测时不允许归类，实际 %s", c.name, got.Class)
			}
			continue
		}
		if len(got.Evidence) == 0 {
			t.Fatalf("%s: 分类必须带证据", c.name)
		}
	}
}

func TestClassifyEvidenceCarriesFailedStep(t *testing.T) {
	in := ClassifyInput{
		Run: mkRun(models.RunStatusFailed, "step failed", 7),
		Steps: []models.JobRunStep{
			{StepIndex: 0, Name: "load", Status: int(models.RunStatusSuccess)},
			{StepIndex: 2, Name: "notify", Status: int(models.RunStatusFailed)},
		},
	}
	got := Classify(in)
	if got.Evidence["failed_step"] != "2 notify" {
		t.Fatalf("应记录第一个失败步骤，实际 %q", got.Evidence["failed_step"])
	}
	if got.Evidence["exit_code"] != "7" {
		t.Fatalf("应记录退出码，实际 %q", got.Evidence["exit_code"])
	}
}

func TestSymptomShape(t *testing.T) {
	run := mkRun(models.RunStatusFailed, "exit status 1", 1)
	c := Classify(ClassifyInput{Run: run})
	s := Symptom("", 21, c, run)
	if s == "" || !containsAny(s, "未命名任务", "command_failed") {
		t.Fatalf("症状文本不合形：%q", s)
	}
	// 症状必须可读且被截断（经验库字段是 512，超长会被 MySQL 截断报错）
	long := mkRun(models.RunStatusFailed, string(make([]rune, 4000)), 1)
	if len([]rune(Symptom("x", 1, Classify(ClassifyInput{Run: long}), long))) > 512 {
		t.Fatal("症状文本必须截断到 512 字符以内")
	}
}

func TestEvalCapsGates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	base := func() Policy {
		p := DefaultPolicy()
		p.Enabled = true
		return p
	}
	ok := CapInput{JobSelfHeal: true, WritesEnabled: true, Now: now}

	t.Run("总开关关闭时先判总开关", func(t *testing.T) {
		d := EvalCaps(DefaultPolicy(), ok)
		if d.Allowed || d.Rule != CapDisabled {
			t.Fatalf("默认策略必须拒绝，实际 %+v", d)
		}
	})
	t.Run("任务未开启自愈", func(t *testing.T) {
		d := EvalCaps(base(), CapInput{WritesEnabled: true, Now: now})
		if d.Allowed || d.Rule != CapJobOptIn {
			t.Fatalf("任务未 opt-in 必须拒绝，实际 %+v", d)
		}
	})
	t.Run("写能力未开启", func(t *testing.T) {
		d := EvalCaps(base(), CapInput{JobSelfHeal: true, Now: now})
		if d.Allowed || d.Rule != CapWriteOff {
			t.Fatalf("写能力关闭必须拒绝，实际 %+v", d)
		}
	})
	t.Run("允许", func(t *testing.T) {
		d := EvalCaps(base(), ok)
		if !d.Allowed || d.Rule != CapAllowed || d.Attempts != 1 {
			t.Fatalf("应允许且是第 1 次，实际 %+v", d)
		}
	})
	t.Run("被拒尝试不消耗次数预算", func(t *testing.T) {
		p := base()
		p.CooldownSeconds = 0
		hist := []*models.JobHeal{
			{Created: now.Unix() - 10, Status: models.HealStatusDenied},
			{Created: now.Unix() - 20, Status: models.HealStatusDenied},
			{Created: now.Unix() - 30, Status: models.HealStatusDenied},
		}
		d := EvalCaps(p, CapInput{JobSelfHeal: true, WritesEnabled: true, History: hist, Now: now})
		// 被拒绝 = 系统没动手。若把它算成"已经试过三次"，就会把从未尝试过的任务
		// 直接推给人工——那是把"暂时没空"伪装成"已经试过了"。
		if !d.Allowed || d.Attempts != 1 {
			t.Fatalf("被拒记录不得消耗次数预算，实际 %+v", d)
		}
		if d.WindowCount != 0 {
			t.Fatalf("窗口内真实尝试数应为 0，实际 %d", d.WindowCount)
		}
	})
	t.Run("转人工结论也不消耗次数预算", func(t *testing.T) {
		p := base()
		p.CooldownSeconds = 0
		hist := []*models.JobHeal{
			{Created: now.Unix() - 10, Status: models.HealStatusEscalated},
			{Created: now.Unix() - 20, Status: models.HealStatusEscalated},
			{Created: now.Unix() - 30, Status: models.HealStatusEscalated},
		}
		d := EvalCaps(p, CapInput{JobSelfHeal: true, WritesEnabled: true, History: hist, Now: now})
		if !d.Allowed || d.Attempts != 1 {
			t.Fatalf("转人工不影响别的失败的尝试预算，实际 %+v", d)
		}
	})
	t.Run("真实尝试触发上限", func(t *testing.T) {
		p := base()
		p.CooldownSeconds = 0
		hist := []*models.JobHeal{
			{Created: now.Unix() - 10, Status: models.HealStatusSucceeded},
			{Created: now.Unix() - 20, Status: models.HealStatusFailed},
			{Created: now.Unix() - 30, Status: models.HealStatusExecuted},
		}
		d := EvalCaps(p, CapInput{JobSelfHeal: true, WritesEnabled: true, History: hist, Now: now})
		if d.Allowed || d.Rule != CapAttempts {
			t.Fatalf("三次真实尝试后必须停止自动尝试，实际 %+v", d)
		}
		if d.Attempts != 4 || d.WindowCount != 3 {
			t.Fatalf("阈值口径应为 含本次=4/窗口=3，实际 %d/%d", d.Attempts, d.WindowCount)
		}
	})
	t.Run("冷却锚点只看真实尝试", func(t *testing.T) {
		p := base()
		hist := []*models.JobHeal{
			{Created: now.Unix() - 10, Status: models.HealStatusDenied},   // 刚被拒（临时原因）
			{Created: now.Unix() - 1000, Status: models.HealStatusFailed}, // 真正动手是 1000s 前
		}
		d := EvalCaps(p, CapInput{JobSelfHeal: true, WritesEnabled: true, History: hist, Now: now})
		// 若把"被拒"当锚点，一次冷却拒绝会把冷却时间不断往后推，形成"永远在冷却"。
		if !d.Allowed {
			t.Fatalf("冷却应从上一次真实尝试起算，实际 %+v", d)
		}
	})
	t.Run("窗口外的历史不计入", func(t *testing.T) {
		p := base()
		p.CooldownSeconds = 0
		hist := []*models.JobHeal{{Created: now.Unix() - 4000, Status: models.HealStatusSucceeded}}
		d := EvalCaps(p, CapInput{JobSelfHeal: true, WritesEnabled: true, History: hist, Now: now})
		if !d.Allowed || d.Attempts != 1 {
			t.Fatalf("窗口外历史不应计入，实际 %+v", d)
		}
	})
	t.Run("冷却期", func(t *testing.T) {
		p := base()
		hist := []*models.JobHeal{{Created: now.Unix() - 10, Status: models.HealStatusExecuted}}
		d := EvalCaps(p, CapInput{JobSelfHeal: true, WritesEnabled: true, History: hist, Now: now})
		if d.Allowed || d.Rule != CapCooldown {
			t.Fatalf("冷却期内必须拒绝，实际 %+v", d)
		}
	})
	t.Run("并发上限", func(t *testing.T) {
		p := base()
		p.CooldownSeconds = 0
		d := EvalCaps(p, CapInput{JobSelfHeal: true, WritesEnabled: true, ActiveCount: p.MaxConcurrent, Now: now})
		if d.Allowed || d.Rule != CapConcurrency {
			t.Fatalf("并发满必须拒绝，实际 %+v", d)
		}
	})
}

func TestPolicyNormalizeFillsDefaults(t *testing.T) {
	p := Policy{Enabled: true}.Normalize() // 所有边界写 0/缺省
	d := DefaultPolicy()
	if p.MaxAttemptsPerJobWindow != d.MaxAttemptsPerJobWindow || p.MaxConcurrent != d.MaxConcurrent ||
		p.ScanLimit != d.ScanLimit || p.ObserveWindowSeconds != d.ObserveWindowSeconds {
		t.Fatalf("缺省值必须被默认值兜住，实际 %+v", p)
	}
	if !p.Enabled {
		t.Fatal("Normalize 不得改动显式开关")
	}
}

func TestNextTimeoutBounded(t *testing.T) {
	p := DefaultPolicy() // factor=2, cap=3600
	if got, ok := p.NextTimeout(60); !ok || got != 120 {
		t.Fatalf("60s 应放大到 120s，实际 %d ok=%v", got, ok)
	}
	if got, ok := p.NextTimeout(3000); !ok || got != 3600 {
		t.Fatalf("放大后不得超过绝对上限 3600，实际 %d ok=%v", got, ok)
	}
	if _, ok := p.NextTimeout(0); ok {
		t.Fatal("当前没有超时上限时不得给出目标值")
	}
	tight := p
	tight.MaxExtendTimeoutSeconds = 30
	if _, ok := tight.NextTimeout(30); ok {
		t.Fatal("已到绝对上限时不得给出目标值（不能靠自愈无限放大超时）")
	}
}

func TestPlanForRuleTable(t *testing.T) {
	job := &models.Job{ID: 21, RunOn: "node-a", Timeout: 60}
	cases := []struct {
		class FailureClass
		want  models.HealAction
	}{
		{ClassCommandFailed, models.HealActionRetrySameNode},
		{ClassDeliveryFailed, models.HealActionRetrySameNode},
		{ClassTimeout, models.HealActionRetryExtendTimeout},
		{ClassNodeUnreachable, models.HealActionEscalate},
		{ClassOrchestrationInvalid, models.HealActionEscalate},
		{ClassUnknown, models.HealActionEscalate},
	}
	for _, c := range cases {
		plan := PlanFor(PlanInput{
			Classification: Classification{Class: c.class},
			Job:            job, Policy: DefaultPolicy(),
		})
		if plan.Action != c.want {
			t.Fatalf("%s: 期望动作 %s，实际 %s（理由：%s）", c.class, c.want, plan.Action, plan.Reason)
		}
		if plan.Reason == "" || len(plan.Candidates) == 0 {
			t.Fatalf("%s: 决策必须带理由与候选明细", c.class)
		}
		if plan.Candidates[0].Chosen != true {
			t.Fatalf("%s: 必须标出被选中的候选", c.class)
		}
		if plan.WriteRequired != c.want.IsWrite() {
			t.Fatalf("%s: WriteRequired 与实际动作不一致", c.class)
		}
	}
}

func TestPlanForTimeoutArgsAndDegrade(t *testing.T) {
	plan := PlanFor(PlanInput{
		Classification: Classification{Class: ClassTimeout},
		Job:            &models.Job{ID: 21, RunOn: "node-a", Timeout: 120},
		Policy:         DefaultPolicy(),
	})
	if plan.ActionArgs.ToTimeout != 240 || plan.ActionArgs.FromTimeout != 120 {
		t.Fatalf("超时动作必须带出具体目标值，实际 %+v", plan.ActionArgs)
	}
	if plan.ActionArgs.JobID != 21 || plan.ActionArgs.NodeUUID != "node-a" {
		t.Fatalf("动作参数必须带任务与节点，实际 %+v", plan.ActionArgs)
	}

	// 没有超时上限 → 退化为重跑（而不是凭空造一个超时值）
	degraded := PlanFor(PlanInput{
		Classification: Classification{Class: ClassTimeout},
		Job:            &models.Job{ID: 21, RunOn: "node-a", Timeout: 0},
		Policy:         DefaultPolicy(),
	})
	if degraded.Action != models.HealActionRetrySameNode {
		t.Fatalf("超时上限缺失时应退化为重跑，实际 %s", degraded.Action)
	}
	// 退化的结论必须写进 action_args（否则执行层会拿到空的 from/to）
	if degraded.ActionArgs.ToTimeout != 0 || degraded.ActionArgs.JobID != 21 {
		t.Fatalf("退化动作仍需带任务号，实际 %+v", degraded.ActionArgs)
	}
}

func TestPlanForHistoryOnlyReordersCandidates(t *testing.T) {
	// 超时类有两个候选：改超时（基础优先 0）与重跑（基础优先 10）。
	// 历史数据说"重跑成功率 0.9、样本 5；改超时成功率 0.1、样本 5" → 应改序到重跑。
	plan := PlanFor(PlanInput{
		Classification: Classification{Class: ClassTimeout},
		Job:            &models.Job{ID: 21, RunOn: "node-a", Timeout: 60},
		Policy:         DefaultPolicy(),
		History: map[models.HealAction]HistoryStats{
			models.HealActionRetrySameNode:      {Success: 4, Fail: 1},
			models.HealActionRetryExtendTimeout: {Success: 0, Fail: 5},
		},
		MemoryIDs:  []string{"mem-1"},
		MemoryHint: "历史上原节点重跑成功率 4/5",
	})
	if plan.Action != models.HealActionRetrySameNode {
		t.Fatalf("历史成功率应改变排序，实际 %s（理由：%s）", plan.Action, plan.Reason)
	}
	if plan.Confidence != "high" {
		t.Fatalf("样本充分且成功率高时置信度应为 high，实际 %s", plan.Confidence)
	}
	if len(plan.MemoryIDs) != 1 {
		t.Fatal("必须把参考过的经验条目带下来")
	}

	// 样本不足（<3）时不得改序：噪声不能决定动作。
	noisy := PlanFor(PlanInput{
		Classification: Classification{Class: ClassTimeout},
		Job:            &models.Job{ID: 21, RunOn: "node-a", Timeout: 60},
		Policy:         DefaultPolicy(),
		History: map[models.HealAction]HistoryStats{
			models.HealActionRetrySameNode: {Success: 1, Fail: 0},
		},
	})
	if noisy.Action != models.HealActionRetryExtendTimeout {
		t.Fatalf("样本不足时不应改序，实际 %s", noisy.Action)
	}
}

func TestPlanForEscalateNeverDisplacedByHistory(t *testing.T) {
	// 造一个"候选里含 escalate"的场景：节点不可达（唯一候选是转人工，天然安全），
	// 这里用历史把 escalate 的成功率写得很低、另一动作写得很高，验证 escalate 不会被顶掉。
	plan := PlanFor(PlanInput{
		Classification: Classification{Class: ClassNodeUnreachable},
		Job:            &models.Job{ID: 21, RunOn: "node-a", Timeout: 60},
		Policy:         DefaultPolicy(),
		History: map[models.HealAction]HistoryStats{
			models.HealActionEscalate:      {Denied: 9},
			models.HealActionRetrySameNode: {Success: 9},
		},
	})
	if plan.Action != models.HealActionEscalate {
		t.Fatalf("转人工是安全兜底，不得被历史数据挤掉，实际 %s", plan.Action)
	}
}

func TestHistoryStatsRateExcludesNeverExecuted(t *testing.T) {
	s := HistoryStats{Denied: 5, Blocked: 3}
	if _, ok := s.Rate(); ok {
		t.Fatal("从未执行过的动作没有成功率（不能被算成 0 或 1）")
	}
	if s.Samples() != 8 {
		t.Fatalf("样本数应包含未执行的记录，实际 %d", s.Samples())
	}
	s2 := HistoryStats{Success: 1, Fail: 1, Denied: 100}
	rate, ok := s2.Rate()
	if !ok || rate != 0.5 {
		t.Fatalf("成功率分母只含 success+fail，实际 %v ok=%v", rate, ok)
	}
}

func TestActionJSONAndCapDecisionSerializable(t *testing.T) {
	if (ActionJSON{JobID: 1, ToTimeout: 60}).JSON() == "{}" {
		t.Fatal("动作参数必须能序列化")
	}
	if (CapDecision{Allowed: true, Rule: CapAllowed}).JSON() == "{}" {
		t.Fatal("策略判定必须能序列化")
	}
	if StatsJSON(map[models.HealAction]HistoryStats{models.HealActionEscalate: {Success: 1}}) == "{}" {
		t.Fatal("历史统计必须能序列化")
	}
	if StatsJSON(nil) != "{}" {
		t.Fatal("空历史应序列化为空对象")
	}
}
