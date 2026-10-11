/**
 * 脚本接口（后端已实现，router.go → /script 组）。
 *   POST /script/search  需要 JWTAuth
 *   GET  /script/find    需要 JWTAuth
 *   POST /script/add     需要管理员（RequireAdmin）
 *   POST /script/del     需要管理员（RequireAdmin）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** POST /script/search */
export function searchScripts(params = {}) {
  if (USE_MOCK) return mockResponse(mock.scriptSearch(params))
  return request.post('/script/search', params)
}

/** GET /script/find?id= */
export function findScript(id) {
  if (USE_MOCK) return mockResponse(mock.scriptFind({ id }))
  return request.get('/script/find', { params: { id } })
}