/**
 * 写动作审批接口（阶段 4.2）。
 *
 * 这是"人工确认闭环"的前端入口：模型想动手时**不会动手**，而是落一张审批单，
 * 人在这里看到"它想干什么、为什么、影响谁、还剩多久过期"，然后批准或驳回。
 *
 * 契约（admin/internal/handler/agent_approval.go，恒 HTTP 200 + {code,data,msg}）：
 *   POST /agent/approval/search   { status, tool, target_node, page, page_size }
 *   POST /agent/approval/find     { approval_id }
 *   POST /agent/approval/approve  { approval_id, note }   ← 批准即执行（一次批准一次执行）
 *   POST /agent/approval/reject   { approval_id, note }
 *   POST /agent/approval/cancel   { approval_id, note }
 *   POST /agent/audit/search      { session_id, tool, decision, page, page_size }
 *
 * 两条前端必须遵守的规则：
 *  1. **不传操作人**：谁批的由后端从 token 里取（decided_by 字段会被后端忽略），
 *     前端不要自己造一个"当前用户"塞进请求体；
 *  2. **业务失败要区分对待**（见 utils/dict.js 的 1013–1018）：
 *     1014（状态已变更）→ 刷新列表；1015（已过期）→ 提示重新发起；
 *     1016/1017（参数不一致/复核不通过）→ 不要重试，这是安全拦截不是故障。
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** POST /agent/approval/search 审批单检索 */
export function searchApprovals(params = {}) {
  if (USE_MOCK) return mockResponse(mock.approvalSearch(params))
  return request.post('/agent/approval/search', params)
}

/** POST /agent/approval/find 单张审批单详情 */
export function findApproval(approvalId) {
  if (USE_MOCK) return mockResponse(mock.approvalFind({ approval_id: approvalId }))
  return request.post('/agent/approval/find', { approval_id: approvalId })
}

/** POST /agent/approval/approve 批准并执行（一次批准一次执行，重复提交会被 1014 拒绝） */
export function approveApproval(approvalId, note = '') {
  if (USE_MOCK) return mockResponse(mock.approvalDecide({ approval_id: approvalId, note, action: 'approve' }))
  return request.post('/agent/approval/approve', { approval_id: approvalId, note })
}

/** POST /agent/approval/reject 驳回（不执行任何动作） */
export function rejectApproval(approvalId, note = '') {
  if (USE_MOCK) return mockResponse(mock.approvalDecide({ approval_id: approvalId, note, action: 'reject' }))
  return request.post('/agent/approval/reject', { approval_id: approvalId, note })
}

/** POST /agent/approval/cancel 撤销（只有待审批能被撤销） */
export function cancelApproval(approvalId, note = '') {
  if (USE_MOCK) return mockResponse(mock.approvalDecide({ approval_id: approvalId, note, action: 'cancel' }))
  return request.post('/agent/approval/cancel', { approval_id: approvalId, note })
}

/** POST /agent/audit/search 工具调用审计（网关每次调用一条，含被拒的） */
export function searchToolAudit(params = {}) {
  if (USE_MOCK) return mockResponse(mock.toolAuditSearch(params))
  return request.post('/agent/audit/search', params)
}