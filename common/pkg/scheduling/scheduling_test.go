package scheduling

import "testing"

func cand(uuid string, label map[string]string, cap, load int) NodeCandidate {
	return NodeCandidate{UUID: uuid, Hostname: uuid, Labels: label, Capacity: cap, Load: load, Reachable: true}
}

func TestHardRejectUnreachable(t *testing.T) {
	c := cand("a", nil, 3, 0)
	c.Reachable = false
	d := Decide(Requirement{}, []NodeCandidate{c}, Weights{})
	if d.Chosen != "" {
		t.Fatalf("不可达节点不得被选中，实际 chosen=%s", d.Chosen)
	}
	if len(d.Rejected) != 1 || d.Rejected[0].Reason != "节点不可达" {
		t.Fatalf("应记录不可达拒绝原因，实际 %+v", d.Rejected)
	}
}

func TestHardRejectMissingLabel(t *testing.T) {
	c := cand("a", map[string]string{"env": "dev"}, 3, 0)
	d := Decide(Requirement{RequireLabels: map[string]string{"env": "prod"}}, []NodeCandidate{c}, Weights{})
	if d.Chosen != "" {
		t.Fatalf("标签不匹配必须被硬拒绝，实际 chosen=%s", d.Chosen)
	}
	if len(d.Rejected) != 1 || d.Rejected[0].Reason == "" {
		t.Fatalf("必须给出拒绝原因，实际 %+v", d.Rejected)
	}
}

func TestHardRejectMissingTool(t *testing.T) {
	c := cand("a", map[string]string{"tool:docker": ""}, 3, 0)
	d := Decide(Requirement{RequireTools: []string{"kubectl"}}, []NodeCandidate{c}, Weights{})
	if d.Chosen != "" || len(d.Rejected) != 1 {
		t.Fatalf("缺少能力标签必须被拒，实际 chosen=%q rejected=%+v", d.Chosen, d.Rejected)
	}
	// RequireTools 不带前缀也应等价。
	d2 := Decide(Requirement{RequireTools: []string{"docker"}}, []NodeCandidate{c}, Weights{})
	if d2.Chosen != "a" {
		t.Fatalf("tool: 前缀应可省略，实际 chosen=%q", d2.Chosen)
	}
}

// 权重只在满足硬约束的候选之间排序：一个"成功率很高"但没标签的节点不能因为分数高被选中。
func TestHardConstraintBeatsWeight(t *testing.T) {
	good := cand("no-label", nil, 1, 0)
	good.SuccessRate, good.Samples = 1.0, 100
	ok := cand("has-label", map[string]string{"env": "prod"}, 8, 0)
	ok.SuccessRate, ok.Samples = 0.5, 100
	d := Decide(Requirement{RequireLabels: map[string]string{"env": "prod"}},
		[]NodeCandidate{good, ok}, Weights{})
	if d.Chosen != "has-label" {
		t.Fatalf("硬约束优先于权重：应选 has-label，实际 %s", d.Chosen)
	}
}

// 负载要相对容量归一：同样是 5 个任务，容量大的节点应胜出。
func TestLoadNormalizedByCapacity(t *testing.T) {
	small := cand("small", nil, 5, 5)  // 打满
	large := cand("large", nil, 50, 5) // 10%
	d := Decide(Requirement{}, []NodeCandidate{small, large}, Weights{})
	if d.Chosen != "large" {
		t.Fatalf("负载应按容量归一：large 应胜出，实际 %s（scores=%+v）", d.Chosen, d.Scores)
	}
}

// 亲和节点在同负载下应胜出。
func TestAffinityPrefersOriginalNode(t *testing.T) {
	a := cand("a", nil, 10, 2)
	b := cand("b", nil, 10, 2)
	base := Decide(Requirement{}, []NodeCandidate{a, b}, Weights{})
	if base.Chosen != "a" {
		t.Fatalf("同分应按 UUID 升序（a），实际 %s", base.Chosen)
	}
	withAff := Decide(Requirement{PreferNode: "b"}, []NodeCandidate{a, b}, Weights{})
	if withAff.Chosen != "b" {
		t.Fatalf("命中亲和应选 b，实际 %s", withAff.Chosen)
	}
}

// 健康度只在样本足够（>=3）时参与；样本不足时按中性处理。
func TestSuccessRequiresSamples(t *testing.T) {
	low := cand("low", nil, 10, 2)
	low.SuccessRate, low.Samples = 0.1, 2 // 样本不足，忽略
	hi := cand("hi", nil, 10, 2)
	hi.SuccessRate, hi.Samples = 0.95, 10
	d := Decide(Requirement{}, []NodeCandidate{low, hi}, Weights{})
	if d.Chosen != "hi" {
		t.Fatalf("样本足够的高成功率节点应胜出，实际 %s", d.Chosen)
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	// 完全等价的两个节点，多次打分结果必须一致（UUID 升序）。
	for i := 0; i < 20; i++ {
		a := cand("b-node", nil, 4, 1)
		b := cand("a-node", nil, 4, 1)
		d := Decide(Requirement{}, []NodeCandidate{a, b}, Weights{})
		if d.Chosen != "a-node" {
			t.Fatalf("同分必须稳定按 UUID 升序，第 %d 次得到 %s", i, d.Chosen)
		}
	}
}

func TestNoCandidates(t *testing.T) {
	d := Decide(Requirement{}, nil, Weights{})
	if d.Chosen != "" {
		t.Fatalf("无候选时 chosen 必须为空")
	}
	if len(d.Notes) == 0 {
		t.Fatalf("无候选时必须给出说明（不许静默返回空）")
	}
}

func TestZeroWeightsUseDefaults(t *testing.T) {
	w := normalizeWeights(Weights{})
	if w != DefaultWeights() {
		t.Fatalf("零值权重应回退到默认，实际 %+v", w)
	}
}

// 容量未声明时不应被当作 0（否则"未声明容量"的节点永远输给任何声明了容量的节点）。
func TestUndeclaredCapacityIsNeutral(t *testing.T) {
	undeclared := NodeCandidate{UUID: "u", Reachable: true, Load: 0}
	declared := cand("d", nil, 1, 1) // 容量 1，打满
	d := Decide(Requirement{}, []NodeCandidate{undeclared, declared}, Weights{})
	if d.Chosen != "u" {
		t.Fatalf("未声明容量的空载节点应优于打满的小容量节点，实际 %s", d.Chosen)
	}
}
