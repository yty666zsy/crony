// Package scheduling 实现阶段 4-D 的「多目标调度」决策内核。
//
// 现状对比：升级前 crony 的分配策略只有一句「任务数最少的节点」（见
// admin/internal/service/job.go 的 AutoAllocateNode），它看得见的维度只有一个——
// 节点上的任务数量。真实运维里"发给哪个节点"至少要同时看四件事：
//
//  1. 负载（相对**容量**归一，而不是绝对条数：16 核机器上跑 5 个任务与 2 核机器上跑 5 个不等价）；
//  2. 亲和/能力（这台任务需要某能力标签、某工具或某机房；缺了就不该发过去）；
//  3. 局部性（尽量留在原节点/原机房，减少跨机房流量与冷启动）；
//  4. 健康度（近期成功率低的节点应该少接新活，直到它自己恢复）。
//
// 本包只做**打分与排序**，是纯函数、无数据库/网络/etcd 调用，因此可以被穷举测试。
// "谁来提供候选节点的观测值"由调用方负责（admin service）。
//
// 一条不可动摇的纪律：**任何一条硬约束不满足即拒绝，而不是靠权重"扣分"**。
// 权重只用于在**都满足硬约束**的候选之间排序；否则"某节点成功率很高"会让
// "它根本没有这个能力标签"的任务被发过去——那是最危险的一类调度。
package scheduling

import (
	"sort"
	"strings"
)

// NodeCandidate 一个候选节点的观测快照（由调用方填充）。
type NodeCandidate struct {
	// UUID 节点唯一标识（排序的最终 tie-break，保证确定性）。
	UUID string
	// Hostname 仅用于可读说明。
	Hostname string
	// Labels 能力/位置标签（如 env=prod、zone=az1、tool:docker=""）。
	Labels map[string]string
	// Capacity 并发上限；<=0 表示未声明（按"未声明"中性处理，不当作 0）。
	Capacity int
	// Load 当前负载（通常是在跑/已分派的任务数）。
	Load int
	// SuccessRate 近期成功率（0~1）；Samples<=0 时无意义。
	SuccessRate float64
	// Samples 近期样本数（样本太少时健康度不参与打分，避免噪声决定调度）。
	Samples int
	// Reachable 节点当前是否可达（false 直接拒绝）。
	Reachable bool
	// Weight 节点自身的权重（吞吐越大越高的正数）；<=0 视为 1。
	Weight float64
}

// Requirement 任务侧的调度要求（由调用方从 job 推导）。
type Requirement struct {
	// RequireLabels 必须匹配的标签（全部子集成立才通过）。
	RequireLabels map[string]string
	// RequireTools 必须存在的能力标签（形如 tool:docker，存在即满足）。
	RequireTools []string
	// PreferNode 亲和节点（例如故障转移前的原节点、或任务显式声明的 run_on）。
	PreferNode string
}

// Weights 各维度权重。默认值见 DefaultWeights；调用方可覆盖。
type Weights struct {
	Load     float64 `json:"load"`     // 负载（越高越偏好空闲）
	Capacity float64 `json:"capacity"` // 容量（未达容量上限的同负载下，容量大者优先）
	Affinity float64 `json:"affinity"` // 亲和（命中 PreferNode）
	Success  float64 `json:"success"`  // 健康度（近期成功率）
}

// DefaultWeights 默认权重：负载最重要，其次是亲和与健康度，容量作为负载归一的分母。
func DefaultWeights() Weights {
	return Weights{Load: 60, Capacity: 10, Affinity: 15, Success: 15}
}

// Score 单个节点的打分结果（含分项，供"为什么选它"复盘）。
type Score struct {
	UUID     string             `json:"uuid"`
	Hostname string             `json:"hostname,omitempty"`
	Total    float64            `json:"total"`
	Parts    map[string]float64 `json:"parts"`
	Reasons  []string           `json:"reasons,omitempty"`
}

// Reject 被硬约束拒绝的节点（附可读原因）。
type Reject struct {
	UUID   string `json:"uuid"`
	Reason string `json:"reason"`
}

// Decision 一次调度决策的完整结果（可解释、可复现）。
type Decision struct {
	Chosen   string   `json:"chosen"`
	Scores   []Score  `json:"scores"`
	Rejected []Reject `json:"rejected,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// chooseDefaultWeights 保持纯函数：调用方传零值 Weights 时用默认。
func normalizeWeights(w Weights) Weights {
	d := DefaultWeights()
	if w == (Weights{}) {
		return d
	}
	return w
}

// Decide 对候选节点打分并选出最优节点。
//
// 返回的 Decision 里：Rejected = 不满足硬约束的；Scores = 通过硬约束的（按分数降序）。
// Chosen 为空表示**没有任何可用节点**（调用方必须据此拒绝执行，而不是"随便挑一个"）。
func Decide(req Requirement, cands []NodeCandidate, w Weights) Decision {
	w = normalizeWeights(w)
	d := Decision{}
	admitted := make([]NodeCandidate, 0, len(cands))
	for _, c := range cands {
		if reject, why := hardReject(req, c); reject {
			d.Rejected = append(d.Rejected, Reject{UUID: c.UUID, Reason: why})
			continue
		}
		admitted = append(admitted, c)
	}
	for _, c := range admitted {
		d.Scores = append(d.Scores, scoreOne(req, c, w))
	}
	// 稳定排序：分数降序，同分按 UUID 升序（确定性）。
	sort.SliceStable(d.Scores, func(i, j int) bool {
		if d.Scores[i].Total != d.Scores[j].Total {
			return d.Scores[i].Total > d.Scores[j].Total
		}
		return d.Scores[i].UUID < d.Scores[j].UUID
	})
	if len(d.Scores) > 0 {
		d.Chosen = d.Scores[0].UUID
	} else if len(cands) == 0 {
		d.Notes = append(d.Notes, "没有候选节点：调度器无从选择")
	} else {
		d.Notes = append(d.Notes, "所有候选节点都被硬约束拒绝：宁可不派发，也不派到不满足条件的节点")
	}
	return d
}

// hardReject 判定硬约束：可达性 + 标签子集 + 能力标签存在。
func hardReject(req Requirement, c NodeCandidate) (bool, string) {
	if !c.Reachable {
		return true, "节点不可达"
	}
	for k, v := range req.RequireLabels {
		if c.Labels[k] != v {
			return true, "缺少必需标签 " + k + "=" + v
		}
	}
	for _, t := range req.RequireTools {
		key := t
		if !strings.HasPrefix(key, "tool:") {
			key = "tool:" + key
		}
		if _, ok := c.Labels[key]; !ok {
			return true, "缺少能力 " + key
		}
	}
	return false, ""
}

// scoreOne 计算单节点分数（各项都归一化到 0~1，再加权）。
func scoreOne(req Requirement, c NodeCandidate, w Weights) Score {
	parts := map[string]float64{}
	reasons := []string{}

	weight := c.Weight
	if weight <= 0 {
		weight = 1
	}

	// 负载分：负载 / 容量，越小越好。容量未声明（<=0）时按"负载本身"算，
	// 但仍归一化到 (0,1]：用 1/(1+load) 兜住"没有容量信息"的情况。
	loadRatio := 0.0
	if c.Capacity > 0 {
		loadRatio = float64(c.Load) / float64(c.Capacity)
	} else {
		loadRatio = 1 - 1/(1+float64(c.Load))
	}
	if loadRatio > 1 {
		loadRatio = 1
	}
	loadScore := (1 - loadRatio) * w.Load
	parts["load"] = loadScore
	if c.Capacity > 0 {
		reasons = append(reasons, "负载 "+itoa(c.Load)+"/"+itoa(c.Capacity))
	} else {
		reasons = append(reasons, "负载 "+itoa(c.Load)+"（未声明容量）")
	}

	// 容量分：容量越大越好（在都没打满时），归一化到 (0,1]。
	capScore := 0.0
	if c.Capacity > 0 {
		capScore = (1 - 1/(1+float64(c.Capacity)*weight)) * w.Capacity
	} else {
		capScore = 0.5 * w.Capacity
	}
	parts["capacity"] = capScore

	// 亲和分：命中 PreferNode。
	if req.PreferNode != "" && c.UUID == req.PreferNode {
		parts["affinity"] = w.Affinity
		reasons = append(reasons, "命中亲和节点（原节点）")
	} else {
		parts["affinity"] = 0
	}

	// 健康度分：样本足够（>=3）时按成功率；否则中性 0.5。
	success := 0.5
	if c.Samples >= 3 {
		success = c.SuccessRate
		if success < 0 {
			success = 0
		}
		if success > 1 {
			success = 1
		}
		reasons = append(reasons, "近期成功率 "+pct(success)+"（"+itoa(c.Samples)+" 样本）")
	}
	parts["success"] = success * w.Success

	total := 0.0
	for _, v := range parts {
		total += v
	}
	return Score{UUID: c.UUID, Hostname: c.Hostname, Total: total, Parts: parts, Reasons: reasons}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func pct(r float64) string {
	n := int(r*100 + 0.5)
	return itoa(n) + "%"
}
