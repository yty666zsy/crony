/**
 * 设置接口（只读）。
 *
 * GET /settings 已由后端实现（阶段 5 补全）：真实读取配置与工具策略，**不含密钥/口令**。
 *
 * 规划中（后续阶段）：
 *   - POST /settings/update 更新（需管理员）
 *   - 并发策略默认值 / 引导管理员（bootstrap）状态的可写配置
 *
 * 阶段 1 约定接口：
 *   GET  /settings        只读配置快照
 *   POST /settings/update 更新（规划中，需管理员）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** GET /settings 只读配置快照 */
export function fetchSettings() {
  if (USE_MOCK) return mockResponse(mock.settingsQuery())
  return request.get('/settings')
}