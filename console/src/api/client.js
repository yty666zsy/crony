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
 * 规划中（后端尚未实现）接口的错误对象。
 *
 * 前端对"后端约定但还没落地"的接口（README §4.2）会在请求上打 `planned: true`，
 * 命中 404 时**不弹错误提示**，而是抛这个类型让页面渲染「规划中」占位——
 * 把"还没做"和"坏了"区分开。
 */
export class NotImplementedError extends Error {
  constructor(url) {
    super(`接口未实现（规划中）：${url}`)
    this.name = 'NotImplementedError'
    this.planned = true
  }
}

/**
 * 未登录 / 令牌失效（业务码 1000，后端同时给 `data.reload = true`）。
 *
 * 这类错误会在**每个并发请求上各出现一次**，直接弹窗会刷屏；因此：
 *   - 清掉本地失效令牌；
 *   - 去重提示（15s 内只提示一次）；
 *   - 广播 `crony:unauthorized` 事件，由应用外壳切到登录页（Mock 模式无此路径）。
 */
let unauthorizedAt = 0

function handleUnauthorized(msg) {
  setToken('')
  const now = Date.now()
  if (now - unauthorizedAt > 15000) {
    unauthorizedAt = now
    ElMessage.warning(msg || '未登录或登录已过期，请重新登录')
  }
  try {
    window.dispatchEvent(new CustomEvent('crony:unauthorized'))
  } catch {
    /* Node（SSR 冒烟）里没有 window，忽略 */
  }
}

/**
 * 拆信封 + 统一业务错误提示。
 * 真实请求与 Mock 请求共用同一条路径，保证两种模式的调用方行为一致。
 *
 * `real=true` 表示来自真实后端（axios）；此时业务码 1000 按"未登录/令牌失效"处理
 * （去重提示 + 广播登录事件），而不是像 Mock 那样当成普通业务错误逐条弹窗。
 */
export function unwrap(body, { real = false, silent = false } = {}) {
  if (body && typeof body === 'object' && !Array.isArray(body) && 'code' in body) {
    if (body.code !== 200) {
      const msg = body.msg || `业务错误（code=${body.code}）`
      if (real && Number(body.code) === 1000 && body?.data?.reload) {
        handleUnauthorized(msg)
      } else if (!silent) {
        ElMessage.error(msg)
      }
      return Promise.reject(new BizError(msg, body.code))
    }
    return body.data
  }
  // 无信封的裸响应（例如 GET /ping → "pong"）
  return body
}

/**
 * 只读请求的「请求链路抖动」一次性重放（2026-10-13 用户实测：点「经验库」弹红条）。
 *
 * 现象与定位：
 *   - 同一次页面挂载里，`POST /agent/memory/stats` 偶发 **502**（空响应体、约 10ms 返回），
 *     紧接着重放就是 200；同一接口连续实测 8 次出现 1 次；
 *   - 换 curl（不经浏览器）连续打 60 次：200 × 60、502 × 0。
 *   - 结论：故障不在 crony 后端，而在**请求链路上的一层代理**。
 *
 * 处理：只读请求拿到 502/503/504、或 5s 内快速断连（没有响应）时，原样重放一次。
 * 只读判定用后端既有的命名约定（见 READ_ONLY_PATH）：写动作（approve/execute/del/once/
 * disable/confirm/tighten/widen…）一律不匹配，避免"重放一次"变成"动作做两次"。
 *
 * 为什么不重放超时：Agent 接口要真跑模型（60s 都可能），重放会把"慢"变成"更慢"；
 * 慢失败更可能是真的网络问题，不属于这层抖动。
 */
const READ_ONLY_PATH =
  /(\/(search|find|stats|overview|list|report|runs|cases|steps|by_run|policy|ping|settings|effective|state|recall|log|catalog)\/?$)|\/statis\//

const RETRY_STATUS = new Set([502, 503, 504])
const RETRY_FAST_FAIL_MS = 5000

/** 该 URL 是否是"只读查询"（可以安全重放）。导出是为了让冒烟测试钉住这条判定。 */
export function isReadOnlyEndpoint(url = '') {
  const path = String(url).split('?')[0]
  return READ_ONLY_PATH.test(path)
}

/** 是否应该重放一次：只读 + （网关 5xx 或快速断连）+ 还没重放过。导出供冒烟测试钉住判定。 */
export function shouldReplayOnce(error) {
  const cfg = error?.config || {}
  if (cfg.__replayed) return false
  if (!isReadOnlyEndpoint(cfg.url || '')) return false
  const status = error?.response?.status
  if (status) return RETRY_STATUS.has(status)
  const started = cfg.__startedAt
  return typeof started === 'number' && Date.now() - started < RETRY_FAST_FAIL_MS
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
    // 记起始时间：断连只在这层抖动（快速失败）时才重放，见 shouldReplayOnce。
    config.__startedAt = Date.now()
    return config
  },
  (error) => Promise.reject(error),
)

request.interceptors.response.use(
  (response) => unwrap(response.data, { real: true, silent: Boolean(response?.config?.silent) }),
  async (error) => {
    // 只读请求撞上请求链路抖动：原样重放一次。第一次失败**不提示**
    // （重放成功就不该报警；重放也失败时，第二次会照常走下面的提示）。
    if (shouldReplayOnce(error)) {
      error.config.__replayed = true
      try {
        return await request.request(error.config)
      } catch (again) {
        error = again
      }
    }

    const status = error?.response?.status

    // 规划中接口：后端没有注册这条路由（404）。这不是故障，页面会渲染「规划中」占位，
    // 因此不弹错误提示，只把类型化的错误抛给调用方。
    if (error?.config?.planned && status === 404) {
      return Promise.reject(new NotImplementedError(error.config.url))
    }

    // HTTP 层错误（网络不通 / 超时 / 5xx）。
    // 只提示一次，避免多个并发请求同时弹窗刷屏；`silent: true` 的后台请求完全不提示。
    const detail =
      error?.response?.data?.msg ||
      error?.response?.data?.message ||
      error?.message ||
      '未知错误'
    const text = status ? `请求失败（HTTP ${status}）：${detail}` : `无法连接到后端：${detail}`
    // 502/503/504 是**请求链路上的网关/代理**给的（crony 后端的业务错误一律 HTTP 200 + body.code），
    // 别再用"请确认后端已启动"把用户引到错误的方向——后端可能正跑得好好的。
    const hint = USE_MOCK
      ? ''
      : RETRY_STATUS.has(status)
        ? `（HTTP ${status} 由请求链路上的网关/代理返回，后端未必故障；只读查询已自动重放一次）`
        : `（当前 VITE_USE_MOCK=false，请确认 ${API_BASE} 已启动）`
    if (!error.__notified && !error?.config?.silent) {
      error.__notified = true
      ElMessage.error(`${text}${hint}`)
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