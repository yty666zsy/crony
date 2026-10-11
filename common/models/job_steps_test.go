package models

import (
	"strings"
	"testing"
)

const diamondPlanJSON = `{
  "max_parallel": 2,
  "steps": [
    {"index":0, "name":"extract",   "kind":"shell", "command":"/opt/extract.sh"},
    {"index":1, "name":"transform", "kind":"shell", "command":"/opt/transform.sh", "depends_on":"0"},
    {"index":2, "name":"enrich",    "kind":"shell", "command":"/opt/enrich.sh",    "depends_on":"0"},
    {"index":3, "name":"load",      "kind":"shell", "command":"/opt/load.sh",      "depends_on":"1,2", "join":"all_success"}
  ]
}`

func TestParseJobPlanEmptyMeansNoOrchestration(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n\t"} {
		spec, err := ParseJobPlan(raw)
		if err != nil {
			t.Fatalf("空 steps_json 必须是合法状态（等于没有编排），实际报错 %v", err)
		}
		if spec != nil {
			t.Fatalf("空 steps_json 必须返回 nil 规格，实际 %+v", spec)
		}
	}
}

func TestParseJobPlanRejectsUnknownFields(t *testing.T) {
	// 拼错的字段名必须当场报错。否则 "depends_ons" 会被忽略，图悄悄变成"并排执行"——
	// 那是最危险的静默降级：既没报错，又改变了执行顺序。
	raw := `{"steps":[{"index":0,"command":"a"},{"index":1,"command":"b","depends_ons":"0"}]}`
	_, err := ParseJobPlan(raw)
	if err == nil {
		t.Fatal("未知字段必须报错")
	}
	if !strings.Contains(err.Error(), "depends_ons") {
		t.Fatalf("错误信息要指出具体字段，实际 %q", err.Error())
	}
}

func TestParseJobPlanDiamondResolvesImplicitAndExplicitDeps(t *testing.T) {
	spec, err := ParseJobPlan(diamondPlanJSON)
	if err != nil {
		t.Fatalf("菱形编排应解析成功：%v", err)
	}
	if spec.MaxParallel != 2 {
		t.Fatalf("max_parallel = %d, want 2", spec.MaxParallel)
	}
	cases := []struct {
		pos  int
		want string
	}{
		{0, ""},    // 索引 0：没有上游
		{1, "0"},   // 显式
		{2, "0"},   // 显式
		{3, "1,2"}, // 多上游
	}
	for _, tc := range cases {
		deps, err := spec.Steps[tc.pos].ResolveDependsOn(spec.Steps, tc.pos)
		if err != nil {
			t.Fatalf("步骤 %d 依赖解析失败：%v", tc.pos, err)
		}
		got := joinInts(deps)
		if got != tc.want {
			t.Fatalf("步骤 %d 依赖 = %q, want %q", tc.pos, got, tc.want)
		}
	}
}

func TestResolveDependsOnDefaultsToPreviousStep(t *testing.T) {
	raw := `{"steps":[{"index":0,"command":"a"},{"index":1,"command":"b"},{"index":2,"command":"c"}]}`
	spec, err := ParseJobPlan(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	// 不写依赖 = 按书写顺序串起来（线性编排不必写依赖）
	if deps, _ := spec.Steps[1].ResolveDependsOn(spec.Steps, 1); joinInts(deps) != "0" {
		t.Fatalf("第二个步骤应默认依赖第一个，实际 %q", joinInts(deps))
	}
	if deps, _ := spec.Steps[2].ResolveDependsOn(spec.Steps, 2); joinInts(deps) != "1" {
		t.Fatalf("第三个步骤应默认依赖第二个，实际 %q", joinInts(deps))
	}
	if deps, _ := spec.Steps[0].ResolveDependsOn(spec.Steps, 0); len(deps) != 0 {
		t.Fatalf("第一个步骤没有上游，实际 %v", deps)
	}
}

func TestResolveDependsOnExplicitRoot(t *testing.T) {
	for _, marker := range []string{"root", "ROOT", "none", "-"} {
		s := JobStepSpec{Index: 5, Command: "a", DependsOn: marker}
		all := []JobStepSpec{{Index: 0, Command: "z"}, s}
		deps, err := s.ResolveDependsOn(all, 1)
		if err != nil {
			t.Fatalf("%q 应被接受：%v", marker, err)
		}
		if len(deps) != 0 {
			t.Fatalf("%q 应解析为「没有上游」，实际 %v", marker, deps)
		}
	}
}

func TestParseJobPlanRejectsStructurallyBrokenPlans(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"没有步骤", `{"steps":[]}`, "没有任何步骤"},
		{"索引重复", `{"steps":[{"index":1,"command":"a"},{"index":1,"command":"b"}]}`, "index 重复"},
		{"索引为负", `{"steps":[{"index":-1,"command":"a"}]}`, "不允许负数"},
		{"缺少命令", `{"steps":[{"index":0,"command":"  "}]}`, "缺少 command"},
		{"kind 非法", `{"steps":[{"index":0,"command":"a","kind":"k8s"}]}`, "kind 非法"},
		{"join 非法", `{"steps":[{"index":0,"command":"a","join":"majority"}]}`, "join 非法"},
		{"condition 非法", `{"steps":[{"index":0,"command":"a","condition":"on_error"}]}`, "condition 非法"},
		{"http_method 非法", `{"steps":[{"index":0,"command":"http://x","http_method":"PATCH"}]}`, "http_method 非法"},
		{"timeout 为负", `{"steps":[{"index":0,"command":"a","timeout_seconds":-1}]}`, "timeout_seconds 不能为负"},
		{"retry_times 超限", `{"steps":[{"index":0,"command":"a","retry_times":999}]}`, "超过上限"},
		{"依赖自身", `{"steps":[{"index":0,"command":"a","depends_on":"0"}]}`, "依赖了自身"},
		{"依赖含非数字", `{"steps":[{"index":0,"command":"a"},{"index":1,"command":"b","depends_on":"第0步"}]}`, "非数字项"},
		{"依赖含空项", `{"steps":[{"index":0,"command":"a"},{"index":1,"command":"b","depends_on":"0,"}]}`, "空的依赖项"},
		{"并发上限为负", `{"max_parallel":-1,"steps":[{"index":0,"command":"a"}]}`, "max_parallel 不能为负"},
		{"不是 JSON 对象", `[]`, "不是合法 JSON 对象"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseJobPlan(tc.raw)
			if err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseJobPlanCaps(t *testing.T) {
	// 步骤数上限：一张人类能看懂的图，而不是把 100 个步骤塞进一个任务
	steps := make([]string, 0, MaxPlanSteps+1)
	for i := 0; i <= MaxPlanSteps; i++ {
		steps = append(steps, `{"index":`+itoa(i)+`,"command":"a"}`)
	}
	_, err := ParseJobPlan(`{"steps":[` + strings.Join(steps, ",") + `]}`)
	if err == nil || !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("超过 MaxPlanSteps 必须报错，实际 %v", err)
	}

	// 上游数量上限
	deps := make([]string, 0, MaxFanIn+1)
	for i := 0; i <= MaxFanIn; i++ {
		deps = append(deps, itoa(i))
	}
	fan := make([]string, 0, MaxFanIn+2)
	for i := 0; i <= MaxFanIn; i++ {
		fan = append(fan, `{"index":`+itoa(i)+`,"command":"a","depends_on":"root"}`)
	}
	fan = append(fan, `{"index":99,"command":"join","depends_on":"`+strings.Join(deps, ",")+`"}`)
	_, err = ParseJobPlan(`{"steps":[` + strings.Join(fan, ",") + `]}`)
	if err == nil || !strings.Contains(err.Error(), "上游数量") {
		t.Fatalf("超过 MaxFanIn 必须报错，实际 %v", err)
	}
}

func TestJobPlanSummarize(t *testing.T) {
	spec, err := ParseJobPlan(diamondPlanJSON)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	summary := spec.Summarize()
	if len(summary.Steps) != 4 || len(summary.Edges) != 4 {
		t.Fatalf("摘要步骤/边数不符: steps=%d edges=%d", len(summary.Steps), len(summary.Edges))
	}
	if summary.MaxParallel != 2 {
		t.Fatalf("摘要 max_parallel = %d, want 2", summary.MaxParallel)
	}
	// 层深：extract=0、transform/enrich=1、load=2
	wantDepth := map[int]int{0: 0, 1: 1, 2: 1, 3: 2}
	for _, s := range summary.Steps {
		if s.Depth != wantDepth[s.Index] {
			t.Fatalf("步骤 %d 层深 = %d, want %d", s.Index, s.Depth, wantDepth[s.Index])
		}
		if s.Name == "" {
			t.Fatalf("步骤 %d 名字丢了", s.Index)
		}
	}
	if summary.MaxDepth != 2 {
		t.Fatalf("MaxDepth = %d, want 2", summary.MaxDepth)
	}
	// 边必须指向正确的方向（from 是上游）
	for _, e := range summary.Edges {
		if e.From >= e.To {
			t.Fatalf("边的方向不对: %+v", e)
		}
	}
}

func TestJobPlanSummarizeWarnsAboutOrphans(t *testing.T) {
	// 1 号和 2 号互为"没有关系"的孤岛：控制台要提醒，而不是静默执行。
	raw := `{"steps":[{"index":0,"command":"a"},{"index":1,"command":"b","depends_on":"root"}]}`
	spec, err := ParseJobPlan(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	summary := spec.Summarize()
	warned := 0
	for _, s := range summary.Steps {
		for _, w := range s.Warnings {
			if strings.Contains(w, "孤岛") {
				warned++
			}
		}
	}
	if warned != 2 {
		t.Fatalf("两个互不相干的步骤都应被提醒，实际提醒 %d 个", warned)
	}
}

func TestSummaryDepthOnCycleTerminates(t *testing.T) {
	// 有环的图：Summarize 不能死循环（它不负责报错，报错由 executor 的 Validate 负责）。
	raw := `{"steps":[{"index":0,"command":"a","depends_on":"2"},{"index":1,"command":"b","depends_on":"0"},{"index":2,"command":"c","depends_on":"1"}]}`
	spec, err := ParseJobPlan(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	summary := spec.Summarize()
	if len(summary.Steps) != 3 {
		t.Fatalf("有环时摘要也必须返回全部步骤，实际 %d", len(summary.Steps))
	}
}

func TestSplitCommandMatchesJobSplitCmd(t *testing.T) {
	// 多步骤编排复用了这条切分规则：两者必须给出完全相同的 argv。
	command := `/opt/backup.sh -name "my file" --exclude=a\b`
	job := &Job{Command: command, Type: JobTypeCmd}
	job.SplitCmd()
	direct := SplitCommand(command)
	if joinStrings(job.Cmd) != joinStrings(direct) {
		t.Fatalf("SplitCmd 与 SplitCommand 不一致：%q vs %q", joinStrings(job.Cmd), joinStrings(direct))
	}
	if len(direct) != 4 {
		t.Fatalf("带引号与转义的命令应切出 4 段，实际 %d：%q", len(direct), joinStrings(direct))
	}
	if direct[2] != "my file" {
		t.Fatalf("引号内的空格必须保留在同一参数里，实际 %q", direct[2])
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

func joinInts(vals []int) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = itoa(v)
	}
	return strings.Join(parts, ",")
}

func joinStrings(vals []string) string { return strings.Join(vals, "\x00") }
