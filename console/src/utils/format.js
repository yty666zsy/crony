/**
 * 时间与数值格式化工具。
 *
 * 时间口径（真机走查时踩过坑）：
 *   - 普通时间字段是 **Unix 秒**（如 `created`）；
 *   - ISO 字符串原样解析；
 *   - `*_ms` 字段是**毫秒**（如 `created_ms`）。
 * 早期实现对数字一律 `* 1000`，于是毫秒字段被当成秒再乘 1000 → 年份溢出到 58748，
 * 相对时间也永远显示"刚刚"。统一交给 `toMillis` 按量级归一，避免各调用点各写一遍。
 */

const pad = (n) => String(n).padStart(2, '0')

/** 秒级时间戳约 1e9、毫秒级约 1e12：以 1e11 为界区分（1e11 秒 = 公元 5138 年，不会出现）。 */
const MS_THRESHOLD = 1e11

/** 把 Unix 秒 / 毫秒 / ISO 字符串统一归一成毫秒；无法解析返回 NaN。 */
export function toMillis(value) {
  if (value === null || value === undefined || value === '') return NaN
  if (typeof value === 'string') return Date.parse(value)
  const n = Number(value)
  if (!Number.isFinite(n) || n <= 0) return NaN
  return n >= MS_THRESHOLD ? n : n * 1000
}

/** Unix 秒 / 毫秒 / ISO 字符串 → 'YYYY-MM-DD HH:mm:ss' */
export function formatTime(value) {
  const ms = toMillis(value)
  if (!Number.isFinite(ms) || ms <= 0) return '—'
  const d = new Date(ms)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(
    d.getMinutes(),
  )}:${pad(d.getSeconds())}`
}

/** 只到分钟：'MM-DD HH:mm'（表格里省空间） */
export function formatTimeShort(value) {
  const full = formatTime(value)
  return full === '—' ? full : full.slice(5)
}

/** 相对时间：'12 秒前' / '3 分钟前' / '2 小时前' / '5 天前' */
export function formatRelative(value) {
  const ms = toMillis(value)
  if (!Number.isFinite(ms) || ms <= 0) return '—'
  const diff = Date.now() - ms
  if (diff < 0) return '刚刚'
  const sec = Math.floor(diff / 1000)
  if (sec < 60) return `${sec} 秒前`
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min} 分钟前`
  const hour = Math.floor(min / 60)
  if (hour < 24) return `${hour} 小时前`
  const day = Math.floor(hour / 24)
  if (day < 30) return `${day} 天前`
  return formatTime(value).slice(0, 10)
}

/** 毫秒 → '1.2s' / '340ms' / '2m 5s' */
export function formatDuration(ms) {
  const n = Number(ms)
  if (!Number.isFinite(n) || n <= 0) return '—'
  if (n < 1000) return `${n}ms`
  const sec = n / 1000
  if (sec < 60) return `${sec.toFixed(sec < 10 ? 2 : 1)}s`
  const min = Math.floor(sec / 60)
  const rest = Math.round(sec % 60)
  if (min < 60) return `${min}m ${rest}s`
  const hour = Math.floor(min / 60)
  return `${hour}h ${min % 60}m`
}

/** 秒 → '1h 30m'（用于节点 uptime / 超时配置） */
export function formatSeconds(sec) {
  const n = Number(sec)
  if (!Number.isFinite(n) || n <= 0) return '—'
  const h = Math.floor(n / 3600)
  const m = Math.floor((n % 3600) / 60)
  if (h > 24) return `${Math.floor(h / 24)}d ${h % 24}h`
  if (h > 0) return `${h}h ${m}m`
  if (m > 0) return `${m}m`
  return `${n}s`
}

/** 大数字千分位 */
export function formatNumber(n) {
  const v = Number(n)
  if (!Number.isFinite(v)) return '—'
  return v.toLocaleString('zh-CN')
}

/** 字节 → 人类可读 */
export function formatBytes(bytes) {
  const n = Number(bytes)
  if (!Number.isFinite(n) || n <= 0) return '—'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i += 1
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

/** 截断长文本（表格单元格用） */
export function truncate(text, len = 48) {
  const s = String(text ?? '')
  return s.length > len ? `${s.slice(0, len)}…` : s
}

/** job_run 的 run_id 是否来自调度槽位（对应 Go 的 IsSlotRunID） */
export function isSlotRunId(runId) {
  return typeof runId === 'string' && /-t\d+$/.test(runId)
}

/** JSON 美化（展示只读工具的 Schema、工具入参 tool_args）；非对象返回原文本 */
export function prettyJson(value) {
  if (value === null || value === undefined || value === '') return '—'
  if (typeof value === 'string') {
    // 有些字段后端给的是 JSON 字符串（例如模型的 tool_calls[].arguments）
    try {
      return JSON.stringify(JSON.parse(value), null, 2)
    } catch {
      return value
    }
  }
  try {
    return JSON.stringify(value, null, 2)
  } catch {
    return String(value)
  }
}

/**
 * 诊断置信度 → 百分比数字（保留一位小数）。
 *
 * 为什么要兼容两种量纲：契约 §3.1 的响应示例是 `"confidence": 72`（0–100 整数），
 * 而 Agent 内核落库的 `Diagnosis.Confidence` 是 `clamp01(percent/100)` 的 0–1 小数
 * （common/pkg/agent/diagnose.go）。后端 handler 若直接透传内核结构，量纲就是 0–1。
 * 这里两种都接受并按"小于等于 1 视为比例"换算，避免某一侧改了单位就显示成 0.7%。
 */
export function formatConfidence(value) {
  const n = Number(value)
  if (!Number.isFinite(n)) return '—'
  const pct = n <= 1 ? n * 100 : n
  return Math.round(pct * 10) / 10
}