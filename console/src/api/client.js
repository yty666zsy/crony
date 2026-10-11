import axios from 'axios'
import { ElMessage } from 'element-plus'

/**
 * 统一 API 客户端（阶段 1）。
 *
 * 后端响应契约（crony-admin，admin/internal/model/resp/rsp.go）：
 *   { "code": 200, "data": <any>, "msg": "operation success" }
 *   - code === 200 视为成功；其余为业务错误；
 *   - 业务错误也走 HTTP 200（不是 HTTP 4xx/5xx），因此必须解析 body.code。
 *
 * 拦截器把信封拆掉：调用方拿到的是 `data`，而不是整个 {code,data,msg}。
 * 例外：GET /ping 直接返回字符串 "pong"（无信封），此时原样返回。
 */

/** 是否使用 Mock。默认 true：前端在没有后端时也能独立运行。 */
export const USE_MOCK = String(import.meta.env.VITE_USE_MOCK ?? 'true').toLowerCase() !== 'false'

export const API_BASE = import.meta.env.VITE_API_BASE || 'http://127.0.0.1:8089'

const TOKEN_KEY = 'crony_token'

export function getToken() {
  try {
    return localStorage.getItem(TOKEN_KEY) || ''
  } catch {
    return ''
  }
}

export function setToken(token) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {
    /* localStorage 不可用时忽略（隐私模式） */
  }
}

/** 业务错误对象：便于调用方区分"网络错误"与"业务错误"。 */
export class BizError extends Error {
  constructor(msg, code) {
    super(msg)
    this.name = 'BizError'
    this.code = code
    this.biz = true
  }
}

/**
 * 拆信封 + 统一业务错误提示。
 * 真实请求与 Mock 请求共用同一条路径，保证两种模式的调用方行为一致。
 */
export function unwrap(body) {
  if (body && typeof body === 'object' && !Array.isArray(body) && 'code' in body) {
    if (body.code !== 200) {
      const msg = body.msg || `业务错误（code=${body.code}）`
      ElMessage.error(msg)
      return Promise.reject(new BizError(msg, body.code))
    }
    return body.data
  }
  // 无信封的裸响应（例如 GET /ping → "pong"）
  return body
}

const request = axios.create({
  baseURL: API_BASE,
  // 60s：Agent 接口（诊断 / 自然语言建任务）要真跑一次模型。
  // 真实联调实测：qwen-plus 一次诊断 19.6~22.8s，原来的 15s 会先被 axios 掐断，
  // 前端只看到"无法连接到后端"，而后端其实还在正常推理。
  // 后端已取消全局 20s WriteTimeout（见 common/pkg/server/api.go），
  // 真正的边界由应用层预算决定（llm.max-duration-seconds 等）。
  timeout: 60000,
  headers: { 'Content-Type': 'application/json' },
})

request.interceptors.request.use(
  (config) => {
    const token = getToken()
    if (token) config.headers.Authorization = `Bearer ${token}`
    return config
  },
  (error) => Promise.reject(error),
)

request.interceptors.response.use(
  (response) => unwrap(response.data),
  (error) => {
    // HTTP 层错误（网络不通 / 超时 / 5xx）。
    // 只提示一次，避免多个并发请求同时弹窗刷屏。
    const status = error?.response?.status
    const detail =
      error?.response?.data?.msg ||
      error?.response?.data?.message ||
      error?.message ||
      '未知错误'
    const text = status ? `请求失败（HTTP ${status}）：${detail}` : `无法连接到后端：${detail}`
    if (!error.__notified) {
      error.__notified = true
      ElMessage.error(
        USE_MOCK ? text : `${text}（当前 VITE_USE_MOCK=false，请确认 ${API_BASE} 已启动）`,
      )
    }
    return Promise.reject(error)
  },
)

export { request }

/** Mock 延迟，模拟网络往返，让加载态可见（默认 180ms）。 */
export function mockDelay(ms = 180) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

/**
 * Mock 响应包装：构造与后端一致的信封，再走 unwrap，
 * 因此 Mock 模式下 code!==200 同样会弹 ElMessage。
 *
 * 为什么要识别"已经是信封"的入参：
 *   mock/*.js 的处理器一律返回**完整信封** { code, data, msg }（与后端响应体逐字一致，
 *   便于 smoke 直接断言），而这里再包一层就会得到
 *   { code:200, data:{code,data,msg} } → unwrap 只拆一层 → 调用方拿到的是**信封本身**，
 *   `res.list` 恒为 undefined。阶段 1 的页面因此在 Mock 模式下全部渲染空列表。
 *   现在：入参是信封就原样拆它的 data；code!==200 仍然抛 BizError（人话提示不变）。
 */
export async function mockResponse(data, { code = 200, msg = 'operation success', delay = 180 } = {}) {
  if (delay > 0) await mockDelay(delay)
  if (data && typeof data === 'object' && !Array.isArray(data) && 'code' in data && 'msg' in data) {
    return unwrap({ code: data.code, data: data.data, msg: data.msg || msg })
  }
  return unwrap({ code, data, msg })
}