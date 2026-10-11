/** 时间与数值格式化工具（后端时间统一为 Unix 秒；部分规划接口返回 ISO 字符串）。 */

const pad = (n) => String(n).padStart(2, '0')

/** Unix 秒 / ISO 字符串 → 'YYYY-MM-DD HH:mm:ss' */
export function formatTime(value) {
  if (!value) return '—'
  const ms = typeof value === 'string' ? Date.parse(value) : Number(value) * 1000
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
  if (!value) return '—'
  const ms = typeof value === 'string' ? Date.parse(value) : Number(value) * 1000
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