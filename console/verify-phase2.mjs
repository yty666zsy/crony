/**
 * 阶段 2 契约自检（开发期工具，不参与构建，也不需要后端/浏览器/额外依赖）。
 *
 * 用法：node verify-phase2.mjs
 * 作用：用 Vite 的 SSR 加载器在 Node 里**真实执行** src/api/agent.js → src/api/mock 的调用链，
 *       逐条断言《PHASE2-接口契约.md》§1–§5 的字段、过滤/分页、三种诊断样本与错误码分支。
 * 为什么值得留一个脚本：这些断言正是"契约实现"与"看起来像契约实现"的区别，
 *       而 `pnpm smoke`（不允许改动）只覆盖 mock 处理器，不经过 src/api 层。
 */
import { register } from 'node:module'
import { join } from 'node:path'
import { writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import assert from 'node:assert/strict'
import './scripts/child-process-sandbox-shim.mjs'

process.env.CRONY_NO_DEP_OPT = '1'
register('./scripts/esbuild-shim-loader.mjs', import.meta.url)

/**
 * Node 里没有 document，client.js 的 unwrap() 在业务错误时会调 ElMessage.error()，
 * 直接跑会抛 "document is not defined" 从而掩盖 BizError。
 * 这里在临时目录生成一个只替换弹窗的 element-plus 替身（unwap 的语义保持真实），
 * 这样校验脚本自己就是单文件，不往仓库里多塞一个 stub。
 */
const stubPath = join(tmpdir(), 'crony-verify-phase2-element-plus-stub.mjs')
writeFileSync(
  stubPath,
  'export const ElMessage = { error() {}, success() {}, warning() {}, info() {} };\n' +
    'export default { ElMessage };\n',
)

const { createServer } = await import('vite')
const server = await createServer({
  configFile: false,
  root: process.cwd(),
  logLevel: 'error',
  resolve: {
    alias: {
      '@': join(process.cwd(), 'src'),
      'element-plus': stubPath,
    },
  },
  server: { middlewareMode: true },
  appType: 'custom',
  esbuild: false,
  optimizeDeps: { noDiscovery: true, include: [] },
})

const api = await server.ssrLoadModule('/src/api/agent.js')
const mock = await server.ssrLoadModule('/src/api/mock/index.js')
const data = await server.ssrLoadModule('/src/api/mock/data.js')

const checks = []
const check = (name, fn) => checks.push({ name, fn })

check('§1 会话列表：PageResult + 过滤 + 分页', async () => {
  const page = await api.searchAgentSessions({ page: 1, page_size: 50 })
  assert.deepEqual(Object.keys(page).sort(), ['list', 'page', 'page_size', 'total'], '应是 PageResult')
  assert.ok(page.total >= 3, `至少 3 条会话，实际 ${page.total}`)
  const one = page.list[0]
  for (const k of [
    'session_id', 'kind', 'job_id', 'run_id', 'node_uuid', 'goal', 'status', 'stop_reason',
    'stop_reason_text', 'provider', 'model', 'prompt_version', 'model_calls', 'tool_calls',
    'total_tokens', 'duration_ms', 'warn_count', 'evidence_count', 'created', 'created_ms',
  ]) {
    assert.ok(k in one, `会话缺少字段 ${k}`)
  }
  const kinds = new Set(page.list.map((s) => s.kind))
  assert.ok(kinds.has('diagnose') && kinds.has('nljob') && kinds.has('job'), '应覆盖三种 kind')
  const statuses = new Set(page.list.map((s) => s.status))
  assert.ok(statuses.has('completed') && statuses.has('failed') && statuses.has('canceled'), '应覆盖三种 status')
  const onlyDiag = await api.searchAgentSessions({ kind: 'diagnose' })
  assert.ok(onlyDiag.list.every((s) => s.kind === 'diagnose'), 'kind 过滤应生效')
  const failed = await api.searchAgentSessions({ status: 'failed' })
  assert.ok(failed.list.every((s) => s.status === 'failed'), 'status 过滤应生效')
  const paged = await api.searchAgentSessions({ page: 2, page_size: 3 })
  assert.equal(paged.list.length, Math.min(3, paged.total - 3), '分页应生效')
})

check('§2 会话详情：轨迹字段齐全且 tool 步骤带 args/observation', async () => {
  const page = await api.searchAgentSessions({ page_size: 50 })
  for (const s of page.list) {
    const d = await api.findAgentSession(s.session_id)
    for (const k of ['system', 'context', 'budget', 'warnings', 'evidence', 'tool_stats', 'steps', 'result']) {
      assert.ok(k in d, `会话 ${s.session_id} 详情缺少 ${k}`)
    }
    for (const k of ['calls', 'tokens', 'duration_ms', 'max_calls', 'max_tokens', 'max_duration_ms']) {
      assert.ok(k in d.budget, `budget 缺少 ${k}`)
    }
    for (const k of ['calls', 'denied', 'truncated', 'remaining']) {
      assert.ok(k in d.tool_stats, `tool_stats 缺少 ${k}`)
    }
    d.steps.forEach((step, i) => {
      assert.ok(['model', 'tool', 'notice'].includes(step.kind), `步骤 kind 非法：${step.kind}`)
      assert.ok('seq' in step && 'created' in step, `步骤缺少 seq/created（index=${i}）`)
      if (step.kind === 'tool') {
        assert.ok('tool_args' in step && 'observation' in step, 'tool 步骤必须有 tool_args 与 observation')
      }
      if (step.kind === 'model') {
        assert.ok('prompt_preview' in step && 'tool_calls' in step && 'total_tokens' in step, 'model 步骤字段不全')
      }
      if (step.kind === 'notice') assert.ok(step.notice, 'notice 步骤必须有 notice 文本')
    })
    for (const e of d.evidence) {
      assert.ok('source' in e && 'ref' in e && 'quote' in e, '§2 evidence 需要 source/ref/quote')
    }
  }
})

check('§2 未详情的会话（新诊断）也能取到详情', async () => {
  const s = await api.searchAgentSessions({ page_size: 50 })
  const target = s.list.find((x) => !data.agentSessionDetails[x.session_id])
  if (!target) return
  const d = await api.findAgentSession(target.session_id)
  assert.ok(Array.isArray(d.steps) && Array.isArray(d.warnings), '兜底详情应可渲染')
})

check('§3.1 三种诊断样本齐备；未诊断的 run 返回 found=false', async () => {
  const samples = Object.entries(data.agentDiagnoses).map(([runId, v]) => ({ runId, ...v }))
  const full = samples.find((s) => s.diagnosis.evidence_sufficient && s.diagnosis.unverified_refs.length === 0)
  const thin = samples.find((s) => !s.diagnosis.evidence_sufficient)
  const unv = samples.find((s) => s.diagnosis.unverified_refs.length > 0)
  assert.ok(full && thin && unv, '必须同时具备"证据充分/证据不足/引用未观测"三类样本')

  const fullRes = await api.findAgentDiagnosis(full.runId)
  assert.equal(fullRes.found, true)
  const thinRes = await api.findAgentDiagnosis(thin.runId)
  assert.ok(thinRes.diagnosis.next_data_to_collect.length > 0, '证据不足必须给出还要采集什么')
  const unvRes = await api.findAgentDiagnosis(unv.runId)
  assert.ok(unvRes.diagnosis.warnings.some((w) => w.includes('未观测')), '该样本必须有醒目警告文案')
  for (const d of [fullRes.diagnosis, thinRes.diagnosis, unvRes.diagnosis]) {
    for (const k of [
      'summary', 'category', 'category_text', 'cause', 'confidence', 'evidence', 'assumptions',
      'ruled_out', 'suggested_actions', 'evidence_sufficient', 'next_data_to_collect',
      'unverified_refs', 'warnings', 'advisory_only', 'provider', 'model', 'prompt_version',
      'stop_reason', 'stop_reason_text', 'duration_ms', 'tool_calls', 'total_tokens',
      'session_id', 'run_id', 'created', 'explain',
    ]) {
      assert.ok(k in d, `diagnosis 缺少字段 ${k}`)
    }
    assert.equal(d.advisory_only, true, '阶段 2 恒为 advisory_only')
    for (const a of d.suggested_actions) {
      for (const k of ['title', 'detail', 'risk', 'risk_text', 'rollback']) assert.ok(k in a, `动作缺少 ${k}`)
    }
    for (const e of d.evidence) {
      for (const k of ['ref', 'quote', 'why']) assert.ok(k in e, `诊断证据缺少 ${k}`)
    }
  }
  const none = await api.findAgentDiagnosis(data.runs[0].run_id)
  assert.equal(none.found, false)
  assert.equal(none.session_id, '')
  assert.equal(none.diagnosis, null)
})

check('§3.2 触发诊断：force=false 幂等、force=true 新增会话', async () => {
  const before = await api.searchAgentSessions({ page_size: 50 })
  const runId = data.runs[0].run_id
  const first = await api.runAgentDiagnosis(runId)
  assert.equal(first.found, true)
  assert.equal(first.diagnosis.evidence_sufficient, false, 'Mock 即时诊断不得编造高置信度结论')
  const second = await api.runAgentDiagnosis(runId, { force: false })
  assert.equal(second.diagnosis.created, first.diagnosis.created, 'force=false 应返回同一条诊断')
  const third = await api.runAgentDiagnosis(runId, { force: true })
  assert.equal(third.found, true)
  const after = await api.searchAgentSessions({ page_size: 50 })
  assert.equal(after.total, before.total + 1, '重诊应新增一条会话')
  const again = await api.searchAgentSessions({ page_size: 50 })
  assert.equal(again.list.filter((s) => s.session_id === third.session_id).length, 1, '同一 run 不应堆重复会话')
})

check('§4 只读工具清单与配额', async () => {
  const r = await api.listAgentTools()
  assert.ok(r.tools.length >= 3, '工具清单非空')
  assert.ok(r.tools.every((t) => t.readonly === true), '阶段 2 工具必须全部只读')
  for (const t of r.tools) {
    for (const k of ['name', 'description', 'readonly', 'timeout_ms', 'schema']) assert.ok(k in t, `工具缺少 ${k}`)
  }
  for (const k of ['max_calls_per_session', 'max_calls_per_tool', 'max_output_bytes', 'sql_max_rows', 'allow_hosts', 'shell_read_paths']) {
    assert.ok(k in r.policy, `policy 缺少 ${k}`)
  }
})

check('§5.1 草案：cmd / http / 五段警告三份样例', async () => {
  const cmd = await api.draftNlJob({ text: '每天凌晨 2 点清理 /tmp 下 7 天前的文件', node_uuid: 'n-8f3d1c2a-01' })
  assert.equal(cmd.job_type, 'cmd')
  assert.ok(Array.isArray(cmd.command) && cmd.command.length > 0, 'cmd 草案必须有 argv')
  const http = await api.draftNlJob({ text: '每小时检查一次健康检查接口', node_uuid: 'n-8f3d1c2a-01' })
  assert.equal(http.job_type, 'http')
  assert.ok(http.http && http.http.url, 'http 草案必须有 http.url')
  const five = await api.draftNlJob({ text: '每天凌晨 2 点执行报表脚本', node_uuid: 'n-8f3d1c2a-01' })
  assert.equal(five.spec.trim().split(/\s+/).length, 5, '要有五段表达式样本')
  assert.ok(five.warnings.some((w) => w.includes('5 段')), '五段表达式必须有醒目警告')
  for (const d of [cmd, http, five]) {
    for (const k of [
      'session_id', 'name', 'description', 'job_type', 'spec', 'command', 'raw_argv', 'http',
      'timeout_seconds', 'retry_times', 'retry_interval', 'concurrency_policy',
      'concurrency_policy_text', 'concurrency_value', 'rationale', 'warnings', 'next_triggers',
      'next_trigger_text', 'trigger_echo', 'requires_confirmation', 'confirmed', 'timezone',
      'provider', 'model', 'prompt_version', 'repairs', 'stop_reason', 'stop_reason_text',
    ]) {
      assert.ok(k in d, `草案缺少字段 ${k}`)
    }
    assert.equal(d.next_triggers.length, 5, 'next_triggers 应为 5 条')
    assert.equal(d.next_trigger_text.length, 5, 'next_trigger_text 应为 5 条')
    assert.equal(d.requires_confirmation, true)
    assert.equal(d.confirmed, false, '草案阶段 confirmed 必须为 false')
    assert.ok(d.trigger_echo.includes('确认前不会写入调度器'), 'trigger_echo 必须写明确认前不落库')
    for (let i = 1; i <= 5; i += 1) {
      assert.ok(d.trigger_echo.includes(`${i})`), `trigger_echo 应列出第 ${i} 次触发时间`)
    }
  }
})

check('§5.2 确认创建：status=0 未分配、复验失败走 1009、未确认不落库', async () => {
  const draft = await api.draftNlJob({ text: '每天凌晨 2 点清理 /tmp 下 7 天前的文件', node_uuid: 'n-8f3d1c2a-01' })
  const created = await api.confirmNlJob({ session_id: draft.session_id, confirmed: true })
  assert.ok(created.job_id > 0)
  assert.equal(created.status, 0, '创建出来的任务必须是未分配')
  assert.equal(created.next_triggers.length, 5)
  assert.ok(created.trigger_echo.includes('确认前不会写入调度器'))
  assert.equal(created.created.length > 0, true)
  const jobs = data
  assert.ok(jobs.jobs.some((j) => Number(j.id) === Number(created.job_id)), '确认后任务应真的出现在任务列表里')

  await assert.rejects(
    () => api.confirmNlJob({ session_id: draft.session_id, confirmed: true, overrides: { spec: 'not a cron' } }),
    (e) => e.code === 1009,
    '非法 spec 必须走 1009',
  )
  await assert.rejects(
    () => api.confirmNlJob({ session_id: draft.session_id, confirmed: false }),
    (e) => e.code === 1009,
    'confirmed=false 不得创建任务',
  )
  await assert.rejects(
    () => api.confirmNlJob({ session_id: draft.session_id, confirmed: true, overrides: { timeout_seconds: 999999 } }),
    (e) => e.code === 1009,
    '越界 timeout 必须走 1009',
  )
  await assert.rejects(
    () => api.draftNlJob({ text: '   ' }),
    (e) => e.code === 1009,
    '空文本必须走 1009',
  )
})

check('错误码：1008 / 1010 / 1011 / 1007 分支', async () => {
  await assert.rejects(() => api.findAgentSession('diag-not-exist'), (e) => e.code === 1008)
  await assert.rejects(() => api.findAgentDiagnosis('run-not-exist'), (e) => e.code === 1010)
  await assert.rejects(() => api.runAgentDiagnosis('run-not-exist'), (e) => e.code === 1010)
  await assert.rejects(() => api.confirmNlJob({ session_id: 'nl-not-exist', confirmed: true }), (e) => e.code === 1008)

  // 1011 / 1007：改设置页快照即可命中（证明分支真的在，而不是装饰）
  const conf = data.settingsSnapshot.agent
  try {
    conf.enabled = false
    await assert.rejects(() => api.searchAgentSessions({}), (e) => e.code === 1011)
    await assert.rejects(() => api.runAgentDiagnosis(data.runs[0].run_id), (e) => e.code === 1011)
    conf.enabled = true
    conf.provider = ''
    await assert.rejects(() => api.runAgentDiagnosis(data.runs[0].run_id), (e) => e.code === 1007)
  } finally {
    conf.enabled = true
    conf.provider = 'offline'
  }
})

check('1012 节点离线分支（把 run 指到失联节点后可命中）', async () => {
  const run = data.runs[0]
  const origin = run.node_uuid
  try {
    run.node_uuid = 'n-7be2a904-01' // node-db-01：status=2 失联
    await assert.rejects(() => api.runAgentDiagnosis(run.run_id), (e) => e.code === 1012)
  } finally {
    run.node_uuid = origin
  }
})

/* ------------------------- §9 执行事实订阅（SSE） ------------------------- */

check('§9 SSE 解析器：跨 chunk 的半帧、CRLF、多行 data、注释行', async () => {
  const { createSSEParser } = await server.ssrLoadModule('/src/api/stream.js')
  const p = createSSEParser()

  // 一帧被切成两半：先给前半，不应吐出事件；补齐后才吐
  const half1 = 'id: 1\nevent: snapshot\ndata: {"seq":1,"kind":"snapshot","run":{"run_id":"r1"}'
  assert.deepEqual(p.push(half1), [], '不完整帧不能提前吐出')
  const evs = p.push('}\n\n')
  assert.equal(evs.length, 1)
  assert.equal(evs[0].event, 'snapshot')
  assert.equal(evs[0].id, '1')
  assert.equal(evs[0].data.run.run_id, 'r1', 'data 必须被解析成对象')

  // 一次 chunk 里多帧 + CRLF + 注释行（代理心跳）+ 多行 data（按 SSE 规范用 \n 连接）
  const multi =
    ': keep-alive\r\n\r\n' +
    'id: 2\r\nevent: step\r\ndata: {"seq":2,"kind":"step","steps":[]}\r\n\r\n' +
    'event: heartbeat\ndata: {"seq":3}\n\n'
  const list = p.push(multi)
  assert.deepEqual(
    list.map((e) => e.event),
    ['step', 'heartbeat'],
    '注释行必须被忽略，且 CRLF 要能正确切帧',
  )
  assert.equal(list[0].data.seq, 2)

  // 不以 data 结尾的半帧要留在缓冲里
  const pending = p.push('event: run\ndata: {"seq":4')
  assert.deepEqual(pending, [])
  assert.ok(p.pending().includes('"seq":4'), '未闭合内容应留在缓冲')
})

check('§9 订阅帧序列：snapshot → step… → run → end，且 end 后不再重连', async () => {
  const { openRunStream, createSSEParser } = await server.ssrLoadModule('/src/api/stream.js')
  assert.equal(typeof createSSEParser, 'function')

  const runId = data.runs[0].run_id
  const seen = []
  let closed = null
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('Mock 订阅 10s 未结束')), 10000)
    openRunStream(runId, {
      tickMs: 5,
      onEvent: (ev) => seen.push(ev.event),
      onClose: (_d, reason) => {
        closed = reason
        clearTimeout(timer)
        resolve()
      },
      onError: (err) => {
        clearTimeout(timer)
        reject(err)
      },
    })
  })
  assert.ok(seen.length >= 3, `事件过少：${seen.join(',')}`)
  assert.equal(seen[0], 'snapshot', '首帧必须是全量快照')
  assert.ok(seen.includes('step'), '必须有 step 增量事件')
  assert.ok(seen.includes('end'), '必须收到 end')
  assert.equal(seen[seen.length - 1], 'end', 'end 必须是最后一帧')
  assert.equal(closed, 'end', '收到 end 后必须停止（不得重连）')
})

check('§9 run_id 不存在：Mock 也必须给出 error + end，而不是静默挂起', async () => {
  const { openRunStream } = await server.ssrLoadModule('/src/api/stream.js')
  const seen = []
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('未结束')), 5000)
    openRunStream('run-not-exist', {
      tickMs: 5,
      onEvent: (ev) => seen.push(ev.event),
      onClose: () => {
        clearTimeout(timer)
        resolve()
      },
    })
  })
  assert.deepEqual(seen, ['error', 'end'])
})

let failed = 0
for (const { name, fn } of checks) {
  try {
    await fn()
    console.log(`  ✓ ${name}`)
  } catch (err) {
    failed += 1
    console.log(`  ✗ ${name}\n      ${err.message}`)
  }
}
await server.close()
console.log(`\n阶段 2 自检：${checks.length - failed}/${checks.length} 通过`)
process.exitCode = failed ? 1 : 0