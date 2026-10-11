/**
 * 鉴权接口。后端已实现（admin/internal/handler/router.go）：
 *   POST /login      公开
 *   POST /register   公开（首个管理员引导；非引导场景需管理员令牌）
 *   GET  /ping       公开，返回裸字符串 "pong"（无 {code,data,msg} 信封）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** POST /login → { token, ... } */
export function login(payload) {
  if (USE_MOCK) return mockResponse(mock.login(payload))
  return request.post('/login', payload)
}

/** POST /register（首个管理员引导） */
export function register(payload) {
  if (USE_MOCK) return mockResponse(mock.register(payload))
  return request.post('/register', payload)
}

/** GET /ping → "pong"（注意：裸响应，不套信封） */
export function ping() {
  if (USE_MOCK) return mockResponse('pong', { delay: 60 })
  return request.get('/ping')
}

/**
 * 当前登录用户信息。
 * 规划中：后端目前只有 GET /user/find?id=，没有 "GET /me"。
 * 因此这里用 GET /user/find 兜底（Mock 下直接返回当前用户）。
 */
export function fetchCurrentUser(id = 1) {
  if (USE_MOCK) return mockResponse(mock.currentUser())
  return request.get('/user/find', { params: { id } })
}

/** POST /user/change_pw（后端已实现） */
export function changePassword(payload) {
  if (USE_MOCK) return mockResponse(mock.changePassword(payload))
  return request.post('/user/change_pw', payload)
}