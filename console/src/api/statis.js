/**
 * 统计接口。
 *
 * 后端已实现（router.go → /statis 组，需要 JWTAuth）：
 *   GET /statis/today   今日统计
 *   GET /statis/week    近 7 日统计
 *   GET /statis/system  系统信息
 *
 * 规划中（本控制台约定，阶段 1；后端尚未提供）：
 *   GET /statis/overview  控制台概览（KPI + 趋势 + 最近任务 + 告警）
 *   GET /statis/health    依赖健康度（API / MySQL / 节点 / 调度器 / 通知 / Agent 运行时）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** GET /statis/today */
export function fetchTodayStatistics() {
  if (USE_MOCK) return mockResponse(mock.statisTodayQuery())
  return request.get('/statis/today')
}

/** GET /statis/week */
export function fetchWeekStatistics() {
  if (USE_MOCK) return mockResponse(mock.statisWeekQuery())
  return request.get('/statis/week')
}

/** GET /statis/system */
export function fetchSystemInfo() {
  if (USE_MOCK) return mockResponse(mock.statisSystemQuery())
  return request.get('/statis/system')
}

/** GET /statis/overview 控制台概览（真实：KPI + 由健康度/SLO 推导的告警） */
export function fetchOverview() {
  if (USE_MOCK) return mockResponse(mock.statisOverview())
  return request.get('/statis/overview')
}

/** GET /statis/health 依赖健康度（真实） */
export function fetchHealth() {
  if (USE_MOCK) return mockResponse(mock.statisHealth())
  return request.get('/statis/health')
}