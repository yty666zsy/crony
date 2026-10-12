/**
 * 审计日志接口 —— **规划中接口（阶段 1-3）**。
 *
 * 后端目前只在 job_log 里记录任务结果，没有独立的审计流水表，
 * 也没有 /audit/* 路由。阶段 0 的安全加固已经引入 RBAC（JWTAuth / RequireAdmin），
 * 但"谁在什么时候对什么对象做了什么"这条流水要等阶段 1 的审计表落地。
 *
 * 规划接口：
 *   POST /audit/search   审计流水检索（时间 / 操作者 / 动作 / 对象 / 结果 / IP）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** 【规划中·阶段 1-3】POST /audit/search */
export function searchAuditLogs(params = {}) {
  if (USE_MOCK) return mockResponse(mock.auditSearch(params))
  return request.post('/audit/search', params, { planned: true })
}