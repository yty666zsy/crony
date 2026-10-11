package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/tmnhs/crony/common/models"
)

// 阶段 4-B：多步骤任务编排（DAG）。
//
// 设计原则（三条，缺一不可）：
//
//  1. **不改变已有行为**。只有当 Plan 里**有步骤显式声明了 DependsOn** 时才走 DAG 引擎；
//     否则一律走阶段 1 的线性引擎（`Runner.Execute`）。老任务、老计划、老测试都不受影响。
//  2. **执行前就能拒绝坏图**。索引重复、依赖不存在的步骤、自依赖、成环、并发上限为负
//     —— 这些都在 `Plan.Validate()` 里报错，**不允许"跑到一半才发现图是坏的"**。
//     半执行的 run 在运维上比不执行更糟：它留下了一堆没人解释得清的副作用。
//  3. **每一步为什么没跑，必须能说清**。被跳过的步骤一律带 `skip_reason`：
//     是哪个上游失败了、是 fail_fast 急停、还是取消；并且**触发 OnStepFinish 落库**
//     （否则 job_run_step 里连这一行都没有，"跳过"这件事本身就不存在）。
//     "静默跳过"是本项目明确不允许的形态（与 denied/blocked 必须留原因同一条规矩）。
//
// 执行模型：**按轮次推进**（每轮把当前"就绪"的步骤一批跑掉，等这一批全部结束再进下一轮）。
// 选择轮次制而不是"完成即补位"的原因：结果可复现、并发上限一眼可验证、调试时不会出现
// "同一张图两次跑出不同交错顺序"。代价是失去一部分流水线重叠——对本项目的任务形态
// （几分钟级的批处理步骤）可以忽略。
//
// fail_fast 的判定点是**启动那一刻**（不是轮次边界）：同一轮里被并发上限挡住、还没
// fork 进程的步骤同样会看到"他处已失败"而不再启动。否则急停的实际效果会随
// max_parallel 变化——并发度与急停是两件互不相关的事。

// JoinPolicy 汇聚策略：一个步骤的多个上游都终结之后，它到底该不该执行。
type JoinPolicy string

const (
	// JoinAllSuccess 所有上游都成功才执行（默认）。
	JoinAllSuccess JoinPolicy = "all_success"
	// JoinAnySuccess 任一上游成功就执行（典型用途：多源取数，有一条通就行）。
	JoinAnySuccess JoinPolicy = "any_success"
	// JoinAllDone 上游终结就执行，不关心成败（典型用途：汇总、清理、释放锁）。
	JoinAllDone JoinPolicy = "all_done"
)

// ParseJoin 解析汇聚策略（大小写不敏感），空串默认 all_success。
func ParseJoin(raw string) (JoinPolicy, bool) {
	switch JoinPolicy(strings.ToLower(strings.TrimSpace(raw))) {
	case "":
		return JoinAllSuccess, true
	case JoinAllSuccess:
		return JoinAllSuccess, true
	case JoinAnySuccess:
		return JoinAnySuccess, true
	case JoinAllDone:
		return JoinAllDone, true
	default:
		return JoinAllSuccess, false
	}
}

// IsDAG 判断计划是否需要走 DAG 引擎。
//
// 判据是**显式依赖**而不是步骤数量：3 个步骤但没人声明依赖时，它就是原来的线性计划
// （否则"给老任务加两个步骤"会悄悄改变失败传播语义）。
func (p *Plan) IsDAG() bool {
	for _, s := range p.Steps {
		if len(s.DependsOn) > 0 {
			return true
		}
	}
	return false
}

// Validate 在执行前校验编排图。
//
// 返回的错误一律包含**具体是哪个步骤/哪条环**，因为调用方会把它直接回显到接口
// （控制台编辑器要指到出错的那一行）。
func (p *Plan) Validate() error {
	if len(p.Steps) == 0 {
		return fmt.Errorf("编排为空：至少需要一个步骤")
	}
	posByIndex := make(map[int]int, len(p.Steps))
	for pos, s := range p.Steps {
		if _, dup := posByIndex[s.Index]; dup {
			return fmt.Errorf("步骤索引重复：%d（每步的 index 必须唯一）", s.Index)
		}
		posByIndex[s.Index] = pos
		if s.Index < 0 {
			return fmt.Errorf("步骤索引非法：%d（不允许负数）", s.Index)
		}
		if _, ok := ParseCondition(string(s.Condition)); !ok {
			return fmt.Errorf("步骤 %d 的条件非法：%q（可选 on_success/on_failure/always）", s.Index, s.Condition)
		}
		if _, ok := ParseJoin(string(s.Join)); !ok {
			return fmt.Errorf("步骤 %d 的汇聚策略非法：%q（可选 all_success/any_success/all_done）", s.Index, s.Join)
		}
	}
	for _, s := range p.Steps {
		for _, dep := range s.DependsOn {
			if dep == s.Index {
				return fmt.Errorf("步骤 %d 依赖了自身", s.Index)
			}
			if _, ok := posByIndex[dep]; !ok {
				return fmt.Errorf("步骤 %d 依赖了不存在的步骤 %d", s.Index, dep)
			}
		}
	}
	if p.MaxParallel < 0 {
		return fmt.Errorf("并发上限不能为负：%d", p.MaxParallel)
	}
	return p.checkCycle(posByIndex)
}

// checkCycle 用 DFS 找环，并在报错里给出环上的完整路径。
//
// 为什么不用 Kahn 只报"有环"：图是运维写的，只说"有环"等于把找环的活丢回给人。
// 报 "2 load → 5 verify → 2 load" 才能直接改。
func (p *Plan) checkCycle(posByIndex map[int]int) error {
	const (
		white = 0 // 未访问
		gray  = 1 // 在当前递归栈上
		black = 2 // 已完成
	)
	color := make(map[int]int, len(p.Steps))
	var stack []int

	var visit func(idx int) error
	visit = func(idx int) error {
		color[idx] = gray
		stack = append(stack, idx)
		for _, dep := range p.Steps[posByIndex[idx]].DependsOn {
			switch color[dep] {
			case gray:
				// 找到环：从栈里截取 dep 之后的路径
				cycle := make([]string, 0, len(stack)+1)
				start := 0
				for i, v := range stack {
					if v == dep {
						start = i
						break
					}
				}
				for _, v := range stack[start:] {
					cycle = append(cycle, describeStep(p.Steps[posByIndex[v]]))
				}
				cycle = append(cycle, describeStep(p.Steps[posByIndex[dep]]))
				return fmt.Errorf("编排存在环：%s", strings.Join(cycle, " → "))
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[idx] = black
		return nil
	}

	// 按 Steps 顺序遍历，保证同一张坏图每次报同一个环（可复现的报错）。
	for _, s := range p.Steps {
		if color[s.Index] == white {
			if err := visit(s.Index); err != nil {
				return err
			}
		}
	}
	return nil
}

func describeStep(s Step) string {
	if strings.TrimSpace(s.Name) != "" {
		return fmt.Sprintf("%d %s", s.Index, s.Name)
	}
	return fmt.Sprintf("%d", s.Index)
}

// executeDAG 执行 DAG 计划，返回与 plan.Steps 顺序一致的结果切片。
//
// 顺序恒定（不是完成顺序）是有意为之：`job_run_step` 按 step_index 落库、控制台按
// step_index 渲染，结果切片一旦按完成顺序返回，调用方就得自己排序——那是接口在泄漏实现。
func (r *Runner) executeDAG(ctx context.Context, plan Plan) []Result {
	n := len(plan.Steps)
	results := make([]Result, n)

	posByIndex := make(map[int]int, n)
	for pos, s := range plan.Steps {
		posByIndex[s.Index] = pos
	}
	depPos := make([][]int, n)
	for pos, s := range plan.Steps {
		for _, dep := range s.DependsOn {
			depPos[pos] = append(depPos[pos], posByIndex[dep])
		}
	}

	parallel := plan.MaxParallel
	if parallel <= 0 {
		parallel = 1
	}
	if parallel > n {
		parallel = n
	}

	done := make([]bool, n)
	status := make([]models.RunStatus, n)
	remaining := n
	aborted := false

	for remaining > 0 {
		if ctx.Err() != nil {
			for pos := range plan.Steps {
				if !done[pos] {
					results[pos] = r.recordSkipped(plan.Steps[pos], fmt.Sprintf("执行已取消: %v", ctx.Err()))
					done[pos] = true
					status[pos] = models.RunStatusSkipped
					remaining--
				}
			}
			break
		}

		// ---- 1) 选出本轮"可以跑"和"应判跳过"的步骤 ----
		runPos := make([]int, 0, n)
		progressed := false
		for pos, s := range plan.Steps {
			if done[pos] {
				continue
			}
			ready := true
			for _, d := range depPos[pos] {
				if !done[d] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			if aborted && s.Join != JoinAllDone && s.Condition != ConditionAlways {
				// fail_fast 急停：只放行"无论如何都要跑"的收尾步骤。
				results[pos] = r.recordSkipped(s, "已中止：他处先失败（fail_fast）")
				done[pos] = true
				status[pos] = models.RunStatusSkipped
				remaining--
				progressed = true
				continue
			}
			ok, reason := joinSatisfied(s, depPos[pos], status)
			if !ok {
				results[pos] = r.recordSkipped(s, reason)
				done[pos] = true
				status[pos] = models.RunStatusSkipped
				remaining--
				progressed = true
				continue
			}
			runPos = append(runPos, pos)
		}

		if len(runPos) == 0 {
			if !progressed {
				// 理论上不可达（图已验证无环），保留兜底：宁可全部记 skipped 并说明，
				// 也不能让调度循环空转成死锁。
				for pos, s := range plan.Steps {
					if !done[pos] {
						results[pos] = r.recordSkipped(s, "编排无法推进：依赖关系不自洽（请用 validate 重新校验）")
						done[pos] = true
						status[pos] = models.RunStatusSkipped
						remaining--
					}
				}
			}
			continue
		}

		// ---- 2) 本轮批量执行（并发上限 = parallel） ----
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			sem     = make(chan struct{}, parallel)
			roundOK = true
		)
		for _, pos := range runPos {
			wg.Add(1)
			sem <- struct{}{}
			go func(pos int) {
				defer wg.Done()
				defer func() { <-sem }()
				step := plan.Steps[pos]
				// fail_fast 必须在**真正启动前**判定，而不是只在本轮结束后判定：
				// 并发上限会把同一轮里排在后面的步骤挡在门外（还没 fork 进程），
				// 它们与"下一轮才轮到"的步骤没有实质区别。只在轮次边界急停会让
				// fail_fast 的实际效果随 max_parallel 变化——而"并发多大"和"要不要急停"
				// 是两件互不相关的事，运维按 max_parallel=1 配的急停必须真的急停。
				mu.Lock()
				stopped := plan.FailFast && aborted &&
					step.Join != JoinAllDone && step.Condition != ConditionAlways
				mu.Unlock()
				if stopped {
					res := r.recordSkipped(step, "已中止：他处先失败（fail_fast）")
					mu.Lock()
					results[pos] = res
					status[pos] = models.RunStatusSkipped
					done[pos] = true
					remaining--
					mu.Unlock()
					return
				}
				res := r.runStep(ctx, plan, step)
				mu.Lock()
				results[pos] = res
				status[pos] = res.Status
				done[pos] = true
				remaining--
				if res.Status != models.RunStatusSuccess && res.Status != models.RunStatusSkipped {
					roundOK = false
					// 立刻置位（而不是等本轮结束）：让同一轮里尚未启动的步骤也能看到失败。
					if plan.FailFast {
						aborted = true
					}
				}
				mu.Unlock()
			}(pos)
		}
		wg.Wait()

		if plan.FailFast && !roundOK {
			aborted = true
		}
	}

	return results
}

// joinSatisfied 判定一个步骤的汇聚条件是否满足，并给出不满足时的原因。
//
// 优先级：**显式 Join 优先**；未设置 Join 时由阶段 1 的 Condition 决定语义
// （on_failure → 任一上游失败即执行；always → 上游终结即执行；on_success → 全成功）。
// 这样"条件分支"（阶段 1 的概念）和"汇聚策略"（4-B 的概念）在 DAG 里是同一件事的两种写法，
// 不会出现两套互相矛盾的判定。
func joinSatisfied(s Step, deps []int, status []models.RunStatus) (bool, string) {
	if len(deps) == 0 {
		return true, "" // 根步骤：没有上游，直接跑
	}
	succeeded, failed, finished := 0, 0, 0
	for _, d := range deps {
		finished++
		switch status[d] {
		case models.RunStatusSuccess:
			succeeded++
		case models.RunStatusSkipped:
			// 跳过的上游既不算成功也不算失败：它什么都没做，不能用来满足 any_success。
		default:
			failed++
		}
	}

	// Condition 优先用于 on_failure / always（这两个是阶段 1 就有的显式意图）。
	if s.Condition == ConditionOnFailure && s.Join == "" {
		if failed > 0 {
			return true, ""
		}
		return false, "无上游失败，跳过失败处理步骤"
	}
	if s.Condition == ConditionAlways && s.Join == "" {
		return true, ""
	}

	join, _ := ParseJoin(string(s.Join))
	switch join {
	case JoinAllDone:
		return true, ""
	case JoinAnySuccess:
		if succeeded > 0 {
			return true, ""
		}
		return false, fmt.Sprintf("%d 个上游没有一个成功（any_success 汇聚不满足）", len(deps))
	default: // JoinAllSuccess
		if succeeded == len(deps) {
			return true, ""
		}
		return false, fmt.Sprintf(
			"上游未全部成功：成功 %d / 失败 %d / 跳过 %d（all_success 汇聚不满足）",
			succeeded, failed, finished-succeeded-failed)
	}
}
