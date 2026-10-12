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
