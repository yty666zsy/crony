/**
 * Mock 处理器（阶段 1 基础数据 + 阶段 2 Agent 内核）。
 *
 * 每个处理器都返回与后端**完全一致**的信封 { code, data, msg }，
 * 由 api/client.js 的 mockResponse() 走统一 unwrap（含业务错误弹窗）。
 * 这样 Mock 与真实后端的调用方行为一致，切换开关时页面无需改动。
 *
 * 分页契约与后端一致（admin/internal/model/resp/rsp.go → PageResult）：
 *   { list, total, page, page_size }
 * 分页入参默认值也一致（admin/internal/model/request/req.go → PageInfo.Check）：
 *   page<=0 → 1；page_size<=0 → 20
 *
 * 阶段 2 新增：Agent 内核 §1–§5（会话列表/详情/诊断读写/工具清单/自然语言建任务）。
 * 这些处理器的错误分支**逐条对齐契约 §0 的业务码**，而不是笼统返回 1000，
 * 因为前端的"人话提示"是挂在 code 上的（utils/dict.js → AGENT_BIZ_HINT）。
 */
import { AGENT_BIZ_HINT } from '@/utils/dict'
import {
  alerts,
  auditLogs,
  buildSteps,
  health,
  jobLogs,
  jobs,
  knowledgeDocs,
  nodes,
  overview,
  RUN_STATUS, // 由 data.js 从 @/utils/dict 转出，避免字典两处维护
  runSteps,
  runs,
  scripts,
  settingsSnapshot,
  statisToday,
  statisWeek,
  // 阶段 2：Agent 内核数据
  AGENT_BUDGET_MAX,
  AGENT_MAX_TOOL_CALLS,
  agentDiagnoses,
  agentDrafts,
  agentSessionDetails,
  agentSessions,
  agentToolPolicy,
  approvalRequests,
  // 阶段 4-A：经验库（记忆）
  agentMemories,
  memorySource,
  toolAuditRows,
  agentTools,
  rfc3339,
  specTriggers,
  triggerEcho,
} from './data'

/** 与后端 PageInfo.Check() 对齐 */
export function normalizePage({ page, page_size: pageSize } = {}) {
  const p = Number(page) > 0 ? Number(page) : 1
  const ps = Number(pageSize) > 0 ? Number(pageSize) : 20
  return { page: p, pageSize: ps }
}

/** 与后端 PageResult 对齐 */
export function paginate(list, { page, pageSize }) {
  const start = (page - 1) * pageSize
  return {
    list: list.slice(start, start + pageSize),
    total: list.length,
    page,
    page_size: pageSize,
  }
}

function includes(haystack, needle) {
  return String(haystack ?? '')
    .toLowerCase()
    .includes(String(needle).trim().toLowerCase())
}

/* ------------------------- 鉴权 ------------------------- */

export function login({ username, password }) {
  if (!username || !password) {
    return { code: 1001, data: {}, msg: '用户名或口令为空' }
  }
  if (String(password).length < 6) {
    // 模拟后端口令错误（业务错误，HTTP 200）
    return { code: 1000, data: {}, msg: '用户名或口令错误' }
  }
  return {
    code: 200,
    data: {
      token: 'mock-jwt.' + btoa(unescape(encodeURIComponent(username))).replace(/=+$/, '') + '.crony',
      username,
      is_admin: username === 'admin',
      expire_hours: 12,
    },
    msg: 'operation success',
  }
}

export function register(payload) {
  if (!payload?.username || !payload?.password) {
    return { code: 1001, data: {}, msg: '用户名或口令为空' }
  }
  return {
    code: 200,
    data: { username: payload.username, is_admin: true, bootstrap: true },
    msg: '首个管理员引导注册成功（Mock）',
  }
}

export function ping() {
  // 注意：真实后端此处返回裸字符串 "pong"，无信封
  return { raw: 'pong' }
}

export function currentUser() {
  return {
    code: 200,
    data: {
      id: 1,
      username: 'admin',
      nickname: '运维管理员',
      is_admin: true,
      email: 'ops@crony.local',
      last_login: new Date(Date.now() - 3600 * 1000).toISOString(),
    },
    msg: 'operation success',
  }
}

/* ------------------------- 任务 ------------------------- */

export function jobSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = [...jobs]
  if (params.name) list = list.filter((j) => includes(j.name, params.name))
  if (params.id) list = list.filter((j) => Number(j.id) === Number(params.id))
  if (params.run_on) list = list.filter((j) => j.run_on === params.run_on)
  if (params.job_type) list = list.filter((j) => Number(j.job_type) === Number(params.job_type))
  if (params.status !== undefined && params.status !== null && params.status !== '') {
    list = list.filter((j) => Number(j.status) === Number(params.status))
  }
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

export function jobFind({ id }) {
  const job = jobs.find((j) => Number(j.id) === Number(id))
  if (!job) return { code: 1000, data: {}, msg: '任务不存在' }
  return { code: 200, data: job, msg: 'operation success' }
}

export function jobOnce({ job_id: jobId }) {
  const job = jobs.find((j) => Number(j.id) === Number(jobId))
  if (!job) return { code: 1000, data: {}, msg: '任务不存在' }
  return {
    code: 200,
    data: { run_id: `j${job.id}-m${Date.now()}000000-1`, job_id: job.id, trigger_source: 2 },
    msg: '已提交一次性执行',
  }
}

/** 规划中：POST /job/kill（router.go 中该路由当前被注释，"阶段 1 恢复"） */
export function jobKill({ job_id: jobId, node_uuid: nodeUuid }) {
  return {
    code: 200,
    data: { job_id: jobId, node_uuid: nodeUuid, signal: 'SIGINT', propagated: 'process_group' },
    msg: '已下发取消信号（Mock：真实接口当前未启用）',
  }
}

export function jobLogSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = [...jobLogs]
  if (params.name) list = list.filter((l) => includes(l.name, params.name))
  if (params.job_id) list = list.filter((l) => Number(l.job_id) === Number(params.job_id))
  if (params.node_uuid) list = list.filter((l) => l.node_uuid === params.node_uuid)
  if (params.success !== undefined && params.success !== null && params.success !== '') {
    const ok = params.success === true || params.success === 'true' || params.success === 1
    list = list.filter((l) => l.success === ok)
  }
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

export function jobSave(payload) {
  // Mock：只回显，不真正落库（后端 POST /job/add 同时承担新增与更新）
  return { code: 200, data: { id: payload?.id || 99, ...payload }, msg: '保存成功（Mock）' }
}

export function jobDelete({ id }) {
  return { code: 200, data: { id }, msg: '删除成功（Mock）' }
}

/* ------------ 阶段 4-B：多步骤编排（DAG）只读控制面 ------------ */

/**
 * mockDagPlan 复刻后端 orchestration.BuildPlan 的裁决规则（只做"合法性 + 摘要"，
 * 不真执行）。规则与 Go 侧保持一致的三点：
 *   1. depends_on 不写 = 依赖索引顺序上的前一步；写 "root"/"none"/"-" = 没有上游；
 *      写 "0,2" = 显式依赖；含非数字项直接报错。
 *   2. 索引重复、自依赖、依赖不存在的步骤、成环、未知字段 → 非法（拒绝保存/执行）。
 *   3. 层深按最长路径计算，摘要里给出步骤、边、深度与告警。
 *
 * 为什么 Mock 也要复刻：Mock 是控制台默认模式（USE_MOCK=true），如果这里一律回
 * "合法"，那么前端在接真实后端之前根本发现不了自己画错了图。
 */
function mockDagPlan(raw) {
  const text = String(raw ?? '').trim()
  if (!text) return { code: 1022, data: { valid: false, error: '该任务没有编排定义（steps_json 为空）：单步任务不需要编排' }, msg: 'no orchestration defined' }

  let spec
  try {
    spec = JSON.parse(text)
  } catch (e) {
    return bizErr(1021, `编排定义不是合法 JSON：${e.message}`)
  }
  if (!spec || typeof spec !== 'object' || Array.isArray(spec)) {
    return bizErr(1021, '编排定义必须是 JSON 对象（{"steps":[...]}）')
  }
  const allowed = ['max_parallel', 'fail_fast', 'steps']
  const unknown = Object.keys(spec).filter((k) => !allowed.includes(k))
  if (unknown.length) return bizErr(1021, `存在未知字段：${unknown.join(',')}（拼错的字段不会被忽略）`)
  const steps = Array.isArray(spec.steps) ? spec.steps : []
  if (!steps.length) return bizErr(1021, '编排定义里没有任何步骤')

  const stepAllowed = ['index', 'name', 'kind', 'command', 'args', 'depends_on', 'join', 'condition', 'continue_on_failure', 'timeout_seconds', 'retry_times', 'retry_interval', 'http_method']
  const seen = new Set()
  const byIndex = new Map()
  for (const s of steps) {
    const bad = Object.keys(s).filter((k) => !stepAllowed.includes(k))
    if (bad.length) return bizErr(1021, `步骤 ${s.index} 存在未知字段：${bad.join(',')}`)
    if (Number(s.index) < 0 || seen.has(Number(s.index))) return bizErr(1021, `步骤 index 非法或重复：${s.index}`)
    seen.add(Number(s.index))
    byIndex.set(Number(s.index), s)
    if (!String(s.command ?? '').trim()) return bizErr(1021, `步骤 ${s.index} 缺少 command`)
    if (s.join && !['all_success', 'any_success', 'all_done'].includes(s.join)) return bizErr(1021, `步骤 ${s.index} 的 join 非法：${s.join}`)
    if (s.condition && !['on_success', 'on_failure', 'always'].includes(s.condition)) return bizErr(1021, `步骤 ${s.index} 的 condition 非法：${s.condition}`)
  }

  // 依赖解析（与 Go 侧同一套规则）
  const depsOf = []
  steps.forEach((s, pos) => {
    const rawDep = String(s.depends_on ?? '').trim().toLowerCase()
    if (rawDep === 'root' || rawDep === 'none' || rawDep === '-') return depsOf.push([])
    if (rawDep === '') return depsOf.push(pos === 0 ? [] : [Number(steps[pos - 1].index)])
    const parts = rawDep.split(',').map((p) => p.trim())
    const deps = []
    for (const p of parts) {
      if (p === '') return depsOf.push({ err: `步骤 ${s.index} 的 depends_on 含有空的依赖项` })
      if (!/^\d+$/.test(p)) return depsOf.push({ err: `步骤 ${s.index} 的 depends_on 含非数字项 ${p}（只支持 "0,2" 或 "root"）` })
      const idx = Number(p)
      if (idx === Number(s.index)) return depsOf.push({ err: `步骤 ${s.index} 依赖了自身` })
      if (!byIndex.has(idx)) return depsOf.push({ err: `步骤 ${s.index} 依赖了不存在的步骤 ${idx}` })
      if (!deps.includes(idx)) deps.push(idx)
    }
    depsOf.push(deps.sort((a, b) => a - b))
  })
  const depErr = depsOf.find((d) => d && !Array.isArray(d) && d.err)
  if (depErr) return bizErr(1021, depErr.err)

  // 环检测（返回完整环路，便于前端直接把环画出来）
  const color = new Map()
  const stack = []
  let cyc = ''
  const visit = (pos) => {
    const idx = Number(steps[pos].index)
    color.set(idx, 1)
    stack.push(`${idx} ${steps[pos].name || `step-${idx}`}`)
    for (const d of depsOf[pos]) {
      const dpos = steps.findIndex((x) => Number(x.index) === d)
      if (color.get(d) === 1) {
        cyc = [...stack.slice(stack.indexOf(stack.find((t) => t.startsWith(`${d} `)))), `${d} ${byIndex.get(d).name || `step-${d}`}`].join(' → ')
        return true
      }
      if (!color.get(d) && visit(dpos)) return true
    }
    stack.pop()
    color.set(idx, 2)
    return false
  }
  for (let pos = 0; pos < steps.length; pos++) {
    if (!color.get(Number(steps[pos].index)) && visit(pos)) return bizErr(1021, `编排存在环：${cyc}`)
  }

  // 摘要（步骤 + 边 + 最长路径层深）
  const depth = new Map(steps.map((s) => [Number(s.index), 0]))
  for (let round = 0; round < steps.length; round++) {
    let changed = false
    steps.forEach((s, pos) => {
      const best = depsOf[pos].reduce((acc, d) => Math.max(acc, (depth.get(d) ?? 0) + 1), 0)
      if (best > depth.get(Number(s.index))) {
        depth.set(Number(s.index), best)
        changed = true
      }
    })
    if (!changed) break
  }
  const hasDownstream = new Set(depsOf.flat())
  const planSteps = steps.map((s, pos) => {
    const warnings = []
    if (!depsOf[pos].length && pos !== 0) warnings.push('该步骤没有上游，会被当作并排分支的起点执行')
    if (!depsOf[pos].length && !hasDownstream.has(Number(s.index)) && steps.length > 1) {
      warnings.push('该步骤既没有上游也没有下游：它是一座孤岛（确认这是有意为之）')
    }
    return {
      index: Number(s.index),
      name: s.name || `step-${s.index}`,
      kind: s.kind || 'shell',
      command: s.command,
      depends_on: depsOf[pos],
      join: s.join || 'all_success',
      condition: s.condition || 'on_success',
      depth: depth.get(Number(s.index)),
      retry_times: Number(s.retry_times || 0),
      timeout_seconds: Number(s.timeout_seconds || 0),
      warnings,
    }
  })
  const edges = []
  steps.forEach((s, pos) => depsOf[pos].forEach((d) => edges.push({ from: d, to: Number(s.index) })))
  return {
    code: 200,
    data: {
      valid: true,
      plan: {
        max_parallel: Number(spec.max_parallel || 0) || 1,
        fail_fast: Boolean(spec.fail_fast),
        steps: planSteps,
        edges,
        max_depth: Math.max(0, ...planSteps.map((s) => s.depth)),
      },
    },
    msg: 'orchestration is valid',
  }
}

/** POST /job/dag/validate */
export function jobDagValidate({ job_id: jobId, steps_json: stepsJSON } = {}) {
  if (jobId) {
    const job = jobs.find((j) => Number(j.id) === Number(jobId))
    if (!job) return bizErr(1002, '任务不存在')
    return mockDagPlan(job.steps_json)
  }
  return mockDagPlan(stepsJSON)
}

/** POST /job/dag/find */
export function jobDagFind({ job_id: jobId } = {}) {
  const job = jobs.find((j) => Number(j.id) === Number(jobId))
  if (!job) return bizErr(1002, '任务不存在')
  const res = mockDagPlan(job.steps_json)
  if (res.code !== 200) {
    // 与后端一致：非法定义也要能打开（否则用户看不到自己存进去的东西），
    // 但必须标明"当前不可执行"。业务码原样透传——1022（本来就没有编排）与
    // 1021（存着的定义是坏的）是两种完全不同的处置方式，合并会让前端误报。
    return {
      code: res.code,
      data: { job_id: job.id, name: job.name, job_type: job.job_type, steps_json: job.steps_json || '', executable: false, error: res.data?.error || res.msg },
      msg: res.msg,
    }
  }
  return {
    code: 200,
    data: {
      job_id: job.id,
      name: job.name,
      job_type: job.job_type,
      steps_json: job.steps_json || '',
      executable: true,
      plan: res.data.plan,
    },
    msg: 'operation success',
  }
}

/* --------------- 执行记录（后端已实现 /job/run/*） --------------- */

export function runSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = runs.map((r) => ({ ...r, step_count: r.step_count }))
  if (params.job_name) list = list.filter((r) => includes(r.job_name, params.job_name))
  if (params.job_id) list = list.filter((r) => Number(r.job_id) === Number(params.job_id))
  if (params.run_id) list = list.filter((r) => includes(r.run_id, params.run_id))
  if (params.node_uuid) list = list.filter((r) => r.node_uuid === params.node_uuid)
  if (params.trace_id) list = list.filter((r) => includes(r.trace_id, params.trace_id))
  if (params.trigger_source) {
    list = list.filter((r) => Number(r.trigger_source) === Number(params.trigger_source))
  }
  if (params.status !== undefined && params.status !== null && params.status !== '') {
    list = list.filter((r) => Number(r.status) === Number(params.status))
  }
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

export function runFind({ run_id: runId }) {
  const run = runs.find((r) => r.run_id === runId)
  if (!run) return { code: 1000, data: {}, msg: '执行记录不存在' }
  return { code: 200, data: run, msg: 'operation success' }
}

export function runStepsQuery({ run_id: runId }) {
  const run = runs.find((r) => r.run_id === runId)
  if (!run) return { code: 1000, data: {}, msg: '执行记录不存在' }
  return { code: 200, data: { list: buildSteps(run), total: buildSteps(run).length }, msg: 'operation success' }
}

/**
 * GET /job/run/overview 的 Mock。
 *
 * 字段与口径**逐项对齐**后端 service.RunOverview / SummarizeRuns
 * （admin/internal/service/job_run.go:118、:165）：
 *   - success_rate = 成功 /（total - skipped）：被并发策略跳过不算失败，
 *     计入分母会让正常的并发保护看起来像故障；
 *   - skip_rate = 跳过 / total：跳过率长期偏高说明调度间隔或并发策略要调整。
 * 时间窗口：since 缺省为最近 24 小时（Mock 用 runs 里 scheduled_time 在窗口内的记录）。
 */
export function runOverviewQuery({ since } = {}) {
  const sinceTs = Number(since) > 0 ? Number(since) : Math.floor(Date.now() / 1000) - 24 * 3600
  const inWindow = runs.filter((r) => Number(r.scheduled_time) >= sinceTs)
  const byStatus = {}
  let retryTotal = 0
  for (const r of inWindow) {
    const key = RUN_STATUS[r.status]?.key || String(r.status)
    byStatus[key] = (byStatus[key] || 0) + 1
    retryTotal += Number(r.retry_count || 0)
  }
  const count = (key) => byStatus[key] || 0
  const total = inWindow.length
  const skipped = count('skipped')
  const executed = total - skipped
  const round = (v) => Math.round(v * 10000) / 10000

  return {
    code: 200,
    data: {
      total,
      success: count('success'),
      failed: count('failed'),
      timeout: count('timeout'),
      canceled: count('canceled'),
      skipped,
      running: count('running'),
      pending: count('pending'),
      retry_total: retryTotal,
      success_rate: executed > 0 ? round(count('success') / executed) : 0,
      skip_rate: total > 0 ? round(skipped / total) : 0,
      by_status: byStatus,
      since: sinceTs,
    },
    msg: 'operation success',
  }
}

/* ------------------------- 节点 ------------------------- */

export function nodeSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = [...nodes]
  if (params.hostname) list = list.filter((n) => includes(n.hostname, params.hostname))
  if (params.ip) list = list.filter((n) => includes(n.ip, params.ip))
  if (params.uuid) list = list.filter((n) => includes(n.uuid, params.uuid))
  if (params.status !== undefined && params.status !== null && params.status !== '') {
    list = list.filter((n) => Number(n.status) === Number(params.status))
  }
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

export function nodeDelete({ uuid }) {
  return { code: 200, data: { uuid }, msg: '节点已删除（Mock）' }
}

/* ------------------------- 脚本 ------------------------- */

export function scriptSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = [...scripts]
  if (params.name) list = list.filter((s) => includes(s.name, params.name))
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

export function scriptFind({ id }) {
  const script = scripts.find((s) => Number(s.id) === Number(id))
  if (!script) return { code: 1000, data: {}, msg: '脚本不存在' }
  return { code: 200, data: script, msg: 'operation success' }
}

/* ------------------------- 统计 ------------------------- */

export function statisTodayQuery() {
  return { code: 200, data: statisToday, msg: 'operation success' }
}

export function statisWeekQuery() {
  return { code: 200, data: statisWeek, msg: 'operation success' }
}

export function statisSystemQuery() {
  return {
    code: 200,
    data: {
      os: 'linux/amd64',
      go_version: 'go1.22',
      memory: 512,
      cpu: 8,
      disk: 240,
      uptime: Math.floor(Date.now() / 1000) - 6 * 3600,
    },
    msg: 'operation success',
  }
}

export function statisOverview() {
  return { code: 200, data: { ...overview, alerts }, msg: 'operation success' }
}

export function statisHealth() {
  return { code: 200, data: health, msg: 'operation success' }
}

/* ==================================================================== *
 * Agent 内核（阶段 2 · 《PHASE2-接口契约.md》§1–§5）
 *
 * 错误分支的可达性（Mock 造不出来的分支绝不假装能造，这里如实标注）：
 *   - 1008 会话不存在：把 /agents/:session_id 的 URL 改成不存在的 id 即可命中；
 *   - 1010 运行记录不存在：正常 UI 走不到（/runs 的每一行都真实存在），只有手改参数才会；
 *   - 1009 草案复验不通过：草案文本为空、或确认时 spec/超时/重试取值非法；
 *   - 1011 / 1007：由 settingsSnapshot.agent（enabled / provider）决定。当前 Mock 快照是
 *     "已启用 + offline provider"，所以默认数据下**不可达**——把 enabled 改成 false 就能试；
 *   - 1012 节点离线：nodes 里 n-7be2a904-01 是 status=2（失联），
 *     但当前没有 run 落在这个节点上，所以默认数据下**不可达**。
 * ==================================================================== */

/** 强制重诊新产生的会话（追加在列表最前，让"刚发生的会话"一眼可见） */
const extraSessions = []
/** 新产生的诊断：run_id → { session_id, diagnosis } */
const extraDiagnoses = {}

const allSessions = () => [...extraSessions, ...agentSessions]

/** 业务错误信封：msg 给后端视角的一句话，人话解释由前端 AGENT_BIZ_HINT 承担 */
function bizErr(code, msg) {
  return { code, data: {}, msg: msg || AGENT_BIZ_HINT[code]?.title || '操作失败' }
}

/**
 * 前置门，对应后端 agentconfig.Resolve() + 节点校验。
 * needModel=false 用于"只读列表/清单"这类不需要模型的接口（例如 §1 会话列表）。
 */
function agentGate(nodeUuid, { needModel = true } = {}) {
  const conf = settingsSnapshot.agent || {}
  if (conf.enabled === false) return bizErr(1011)
  if (needModel && !conf.provider) return bizErr(1007)
  if (nodeUuid) {
    const node = nodes.find((n) => n.uuid === nodeUuid)
    if (node && Number(node.status) !== 1) {
      return bizErr(1012, `节点 ${node.hostname || nodeUuid} 当前失联，无法在其上采集只读证据`)
    }
  }
  return null
}

/** §1 会话列表：支持 job_id / run_id / kind / status 过滤 + 分页 */
export function agentSessionSearch(params = {}) {
  const gate = agentGate(null, { needModel: false })
  if (gate) return gate
  const { page, pageSize } = normalizePage(params)
  let list = allSessions()
  if (params.job_id) list = list.filter((s) => Number(s.job_id) === Number(params.job_id))
  if (params.run_id) list = list.filter((s) => includes(s.run_id, params.run_id))
  if (params.kind) list = list.filter((s) => s.kind === params.kind)
  if (params.status) list = list.filter((s) => s.status === params.status)
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

/** 列表里有、但没单独写详情的会话：给出可渲染的空轨迹，而不是报错 */
function fallbackDetail(session) {
  return {
    ...session,
    system: '',
    context: [],
    budget: {
      calls: session.model_calls,
      tokens: session.total_tokens,
      duration_ms: session.duration_ms,
      ...AGENT_BUDGET_MAX,
    },
    warnings: [],
    evidence: [],
    tool_stats: {
      calls: session.tool_calls,
      denied: 0,
      truncated: 0,
      remaining: Math.max(0, AGENT_MAX_TOOL_CALLS - session.tool_calls),
    },
    steps: [],
    result: session.kind === 'diagnose' ? extraDiagnoses[session.run_id]?.diagnosis || null : null,
  }
}

/** §2 会话详情（轨迹回放） */
export function agentSessionFind({ session_id: sessionId } = {}) {
  const gate = agentGate(null, { needModel: false })
  if (gate) return gate
  const detail = agentSessionDetails[sessionId]
  if (detail) return { code: 200, data: detail, msg: 'operation success' }
  const found = allSessions().find((s) => s.session_id === sessionId)
  if (!found) return bizErr(1008, `会话 ${sessionId || '(空)'} 不存在`)
  return { code: 200, data: fallbackDetail(found), msg: 'operation success' }
}

/** §3.1 读取已存诊断；found=false 是"还没诊断过"的正常状态，不是错误 */
export function agentDiagnosisFind({ run_id: runId } = {}) {
  const gate = agentGate(null, { needModel: false })
  if (gate) return gate
  if (!runs.some((r) => r.run_id === runId)) return bizErr(1010)
  const hit = extraDiagnoses[runId] || agentDiagnoses[runId]
  if (!hit) {
    return { code: 200, data: { found: false, session_id: '', diagnosis: null }, msg: 'operation success' }
  }
  return {
    code: 200,
    data: { found: true, session_id: hit.session_id, diagnosis: hit.diagnosis },
    msg: 'operation success',
  }
}

/**
 * 即时生成一条"证据不足"型诊断。
 *
 * 为什么 Mock 不用一个漂亮结论：Mock 没有推理能力，凭 run 记录编出一个高置信度结论
 * 会让人误以为"诊断很准"。这里固定给出 evidence_sufficient=false + 还要采集什么，
 * 并在 warnings 里写明这是即时生成的示例。
 */
function buildDiagnosis(run) {
  const statusText = RUN_STATUS[run.status]?.text || String(run.status)
  const category = run.status === 6 ? 'dependency' : run.status === 3 ? 'unknown' : 'schedule'
  const categoryText = { dependency: '依赖不可用', unknown: '未归类', schedule: '调度问题' }[category]
  const created = rfc3339(Date.now())
  const diag = {
    summary: `任务「${run.job_name}」本次状态为 ${statusText}；仅凭 run 记录无法定位原因（示例诊断）`,
    category,
    category_text: categoryText,
    cause: run.error_msg || run.output || '这条 run 记录里没有错误信息，拿不到可归因的线索。',
    confidence: 35,
    evidence: [
      {
        ref: `run:${run.run_id}`,
        quote: run.error_msg || `${run.job_name}：status=${statusText}，exit_code=${run.exit_code}`,
        why: 'run 记录本身：状态、退出码、重试次数与错误信息',
      },
    ],
    assumptions: [],
    ruled_out: [],
    suggested_actions: [
      {
        title: '先补齐可观测数据，再重诊一次',
        detail:
          '当前证据链只有 run 记录一层。请在同节点上补齐该 run 的日志与节点状态后再触发诊断，' +
          '否则任何结论都只是猜测。',
        risk: 'low',
        risk_text: '低风险',
        rollback: '无（只读排查）',
      },
    ],
    evidence_sufficient: false,
    next_data_to_collect: [
      `run ${run.run_id} 在 ${run.hostname} 上的完整日志（log_search）`,
      '目标节点当前心跳与资源水位（node_status）',
    ],
    unverified_refs: [],
    warnings: [
      '（Mock 数据）这条诊断是按 run 记录即时生成的示例，不是真实模型推理结果',
      '证据不足以支撑明确结论：已按 evidence_sufficient=false 处理，未给出根因断言',
    ],
    advisory_only: true,
    provider: 'offline',
    model: '',
    prompt_version: 'diagnose-v1',
    stop_reason: 'completed',
    stop_reason_text: '正常完成',
    duration_ms: 3600,
    tool_calls: 0,
    total_tokens: 1280,
    run_id: run.run_id,
    job_id: run.job_id,
    session_id: `diag-j${run.job_id}-${run.run_id}`,
    created,
  }
  diag.explain =
    `【诊断·示例】任务 ${run.job_id}（run ${run.run_id}）\n` +
    `结论: ${diag.summary}\n` +
    `分类: ${diag.category_text}  置信度: ${diag.confidence}%（模型自评）  证据充分: 否\n` +
    `原因: ${diag.cause}\n` +
    '证据链:\n' +
    diag.evidence.map((e, i) => `  ${i + 1}. [${e.ref}] ${e.quote}\n     作用: ${e.why}`).join('\n') +
    '\n还需要采集:\n' +
    diag.next_data_to_collect.map((n) => `  - ${n}`).join('\n') +
    `\n终止原因: ${diag.stop_reason}（${diag.stop_reason_text}）`
  return diag
}

/** 把新产生的诊断登记成一条会话，保证 /agents 立刻能看到这次调用 */
function registerDiagnosisSession(run, diag, { forced, existing }) {
  const ms = Date.now()
  const session = {
    session_id: diag.session_id,
    kind: 'diagnose',
    job_id: run.job_id,
    run_id: run.run_id,
    node_uuid: run.node_uuid,
    goal: `${forced ? '重新诊断' : '诊断'}任务「${run.job_name}」（run ${run.run_id}）`,
    status: 'completed',
    stop_reason: diag.stop_reason,
    stop_reason_text: diag.stop_reason_text,
    provider: diag.provider,
    model: diag.model,
    prompt_version: diag.prompt_version,
    model_calls: 1,
    tool_calls: diag.tool_calls,
    total_tokens: diag.total_tokens,
    duration_ms: diag.duration_ms,
    warn_count: diag.warnings.length,
    evidence_count: diag.evidence.length,
    created: diag.created,
    created_ms: ms,
  }
  if (existing) {
    // 同一 run 重新诊断：覆盖旧会话条目，而不是堆两条同 session_id 的记录
    const idx = extraSessions.findIndex((s) => s.session_id === session.session_id)
    if (idx >= 0) extraSessions.splice(idx, 1)
  }
  extraSessions.unshift(session)
  return session
}

/** §3.2 触发诊断：force=false 幂等返回已有诊断；force=true 重新诊断（消耗 token） */
export function agentDiagnosisRun({ run_id: runId, force } = {}) {
  const run = runs.find((r) => r.run_id === runId)
  if (!run) return bizErr(1010)
  const gate = agentGate(run.node_uuid)
  if (gate) return gate
  const existed = extraDiagnoses[runId] || agentDiagnoses[runId]
  if (existed && force !== true) {
    return {
      code: 200,
      data: { found: true, session_id: existed.session_id, diagnosis: existed.diagnosis },
      msg: 'operation success',
    }
  }
  const diagnosis = buildDiagnosis(run)
  extraDiagnoses[runId] = { session_id: diagnosis.session_id, diagnosis }
  registerDiagnosisSession(run, diagnosis, { forced: Boolean(existed), existing: Boolean(existed) })
  return {
    code: 200,
    data: { found: true, session_id: diagnosis.session_id, diagnosis },
    msg: existed ? '已重新诊断（本次调用消耗了模型预算）' : '诊断完成（Mock 示例）',
  }
}

/** §4 只读工具清单 + 策略配额 */
export function agentToolList() {
  const gate = agentGate(null, { needModel: false })
  if (gate) return gate
  return { code: 200, data: { tools: agentTools, policy: agentToolPolicy }, msg: 'operation success' }
}

/**
 * §5.1 自然语言 → 任务草案。
 *
 * Mock **没有理解能力**：按关键词在三份样例草案里挑一份，并在 warnings 里写明这一点。
 * 真实实现由后端模型产出（同一套字段），所以页面不需要为 Mock 写分支。
 */
const DRAFT_RULES = [
  { re: /http|接口|url|健康检查|探活|webhook/i, index: 1, label: 'http 类' },
  { re: /报表|report|审计|五段|脚本|sh\b/i, index: 2, label: '含五段表达式的 cmd 类' },
]

export function agentNlJobDraft({ text, node_uuid: nodeUuid, timezone } = {}) {
  const gate = agentGate(nodeUuid)
  if (gate) return gate
  const raw = String(text ?? '').trim()
  if (!raw) return bizErr(1009, 'text 为空：请先用一句话描述要做什么')
  if (raw.length > 2000) return bizErr(1009, `text 长度 ${raw.length} 超过 2000 字，请精简后重试`)
  const hit = DRAFT_RULES.find((r) => r.re.test(raw))
  const draft = { ...(hit ? agentDrafts[hit.index] : agentDrafts[0]) }
  if (timezone) draft.timezone = timezone
  draft.warnings = [
    ...(draft.warnings || []),
    `（Mock 数据）草案由关键词匹配本地样例生成（命中：${hit ? hit.label : 'cmd 类'}），未调用真实模型`,
  ]
  return { code: 200, data: draft, msg: 'operation success' }
}

/** 复验单条 overrides 字段：空值表示"不改"（与后端 §5.2 一致） */
function overrideNum(value, { min, max, field }) {
  if (value === undefined || value === null || value === '' || Number(value) === -1) return { keep: true }
  const n = Number(value)
  if (!Number.isFinite(n) || n < min || n > max) {
    return { error: `${field}=${value} 不在允许范围 ${min}~${max}（-1 表示不改）` }
  }
  return { value: Math.round(n) }
}

const CRON_PART = /^(\*|\d+|\*\/\d+|\d+-\d+|\d+(,\d+)*)$/

/** §5.2 确认创建：确认前不会写入调度器；确认后落成 status=0（未分配）的任务 */
export function agentNlJobConfirm({ session_id: sessionId, confirmed, overrides } = {}) {
  const draft = agentDrafts.find((d) => d.session_id === sessionId)
  if (!draft) return bizErr(1008, `草案会话 ${sessionId || '(空)'} 不存在或已过期`)
  const gate = agentGate(null)
  if (gate) return gate
  if (confirmed !== true) {
    return bizErr(1009, 'confirmed 不为 true：确认前不会写入调度器，也不会创建任务')
  }
  const ov = overrides || {}
  const name = String(ov.name ?? '').trim() || draft.name
  const spec = String(ov.spec ?? '').trim() || draft.spec
  const timeout = overrideNum(ov.timeout_seconds, { min: 1, max: 86400, field: 'timeout_seconds' })
  const retry = overrideNum(ov.retry_times, { min: 0, max: 100, field: 'retry_times' })
  if (timeout.error) return bizErr(1009, timeout.error)
  if (retry.error) return bizErr(1009, retry.error)
  if (name.length > 64) return bizErr(1009, `name 长度 ${name.length} 超过 64 字`)
  const parts = spec.split(/\s+/)
  if (parts.length !== 5 && parts.length !== 6) {
    return bizErr(1009, `spec "${spec}" 有 ${parts.length} 段：本系统要求 6 段（秒 分 时 日 月 周）`)
  }
  const bad = parts.find((p) => !CRON_PART.test(p))
  if (bad) return bizErr(1009, `spec "${spec}" 的第 ${parts.indexOf(bad) + 1} 段 "${bad}" 不是合法取值`)
  if (draft.job_type === 'cmd' && (!draft.command || draft.command.length === 0)) {
    return bizErr(1009, 'command 为空：cmd 类型任务必须给出 argv')
  }

  const jobId = jobs.reduce((max, j) => Math.max(max, Number(j.id) || 0), 0) + 1
  const isHttp = draft.job_type === 'http'
  const job = {
    id: jobId,
    name,
    command: isHttp ? '' : draft.command.join(' '),
    spec,
    run_on: '',
    host_name: '',
    ip: '',
    status: 0, // 未分配：等调度器分配节点
    job_type: isHttp ? 2 : 1,
    http_method: isHttp ? 1 : 0,
    retry_times: retry.keep ? draft.retry_times : retry.value,
    retry_interval: draft.retry_interval,
    timeout: timeout.keep ? draft.timeout_seconds : timeout.value,
    notify_type: 0,
    note: `由自然语言草案创建（会话 ${sessionId}，provider=${draft.provider}，prompt=${draft.prompt_version}）`,
    created: Math.floor(Date.now() / 1000),
    updated: Math.floor(Date.now() / 1000),
    concurrency_policy: draft.concurrency_policy,
    step_count: 0,
    last_run_status: 0,
    success_rate: 0,
  }
  jobs.push(job)

  const times = specTriggers(spec)
  return {
    code: 200,
    data: {
      job_id: jobId,
      spec,
      status: 0,
      next_triggers: times.map((t) => rfc3339(t.getTime())),
      trigger_echo: triggerEcho(spec, times),
      created: rfc3339(Date.now()),
    },
    msg: '任务已创建（未分配，等待调度器分配节点）',
  }
}

/* ------------------------------------------------------------------ *
 * 阶段 1 遗留的两个 Agent Mock —— **只为不破坏 scripts/smoke-mock.mjs**（该脚本不在本次改动范围内）。
 * 阶段 2 真实契约里**没有** /agent/chat 与 /agent/dispatch：Agent 不再是一个可以"聊天"的对象，
 * 而是 diagnose / nljob / job 三类会话（§1）。这里返回的文案如实说明这一点，
 * 不再假装有一个多 Agent 编排层。新页面一律使用上面的 §1–§5 接口。
 * ------------------------------------------------------------------ */

export function agentChat({ agent_id: agentId, message }) {
  const reply = [
    '阶段 2 没有 /agent/chat 这个接口：Agent 内核不是"聊天对象"，而是受预算约束的会话（session）。',
    '',
    `你刚才说的是：${message || '(空)'}`,
    '',
    '对应的真实入口：',
    '· 想让模型诊断某次失败 → 在 /runs 里对该 run 点"AI 诊断"（§3.1 读 / §3.2 触发）；',
    '· 想用一句话建任务 → /jobs 的"自然语言建任务"（§5.1 草案 → §5.2 确认）；',
    '· 想看 Agent 到底做了什么 → /agents 的会话列表与轨迹回放（§1 / §2）。',
    '',
    `（本回复来自 Mock；agent_id=${agentId || '(未提供)'} 在当前契约里没有对应实体。）`,
  ].join('\n')
  return { code: 200, data: { reply, created: new Date().toISOString() }, msg: 'operation success' }
}

export function agentDispatch({ agent_id: agentId, goal, task }) {
  return {
    code: 200,
    data: {
      accepted: false,
      agent_id: agentId || '',
      goal: goal || task || '',
      reason: '阶段 2 的 Agent 不会接受"下发任务"：所有工具都是只读的，诊断结果也恒为 advisory_only（只给建议）。',
      real_entry: '自然语言建任务请用 /agent/nljob/draft（§5.1）→ /agent/nljob/confirm（§5.2）',
    },
    msg: '未受理：阶段 2 不存在 /agent/dispatch（Mock 兼容返回）',
  }
}
  /* ------------------- 知识库 / 审计（规划中） ------------------- */

export function knowledgeSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = [...knowledgeDocs]
  const kw = params.keyword
  if (kw) {
    list = list.filter((d) => includes(d.title, kw) || includes(d.summary, kw) || includes(d.tags.join(','), kw))
  }
  if (params.tag) list = list.filter((d) => d.tags.includes(params.tag))
  if (params.type) list = list.filter((d) => d.type === params.type)
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

export function auditSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = [...auditLogs]
  if (params.operator) list = list.filter((l) => includes(l.operator, params.operator))
  if (params.action) list = list.filter((l) => includes(l.action, params.action) || includes(l.action_text, params.action))
  if (params.target) list = list.filter((l) => includes(l.target, params.target))
  if (params.result) list = list.filter((l) => l.result === params.result)
  if (params.ip) list = list.filter((l) => includes(l.ip, params.ip))
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

/* ------------------------- 设置 ------------------------- */

export function settingsQuery() {
  return { code: 200, data: settingsSnapshot, msg: 'operation success' }
}

export function changePassword({ username, old_pw: oldPw, new_pw: newPw }) {
  if (!oldPw || !newPw) return { code: 1001, data: {}, msg: '口令不能为空' }
  if (String(newPw).length < 8) return { code: 1000, data: {}, msg: '新口令长度不足 8 位' }
  return { code: 200, data: { username }, msg: '口令修改成功（Mock）' }
}

export { runSteps }
/* ==========================================================================
 * 阶段 4.2：写动作审批 + 工具调用审计（与 admin/internal/handler/agent_approval.go 同契约）
 *
 * Mock 的职责是"让页面在没有后端时也能把状态看全"，所以：
 *   - 六种状态都有样例（pending/executed/execution_failed/blocked/rejected/expired）；
 *   - 决策接口只动内存里的那一行，且**复刻后端的业务码**：
 *       pending 之外的单子再决策 → 1014（状态已变更）；
 *       blocked/expired 的单子 → 1015/1014；
 *       找不到单号 → 1013。
 *     前端因此不能在 Mock 模式下"点什么成什么"——错误分支必须真的走一遍，
 *     否则真实联调时才发现提示文案不对就晚了。
 * ========================================================================== */

/** 把审批行按"现在"重新算剩余秒数（样例数据是相对时间生成的）。 */
function approvalView(row) {
  const remain = row.status === 'pending' ? Math.max(0, row.expires_at - NOW_SECONDS) : 0
  return { ...row, remain_seconds: remain }
}

function nowSeconds() {
  return Math.floor(Date.now() / 1000)
}

const NOW_SECONDS = nowSeconds()

/** POST /agent/approval/search */
export function approvalSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = approvalRequests.map(approvalView)
  if (params.status) list = list.filter((r) => r.status === params.status)
  if (params.tool) list = list.filter((r) => includes(r.tool, params.tool))
  if (params.target_node) list = list.filter((r) => includes(r.target_node, params.target_node))
  // 待审批在前（与人处理顺序一致），其余按创建时间倒序。
  list.sort((a, b) => (a.status === 'pending' ? -1 : 0) - (b.status === 'pending' ? -1 : 0) || b.created - a.created)
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

/** POST /agent/approval/find */
export function approvalFind({ approval_id: approvalId } = {}) {
  const row = approvalRequests.find((r) => r.approval_id === approvalId)
  if (!row) return bizErr(1013, '审批单不存在（Mock）')
  return { code: 200, data: { found: true, approval: approvalView(row) }, msg: 'operation success' }
}

/**
 * approve / reject / cancel 三个动作共用一个处理器（与后端 decide() 同形状）。
 * action: 'approve' | 'reject' | 'cancel'
 */
export function approvalDecide({ approval_id: approvalId, note, action } = {}) {
  const row = approvalRequests.find((r) => r.approval_id === approvalId)
  if (!row) return bizErr(1013, '审批单不存在（Mock）')
  if (row.status !== 'pending') {
    // 复刻后端：只有 pending 能被决策（CAS 语义）。
    return bizErr(1014, `审批单状态已变更（当前 ${row.status_text || row.status}），本次决策未生效`)
  }
  if (action === 'reject') {
    row.status = 'rejected'
    row.status_text = '已驳回'
    row.decided_by = 'admin'
    row.decided_at = nowSeconds()
    row.decision_note = note || ''
    row.remain_seconds = 0
    return { code: 200, data: { executed: false, approval: approvalView(row) }, msg: '已驳回（未执行任何动作）' }
  }
  if (action === 'cancel') {
    row.status = 'canceled'
    row.status_text = '已撤销'
    row.decided_by = 'admin'
    row.decided_at = nowSeconds()
    row.decision_note = note || ''
    row.remain_seconds = 0
    return { code: 200, data: { executed: false, approval: approvalView(row) }, msg: '已撤销' }
  }
  // 批准：Mock 里"执行成功"。真实环境这里是**真的会动手**的一步。
  row.status = 'executed'
  row.status_text = '已执行'
  row.decided_by = 'admin'
  row.decided_at = nowSeconds()
  row.decision_note = note || ''
  row.executed_at = nowSeconds()
  row.execution_result = '（Mock）已执行：真实环境这里会向原节点投递一次重跑'
  row.executed = true
  row.remain_seconds = 0
  return { code: 200, data: { executed: true, approval: approvalView(row) }, msg: '已批准并执行（一次批准一次执行）' }
}

/** POST /agent/audit/search —— 工具网关审计（decision ∈ allowed/denied/approval_required/approval_executed） */
export function toolAuditSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = [...toolAuditRows]
  if (params.session_id) list = list.filter((r) => includes(r.session_id, params.session_id))
  if (params.tool) list = list.filter((r) => includes(r.tool, params.tool))
  if (params.decision) list = list.filter((r) => r.decision === params.decision)
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

/* ==========================================================================
 * 阶段 4-A：Agent 经验库（记忆）—— Mock 处理器
 *
 * 与后端 agent_memory.go 逐条对齐的三件事，Mock 里也必须成立（否则切到真机会翻车）：
 *  1. **没有编辑接口**：只有 disable/enable；
 *  2. **停用必须写原因**：空原因返回 1001（参数错误），且状态不变；
 *  3. **经验不是证据**：返回值里写死 advisory_only=true / usable_as_evidence=false，
 *     并带 notice（前端照着渲染，不自己拼中文）。
 * ========================================================================== */

/** 后端 source 元信息块（machine-readable 的约束）。 */
function memorySourceBlock() {
  return { ...memorySource }
}

/** POST /agent/memory/search */
export function memorySearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = agentMemories.map((m) => ({ ...m }))
  if (params.outcome) list = list.filter((m) => m.outcome === params.outcome)
  if (params.action_kind) list = list.filter((m) => m.action_kind === params.action_kind)
  if (params.tool) list = list.filter((m) => includes(m.tool, params.tool))
  if (params.target_node) list = list.filter((m) => includes(m.target_node, params.target_node))
  if (params.session_id) list = list.filter((m) => includes(m.session_id, params.session_id))
  // enabled：undefined/null → 全部；true/false → 精确匹配（与后端 *bool 三态一致）。
  if (params.enabled === true || params.enabled === false) {
    list = list.filter((m) => m.enabled === params.enabled)
  }
  if (params.keyword) {
    const kw = String(params.keyword)
    list = list.filter((m) => includes(m.symptom, kw) || includes(m.action_text, kw) || includes(m.result_text, kw))
  }
  // 最近出现靠前：经验库里"最近被反复撞见"的条目最值得看。
  list.sort((a, b) => b.last_seen - a.last_seen)
  const pageData = paginate(list, { page, pageSize })
  return { code: 200, data: { ...pageData, source: memorySourceBlock() }, msg: 'operation success' }
}

/** POST /agent/memory/find */
export function memoryFind({ memory_id: memoryId } = {}) {
  const row = agentMemories.find((m) => m.memory_id === memoryId)
  if (!row) return bizErr(1019, '经验条目不存在（Mock）')
  return { code: 200, data: { found: true, memory: { ...row }, source: memorySourceBlock() }, msg: 'operation success' }
}

/**
 * POST /agent/memory/recall —— 以症状试召回。
 *
 * Mock 用**与后端同思路**的粗打分：按症状的字符二元组覆盖率算相似度，
 * 不做花哨算法，但保证"相似的高、不相似的不出现"。空结果是合法结果。
 */
function bigrams(text) {
  const s = String(text || '').replace(/\s+/g, '')
  const set = new Set()
  if (s.length === 1) set.add(s)
  for (let i = 0; i + 2 <= s.length; i += 1) set.add(s.slice(i, i + 2))
  return set
}

export function memoryRecall({ symptom, agent_kind: agentKind, target_node: targetNode, top_k: topK, min_score: minScore } = {}) {
  const q = bigrams(symptom)
  if (q.size === 0) return bizErr(1001, '症状不能为空（没有症状就无从召回）')
  const min = Number(minScore) > 0 ? Number(minScore) : 0.34
  const k = Number(topK) > 0 ? Number(topK) : 3
  const hits = []
  for (const m of agentMemories) {
    if (!m.enabled) continue // 被停用的**绝不**参与召回
    const t = bigrams(m.symptom)
    let inter = 0
    for (const g of q) if (t.has(g)) inter += 1
    const score = inter / q.size
    if (score < min) continue
    hits.push({
      memory_id: m.memory_id,
      score: Number(score.toFixed(4)),
      matched: [...q].filter((g) => t.has(g)).slice(0, 8),
      reasons: [agentKind && m.agent_kind === agentKind ? '同类任务（diagnose）' : '', targetNode && m.target_node === targetNode ? '同一目标节点' : ''].filter(Boolean),
      symptom: m.symptom,
      action_kind: m.action_kind,
      action_text: m.action_text,
      target_node: m.target_node,
      outcome: m.outcome,
      outcome_text: m.outcome_text,
      occurrence_count: m.occurrence_count,
      success_count: m.success_count,
      fail_count: m.fail_count,
      blocked_count: m.blocked_count,
      denied_count: m.denied_count,
      advised_count: m.advised_count,
      success_rate: m.success_rate,
      success_rate_known: m.success_rate_known,
      same_kind: Boolean(agentKind) && m.agent_kind === agentKind,
      same_target: Boolean(targetNode) && m.target_node === targetNode,
      is_failed: m.outcome !== 'success',
      last_seen: m.last_seen,
    })
  }
  hits.sort((a, b) => b.score - a.score)
  return {
    code: 200,
    data: {
      hits: hits.slice(0, k),
      considered: agentMemories.filter((m) => m.enabled).length,
      query_tokens: [...q].slice(0, 20),
      min_score: min,
      top_k: k,
      // 恒真：召回结果只是线索。前端**必须**把这句话渲染出来。
      advisory_only: memorySource.advisory_only,
      usable_as_evidence: memorySource.usable_as_evidence,
      notice: memorySource.notice,
    },
    msg: 'operation success',
  }
}

/** POST /agent/memory/stats */
export function memoryStats() {
  const byOutcome = {}
  for (const m of agentMemories) byOutcome[m.outcome] = (byOutcome[m.outcome] || 0) + 1
  let successSum = 0
  let failSum = 0
  for (const m of agentMemories) {
    successSum += m.success_count
    failSum += m.fail_count
  }
  const known = successSum + failSum > 0
  return {
    code: 200,
    data: {
      total: agentMemories.length,
      enabled: agentMemories.filter((m) => m.enabled).length,
      disabled: agentMemories.filter((m) => !m.enabled).length,
      by_outcome: byOutcome,
      success_sum: successSum,
      fail_sum: failSum,
      success_rate: known ? Number((successSum / (successSum + failSum)).toFixed(6)) : 0,
      success_rate_known: known,
      success_rate_text: known
        ? `${((successSum / (successSum + failSum)) * 100).toFixed(1)}%（成功 ${successSum} / 失败 ${failSum}）`
        : '无执行样本（被拒/未执行不计入成功率）',
      definition: {
        success_rate: '成功 /（成功 + 失败）；分母只含**真正执行过**的动作',
        excluded: ['blocked（转人工/复核未通过，未执行）', 'denied（被闸门拒绝，未执行）', 'advised（只给建议，没有动作）'],
        advisory_only: '经验只作排查线索，不计入本次证据',
      },
      source: memorySourceBlock(),
    },
    msg: 'operation success',
  }
}

/** POST /agent/memory/disable|enable —— 共用处理器（enabled 决定方向） */
export function memorySetEnabled({ memory_id: memoryId, enabled, reason } = {}) {
  const row = agentMemories.find((m) => m.memory_id === memoryId)
  if (!row) return bizErr(1019, '经验条目不存在（Mock）')
  if (enabled === false) {
    // 复刻后端：停用必须写原因（没原因的停用事后无人能复盘）。
    if (!String(reason || '').trim()) {
      return bizErr(1001, '停用经验必须填写原因（治理要求：撤下动作要可复盘）')
    }
    if (!row.enabled) return bizErr(1020, '该经验已处于停用状态（状态已变更，请刷新）')
    row.enabled = false
    row.disabled_by = 'admin'
    row.disabled_reason = String(reason).trim()
    row.disabled_at = new Date().toISOString()
    return { code: 200, data: { memory: { ...row }, source: memorySourceBlock() }, msg: '已停用（条目保留，仅不再参与召回）' }
  }
  if (row.enabled) return bizErr(1020, '该经验已处于启用状态（状态已变更，请刷新）')
  row.enabled = true
  // 重新启用必须清空停用审计（否则页面上会显示"已启用但还带着上次的停用理由"）。
  row.disabled_by = ''
  row.disabled_reason = ''
  row.disabled_at = ''
  return { code: 200, data: { memory: { ...row }, source: memorySourceBlock() }, msg: '已启用' }
}