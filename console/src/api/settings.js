/**
 * 设置接口 —— 混合：部分后端已支持，部分规划中。
 *
 * 已支持（读取来源为后端配置文件 admin/conf/app.ini + 环境变量）：
 *   - JWT 签发方/有效期、口令策略 → 目前**没有只读查询接口**，
 *     需要后端补 GET /settings 或 GET /statis/system 扩展。
 *   - NOTIFY 渠道与重试策略（阶段 0 已做上限约束）
 *
 * 规划中（阶段 1-3）：
 *   - 并发策略默认值（skip / queue / forbid）
 *   - 引导管理员（bootstrap）状态查询
 *   - Agent 自治分级（L0~L3）
 *
 * 阶段 1 约定接口：
 *   GET  /settings        只读配置快照（规划中）
 *   POST /settings/update 更新（规划中，需管理员）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** 【规划中·阶段 1】GET /settings */
export function fetchSettings() {
  if (USE_MOCK) return mockResponse(mock.settingsQuery())
  return request.get('/settings')
}