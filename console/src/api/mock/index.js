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
  // 与后端契约对齐：每行带 source 标注来源，并写明合并窗口（Mock 也要有同样的响应形状）。
  const withSource = list.map((l) => ({
    ...l,
    source: l.operator_type === 'agent' ? 'tool_audit' : 'approval',
  }))
  const data = paginate(withSource, { page, pageSize })
  data.source_window = withSource.length
  data.notice = '合并自 agent_tool_audit 与 approval_request 两个真实来源（各取最近 N 行）；ip 当前无采集恒为空。'
  return { code: 200, data, msg: 'operation success' }
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

/* ==========================================================================
 * 阶段 4-C：自愈调度 —— Mock 处理器
 *
 * 与后端 job_heal.go 逐条对齐的约定（Mock 里也必须成立，否则切到真机会翻车）：
 *  1. **没有"直接执行动作"的接口**：只有 search/find/by_run/policy/scan；
 *  2. **scan 不绕过闸门**：它跟自动轮次走同一套判定，返回值里必须带 policy 快照；
 *  3. **executed ≠ 成功**：中间态保留 deadline，且不写 finished_at；
 *  4. **每个动作都留痕**：连"被拒绝、没动手"也有记录（denied 是结论，不是没发生）。
 * ========================================================================== */

/** 后端 models.HealStatus → 中文（与 common/models/job_heal.go 一致）。 */
const HEAL_STATUS_NAMES = {
  planned: '已规划',
  denied: '被策略拒绝（未动手）',
  executed: '已执行待观测',
  succeeded: '自愈成功',
  failed: '自愈失败',
  escalated: '已转人工',
}

/** 后端 models.HealActionNames。 */
const HEAL_ACTION_NAMES = {
  retry_same_node: '原节点重跑',
  retry_extend_timeout: '调大超时后重跑',
  escalate: '转人工处理',
}

/** 每类失败允许的动作（与 handler/job_heal.go 的 classes 同源）。 */
const HEAL_CLASS_ACTIONS = [
  { class: 'node_unreachable', desc: '节点不可达', actions: ['escalate'] },
  { class: 'timeout', desc: '执行超时', actions: ['retry_extend_timeout', 'retry_same_node'] },
  { class: 'command_failed', desc: '命令执行失败', actions: ['retry_same_node'] },
  { class: 'delivery_failed', desc: '投递失败', actions: ['retry_same_node'] },
  { class: 'orchestration_invalid', desc: '编排非法', actions: ['escalate'] },
  { class: 'unknown', desc: '未知失败', actions: ['escalate'] },
]

/** Mock 策略：默认整段是"关着"的那一档，与仓库默认配置一致。 */
const healPolicyState = {
  enabled: true,
  interval_seconds: 60,
  max_attempts_per_job_window: 3,
  window_seconds: 1800,
  cooldown_seconds: 300,
  extend_timeout_factor: 2,
  max_extend_timeout_seconds: 3600,
  max_concurrent: 2,
  observe_window_seconds: 900,
  scan_limit: 50,
  lookback_seconds: 1800,
}

const nowSec = () => Math.floor(Date.now() / 1000)

/** 自愈记录（事件，不可去重）。status 覆盖全部 6 种，便于前端把状态机画全。 */
const healRows = [
  {
    heal_id: 'heal-1700000001000000000-1',
    source_run_id: 'j910001-t1700000000',
    job_id: 910001,
    job_name: '每日对账（示例）',
    attempt: 1,
    status: 'executed',
    failure_class: 'command_failed',
    symptom: '任务「每日对账（示例）」(#910001) 命令执行失败：exit status 1（最近输出：对账差异 3 条）',
    action: 'retry_same_node',
    action_json: '{"job_id":910001,"node_uuid":"node-it-01","max_attempts":1}',
    reason: '命令失败：原地重跑一次（失败原因可能是瞬时的，重跑成本低且可逆）',
    policy_json: '{"allowed":true,"rule":"allowed","reason":"策略允许：任务已开启自愈、写能力已开、窗口内第 1 次（上限 3）","attempts":1,"window_count":0}',
    evidence_json: '{"run_id":"j910001-t1700000000","status":"failed","exit_code":1,"error":"exit status 1","node_reachable":true}',
    memory_json: '{"used":1,"top_score":0.62,"hint":"历史经验：本任务同类失败在 node-it-01 上原地重跑成功率 2/3"}',
    node_uuid: 'node-it-01',
    new_run_id: '',
    outcome_note: '已投递重跑，等待观测（窗口内没有新执行则判失败，不算成功）',
    deadline: nowSec() + 900,
    memory_ids: '',
    executed_by: 'self_heal_loop',
    created: nowSec() - 60,
    created_ms: 0,
    updated: nowSec() - 60,
    updated_ms: 0,
    finished_at: 0,
  },
  {
    heal_id: 'heal-1700000002000000000-2',
    source_run_id: 'j910002-t1699999000',
    job_id: 910002,
    job_name: '数据同步（示例）',
    attempt: 1,
    status: 'succeeded',
    failure_class: 'timeout',
    symptom: '任务「数据同步（示例）」(#910002) 执行超时：已超过 timeout=60s 仍未结束',
    action: 'retry_extend_timeout',
    action_json: '{"job_id":910002,"from_timeout":60,"to_timeout":120,"node_uuid":"node-it-01","max_attempts":1}',
    reason: '超时：先把超时上限 60s 调到 120s 再重跑（先改配置、改成功才动手）',
    policy_json: '{"allowed":true,"rule":"allowed","reason":"策略允许：任务已开启自愈、写能力已开、窗口内第 2 次（上限 3）","attempts":2,"window_count":1}',
    evidence_json: '{"run_id":"j910002-t1699999000","status":"timeout","node_reachable":true}',
    memory_json: '{"used":0,"top_score":0,"hint":"无相似历史经验"}',
    node_uuid: 'node-it-01',
    new_run_id: 'j910002-t1700000100',
    outcome_note: '自愈后观测到新执行成功（101s，未再超时）',
    deadline: 0,
    memory_ids: '["mem-1700000100000000000-1"]',
    executed_by: 'self_heal_loop',
    created: nowSec() - 900,
    created_ms: 0,
    updated: nowSec() - 600,
    updated_ms: 0,
    finished_at: nowSec() - 600,
  },
  {
    heal_id: 'heal-1700000003000000000-3',
    source_run_id: 'j910003-t1699998000',
    job_id: 910003,
    job_name: '日志清理（示例）',
    attempt: 2,
    status: 'failed',
    failure_class: 'delivery_failed',
    symptom: '任务「日志清理（示例）」(#910003) 投递失败：重跑请求未能送达节点',
    action: 'retry_same_node',
    action_json: '{"job_id":910003,"node_uuid":"node-it-02","max_attempts":1}',
    reason: '投递失败：原地重跑一次（投递通道抖动是常见的瞬时故障）',
    policy_json: '{"allowed":true,"rule":"allowed","reason":"策略允许：任务已开启自愈、写能力已开、窗口内第 2 次（上限 3）","attempts":2,"window_count":1}',
    evidence_json: '{"run_id":"j910003-t1699998000","status":"failed","error":"put key failed","node_reachable":true}',
    memory_json: '{"used":1,"top_score":0.41,"hint":"历史经验：投递失败多与 etcd 抖动相关，重跑一次即可"}',
    node_uuid: 'node-it-02',
    new_run_id: '',
    outcome_note: '动作未能生效：投递失败（观测窗口内没有出现新的执行）',
    deadline: 0,
    memory_ids: '["mem-1700000200000000000-2"]',
    executed_by: 'manual:admin',
    created: nowSec() - 1800,
    created_ms: 0,
    updated: nowSec() - 1700,
    updated_ms: 0,
    finished_at: nowSec() - 1700,
  },
  {
    heal_id: 'heal-1700000004000000000-4',
    source_run_id: 'j910004-t1699997000',
    job_id: 910004,
    job_name: '证书续签（示例）',
    attempt: 1,
    status: 'denied',
    failure_class: 'node_unreachable',
    symptom: '任务「证书续签（示例）」(#910004) 失败：连接被拒绝（节点不可达）',
    action: 'retry_same_node',
    action_json: '',
    reason: '节点不可达：本动作不可执行（应当转人工，而不是往不可达节点重投）',
    policy_json: '{"allowed":false,"rule":"unsafe_action","reason":"节点不可达时不允许重跑（会把失败再制造一次）","attempts":1,"window_count":0}',
    evidence_json: '{"run_id":"j910004-t1699997000","status":"failed","error":"connection refused","node_reachable":false}',
    memory_json: '{"used":0,"top_score":0,"hint":"无相似历史经验"}',
    node_uuid: 'node-it-03',
    new_run_id: '',
    outcome_note: '策略闸门：节点不可达时不允许重跑（会把失败再制造一次）',
    deadline: 0,
    memory_ids: '[]',
    executed_by: 'self_heal_loop',
    created: nowSec() - 3000,
    created_ms: 0,
    updated: nowSec() - 3000,
    updated_ms: 0,
    finished_at: nowSec() - 3000,
  },
  {
    heal_id: 'heal-1700000005000000000-5',
    source_run_id: 'j910005-t1699996000',
    job_id: 910005,
    job_name: '镜像构建（示例）',
    attempt: 3,
    status: 'escalated',
    failure_class: 'command_failed',
    symptom: '任务「镜像构建（示例）」(#910005) 命令执行失败：exit status 2（编译错误）',
    action: 'escalate',
    action_json: '',
    reason: '窗口内已尝试 3 次仍未恢复：交给人接手（继续自动重跑只是消耗预算）',
    policy_json: '{"allowed":false,"rule":"attempt_cap","reason":"窗口内真实尝试已达上限（3）：交给人接手","attempts":4,"window_count":3}',
    evidence_json: '{"run_id":"j910005-t1699996000","status":"failed","exit_code":2,"error":"exit status 2"}',
    memory_json: '{"used":2,"top_score":0.55,"hint":"历史经验：编译错误类失败重跑无效（成功率 0/4）"}',
    node_uuid: 'node-it-01',
    new_run_id: '',
    outcome_note: '超过尝试上限，已转人工并通知',
    deadline: 0,
    memory_ids: '["mem-1700000300000000000-3"]',
    executed_by: 'self_heal_loop',
    created: nowSec() - 5400,
    created_ms: 0,
    updated: nowSec() - 5000,
    updated_ms: 0,
    finished_at: nowSec() - 5000,
  },
]

/** 视图化：补中文名与"是中间态吗"这两个前端必须照着渲染的字段。 */
function healView(row) {
  const status = row.status
  return {
    ...row,
    status_text: HEAL_STATUS_NAMES[status] || status,
    action_text: HEAL_ACTION_NAMES[row.action] || row.action || '',
    // executed/planned 是中间态：页面上不能当成功渲染，也不能写结束时间。
    pending_observation: status === 'executed' || status === 'planned',
    finished: status === 'denied' || status === 'succeeded' || status === 'failed' || status === 'escalated',
  }
}

/** POST /agent/heal/search */
export function healSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = healRows.map((r) => healView({ ...r }))
  if (params.job_id) list = list.filter((r) => Number(r.job_id) === Number(params.job_id))
  if (params.status) list = list.filter((r) => r.status === params.status)
  if (params.failure_class) list = list.filter((r) => r.failure_class === params.failure_class)
  if (params.action) list = list.filter((r) => r.action === params.action)
  if (params.source_run_id) list = list.filter((r) => includes(r.source_run_id, params.source_run_id))
  if (params.node_uuid) list = list.filter((r) => includes(r.node_uuid, params.node_uuid))
  // 最近发生的靠前（事故看的是最近发生了什么，不是最早）。
  list.sort((a, b) => b.created - a.created)
  return {
    code: 200,
    data: {
      ...paginate(list, { page, pageSize }),
      status_names: { ...HEAL_STATUS_NAMES },
      action_names: { ...HEAL_ACTION_NAMES },
      // 机器可读的约束：页面上"没有直接执行按钮"不是遗漏，而是设计。
      source: {
        direct_action_api: false,
        manual_scan_bypasses_gates: false,
        executed_is_success: false,
      },
    },
    msg: 'operation success',
  }
}

/** POST /agent/heal/find */
export function healFind({ heal_id: healId } = {}) {
  const row = healRows.find((r) => r.heal_id === healId)
  if (!row) return bizErr(1023, '自愈记录不存在（Mock）')
  return {
    code: 200,
    data: {
      heal: healView({ ...row }),
      status_names: { ...HEAL_STATUS_NAMES },
      action_names: { ...HEAL_ACTION_NAMES },
    },
    msg: 'operation success',
  }
}

/** POST /agent/heal/by_run（按触发它的那次失败执行查） */
export function healByRun({ run_id: runId } = {}) {
  const id = String(runId || '').trim()
  if (!id) return bizErr(1001, 'run_id 不能为空（自愈记录以失败执行为锚点）')
  const list = healRows.filter((r) => r.source_run_id === id).map((r) => healView({ ...r }))
  return { code: 200, data: { list, total: list.length }, msg: 'operation success' }
}

/** POST /agent/heal/policy（只读边界） */
export function healPolicy() {
  return {
    code: 200,
    data: {
      policy: { ...healPolicyState },
      // 与仓库默认一致：写能力默认关闭。写能力关着时，重跑/改超时都会被拒（denied）。
      writes_enabled: false,
      status_names: { ...HEAL_STATUS_NAMES },
      classes: HEAL_CLASS_ACTIONS.map((c) => ({ ...c, actions: [...c.actions] })),
      action_names: { ...HEAL_ACTION_NAMES },
      source: {
        direct_action_api: false,
        manual_scan_bypasses_gates: false,
        three_gates: ['self_heal.enabled', 'job.self_heal=1', 'tools.allow-write'],
      },
    },
    msg: 'operation success',
  }
}

/**
 * POST /agent/heal/scan —— 手动跑一轮（Mock 只做"判定预览"，不产生新的历史）。
 *
 * 关键点：返回值里带上 policy 快照与"本次没有绕过闸门"的自述；
 * 若传了 run_id 但该执行不存在/不是失败态，返回 1025（与后端一致）。
 */
export function healScan({ run_id: runId } = {}) {
  const id = String(runId || '').trim()
  if (id) {
    const row = healRows.find((r) => r.source_run_id === id)
    if (!row) return bizErr(1023, `指定的执行记录没有失败自愈记录（run_id=${id}）`)
    return {
      code: 200,
      data: {
        heal: healView({ ...row }),
        policy: { ...healPolicyState },
        writes_enabled: false,
        gates_skipped: [],
        note: '手动触发不绕过闸门：本次判定与自动轮次完全一致',
      },
      msg: `已处置该次失败（结论：${HEAL_STATUS_NAMES[row.status] || row.status}）`,
    }
  }
  const executed = healRows.filter((r) => r.status === 'executed').length
  const succeeded = healRows.filter((r) => r.status === 'succeeded').length
  const denied = healRows.filter((r) => r.status === 'denied').length
  const escalated = healRows.filter((r) => r.status === 'escalated').length
  const failed = healRows.filter((r) => r.status === 'failed').length
  return {
    code: 200,
    data: {
      scanned: healRows.length,
      skipped: 0,
      denied,
      executed,
      escalated,
      failed,
      observed: 0,
      succeeded,
      errors: [],
      policy: { ...healPolicyState },
      writes_enabled: false,
      gates_skipped: [],
      note: '手动触发不绕过闸门：本次判定与自动轮次完全一致',
    },
    msg: '扫描完成',
  }
}
/* ===================== 阶段 4-D / 阶段 5：调度智能化与评测 Mock ===================== */

const OPS_WEIGHTS = { load: 60, capacity: 10, affinity: 15, success: 15 }

export function schedulerPolicy() {
  return {
    code: 200,
    data: {
      weights: { ...OPS_WEIGHTS },
      dimensions: [
        { key: 'load', desc: '负载（相对容量归一：任务数 / 并发上限）' },
        { key: 'capacity', desc: '容量（未打满时容量大者优先）' },
        { key: 'affinity', desc: '亲和（命中 prefer_node / 原节点）' },
        { key: 'success', desc: '健康度（近期成功率；样本 <3 时按中性处理）' },
      ],
      hard_constraints: [
        '节点必须可达（status != ConnFail）',
        '必须满足 job.node_selector.require_labels 全部标签',
        '必须存在 require_tools 声明的能力标签（tool:<name>）',
      ],
      note: '硬约束不满足即拒绝，绝不靠权重“扣分”放行；无可用节点时宁可不派发。',
    },
    msg: 'ok',
  }
}

export function schedulerAllocate({ job_id: jobId } = {}) {
  const chosen = 'node-a'
  const scores = [
    { uuid: 'node-a', hostname: 'node-a', total: 57.5, parts: { load: 30, capacity: 5, affinity: 15, success: 7.5 }, reasons: ['负载 1（未声明容量）', '命中亲和节点（原节点）'] },
    { uuid: 'node-b', hostname: 'node-b', total: 40, parts: { load: 25, capacity: 5, affinity: 0, success: 10 }, reasons: ['负载 3/8', '近期成功率 80%（5 样本）'] },
  ]
  return {
    code: 200,
    data: {
      decision: { chosen, scores, notes: [] },
      requirement: { RequireLabels: null, RequireTools: null, PreferNode: 'node-a' },
      job_id: jobId || 0,
    },
    msg: 'ok',
  }
}

export function opsMetrics() {
  return {
    code: 200,
    data: {
      runs_today: 42, runs_success_today: 39, success_rate_today: 0.928,
      avg_duration_ms_today: 3120, jobs_total: 18, jobs_assigned: 12,
      nodes_total: 3, nodes_up: 3, approvals_pending: 1,
      heals_by_status: { denied: 2, executed: 1, succeeded: 3, escalated: 1 },
      memory_enabled: 45, generated_at: Math.floor(Date.now() / 1000),
    },
    msg: 'ok',
  }
}

const OPS_SLO_ROWS = [
  { id: 1, name: 'orders-daily', job_id: 21, target_success_rate: 0.99, target_on_time_rate: 0.95, window_seconds: 86400, grace_seconds: 300, enabled: true, breach_action: 'diagnose' },
  { id: 2, name: 'cleanup-weekly', job_id: 0, target_success_rate: 0.9, target_on_time_rate: 0.8, window_seconds: 604800, grace_seconds: 600, enabled: true, breach_action: 'diagnose' },
]

export function sloList() {
  return { code: 200, data: { list: OPS_SLO_ROWS.map((r) => ({ ...r })), total: OPS_SLO_ROWS.length }, msg: 'ok' }
}

export function sloEvaluate({ job_id: jobId } = {}) {
  const list = OPS_SLO_ROWS.filter((r) => !jobId || r.job_id === jobId).map((r) => ({
    policy: { ...r },
    job_name: r.job_id ? 'orders-sync' : '',
    report: {
      total: 40, success: 38, on_time: 37, on_time_eligible: 40,
      success_rate: 0.95, on_time_rate: 0.925, breached: true, unverifiable: false,
      reasons: [`成功率 95% 低于目标 ${(r.target_success_rate * 100) | 0}%`],
    },
  }))
  const breached = list.filter((e) => e.report.breached).length
  return { code: 200, data: { list, total: list.length, breached }, msg: 'ok' }
}

const opsAutonomy = {
  'job_retry@prod': { scope: 'job_retry@prod', level: 2, streak: 2, total_up: 12, total_down: 0, frozen: false, frozen_reason: '', updated: Math.floor(Date.now() / 1000) },
  'node_drain@prod': { scope: 'node_drain@prod', level: 1, streak: 0, total_up: 3, total_down: 1, frozen: true, frozen_reason: '最近一次动作产生了不良后果：已降级并冻结，需人工解冻', updated: Math.floor(Date.now() / 1000) },
}
const LVL_NAME = ['L0', 'L1', 'L2', 'L3']

export function autonomyList() {
  const list = Object.values(opsAutonomy).map((r) => ({
    ...r,
    level_name: LVL_NAME[r.level] || 'L?',
    // 放行判定用的等级：冻结回落 L1，且不低于 L1（信用只能决定"要不要自动"）。
    effective_level: LVL_NAME[Math.max(1, r.frozen ? 1 : r.level)],
  }))
  return { code: 200, data: { list, total: list.length }, msg: 'ok' }
}

export function autonomyObserve({ scope, success } = {}) {
  if (!scope || typeof success !== 'boolean') return bizErr(1001, '需要 scope 与 success')
  const cur = opsAutonomy[scope] || { scope, level: 0, streak: 0, total_up: 0, total_down: 0, frozen: false, frozen_reason: '', updated: 0 }
  if (success) {
    cur.total_up += 1
    cur.streak += 1
    if (!cur.frozen && cur.streak >= 5 && cur.level < 2) { cur.level += 1; cur.streak = 0 }
  } else {
    cur.total_down += 1
    cur.streak = 0
    cur.level = Math.max(0, cur.level - 1)
    cur.frozen = true
    cur.frozen_reason = '最近一次动作产生了不良后果：已降级并冻结，需人工解冻'
  }
  cur.updated = Math.floor(Date.now() / 1000)
  opsAutonomy[scope] = cur
  return { code: 200, data: { credit: { ...cur } }, msg: 'ok' }
}

export function autonomyUnfreeze({ scope } = {}) {
  const cur = opsAutonomy[scope]
  if (!cur) return bizErr(1001, '该 scope 不存在')
  cur.frozen = false
  cur.frozen_reason = ''
  cur.streak = 0
  return { code: 200, data: { credit: { ...cur } }, msg: 'ok' }
}

// autonomyEffective 解释某工具当下实际生效的自治级别（与后端 /ops/autonomy/effective 同形状）。
// 判据与网关一致：放行等级 = max(L1, 信用级别)（信用只决定"要不要自动"）；
// 生效级别 = min(配置上限, 放行等级)；无历史则沿用配置上限。
export function autonomyEffective({ tool, env } = {}) {
  if (!tool) return bizErr(1001, '需要 tool')
  const e = env || 'default'
  const scope = `${tool}@${e}`
  const ceilingRank = 2 // Mock 里运维配置上限取 L2
  const out = {
    tool, env: e, scope,
    configured_ceiling: LVL_NAME[ceilingRank],
    frozen: false,
    effective: LVL_NAME[ceilingRank],
    source: 'config',
    credit_enabled: true,
    credit_known: false,
  }
  const row = opsAutonomy[scope]
  if (row) {
    const effectiveCreditRank = row.frozen ? 1 : row.level
    const enforcedRank = Math.max(1, effectiveCreditRank) // L1 底线
    out.credit_level = LVL_NAME[effectiveCreditRank]
    out.enforcement_level = LVL_NAME[enforcedRank]
    out.frozen = row.frozen
    out.frozen_reason = row.frozen_reason
    out.credit_known = true
    out.effective = LVL_NAME[Math.min(ceilingRank, enforcedRank)]
    out.source = enforcedRank > ceilingRank ? 'credit_capped' : 'credit'
  }
  return { code: 200, data: out, msg: 'ok' }
}

const OPS_CHAOS = [
  { kind: 'latency', target: 'db', severity: 1, params: { ms: '3000' } },
  { kind: 'disk_full', target: 'node', severity: 2, params: { fill: '95%' } },
  { kind: 'node_down', target: 'worker-1', severity: 3 },
]

export function chaosCatalog() {
  return { code: 200, data: { catalog: OPS_CHAOS.map((f) => ({ ...f })), note: '受控、可复现、可回滚；severity 越高影响面越大。' }, msg: 'ok' }
}

export function chaosGrade({ attempts } = {}) {
  const rows = Array.isArray(attempts) ? attempts : []
  if (rows.length === 0) return { code: 200, data: { score: { total: 0, license: -1, by_kind: {} } }, msg: 'ok' }
  let detected = 0, handled = 0, correct = 0, detSum = 0
  const kt = {}, kc = {}
  for (const a of rows) {
    if (a.detected) detected++
    if (a.handled) handled++
    if (a.correct) { correct++; kc[a.fault.kind] = (kc[a.fault.kind] || 0) + 1 }
    detSum += a.detection_ms || 0
    kt[a.fault.kind] = (kt[a.fault.kind] || 0) + 1
  }
  const by_kind = {}
  for (const k of Object.keys(kt)) by_kind[k] = (kc[k] || 0) / kt[k]
  const correct_rate = correct / rows.length
  return {
    code: 200,
    data: {
      score: {
        total: rows.length, detected, handled, correct,
        detect_rate: detected / rows.length, handle_rate: handled / rows.length,
        correct_rate, avg_detection_ms: Math.round(detSum / rows.length),
        license: Math.round(correct_rate * 100), by_kind,
      },
    },
    msg: 'ok',
  }
}

export function shadowReconcile({ baseline, candidate } = {}) {
  const b = baseline || {}, c = candidate || {}
  const diffs = []
  if (b.exit_code !== c.exit_code) diffs.push(`退出码不同：${b.exit_code} → ${c.exit_code}`)
  if (b.stdout_hash !== c.stdout_hash) diffs.push('stdout 不同')
  if (b.stderr_hash !== c.stderr_hash) diffs.push('stderr 不同')
  const key = (e) => `${e.kind}|${e.key}|${e.change}`
  const am = new Set((b.side_effects || []).map(key))
  const bm = new Set((c.side_effects || []).map(key))
  for (const k of am) if (!bm.has(k)) diffs.push('基线有而影子没有：' + k)
  for (const k of bm) if (!am.has(k)) diffs.push('影子多出副作用：' + k)
  const same = diffs.length === 0
  return { code: 200, data: { diff: { same, unexpected: !same, differences: diffs }, allow_promotion: same }, msg: 'ok' }
}
