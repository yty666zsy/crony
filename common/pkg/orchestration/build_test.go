package orchestration

import (
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/executor"
)

func shellJob(stepsJSON string) *models.Job {
	return &models.Job{
		ID:        7,
		Name:      "备份流水线",
		Command:   "/opt/fallback.sh",
		Type:      models.JobTypeCmd,
		Timeout:   120,
		StepsJSON: stepsJSON,
	}
}

const linearPlan = `{"steps":[
  {"index":0,"name":"extract","command":"/opt/extract.sh"},
  {"index":1,"name":"transform","command":"/opt/transform.sh"},
  {"index":2,"name":"load","command":"/opt/load.sh"}
]}`

// TestBuildPlanWithoutStepsJSONKeepsLegacyPath 是本包最重要的契约：
// 没有 steps_json 时必须返回 used=false，让调用方继续走单步逻辑——老任务行为零变化。
func TestBuildPlanWithoutStepsJSONKeepsLegacyPath(t *testing.T) {
	for _, raw := range []string{"", "  ", "\n"} {
		plan, used, err := BuildPlan(shellJob(raw), "r-1")
		if err != nil {
			t.Fatalf("空编排不该报错：%v", err)
		}
		if used {
			t.Fatalf("空编排必须返回 used=false（交给单步逻辑），实际 plan=%+v", plan)
		}
	}
}

func TestBuildPlanLinearImplicitChain(t *testing.T) {
	plan, used, err := BuildPlan(shellJob(linearPlan), "r-lin")
	if err != nil || !used {
		t.Fatalf("线性编排应编译成功：used=%v err=%v", used, err)
	}
	if !plan.IsDAG() {
		t.Fatal("编译后的计划带显式依赖，必须走 DAG 引擎")
	}
	if len(plan.Steps) != 3 {
		t.Fatalf("步骤数 = %d, want 3", len(plan.Steps))
	}
	wantDeps := [][]int{nil, {0}, {1}}
	for i, s := range plan.Steps {
		if len(s.DependsOn) != len(wantDeps[i]) {
			t.Fatalf("步骤 %d 依赖 = %v, want %v", i, s.DependsOn, wantDeps[i])
		}
		for j := range wantDeps[i] {
			if s.DependsOn[j] != wantDeps[i][j] {
				t.Fatalf("步骤 %d 依赖 = %v, want %v", i, s.DependsOn, wantDeps[i])
			}
		}
		if s.Name == "" || s.Request.Name != s.Name {
			t.Fatalf("步骤 %d 的名字必须带到请求里：name=%q req.name=%q", i, s.Name, s.Request.Name)
		}
		if s.Request.RunID != "r-lin" || s.Request.JobID != 7 {
			t.Fatalf("步骤 %d 缺少运行上下文: %+v", i, s.Request)
		}
		if s.Request.Timeout != 120*time.Second {
			t.Fatalf("步骤 %d 应继承任务超时 120s，实际 %v", i, s.Request.Timeout)
		}
	}
}

func TestBuildPlanAgentAndHTTPKinds(t *testing.T) {
	// 不写 kind 时继承任务类型；写 kind 时以步骤为准。
	httpJob := &models.Job{ID: 1, Type: models.JobTypeHttp, HttpMethod: models.HTTPMethodPost,
		StepsJSON: `{"steps":[{"index":0,"command":"https://api.example.com/hook?{\"a\":1}"}]}`}
	plan, _, err := BuildPlan(httpJob, "r-http")
	if err != nil {
		t.Fatalf("http 步骤编译失败：%v", err)
	}
	if plan.Steps[0].Kind != executor.KindHTTP {
		t.Fatalf("应继承 http 类型，实际 %s", plan.Steps[0].Kind)
	}
	if plan.Steps[0].Request.Method != "POST" || plan.Steps[0].Request.URL != "https://api.example.com/hook" ||
		plan.Steps[0].Request.Body != `{"a":1}` {
		t.Fatalf("POST 的 URL/body 拆分不符: %+v", plan.Steps[0].Request)
	}

	agentJob := &models.Job{ID: 2, Type: models.JobTypeAgent, StepsJSON: `{"steps":[{"index":0,"command":"检查昨晚备份"}]}`}
	plan2, _, err := BuildPlan(agentJob, "r-agent")
	if err != nil {
		t.Fatalf("agent 步骤编译失败：%v", err)
	}
	if plan2.Steps[0].Kind != executor.KindAgent || plan2.Steps[0].Request.AgentGoal != "检查昨晚备份" {
		t.Fatalf("agent 步骤映射不符: %+v", plan2.Steps[0])
	}
	if plan2.Steps[0].Request.AgentKind != models.AgentKindJob {
		t.Fatalf("agent 步骤的会话类型应为 job，实际 %q", plan2.Steps[0].Request.AgentKind)
	}

	// 混合编排：shell 任务的编排里放一个 http 步骤
	mixed := shellJob(`{"steps":[{"index":0,"command":"/opt/pre.sh"},{"index":1,"kind":"http","command":"https://api.example.com/notify","depends_on":"0"}]}`)
	plan3, _, err := BuildPlan(mixed, "r-mixed")
	if err != nil {
		t.Fatalf("混合编排编译失败：%v", err)
	}
	if plan3.Steps[0].Kind != executor.KindShell || plan3.Steps[1].Kind != executor.KindHTTP {
		t.Fatalf("混合编排类型映射不符: %s / %s", plan3.Steps[0].Kind, plan3.Steps[1].Kind)
	}
}

func TestBuildPlanShellArgvMatchesSingleStepRule(t *testing.T) {
	// 编排里的命令切分必须与单步任务逐字一致（同一个 models.SplitCommand）。
	job := shellJob(`{"steps":[{"index":0,"command":"/opt/backup.sh -name \"my file\" --fast"}]}`)
	plan, _, err := BuildPlan(job, "r-argv")
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	req := plan.Steps[0].Request
	if !req.RawArgv {
		t.Fatal("shell 步骤必须走 argv 模式（不做 shell 解析）")
	}
	if req.Command != "/opt/backup.sh" {
		t.Fatalf("程序名 = %q", req.Command)
	}
	want := []string{"-name", "my file", "--fast"}
	if len(req.Args) != len(want) {
		t.Fatalf("参数个数 = %d, want %d (%v)", len(req.Args), len(want), req.Args)
	}
	for i := range want {
		if req.Args[i] != want[i] {
			t.Fatalf("参数 %d = %q, want %q", i, req.Args[i], want[i])
		}
	}

	// 显式 args 时按原样传参，不再切分 command
	job2 := shellJob(`{"steps":[{"index":0,"command":"/opt/backup.sh","args":["-name","my file"]}]}`)
	plan2, _, err := BuildPlan(job2, "r-argv2")
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	req2 := plan2.Steps[0].Request
	if req2.Command != "/opt/backup.sh" || len(req2.Args) != 2 || req2.Args[1] != "my file" {
		t.Fatalf("显式 args 应原样使用: %+v", req2)
	}
}

func TestBuildPlanRejectsUnrunnableGraphs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "依赖不存在的步骤",
			raw:  `{"steps":[{"index":0,"command":"a"},{"index":1,"command":"b","depends_on":"9"}]}`,
			want: "不存在的步骤 9",
		},
		{
			name: "成环",
			raw:  `{"steps":[{"index":0,"command":"a","depends_on":"2"},{"index":1,"command":"b","depends_on":"0"},{"index":2,"command":"c","depends_on":"1"}]}`,
			want: "编排存在环",
		},
		{
			name: "未知字段（拼写错误不能被静默忽略）",
			raw:  `{"steps":[{"index":0,"command":"a"},{"index":1,"command":"b","depends_ons":"0"}]}`,
			want: "depends_ons",
		},
		{
			name: "JSON 不是对象",
			raw:  `[{"index":0}]`,
			want: "不是合法 JSON 对象",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, used, err := BuildPlan(shellJob(tc.raw), "r-bad")
			if err == nil {
				t.Fatal("坏编排必须编译失败（拒绝执行）")
			}
			if used {
				t.Fatal("坏编排不得宣称「可用」")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestBuildPlanDiamondValidates(t *testing.T) {
	job := shellJob(`{
	  "max_parallel": 3,
	  "fail_fast": true,
	  "steps": [
	    {"index":0,"command":"extract"},
	    {"index":1,"command":"transform","depends_on":"0"},
	    {"index":2,"command":"enrich","depends_on":"0"},
	    {"index":3,"command":"load","depends_on":"1,2","join":"all_done","timeout_seconds":30}
	  ]
	}`)
	plan, used, err := BuildPlan(job, "r-diamond")
	if err != nil || !used {
		t.Fatalf("菱形编排应编译成功：used=%v err=%v", used, err)
	}
	if plan.MaxParallel != 3 || !plan.FailFast {
		t.Fatalf("并发/急停设置未透传: %+v", plan)
	}
	if plan.Steps[3].Join != executor.JoinAllDone {
		t.Fatalf("join 未透传: %s", plan.Steps[3].Join)
	}
	if plan.Steps[3].Request.Timeout != 30*time.Second {
		t.Fatalf("步骤级超时应覆盖任务超时，实际 %v", plan.Steps[3].Request.Timeout)
	}
	if plan.Steps[0].Request.Timeout != 120*time.Second {
		t.Fatalf("未写超时的步骤应继承任务超时，实际 %v", plan.Steps[0].Request.Timeout)
	}
}

func TestBuildPlanStepRetryPolicy(t *testing.T) {
	job := shellJob(`{"steps":[
	  {"index":0,"command":"a","retry_times":3,"retry_interval":2},
	  {"index":1,"command":"b","depends_on":"0"}
	]}`)
	plan, _, err := BuildPlan(job, "r-retry")
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	if plan.Steps[0].Policy == nil {
		t.Fatal("写了 retry_times 的步骤必须带重试策略")
	}
	if plan.Steps[1].Policy != nil {
		t.Fatal("没写 retry_times 的步骤不得默认重试")
	}
}

func TestValidateJobPlan(t *testing.T) {
	if err := ValidateJobPlan(&models.Job{}); err != nil {
		t.Fatalf("没有编排的任务必须通过校验：%v", err)
	}
	if err := ValidateJobPlan(shellJob(linearPlan)); err != nil {
		t.Fatalf("合法编排必须通过校验：%v", err)
	}
	bad := shellJob(`{"steps":[{"index":0,"command":"a","depends_on":"0"}]}`)
	if err := ValidateJobPlan(bad); err == nil {
		t.Fatal("自依赖必须被拒绝")
	}
}

func TestSummarize(t *testing.T) {
	summary, err := Summarize(shellJob(linearPlan))
	if err != nil {
		t.Fatalf("摘要应成功：%v", err)
	}
	if len(summary.Steps) != 3 || summary.MaxDepth != 2 {
		t.Fatalf("摘要不符: %+v", summary)
	}

	if _, err := Summarize(&models.Job{}); err == nil {
		t.Fatal("没有编排的任务做摘要必须报错（而不是给一张空图）")
	}

	// 非法图：摘要照给，但必须把错误一起抛出来（控制台要"画出图 + 标出错处"）
	if _, err := Summarize(shellJob(`{"steps":[{"index":0,"command":"a","depends_on":"5"}]}`)); err == nil {
		t.Fatal("非法图的摘要必须带上错误")
	}
}
