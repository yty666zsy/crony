// Package selfheal 实现阶段 4-C「自愈调度」的**决策内核**：把一次失败的执行
// 分类成可枚举的失败类型，再在白名单动作里挑一个，最后由策略闸门决定"能不能真动手"。
//
// 为什么把决策与执行分开（本包不含任何数据库/网络/etcd 调用）：
//  1. 自愈是写动作，决策规则必须能被**穷举测试**——每种失败类型、每种策略组合都该有断言；
//  2. 决策要能被复现：出事后要看的是"当时依据什么规则选了这一步"，
//     而不是"当时那个 goroutine 恰好读到了什么"。
//
// 一句话原则：**规则决定候选，历史（经验库）只能改排序，模型不能发明动作。**
package selfheal

import (
	"strings"

	"github.com/tmnhs/crony/common/models"
)

// FailureClass 失败分类（确定性规则给出）。
//
// 分类的作用不是"给失败起个名"，而是**决定有哪些安全动作可用**：
// 节点不可达时重跑同一节点是徒劳，编排非法时重跑一百次也一样非法。
type FailureClass string

const (
	// ClassNodeUnreachable 目标节点不可达/连接异常。
	ClassNodeUnreachable FailureClass = "node_unreachable"
	// ClassTimeout 执行超时。
	ClassTimeout FailureClass = "timeout"
	// ClassCommandFailed 命令本身失败（退出码非 0 / 有失败步骤）。
	ClassCommandFailed FailureClass = "command_failed"
	// ClassDeliveryFailed 投递失败（etcd 写入/网络），任务根本没被节点接到。
	ClassDeliveryFailed FailureClass = "delivery_failed"
	// ClassOrchestrationInvalid 编排定义非法：重跑无意义，必须改定义。
	ClassOrchestrationInvalid FailureClass = "orchestration_invalid"
	// ClassUnknown 无法归类：不做自动动作，直接转人工。
	ClassUnknown FailureClass = "unknown"
)

// classNames 中文解释（控制台与报告直接使用）。
var classNames = map[FailureClass]string{
	ClassNodeUnreachable:      "目标节点不可达",
	ClassTimeout:              "执行超时",
	ClassCommandFailed:        "命令执行失败",
	ClassDeliveryFailed:       "任务投递失败",
	ClassOrchestrationInvalid: "编排定义非法",
	ClassUnknown:              "未归类失败",
}

// String 实现 fmt.Stringer（返回中文，因为它的读者是运维）。
func (c FailureClass) String() string {
	if n, ok := classNames[c]; ok {
		return n
	}
	return string(c)
}

// Classification 分类结果：类型 + 人可读判据 + 观测证据。
//
// Evidence 只允许放**本系统自己的观测**（run 行、步骤行、节点状态），
// 不放解释性文字——证据要能被别处复核。
type Classification struct {
	Class      FailureClass
	Reason     string
	Evidence   map[string]string
	SymptomKey string
}

// ClassifyInput 分类所需的全部输入（显式传参，便于穷举测试）。
type ClassifyInput struct {
	Run *models.JobRun
	// Steps 该次执行的步骤明细（可为空：单步任务或未落步骤）。
	Steps []models.JobRunStep
	// NodeReachable 目标节点当前是否可达（由调用方查节点状态后传入；
	// nil 表示"未知"，此时不据此判类——未知不该被当成不可达）。
	NodeReachable *bool
}

// Classify 按**固定优先级**分类失败。
//
// 优先级为什么这样排（顺序即语义）：
//  1. 编排非法：这条最特殊——不修定义永远跑不通，必须先判出来，避免把它当成普通失败去重试；
//  2. 超时：run 状态是 timeout，或错误文本是超时；超时有明确的安全动作（调大上限后重跑）；
//  3. 投递失败：任务压根没到节点，重跑是"再投一次"，与"命令失败"处置不同；
//  4. 节点不可达：节点层面问题，重跑同节点无意义；
//  5. 命令失败：最常见，重跑同节点是标准动作；
//  6. 其余：unknown，转人工。
//
// 为什么把"投递失败"排在"节点不可达"之前：投递失败的错误文本里也可能出现节点信息，
// 但它的事实是"控制面没投出去"，处置在控制面；顺序反了会导致把控制面问题误判成节点问题。
func Classify(in ClassifyInput) Classification {
	ev := map[string]string{}
	if in.Run == nil {
		return Classification{Class: ClassUnknown, Reason: "没有执行记录可判定（run 为空）", Evidence: ev}
	}
	run := in.Run
	text := strings.ToLower(strings.TrimSpace(run.ErrorMsg))
	ev["run_id"] = run.RunID
	ev["job_id"] = itoa(run.JobID)
	ev["status"] = models.RunStatus(run.Status).String()
	ev["exit_code"] = itoa(run.ExitCode)
	ev["attempt"] = itoa(run.Attempt)
	if text != "" {
		ev["error_msg"] = clip(run.ErrorMsg, 300)
	}
	if run.NodeUUID != "" {
		ev["node_uuid"] = run.NodeUUID
	}

	// 1. 编排非法（含"已拒绝执行"的中文原因）
	if containsAny(text, "编排定义非法", "编排非法", "存在环", "orchestration") {
		return Classification{
			Class:    ClassOrchestrationInvalid,
			Reason:   "编排定义被判定为非法：重跑不会改变结论，必须修改任务定义",
			Evidence: ev, SymptomKey: "编排非法",
		}
	}
	// 2. 超时
	if models.RunStatus(run.Status) == models.RunStatusTimeout || containsAny(text, "超时", "timeout", "deadline exceeded") {
		return Classification{
			Class:    ClassTimeout,
			Reason:   "本次执行超时：可在受控幅度内调大超时上限后重跑",
			Evidence: ev, SymptomKey: "执行超时",
		}
	}
	// 3. 投递失败（控制面没投出去）
	if containsAny(text, "投递", "etcd", "put ", "put error", "deliver") {
		return Classification{
			Class:    ClassDeliveryFailed,
			Reason:   "任务没有被投递到节点（控制面投递环节失败）：可重新投递",
			Evidence: ev, SymptomKey: "投递失败",
		}
	}
	// 4. 节点不可达（显式观测优先于文本）
	if in.NodeReachable != nil && !*in.NodeReachable {
		ev["node_reachable"] = "false"
		return Classification{
			Class:    ClassNodeUnreachable,
			Reason:   "目标节点当前不可达：在原节点重跑无效（换节点属于多目标调度，见阶段 4-D）",
			Evidence: ev, SymptomKey: "节点不可达",
		}
	}
	if containsAny(text, "connection refused", "conn fail", "连接异常", "连接失败", "no route to host") {
		return Classification{
			Class:    ClassNodeUnreachable,
			Reason:   "错误文本表明节点连接异常：在原节点重跑无效",
			Evidence: ev, SymptomKey: "节点不可达",
		}
	}
	// 5. 命令失败：退出码非 0 或存在失败步骤
	// 5. 命令失败：退出码非 0，或 run 计数说有失败步骤，或步骤明细里能看出失败。
	// 三者取或：run 的汇总列可能因为老数据/部分落库而缺失，步骤明细是更硬的事实。
	badStep := firstFailedStep(in.Steps)
	if run.ExitCode != 0 || run.FailedSteps > 0 || badStep != "" {
		ev["failed_steps"] = itoa(run.FailedSteps)
		if badStep != "" {
			ev["failed_step"] = badStep
		}
		return Classification{
			Class:    ClassCommandFailed,
			Reason:   "命令本身失败（退出码非 0 或存在失败步骤）：可在原节点重跑一次",
			Evidence: ev, SymptomKey: "命令失败",
		}
	}
	return Classification{
		Class:    ClassUnknown,
		Reason:   "没有足够观测把这次失败归类：不做自动动作，直接转人工",
		Evidence: ev, SymptomKey: "未归类失败",
	}
}

// firstFailedStep 找出第一个失败步骤的可读名（含索引），用于证据与症状文本。
func firstFailedStep(steps []models.JobRunStep) string {
	for _, s := range steps {
		if s.Status == int(models.RunStatusFailed) || s.Status == int(models.RunStatusTimeout) {
			name := strings.TrimSpace(s.Name)
			if name == "" {
				name = "未命名步骤"
			}
			return itoa(s.StepIndex) + " " + name
		}
	}
	return ""
}

// Symptom 组装人可读的症状（也是经验库召回的查询文本）。
//
// 形态固定为「任务 <名>(id=<id>) 失败：<类> —— <错误摘要>」，
// 这样同一类失败在不同任务上的症状文本高度相似，词元覆盖率召回才有效。
func Symptom(jobName string, jobID int, c Classification, run *models.JobRun) string {
	name := strings.TrimSpace(jobName)
	if name == "" {
		name = "未命名任务"
	}
	b := &strings.Builder{}
	b.WriteString("任务 ")
	b.WriteString(name)
	b.WriteString("(id=")
	b.WriteString(itoa(jobID))
	b.WriteString(") 失败：")
	b.WriteString(string(c.Class))
	if run != nil {
		if msg := strings.TrimSpace(run.ErrorMsg); msg != "" {
			b.WriteString(" —— ")
			b.WriteString(clip(msg, 200))
		}
	}
	return b.String()
}

func containsAny(text string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(text, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
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
