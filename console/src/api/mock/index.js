/**
 * Mock 处理器（阶段 1）。
 *
 * 每个处理器都返回与后端**完全一致**的信封 { code, data, msg }，
 * 由 api/client.js 的 mockResponse() 走统一 unwrap（含业务错误弹窗）。
 * 这样 Mock 与真实后端的调用方行为一致，切换开关时页面无需改动。
 *
 * 分页契约与后端一致（admin/internal/model/resp/rsp.go → PageResult）：
 *   { list, total, page, page_size }
 * 分页入参默认值也一致（admin/internal/model/request/req.go → PageInfo.Check）：
 *   page<=0 → 1；page_size<=0 → 20
 */
import {
  agents,
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

/* ------------------- Agent / 知识库 / 审计（规划中） ------------------- */

export function agentSearch(params = {}) {
  const { page, pageSize } = normalizePage(params)
  let list = [...agents]
  if (params.name) list = list.filter((a) => includes(a.name, params.name))
  if (params.status) list = list.filter((a) => a.status === params.status)
  if (params.role) list = list.filter((a) => a.role === params.role)
  return { code: 200, data: paginate(list, { page, pageSize }), msg: 'operation success' }
}

export function agentChat({ agent_id: agentId, message }) {
  const agent = agents.find((a) => a.id === agentId) || agents[0]
  const reply = [
    `收到，我是「${agent.name}」。当前环境为 Mock（VITE_USE_MOCK=true），不会真正执行任何变更。`,
    '',
    `你的意图：${message}`,
    '',
    '我建议的执行路径：',
    '1. 先用只读工具采集现状（ssh_exec / prom_query），不产生副作用；',
    '2. 把采集结果与知识库 RB-1002《容器 unhealthy 自愈 Runbook》比对；',
    '3. 命中已知模式则给出修复步骤，变更类动作标记为 L2（需人工确认）；',
    '4. 全程写入审计链路（audit_id 由后端分配）。',
    '',
    '是否把第 1 步转成一个一次性 job_run？（阶段 2 才会真正下发）',
  ].join('\n')
  return { code: 200, data: { agent_id: agent.id, reply, created: new Date().toISOString() }, msg: 'operation success' }
}

export function agentDispatch({ agent_id: agentId, task }) {
  return {
    code: 200,
    data: { agent_id: agentId, task, run_id: `agent-${Date.now()}`, accepted: true },
    msg: '任务已受理（Mock）',
  }
}

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