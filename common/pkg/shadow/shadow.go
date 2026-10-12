// Package shadow 实现阶段 5 的「影子执行 + 结果对账」（方案 §8-1 Shadow Run）。
//
// 机制：新版本的脚本 / Agent 生成的动作先在**影子环境**里执行，采集
// "输出 + 副作用清单（文件、进程、DB 行变更、网络连接）"，与基线做 diff；
// 只有**零意外差异**才允许放量。它把"AI 生成的运维动作"变成可 A/B 的软件交付物，
// 而不是"点了就跑"。
//
// 本包只做**对账**（纯函数）。真正的影子执行（起影子容器、采集副作用）由基础设施提供，
// 采集到的两份 Observation 交给这里比对。这样"什么算意外差异"这条判据可以被穷举测试。
package shadow

import (
	"sort"
	"strings"
)

// SideEffect 一条副作用记录（键值对形式，便于跨环境比较）。
type SideEffect struct {
	// Kind 类别：file / process / dbrow / net。
	Kind string `json:"kind"`
	// Key 受影响对象的标识（文件路径 / 进程名 / 表:主键 / host:port）。
	Key string `json:"key"`
	// Change 变更描述（created/deleted/modified / started/stopped / +n rows…）。
	Change string `json:"change"`
}

// Observation 一次执行的可观测结果（基线或影子）。
type Observation struct {
	ExitCode   int          `json:"exit_code"`
	StdoutHash string       `json:"stdout_hash"`
	StderrHash string       `json:"stderr_hash"`
	SideEffects []SideEffect `json:"side_effects"`
}

// Diff 对账结果。
type Diff struct {
	// Same 是否零差异（退出码、stdout、stderr、副作用集合全等）。
	Same bool `json:"same"`
	// Differences 逐条差异（人可读）。
	Differences []string `json:"differences,omitempty"`
	// Unexpected 是否为"意外差异"（放量的拦截判据）。
	Unexpected bool `json:"unexpected"`
}

// Reconcile 对账基线 vs 影子。
//
// 判据（刻意严格）：
//   - 退出码必须一致；
//   - stdout / stderr 哈希必须一致（有输出差异就是差异，不"看情况"）；
//   - 副作用集合必须**完全相同**（多一条少一条都算差异）。
//
// 只要有任何一条不一致即 Unexpected=true。这是放量的硬门槛：
// "看起来差不多"不是放量理由，"逐条对得上"才是。
func Reconcile(baseline, candidate Observation) Diff {
	d := Diff{}
	if baseline.ExitCode != candidate.ExitCode {
		d.Differences = append(d.Differences, "退出码不同："+itoa(baseline.ExitCode)+" → "+itoa(candidate.ExitCode))
	}
	if baseline.StdoutHash != candidate.StdoutHash {
		d.Differences = append(d.Differences, "stdout 不同（基线 "+short(baseline.StdoutHash)+" / 影子 "+short(candidate.StdoutHash)+"）")
	}
	if baseline.StderrHash != candidate.StderrHash {
		d.Differences = append(d.Differences, "stderr 不同")
	}
	d.Differences = append(d.Differences, diffEffects(baseline.SideEffects, candidate.SideEffects)...)
	d.Same = len(d.Differences) == 0
	d.Unexpected = !d.Same
	return d
}

// diffEffects 计算副作用集合差异（按 kind|key|change 归一去重后比对）。
func diffEffects(a, b []SideEffect) []string {
	am := effectSet(a)
	bm := effectSet(b)
	out := []string{}
	for k := range am {
		if _, ok := bm[k]; !ok {
			out = append(out, "基线有而影子没有："+k)
		}
	}
	for k := range bm {
		if _, ok := am[k]; !ok {
			out = append(out, "影子多出副作用："+k)
		}
	}
	sort.Strings(out)
	return out
}

func effectSet(s []SideEffect) map[string]struct{} {
	m := map[string]struct{}{}
	for _, e := range s {
		m[strings.TrimSpace(e.Kind+"|"+e.Key+"|"+e.Change)] = struct{}{}
	}
	return m
}

// AllowPromotion 是否允许放量（零意外差异）。
func AllowPromotion(d Diff) bool { return d.Same && !d.Unexpected }

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	if s == "" {
		return "(空)"
	}
	return s
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
