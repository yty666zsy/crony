/**
 * 审计日志接口。
 *
 * `POST /audit/search` 已由后端实现（阶段 5 补全）：合并两个**真实**事实源——
 *   - `agent_tool_audit`：工具网关每次调用一行（含被拒/转审批）；
 *   - `approval_request`：写动作审批单（谁批的、结果如何）。
 * 响应里 `source` 标注每行来源，`notice` 写明合并口径与窗口上限。
 *
 * 注意：平台**没有**独立的"用户登录审计"表，因此本接口不返回登录类事件（不编造）。
 *
 * 规划中（后续阶段）：
 *   - 登录 / 配置变更审计（需要新的落点表）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** POST /audit/search 审计流水检索（时间 / 操作者 / 动作 / 对象 / 结果） */
export function searchAuditLogs(params = {}) {
  if (USE_MOCK) return mockResponse(mock.auditSearch(params))
  return request.post('/audit/search', params)
}