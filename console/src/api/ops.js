/**
 * 运维智能化接口（阶段 4-D / 阶段 5）。
 *
 * 与后端 `admin/internal/handler/ops.go` 逐条对齐（HTTP 200 + {code,data,msg}）：
 *   POST /ops/scheduler/policy    读调度策略（权重与硬约束）
 *   POST /ops/scheduler/allocate  展示"若分派该任务会选哪个节点、为什么"（只读解释）
 *   POST /ops/slo/search          列出 SLO 契约
 *   POST /ops/slo/save            新建/更新 SLO 契约
 *   POST /ops/slo/evaluate        评估契约（只回报事实，不触发动作）
 *   POST /ops/metrics/overview    核心指标汇总
 *   POST /ops/eval/score          对一次会话轨迹做确定性回归打分
 *   POST /ops/eval/cases          列出评测样本（种子评测集 + 手工样本）
 *   POST /ops/eval/runs           列出最近若干次跑批的汇总（历次跑批选择器）
 *   POST /ops/eval/seed/sync      把内置种子评测集同步进库（幂等）
 *   POST /ops/eval/run            对已录制的真实轨迹回放跑批
 *   POST /ops/eval/report         取一次跑批报告（run_id<=0 取最近一次）
 *   POST /ops/autonomy/list       自治信用列表（含实际生效等级）
 *   POST /ops/autonomy/observe    记录一次动作结果，推进信用
 *   POST /ops/autonomy/unfreeze   人工解冻
 *   POST /ops/autonomy/effective  解释某工具当下实际生效的自治级别（配置上限 ∩ 信用）
 *   POST /ops/chaos/catalog       混沌故障目录
 *   POST /ops/chaos/grade         混沌考核打分（驾照分）
 *   POST /ops/shadow/reconcile    影子执行对账（只读计算）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

export function fetchSchedulerPolicy() {
  if (USE_MOCK) return mockResponse(mock.schedulerPolicy())
  return request.post('/ops/scheduler/policy', {})
}

export function explainAllocation(jobId) {
  if (USE_MOCK) return mockResponse(mock.schedulerAllocate({ job_id: jobId }))
  return request.post('/ops/scheduler/allocate', { job_id: jobId })
}

export function searchSlo(jobId = 0) {
  if (USE_MOCK) return mockResponse(mock.sloList())
  return request.post('/ops/slo/search', { job_id: jobId })
}

export function saveSlo(policy) {
  if (USE_MOCK) return mockResponse({ code: 200, data: { policy }, msg: 'saved' })
  return request.post('/ops/slo/save', policy)
}

export function evaluateSlo(jobId = 0) {
  if (USE_MOCK) return mockResponse(mock.sloEvaluate({ job_id: jobId }))
  return request.post('/ops/slo/evaluate', { job_id: jobId })
}

export function fetchMetricsOverview() {
  if (USE_MOCK) return mockResponse(mock.opsMetrics())
  return request.post('/ops/metrics/overview', {})
}

export function scoreSession(sessionId, expectedTools = [], maxSteps = 0) {
  if (USE_MOCK) return mockResponse({ code: 200, data: { result: { case_id: sessionId, passed: true, score: 1, checks: [{ name: 'tool-sequence', passed: true, detail: '序列匹配：[log_search,node_status]' }] } }, msg: 'ok' })
  return request.post('/ops/eval/score', { session_id: sessionId, expected_tools: expectedTools, max_steps: maxSteps })
}

/* ---------------------- 回放评测台（阶段 5） ---------------------- */

// 列出评测样本。样本期望值全部可机械判定（没有 LLM 评委）。
export function listEvalCases(setName = '') {
  if (USE_MOCK) return mockResponse(mock.evalCases({ set_name: setName }))
  return request.post('/ops/eval/cases', { set_name: setName })
}

// 历次跑批的汇总（不含逐样本明细）。列表只够"挑一次看"，明细走 getEvalReport。
export function listEvalRuns(limit = 20) {
  if (USE_MOCK) return mockResponse(mock.evalRuns({ limit }))
  return request.post('/ops/eval/runs', { limit })
}

// 把内置种子评测集同步进库（幂等：按 case_key）。已存在样本的启停以库里为准。
export function syncEvalSeed() {
  if (USE_MOCK) return mockResponse(mock.evalSeedSync({}))
  return request.post('/ops/eval/seed/sync', {})
}

// 跑一次回放。**不调用模型**：回放对象是库里 status=completed 的历史会话。
export function runEvalBench({ setName = '', limitPerCase = 0, trigger = 'manual', persist = true } = {}) {
  if (USE_MOCK) {
    return mockResponse(mock.evalRun({ set_name: setName, limit_per_case: limitPerCase, trigger, persist }))
  }
  return request.post('/ops/eval/run', {
    set_name: setName, limit_per_case: limitPerCase, trigger, persist,
  })
}

// 取一次跑批报告（run_id<=0 取最近一次）。
export function getEvalReport(runId = 0) {
  if (USE_MOCK) return mockResponse(mock.evalReport({ run_id: runId }))
  return request.post('/ops/eval/report', { run_id: runId })
}

export function listAutonomy() {
  if (USE_MOCK) return mockResponse(mock.autonomyList())
  return request.post('/ops/autonomy/list', {})
}

export function observeAutonomy(scope, success) {
  if (USE_MOCK) return mockResponse(mock.autonomyObserve({ scope, success }))
  return request.post('/ops/autonomy/observe', { scope, success })
}

export function unfreezeAutonomy(scope) {
  if (USE_MOCK) return mockResponse(mock.autonomyUnfreeze({ scope }))
  return request.post('/ops/autonomy/unfreeze', { scope })
}

export function effectiveAutonomy(tool, env = '') {
  if (USE_MOCK) return mockResponse(mock.autonomyEffective({ tool, env }))
  return request.post('/ops/autonomy/effective', { tool, env })
}

export function fetchChaosCatalog() {
  if (USE_MOCK) return mockResponse(mock.chaosCatalog())
  return request.post('/ops/chaos/catalog', {})
}

export function gradeChaos(attempts) {
  if (USE_MOCK) return mockResponse(mock.chaosGrade({ attempts }))
  return request.post('/ops/chaos/grade', { attempts })
}

export function reconcileShadow(baseline, candidate) {
  if (USE_MOCK) return mockResponse(mock.shadowReconcile({ baseline, candidate }))
  return request.post('/ops/shadow/reconcile', { baseline, candidate })
}
