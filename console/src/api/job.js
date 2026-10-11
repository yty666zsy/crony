/**
 * 定时任务接口。
 *
 * 后端已实现（router.go → /job 组，需要 JWTAuth）：
 *   POST /job/search   分页检索  → { list, total, page, page_size }
 *   GET  /job/find?id= 详情
 *   POST /job/add      新增/更新（同一个 handler：CreateOrUpdate）
 *   POST /job/del      删除
 *   POST /job/once     一次性立即执行
 *   POST /job/log      执行日志检索（旧模型 job_log）
 *
 * 已实现（阶段 1 恢复，见 router.go:73-74）：
 *   POST /job/kill     取消正在执行的 run。节点侧已实现基于 context + 进程树终止的
 *                      取消传播，因此该路由不再处于注释状态。
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** POST /job/search */
export function searchJobs(params = {}) {
  if (USE_MOCK) return mockResponse(mock.jobSearch(params))
  return request.post('/job/search', params)
}

/** GET /job/find?id= */
export function findJob(id) {
  if (USE_MOCK) return mockResponse(mock.jobFind({ id }))
  return request.get('/job/find', { params: { id } })
}

/** POST /job/add */
export function saveJob(payload) {
  if (USE_MOCK) return mockResponse(mock.jobSave(payload))
  return request.post('/job/add', payload)
}

/** POST /job/del */
export function deleteJob(id) {
  if (USE_MOCK) return mockResponse(mock.jobDelete({ id }))
  return request.post('/job/del', { id })
}

/** POST /job/once —— 立即执行一次 */
export function runJobOnce(payload) {
  if (USE_MOCK) return mockResponse(mock.jobOnce(payload))
  return request.post('/job/once', payload)
}

/** POST /job/log —— 旧版执行日志（job_log 模型） */
export function searchJobLogs(params = {}) {
  if (USE_MOCK) return mockResponse(mock.jobLogSearch(params))
  return request.post('/job/log', params)
}

/** POST /job/kill —— 取消执行（阶段 1 已恢复） */
export function killJob(payload) {
  if (USE_MOCK) return mockResponse(mock.jobKill(payload))
  return request.post('/job/kill', payload)
}