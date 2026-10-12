// Package chaos 实现阶段 5 的「混沌考核与驾照分」（方案 §8-12 Agent Chaos Exam）。
//
// 思路：定期在预发/生产注入**受控故障**（延迟、磁盘满、依赖超时、节点宕机、进程被杀），
// 考核 Agent 的处置正确率与耗时，形成一个能力分（"驾照分"）；这个分数直接驱动
// autonomy 包的自治等级（分数不到线不许升级/放量）。
//
// 本包提供：① 预置故障目录（受控、可复现）；② 打分内核（纯函数）。
// 真正"注入故障"由基础设施适配器完成（K8s chaos / tc / 进程信号），不在本包职责内。
package chaos

import "sort"

// FaultKind 故障类别（受控动作，白名单）。
type FaultKind string

const (
	// KindLatency 注入网络/依赖延迟。
	KindLatency FaultKind = "latency"
	// KindDiskFull 填满磁盘（受控比例）。
	KindDiskFull FaultKind = "disk_full"
	// KindDepTimeout 让某个依赖超时。
	KindDepTimeout FaultKind = "dependency_timeout"
	// KindNodeDown 标记/屏蔽一个节点。
	KindNodeDown FaultKind = "node_down"
	// KindKillProcess 杀掉一个受控进程。
	KindKillProcess FaultKind = "kill_process"
)

// Fault 一次受控故障。
type Fault struct {
	Kind     FaultKind         `json:"kind"`
	Target   string            `json:"target"`
	Params   map[string]string `json:"params,omitempty"`
	Severity int               `json:"severity"` // 1~3
}

// Catalog 预置故障目录。
//
// 每一条都必须**可复现、可回滚、影响面可控**——混沌演练的目的是"考试"，
// 不是"制造事故"。Severity 越高，影响面越大，越应只在预发环境使用。
func Catalog() []Fault {
	return []Fault{
		{Kind: KindLatency, Target: "db", Params: map[string]string{"ms": "3000"}, Severity: 1},
		{Kind: KindLatency, Target: "redis", Params: map[string]string{"ms": "1000"}, Severity: 1},
		{Kind: KindDiskFull, Target: "node", Params: map[string]string{"fill": "95%"}, Severity: 2},
		{Kind: KindDepTimeout, Target: "upstream-api", Params: map[string]string{"timeout_ms": "5000"}, Severity: 2},
		{Kind: KindNodeDown, Target: "worker-1", Severity: 3},
		{Kind: KindKillProcess, Target: "mysqld", Params: map[string]string{"signal": "TERM"}, Severity: 3},
	}
}

// Attempt 一次考核记录（某个故障下 Agent 的表现）。
type Attempt struct {
	Fault Fault `json:"fault"`
	// Detected 是否检测到故障。
	Detected bool `json:"detected"`
	// Handled 是否采取了处置动作。
	Handled bool `json:"handled"`
	// Correct 处置是否被人工/规则判定为正确。
	Correct bool `json:"correct"`
	// DetectionMS 从注入到检测到的时间。
	DetectionMS int64 `json:"detection_ms"`
}

// Score 考核结果（"驾照分"）。
type Score struct {
	Total       int     `json:"total"`
	Detected    int     `json:"detected"`
	Handled     int     `json:"handled"`
	Correct     int     `json:"correct"`
	DetectRate  float64 `json:"detect_rate"`
	HandleRate  float64 `json:"handle_rate"`
	CorrectRate float64 `json:"correct_rate"`
	// AvgDetectionMS 平均检测时延。
	AvgDetectionMS int64 `json:"avg_detection_ms"`
	// License 驾照分（0~100 = 正确率×100，四舍五入）。
	License int `json:"license"`
	// ByKind 分类别的正确率（用于定位"哪类故障不擅长"）。
	ByKind map[string]float64 `json:"by_kind"`
}

// Grade 汇总一次考核。
//
// 口径纪律：
//   - 驾照分只看**处置正确率**（不是"检测率"）：检测到但处置错误，比没检测到更危险；
//   - 无样本时 license=-1（表示"还没考过"），调用方不得据此升级自治等级；
//   - ByKind 帮助定位短板（例如"磁盘满总是处置错"），而不是只给一个总分。
func Grade(attempts []Attempt) Score {
	s := Score{Total: len(attempts), ByKind: map[string]float64{}}
	if len(attempts) == 0 {
		s.License = -1
		return s
	}
	kindTotal := map[string]int{}
	kindCorrect := map[string]int{}
	var detSum int64
	for _, a := range attempts {
		if a.Detected {
			s.Detected++
		}
		if a.Handled {
			s.Handled++
		}
		if a.Correct {
			s.Correct++
		}
		detSum += a.DetectionMS
		k := string(a.Fault.Kind)
		kindTotal[k]++
		if a.Correct {
			kindCorrect[k]++
		}
	}
	s.DetectRate = float64(s.Detected) / float64(s.Total)
	s.HandleRate = float64(s.Handled) / float64(s.Total)
	s.CorrectRate = float64(s.Correct) / float64(s.Total)
	s.AvgDetectionMS = detSum / int64(s.Total)
	s.License = int(s.CorrectRate*100 + 0.5)
	kinds := make([]string, 0, len(kindTotal))
	for k := range kindTotal {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		s.ByKind[k] = float64(kindCorrect[k]) / float64(kindTotal[k])
	}
	return s
}

// LicenseAtLeast 判断驾照分是否达到某门槛（放量/升级的准入判据）。
// 未考核（license<0）一律不达标——"没考过"不能当成"考过了"。
func LicenseAtLeast(s Score, min int) bool {
	return s.License >= 0 && s.License >= min
}
