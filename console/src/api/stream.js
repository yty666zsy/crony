/**
 * 执行事实订阅（SSE）客户端 —— 对应后端 GET /job/run/stream（阶段 2 收尾新增）。
 *
 * 为什么不用 `new EventSource`：
 *   EventSource 不能设置自定义请求头，而本项目的鉴权是 `Authorization: Bearer <jwt>`。
 *   把 token 放进 query string 会进浏览器历史、代理日志与 Referer，是明确的安全退化。
 *   因此这里用 fetch + ReadableStream 手工解析 SSE 帧——顺便得到一个可单测的纯函数解析器。
 *
 * 后端契约（PHASE2-接口契约.md §9）：
 *   event: snapshot | run | step | heartbeat | error | end
 *   data:  {"seq":1,"kind":"snapshot","server_time":...,"run":{…},"steps":[…]}
 *   - 首帧 snapshot 是**全量**（run），其后的 step 事件逐个补齐已有步骤与增量；
 *   - 无变化时发 heartbeat（不带数据）；
 *   - 终态后服务端发 end 并关闭连接：客户端**不得**重连（否则就是死循环）；
 *   - 连接异常断开时允许退避重连：服务端每轮都给新的 snapshot，天然收敛。
 */
import { API_BASE, USE_MOCK, getToken } from './client'
import * as mock from './mock'

/**
 * 纯函数 SSE 帧解析器：喂任意大小的字符串分片，吐出完整事件。
 *
 * 必须按分片解析：浏览器给的是一个 chunk 流，"一帧"可能被切成两半、
 * 一帧也可能横跨多个 chunk。直接按 chunk 正则匹配是这类实现最常见的 bug。
 */
export function createSSEParser() {
  let buffer = ''
  return {
    /** @returns {Array<{event:string,id:string|null,data:any}>} */
    push(chunk) {
      buffer += chunk
      const events = []
      let sep
      // 同时兼容 \n\n 与 \r\n\r\n（代理偶尔会改写换行）
      while ((sep = findFrameEnd(buffer)) !== -1) {
        const frame = buffer.slice(0, sep.index)
        buffer = buffer.slice(sep.index + sep.length)
        const ev = parseFrame(frame)
        if (ev) events.push(ev)
      }
      return events
    },
    /** 尚未闭合的残余内容（调试/自检用） */
    pending() {
      return buffer
    },
  }
}

function findFrameEnd(buf) {
  const a = buf.indexOf('\n\n')
  const b = buf.indexOf('\r\n\r\n')
  if (a === -1 && b === -1) return -1
  if (b === -1 || (a !== -1 && a < b)) return { index: a, length: 2 }
  return { index: b, length: 4 }
}

function parseFrame(frame) {
  let event = 'message'
  let id = null
  const dataLines = []
  for (const rawLine of frame.split('\n')) {
    const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine
    if (!line || line.startsWith(':')) continue // 注释行（部分代理用它做心跳）
    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'event') event = value
    else if (field === 'id') id = value
    else if (field === 'data') dataLines.push(value)
  }
  if (!dataLines.length) return null // 没有 data 的帧（如某些代理的心跳）直接忽略
  const raw = dataLines.join('\n')
  let data = raw
  try {
    data = JSON.parse(raw)
  } catch {
    /* 保留原始文本，交给调用方 */
  }
  return { event, id, data }
}

/**
 * 订阅一次执行。
 *
 * @param {string} runId  run_id
 * @param {object} opts
 *   onEvent({event,id,data})   每个事件
 *   onOpen()                   连接建立（含每次重连）
 *   onRetry(attempt, waitMs)   连接中断后即将重连
 *   onError(err)               放弃前的最后一次错误（重试耗尽或响应不是 SSE）
 *   onClose(data, reason)      结束（服务端 end / 主动 close / 放弃）
 *   intervalMs/maxSeconds      与服务端参数一致（服务端会各自收敛到边界）
 * @returns {{close:Function, closed:Function}}
 */
export function openRunStream(runId, opts = {}) {
  const {
    onEvent,
    onOpen,
    onRetry,
    onError,
    onClose,
    intervalMs = 1000,
    maxSeconds = 600,
    jobId,
    maxRetry = 5,
  } = opts

  if (USE_MOCK) return openMockRunStream(runId, opts)

  const controller = new AbortController()
  let closed = false
  let ended = false
  let attempt = 0
  let closeReason = null

  const finish = (reason, data) => {
    if (closed) return
    closed = true
    try {
      controller.abort()
    } catch {
      /* 忽略 */
    }
    onClose?.(data, reason)
  }

  const params = new URLSearchParams()
  if (runId) params.set('run_id', runId)
  else if (jobId) params.set('job_id', String(jobId))
  params.set('interval_ms', String(intervalMs))
  params.set('max_seconds', String(maxSeconds))

  async function loop() {
    while (!closed) {
      try {
        const token = getToken()
        const resp = await fetch(`${API_BASE}/job/run/stream?${params.toString()}`, {
          method: 'GET',
          headers: {
            Accept: 'text/event-stream',
            ...(token ? { Authorization: `Bearer ${token}` } : {}),
          },
          signal: controller.signal,
        })
        if (!resp.ok && resp.status !== 200) throw new Error(`HTTP ${resp.status}`)
        const ctype = resp.headers.get('content-type') || ''
        if (!ctype.includes('text/event-stream')) {
          // 后端在建立 SSE 之前返回普通 JSON 信封（参数错误/没登录）：
          // 这类错误重试没有意义，直接放弃并如实提示。
          const body = await resp.json().catch(() => null)
          finish('rejected', body)
          onError?.(new Error(body?.msg || `订阅接口返回了 ${ctype || '空'} 响应，不是 SSE`))
          return
        }
        attempt = 0
        onOpen?.()
        const reader = resp.body.getReader()
        const decoder = new TextDecoder('utf-8')
        const parser = createSSEParser()
        while (!closed) {
          const { value, done } = await reader.read()
          if (done) break
          const events = parser.push(decoder.decode(value, { stream: true }))
          for (const ev of events) {
            onEvent?.(ev)
            if (ev.event === 'end') {
              ended = true
              break
            }
          }
          if (ended) break
        }
        if (ended) {
          finish('end', closeReason)
          return
        }
        if (closed) return
        // 服务端关了连接却没有 end：按"连接被中断"处理，退避后重连（重连会拿到新 snapshot）
        throw new Error('连接被中断')
      } catch (err) {
        if (closed) return
        if (err?.name === 'AbortError') return
        attempt += 1
        if (attempt > maxRetry) {
          finish('failed', err)
          onError?.(err)
          return
        }
        const wait = Math.min(500 * attempt, 3000)
        onRetry?.(attempt, wait)
        await new Promise((r) => setTimeout(r, wait))
      }
    }
  }
  loop()

  return {
    close() {
      if (closed) return
      closeReason = 'client'
      finish('client')
    },
    closed: () => closed,
  }
}

/**
 * Mock 模式：**模拟回放**一条执行（不是真的实时）。
 *
 * 为什么也要实现：控制台默认 VITE_USE_MOCK=true，页面必须能独立演示。
 * 但它就是回放——界面会显示 Mock 角标，避免把"演示"当成"实时能力"。
 */
function openMockRunStream(runId, opts = {}) {
  const { onEvent, onOpen, onClose, tickMs = 350 } = opts
  let closed = false
  let seq = 0
  const timers = []
  const emit = (event, data) => {
    if (closed) return
    onEvent?.({ event, id: String(++seq), data: { seq, kind: event, server_time: Math.floor(Date.now() / 1000), ...data } })
  }
  const later = (fn, ms) =>
    timers.push(
      setTimeout(() => {
        if (!closed) fn()
      }, ms),
    )

  ;(async () => {
    onOpen?.()
    const found = mock.runFind({ run_id: runId })
    const run = found?.data && found.code === 200 ? found.data : null
    if (!run) {
      emit('error', { detail: '执行记录不存在（Mock）' })
      emit('end', { reason: 'error' })
      closed = true
      onClose?.({ reason: 'error' }, 'end')
      return
    }
    const stepsRes = mock.runStepsQuery({ run_id: runId })
    const steps = [...((stepsRes?.data?.list) || [])].sort((a, b) => a.step_index - b.step_index)

    emit('snapshot', { run })
    let i = 0
    const total = Math.max(steps.length, 1)
    const stepOnce = () => {
      if (closed) return
      if (i < steps.length) {
        emit('step', { steps: [steps[i]] })
        i += 1
        later(stepOnce, tickMs)
        return
      }
      // 回放结束：终态 run + end（与服务端语义一致：end 后停止订阅）
      const finalStatus = [2, 3, 4, 5, 6].includes(run.status) ? run.status : 2
      emit('run', { run: { ...run, status: finalStatus, finished_at: run.finished_at || Math.floor(Date.now() / 1000) } })
      emit('end', { reason: 'final', status: ['pending', 'running', 'success', 'failed', 'canceled', 'skipped', 'timeout'][finalStatus] })
      closed = true
      onClose?.({ reason: 'final' }, 'end')
    }
    later(stepOnce, tickMs)
    // 兜底：极端情况下（无步骤）也要推进
    if (total === 0) later(stepOnce, tickMs)
  })()

  return {
    close() {
      closed = true
      timers.forEach((t) => clearTimeout(t))
      onClose?.(null, 'client')
    },
    closed: () => closed,
  }
}