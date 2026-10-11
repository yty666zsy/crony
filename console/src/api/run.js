/**
 * 执行记录（job_run / job_run_step）接口。
 *
 * 后端**已实现**（admin/internal/handler/router.go → /job 组，需要 JWTAuth）：
 *   POST /job/run/search     分页检索 job_run                 （router.go:80）
 *   GET  /job/run/find       按 run_id 取单条 job_run          （router.go:81）
 *   POST /job/run/steps      按 run_id 取步骤时间线            （router.go:82）
 *   GET  /job/run/overview   执行概览聚合（since 缺省近 24h）  （router.go:83）
 *   POST /job/kill           取消执行（阶段 1 已恢复）         （router.go:74）
 *
 * 说明：这些路由与 crony/common/models/run.go 的模型（幂等键、触发来源、状态机、
 * 策略快照、步骤明细）是同一批交付；Mock 侧返回与后端 DTO 同名同形的字段，
 * 因此 VITE_USE_MOCK=false 时页面代码无需改动。
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** POST /job/run/search → { list, total, page, page_size } */
export function searchRuns(params = {}) {
  if (USE_MOCK) return mockResponse(mock.runSearch(params))
  return request.post('/job/run/search', params)
}

/** GET /job/run/find?run_id= */
export function findRun(runId) {
  if (USE_MOCK) return mockResponse(mock.runFind({ run_id: runId }))
  return request.get('/job/run/find', { params: { run_id: runId } })
}

/** POST /job/run/steps → { list, total } */
export function fetchRunSteps(runId) {
  if (USE_MOCK) return mockResponse(mock.runStepsQuery({ run_id: runId }))
  return request.post('/job/run/steps', { run_id: runId })
}

/**
 * GET /job/run/overview?since=<Unix 秒>
 *
 * 后端返回 service.RunOverview（admin/internal/service/job_run.go:118）：
 *   { total, success, failed, timeout, canceled, skipped, running, pending,
 *     retry_total, success_rate, skip_rate, by_status, since }
 * 口径：success_rate = 成功 /（total - skipped），skip_rate = 跳过 / total。
 */
export function fetchRunOverview(params = {}) {
  if (USE_MOCK) return mockResponse(mock.runOverviewQuery(params))
  return request.get('/job/run/overview', { params })
}

/** POST /job/kill —— 取消执行（阶段 1 已恢复，见 router.go:74） */
export function killRun(payload) {
  if (USE_MOCK) return mockResponse(mock.jobKill(payload))
  return request.post('/job/kill', payload)
}