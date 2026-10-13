package service

import (
	"testing"

	"github.com/tmnhs/crony/common/models"
)

// F3 的地基：兜底路径必须能分辨"打分没选出偏好"和"硬约束没人满足"。
// 前者可以让给旧策略（任务数最少），后者绝不能让——否则硬约束名存实亡，
// 任务会被派到一台装不了它的机器上。
func TestHasHardRequirement(t *testing.T) {
	s := new(SchedulerService)
	cases := []struct {
		name     string
		selector string
		want     bool
	}{
		{"未声明选择器", "", false},
		{"只声明亲和（prefer_node 是偏好不是约束）", `{"prefer_node":"n01"}`, false},
		{"声明标签硬约束", `{"require_labels":{"env":"prod"}}`, true},
		{"声明能力硬约束", `{"require_tools":["docker"]}`, true},
		{"标签硬约束为空对象", `{"require_labels":{}}`, false},
		{"能力硬约束为空数组", `{"require_tools":[]}`, false},
		{"非法 JSON：宽松解析后无硬约束", `{not json`, false},
	}
	for _, c := range cases {
		job := &models.Job{NodeSelector: c.selector}
		if got := s.HasHardRequirement(job); got != c.want {
			t.Fatalf("%s: HasHardRequirement=%v，期望 %v（node_selector=%s）", c.name, got, c.want, c.selector)
		}
	}
	if s.HasHardRequirement(nil) {
		t.Fatal("nil 任务不得被判为有硬约束")
	}
}

// BuildRequirement 的两条口径：解析 node_selector 的硬约束；无显式 prefer_node 时
// 用当前 RunOn 作亲和（"能不搬就不搬"）。
func TestBuildRequirementParsesSelectorAndAffinity(t *testing.T) {
	s := new(SchedulerService)
	req := s.BuildRequirement(&models.Job{
		RunOn:        "n07",
		NodeSelector: `{"require_labels":{"zone":"az1"},"require_tools":["docker"]}`,
	})
	if req.RequireLabels["zone"] != "az1" {
		t.Fatalf("require_labels 未解析: %+v", req.RequireLabels)
	}
	if len(req.RequireTools) != 1 || req.RequireTools[0] != "docker" {
		t.Fatalf("require_tools 未解析: %+v", req.RequireTools)
	}
	if req.PreferNode != "n07" {
		t.Fatalf("未显式声明 prefer_node 时应以当前 RunOn 作亲和，实际 %q", req.PreferNode)
	}

	// 显式 prefer_node 优先于 RunOn（"用户说了想去哪，就按用户说的算"）。
	req2 := s.BuildRequirement(&models.Job{RunOn: "n07", NodeSelector: `{"prefer_node":"n02"}`})
	if req2.PreferNode != "n02" {
		t.Fatalf("显式 prefer_node 必须优先，实际 %q", req2.PreferNode)
	}
	if s.HasHardRequirement(&models.Job{NodeSelector: `{"prefer_node":"n02"}`}) {
		t.Fatal("仅 prefer_node 不得算作硬约束")
	}
}
