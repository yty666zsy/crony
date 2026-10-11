/**
 * 节点（crony-node）接口。
 *
 * 后端已实现（router.go → /node 组）：
 *   POST /node/search   分页检索（需要 JWTAuth）→ { list, total, page, page_size }
 *   POST /node/del      删除（需要管理员，RequireAdmin）
 *
 * 响应元素 = models.Node + job_count（admin/internal/model/resp/node.go → RspNodeSearch）。
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** POST /node/search */
export function searchNodes(params = {}) {
  if (USE_MOCK) return mockResponse(mock.nodeSearch(params))
  return request.post('/node/search', params)
}

/** POST /node/del（管理员） */
export function deleteNode(uuid) {
  if (USE_MOCK) return mockResponse(mock.nodeDelete({ uuid }))
  return request.post('/node/del', { uuid })
}