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

/**
 * POST /job/dag/validate —— 校验一份编排定义（阶段 4-B）。
 *
 * 两种用法：给 steps_json（编辑器草稿，还没保存），或给 job_id（校验库里已有的任务）。
 * 失败时后端返回业务码 1021（编排非法）并带上具体原因（哪个步骤、哪个环），
 * 1022 表示"该任务本来就没有编排"。调用方应捕获 BizError 并**内联展示原因**，
 * 只靠 toast 会丢掉"具体哪一行错了"。
 */
export function validateJobDag(payload) {
  if (USE_MOCK) return mockResponse(mock.jobDagValidate(payload))
  return request.post('/job/dag/validate', payload)
}

/** POST /job/dag/find —— 读取某个任务的编排摘要（步骤 / 边 / 层深 / 告警） */
export function findJobDag(jobId) {
  if (USE_MOCK) return mockResponse(mock.jobDagFind({ job_id: jobId }))
  return request.post('/job/dag/find', { job_id: jobId })
}