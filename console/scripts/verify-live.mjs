/**
 * 真实联调自检：用**控制台自己的 API 层**打真实 admin（VITE_USE_MOCK=false）。
 *
 * 为什么不用浏览器：
 *   本机没有可用的浏览器/无头内核，所以这里不测"像素"，
 *   而是逐字执行控制台运行时真正走的那条路径：
 *     Vite SSR 加载 src/api/*.js（同一个 client.js 拦截器、同一个 unwrap、
 *     同一个 stream.js 解析器），对真实 admin 发请求。
 *   也就是说：**契约与数据通路是真的，视觉与交互没验**（会在报告里如实标注）。
 *
 * 用法（admin 与 MySQL 已在跑）：
 *   $env:CRONY_LIVE_USER='admin'; $env:CRONY_LIVE_PASS='...'
 *   node scripts/verify-live.mjs
 *
 * 退出码：0 = 全部通过；1 = 有失败项（逐项打印原因）。
 */
import { register } from 'node:module'
import { join } from 'node:path'
import './child-process-sandbox-shim.mjs'

// 与 scripts/smoke-mock.mjs、scripts/run-vite.mjs 完全一致的三步受限环境兼容：
//   1) child-process-sandbox-shim：把沙箱"同步抛 spawn EPERM"还原成标准异步 'error'
//   2) esbuild-shim-loader：把 esbuild 换成纯 JS 替身（Vite 的 define 替换会调用它）
//   3) 关掉依赖预打包（预打包依赖 esbuild 常驻子进程 + 管道）
// 没有这三步，本脚本会直接 `spawn EPERM` 崩掉（实测过）。
process.env.CRONY_NO_DEP_OPT = '1'
register('./esbuild-shim-loader.mjs', import.meta.url)

const { createServer } = await import('vite')

const BASE = process.env.VITE_API_BASE || 'http://127.0.0.1:8089'
const USER = process.env.CRONY_LIVE_USER || 'admin'
const PASS = process.env.CRONY_LIVE_PASS || ''
const RUN_A = process.env.CRONY_LIVE_RUN_A || ''
const RUN_B = process.env.CRONY_LIVE_RUN_B || ''
/** 只跑到这里：draft/confirm 会真花 token 并落一条任务，默认跑 */
const SKIP_MODEL_WRITE = process.env.CRONY_LIVE_SKIP_WRITE === '1'

const results = []
let failed = 0
function check(name, ok, detail = '') {
  results.push({ name, ok, detail })
  if (!ok) failed += 1
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  — ' + detail : ''}`)
}
function summary(v, n = 160) {
  const s = typeof v === 'string' ? v : JSON.stringify(v)
  return s.length > n ? s.slice(0, n) + '…' : s
}

// ---------------------------------------------------------------------------
// 浏览器环境垫片：控制台用 localStorage 存 token、用 fetch 发 SSE。
// Node 18+ 自带 fetch/ReadableStream；localStorage 需要自己补一个等价实现，
// 这样 client.js 的 getToken/setToken 与 axios 拦截器走的是**原代码**。
// ---------------------------------------------------------------------------
const store = new Map()
globalThis.localStorage = {
  getItem: (k) => (store.has(k) ? store.get(k) : null),
  setItem: (k, v) => store.set(k, String(v)),
  removeItem: (k) => store.delete(k),
  clear: () => store.clear(),
}

process.env.VITE_USE_MOCK = 'false'
process.env.VITE_API_BASE = BASE

const server = await createServer({
  // configFile:false：Vite 加载 vite.config.js 会 spawn esbuild 子进程（管道通信），
  // 本机沙箱禁止 → EPERM。这里只加载 .js 模块，不需要 vue 插件，别名自己给。
  configFile: false,
  root: process.cwd(),
  server: { middlewareMode: true },
  appType: 'custom',
  logLevel: 'warn',
  esbuild: false,
  optimizeDeps: { noDiscovery: true, include: [] },
  resolve: {
    alias: {
      '@': join(process.cwd(), 'src'),
      // client.js 只用到 ElMessage；用同名替身避免在无 DOM 的 Node 里加载整个组件库
      'element-plus': new URL('./stub-element-plus.mjs', import.meta.url).pathname,
    },
  },
  define: {
    // 显式钉住，避免依赖 Vite 的 env 合并规则（真值必须在下面被断言）
    'import.meta.env.VITE_USE_MOCK': JSON.stringify('false'),
    'import.meta.env.VITE_API_BASE': JSON.stringify(BASE),
  },
})

let mods
try {
  mods = {
    client: await server.ssrLoadModule('/src/api/client.js'),
    auth: await server.ssrLoadModule('/src/api/auth.js'),
    agent: await server.ssrLoadModule('/src/api/agent.js'),
    stream: await server.ssrLoadModule('/src/api/stream.js'),
    // 阶段 4.2：写动作审批台走的是 api/approval.js，与页面同一条链路。
    approval: await server.ssrLoadModule('/src/api/approval.js'),
    // 阶段 4-A：Agent 经验库走的是 api/memory.js。
    memory: await server.ssrLoadModule('/src/api/memory.js'),
    // 阶段 4-B：任务编排只读控制面走的是 api/job.js（与 JobsView/OrchestrationView 同一条链路）。
    job: await server.ssrLoadModule('/src/api/job.js'),
    // 阶段 5：回放评测台走的是 api/ops.js（与 EvalView 同一条链路）。
    ops: await server.ssrLoadModule('/src/api/ops.js'),
  }
} catch (err) {
  console.error('加载控制台 API 模块失败:', err?.message || err)
  await server.close()
  process.exit(1)
}

const { USE_MOCK, API_BASE, getToken } = mods.client
console.log(`API_BASE=${API_BASE}  USE_MOCK=${USE_MOCK}`)
check('VITE_USE_MOCK=false 已生效（否则整份报告都是 Mock 数据）', USE_MOCK === false, `USE_MOCK=${USE_MOCK}`)
check('API_BASE 指向真实 admin', API_BASE === BASE, `${API_BASE} vs ${BASE}`)

// ---------------------------------------------------------------------------
// 0) 登录：走**控制台自己的状态层** stores/auth.js → api/auth.js → api/client.js
//    这条链路包含 token 落 localStorage 与 axios 请求拦截器加 Authorization，
//    正是"Bearer 修复后第一次可能"的那条路径。
// ---------------------------------------------------------------------------
let token = ''
try {
  const { createPinia, setActivePinia } = await import('pinia')
  setActivePinia(createPinia())
  const authStoreMod = await server.ssrLoadModule('/src/stores/auth.js')
  const store = authStoreMod.useAuthStore()
  await store.login({ username: USER, password: PASS })
  token = store.token || ''
  check(
    'POST /login（控制台 stores/auth.js 的 login action）',
    !!token,
    token ? `token 长度 ${token.length}，用户 ${store.user?.username} is_admin=${store.user?.is_admin}` : '未拿到 token',
  )
  check('token 已写入 localStorage（stores/auth.js:42 setToken）', getToken() === token && !!token)
  check('登录后 isLoggedIn 为真', store.isLoggedIn === true)
} catch (err) {
  check('POST /login（控制台 stores/auth.js 的 login action）', false, err?.message || String(err))
}

// ---------------------------------------------------------------------------
// 1) 会话列表 / 2) 会话详情 / 3.1) 读诊断 / 3.2) 触发诊断 / 4) 工具清单
// ---------------------------------------------------------------------------
async function step(name, fn) {
  try {
    const value = await fn()
    return { ok: true, value }
  } catch (err) {
    check(name, false, err?.message || String(err))
    return { ok: false }
  }
}

const search = await step('POST /agent/session/search', () =>
  mods.agent.searchAgentSessions({ page: 1, page_size: 5 }),
)
if (search.ok) {
  const d = search.value || {}
  check(
    'POST /agent/session/search',
    Array.isArray(d.list) && typeof d.total === 'number',
    `total=${d.total} page=${d.page} page_size=${d.page_size} 首条 kind=${d.list?.[0]?.kind}`,
  )
  var firstSessionId = d.list?.[0]?.session_id || ''
}

const detail = await step('POST /agent/session/find', () =>
  mods.agent.findAgentSession(firstSessionId || 'diagnose:none'),
)
if (detail.ok) {
  const d = detail.value || {}
  check(
    'POST /agent/session/find（轨迹回放）',
    !!d.session_id && Array.isArray(d.steps),
    `session=${d.session_id} steps=${d.steps?.length} evidence=${d.evidence?.length} warnings=${d.warnings?.length}`,
  )
}

const tools = await step('POST /agent/tool/list', () => mods.agent.listAgentTools({}))
if (tools.ok) {
  const list = tools.value?.tools || []
  const names = list.map((t) => t.name)
  const policy = tools.value?.policy || {}
  check(
    'POST /agent/tool/list（能力边界来自后端注册表）',
    names.length > 0 && list.every((t) => typeof t.risk === 'string' && t.risk.length > 0),
    `${names.length} 个工具: ${names.join(', ')}；风险级别: ${list.map((t) => `${t.name}=${t.risk}`).join(', ')}；配额 max_calls_per_session=${policy.max_calls_per_session}`,
  )
  // 阶段 4：策略视图必须如实暴露写动作开关与自治级别（控制台要据此展示"这台部署放开了什么"）
  check(
    'POST /agent/tool/list（阶段 4 风险策略字段）',
    policy.allow_write === false &&
      policy.allow_destructive === false &&
      policy.autonomy === 'L1' &&
      policy.approval_ttl_seconds > 0 &&
      Array.isArray(policy.auto_approve),
    `allow_write=${policy.allow_write} allow_destructive=${policy.allow_destructive} autonomy=${policy.autonomy} approval_ttl=${policy.approval_ttl_seconds}s max_calls_per_minute=${policy.max_calls_per_minute} max_concurrent=${policy.max_concurrent_calls} auto_approve=${JSON.stringify(policy.auto_approve)}`,
  )
}

// 诊断：读（不加 force，命中库里已有记录 → 不烧 token）
for (const [label, runId] of [
  ['A', RUN_A],
  ['B', RUN_B],
]) {
  if (!runId) continue
  const found = await step(`POST /agent/diagnosis/find(${label})`, () =>
    mods.agent.findAgentDiagnosis(runId),
  )
  if (found.ok) {
    const d = found.value || {}
    check(
      `POST /agent/diagnosis/find(${label})`,
      d.found === true && !!d.diagnosis?.summary,
      `found=${d.found} session=${d.session_id} provider=${d.diagnosis?.provider} model=${d.diagnosis?.model} 置信度=${d.diagnosis?.confidence} 证据=${d.diagnosis?.evidence?.length} 未验证引用=${d.diagnosis?.unverified_refs?.length ?? 0}`,
    )
  }
  const replay = await step(`POST /agent/diagnosis/run(${label}, force=false)`, () =>
    mods.agent.runAgentDiagnosis(runId, { force: false }),
  )
  if (replay.ok) {
    const d = replay.value || {}
    check(
      `POST /agent/diagnosis/run(${label}, force=false) 幂等回放`,
      d.replayed === true && !d.persist_error,
      `replayed=${d.replayed} persist_error=${d.persist_error || '(无)'} 结论=${summary(d.diagnosis?.summary, 60)}`,
    )
  }
}

// ---------------------------------------------------------------------------
// 5) SSE：真流（不是 Mock）
//    两个场景：
//      a) 已终态的 run → snapshot/run + end(reason=final)
//      b) 不存在的 run_id → 服务端只等心跳，不报错（证明 unknown run 不 1010）
//         并且**跑满 35s**：这是对"20s 全局 WriteTimeout 会把 SSE 切断"的直接验证
// ---------------------------------------------------------------------------
async function sseOnce(runId, { maxSeconds = 30, closeAfterMs = 0, onEvent } = {}) {
  return await new Promise((resolve) => {
    const events = []
    const started = Date.now()
    const handle = mods.stream.openRunStream(runId, {
      intervalMs: 500,
      maxSeconds,
      onEvent: (ev) => {
        events.push(ev)
        onEvent?.(ev, Date.now() - started)
      },
      onError: (err) => resolve({ events, error: err?.message || String(err), ms: Date.now() - started }),
      onClose: (data, reason) => resolve({ events, reason, data, ms: Date.now() - started }),
    })
    // 客户端主动断开（用于"服务端还活着，但我们只观察 N 秒"的场景）
    if (closeAfterMs > 0) setTimeout(() => handle.close(), closeAfterMs)
    else setTimeout(() => handle.close(), (maxSeconds + 10) * 1000)
  })
}

if (RUN_A) {
  const a = await sseOnce(RUN_A, { maxSeconds: 30 })
  const kinds = a.events.map((e) => e.event)
  const end = a.events.find((e) => e.event === 'end')
  check(
    'SSE 终态运行：snapshot → end(reason=final)',
    kinds.includes('snapshot') && end?.data?.reason === 'final',
    `kinds=[${kinds.join(',')}] 用时 ${a.ms}ms end.status=${end?.data?.status} reason=${a.reason}`,
  )
}

{
  const unknown = `j0-live-verify-${Date.now()}`
  const hb = []
  // maxSeconds 给足（服务端不会自己 end），32s 后由客户端主动断开：
  // 若存在 20s 全局 WriteTimeout，这条连接会在 ~20s 被服务端切断并触发 stream.js 重连。
  const b = await sseOnce(unknown, {
    maxSeconds: 120,
    closeAfterMs: 32000,
    onEvent: (ev, ms) => {
      if (ev.event === 'heartbeat') hb.push(ms)
    },
  })
  const kinds = [...new Set(b.events.map((e) => e.event))]
  check(
    'SSE 不存在的 run_id：只发心跳、不报 1010（不会提前 end）',
    !b.events.some((e) => e.event === 'end') && hb.length > 0,
    `用时 ${b.ms}ms 唯一事件类型=[${kinds.join(',')}] 心跳 ${hb.length} 次 @[${hb.map((x) => x + 'ms').join(', ')}]`,
  )
  check(
    'SSE 长连接 >20s 不被切断（原 20s WriteTimeout 缺陷已修复）',
    b.ms >= 30000 && !b.error,
    `连接存活 ${b.ms}ms 后由客户端主动断开${b.error ? '（异常：' + b.error + '）' : ''}`,
  )
}

// ---------------------------------------------------------------------------
// 5.1) 自然语言建任务：草案（真跑一次模型）→ 确认创建（真落一条任务）
// ---------------------------------------------------------------------------
if (!SKIP_MODEL_WRITE) {
  const draftRes = await step('POST /agent/nljob/draft', () =>
    mods.agent.draftNlJob({ text: '每天凌晨 3 点清理 /tmp 下 7 天前的文件' }),
  )
  if (draftRes.ok) {
    const d = draftRes.value || {}
    // 这里同时验证"控制台读得到草案字段"——真实后端 data 是 { draft: {...} } 多一层
    check(
      'POST /agent/nljob/draft（控制台 normalizeDraft 抹平 data.draft）',
      !!d.spec && !!d.name && Array.isArray(d.next_triggers) && d.next_triggers.length > 0,
      `name=${summary(d.name, 24)} spec=${d.spec} argv=${summary(d.command)} 下次触发=${summary(d.next_trigger_text || d.next_triggers, 60)} warnings=${d.warnings?.length}`,
    )
    if (d.session_id) {
      const confirmed = await step('POST /agent/nljob/confirm', () =>
        mods.agent.confirmNlJob({
          session_id: d.session_id,
          confirmed: true,
          overrides: { name: d.name, spec: d.spec, timeout_seconds: d.timeout_seconds ?? null, retry_times: d.retry_times ?? null },
        }),
      )
      if (confirmed.ok) {
        const c = confirmed.value || {}
        check(
          'POST /agent/nljob/confirm（确认创建，仅落任务定义不派发）',
          !!c.job_id && typeof c.spec === 'string',
          `job_id=${c.job_id} spec=${c.spec} status=${c.status}（0=未分配） created=${c.created} requires_assign=${c.requires_assign} 警告=${summary(c.warnings, 80)}`,
        )
      }
    }
  }
} else {
  console.log('SKIP  nljob/draft + confirm（CRONY_LIVE_SKIP_WRITE=1）')
}

// ---------------------------------------------------------------------------
// 阶段 4.2) 写动作审批台：走控制台自己的 api/approval.js（真链路，不是直连 curl）
//
// 这一节**不制造任何审批单**：造单要用写工具，而本机部署是只读策略
// （allow_write=false），写能力面里根本没有 job_retry。
// 所以这里验的是"接口存在、契约正确、封死的那一侧真的封死"：
//   - search 返回分页结构，且当前策略下不会有任何写动作单子；
//   - 不存在的单号 → 1013（错误码必须与前端 dict 一致，否则提示走错分支）；
//   - 审批接口对未知单号一律 1013，说明服务装配正常（而不是 1007/1011）；
//   - 审计接口能返回真实流水，且决策枚举都在已知集合内。
// ---------------------------------------------------------------------------
{
  const SAFE_DECISIONS = new Set(['allowed', 'denied', 'approval_required', 'approval_executed'])

  const apSearch = await step('POST /agent/approval/search', () =>
    mods.approval.searchApprovals({ page: 1, page_size: 20 }),
  )
  if (apSearch.ok) {
    const r = apSearch.value || {}
    check(
      'POST /agent/approval/search 返回分页结构且无写动作单子（只读部署的应有形态）',
      Array.isArray(r.list) && typeof r.total === 'number' && r.total === 0,
      `total=${r.total} page=${r.page} size=${r.page_size}`,
    )
  }

  for (const [name, fn] of [
    ['POST /agent/approval/find', () => mods.approval.findApproval('ap-live-verify-none')],
    ['POST /agent/approval/approve', () => mods.approval.approveApproval('ap-live-verify-none', '联调自检')],
    ['POST /agent/approval/reject', () => mods.approval.rejectApproval('ap-live-verify-none', '联调自检')],
    ['POST /agent/approval/cancel', () => mods.approval.cancelApproval('ap-live-verify-none', '联调自检')],
  ]) {
    // 这里**故意不用 step()**：step() 会把抛错直接记成一条 FAIL，
    // 而本节要验的正是"被拒绝"——失败是期望结果，必须自己接住并断言错误码。
    let code = null
    let msg = ''
    let unexpected = ''
    try {
      const v = await fn()
      unexpected = summary(v)
    } catch (err) {
      code = err?.code
      msg = err?.message || String(err)
    }
    check(
      `${name} 对不存在的单号返回 1013（服务已装配、错误码可区分）`,
      !unexpected && code === 1013,
      unexpected ? `意外成功：${unexpected}` : `code=${code} msg=${summary(msg, 80)}`,
    )
  }

  const auditSearch = await step('POST /agent/audit/search', () =>
    mods.approval.searchToolAudit({ page: 1, page_size: 20 }),
  )
  if (auditSearch.ok) {
    const r = auditSearch.value || {}
    const bad = (r.list || []).filter((x) => !SAFE_DECISIONS.has(x.decision) || !x.decision_text || !x.risk)
    check(
      'POST /agent/audit/search 返回工具网关流水（决策枚举与中文解释齐全）',
      Array.isArray(r.list) && typeof r.total === 'number' && bad.length === 0,
      `total=${r.total} 本页=${r.list?.length} 决策=[${[...new Set((r.list || []).map((x) => x.decision))].join(',')}]`,
    )
  }
}

// ---------------------------------------------------------------------------
// 阶段 4-A) Agent 经验库（记忆）：走控制台自己的 api/memory.js（真链路）
//
// 这一节**只读 + 只碰自己造的夹具**：
//   - search/find/stats 是只读接口，直接验契约与口径；
//   - recall 用一段"故意不像任何真实症状"的文本，验的是"空结果是正确答案"
//     以及 advisory_only/usable_as_evidence 这两条硬约束；
//   - **停用/启用不在这里做**：那是写治理动作，需要真实条目且要写原因，
//     由 Go 侧验收用例 AC-27 在真实库上做（本节只验前端能拿到的契约）。
// 另外必须验一条"经验不是证据"的运行时痕迹：只要历史上有过一次注入，
// 那条会话必须记为 diagnose-v5，且带"历史经验不是证据"的警示。
// ---------------------------------------------------------------------------
{
  const memSearch = await step('POST /agent/memory/search', () =>
    mods.memory.searchMemories({ page: 1, page_size: 10 }),
  )
  if (memSearch.ok) {
    const r = memSearch.value || {}
    const src = r.source || {}
    // 结果取值必须都在已知集合内：前端按 outcome 上色，出现未知值会渲染成灰色"未知"。
    const known = new Set(['success', 'failed', 'blocked', 'denied', 'advised'])
    const bad = (r.list || []).filter(
      (m) =>
        !m.memory_id ||
        !m.outcome_text ||
        !known.has(m.outcome) ||
        m.occurrence_count !==
          m.success_count + m.fail_count + m.blocked_count + m.denied_count + m.advised_count,
    )
    check(
      'POST /agent/memory/search 返回经验列表（结果可解释、计数自洽）',
      Array.isArray(r.list) && typeof r.total === 'number' && bad.length === 0,
      `total=${r.total} 本页=${r.list?.length} 结果=[${[...new Set((r.list || []).map((m) => m.outcome))].join(',')}] 计数不自洽=${bad.length}`,
    )
    check(
      'POST /agent/memory/search 自述治理约束（仅线索/不可编辑/停用需原因）',
      src.advisory_only === true && src.usable_as_evidence === false && src.editable === false && src.disable_requires_reason === true,
      `advisory_only=${src.advisory_only} usable_as_evidence=${src.usable_as_evidence} editable=${src.editable} disable_requires_reason=${src.disable_requires_reason}`,
    )
  }

  const memStats = await step('POST /agent/memory/stats', () => mods.memory.memoryStats())
  if (memStats.ok) {
    const d = memStats.value || {}
    const bySum = Object.values(d.by_outcome || {}).reduce((a, b) => a + Number(b), 0)
    check(
      'POST /agent/memory/stats 口径自洽（分组和=总数，成功率分母只含执行过的动作）',
      d.enabled + d.disabled === d.total &&
        bySum === d.total &&
        d.definition?.success_rate &&
        Array.isArray(d.definition?.excluded) &&
        d.definition.excluded.length >= 3,
      `total=${d.total} enabled=${d.enabled} disabled=${d.disabled} 分组和=${bySum} 成功=${d.success_sum} 失败=${d.fail_sum} known=${d.success_rate_known} 口径=${summary(d.definition?.success_rate, 60)}`,
    )
  }

  const memRecall = await step('POST /agent/memory/recall', () =>
    mods.memory.recallMemories({
      symptom: '联调自检：一段故意不像任何真实症状的文本（应召回到空）',
      agent_kind: 'diagnose',
      top_k: 3,
    }),
  )
  if (memRecall.ok) {
    const r = memRecall.value || {}
    check(
      'POST /agent/memory/recall 空结果是正确答案，且结果自述"不作证据"',
      Array.isArray(r.hits) &&
        r.hits.length === 0 &&
        r.advisory_only === true &&
        r.usable_as_evidence === false &&
        typeof r.notice === 'string' &&
        r.notice.length > 0 &&
        Array.isArray(r.query_tokens) &&
        r.query_tokens.length > 0,
      `hits=${r.hits?.length} advisory_only=${r.advisory_only} usable_as_evidence=${r.usable_as_evidence} 比较=${r.considered} 词元=${r.query_tokens?.length} notice=${summary(r.notice, 40)}`,
    )
  }

  // 单条详情：拿列表里第一条（没有则为空库，跳过一次断言而不是伪造）。
  const memList = memSearch.ok ? memSearch.value?.list || [] : []
  if (memList.length) {
    const memFind = await step('POST /agent/memory/find', () => mods.memory.findMemory(memList[0].memory_id))
    if (memFind.ok) {
      const m = memFind.value?.memory || {}
      check(
        'POST /agent/memory/find 返回单条详情（含分词与结果原文，可复盘"为什么算相似"）',
        m.memory_id === memList[0].memory_id && typeof m.symptom === 'string' && 'symptom_tokens' in m,
        `memory=${m.memory_id} 症状=${summary(m.symptom, 40)} 计数=${m.occurrence_count}`,
      )
    }
  }

  // "经验不是证据"的运行时痕迹：历史上有过注入的会话必须记为 diagnose-v5 且带警示。
  const injectCheck = await step('POST /agent/session/find（抽查会话找注入痕迹）', async () => {
    const list = (await mods.agent.searchAgentSessions({ page: 1, page_size: 50 }))?.list || []
    for (const s of list) {
      if (s.kind !== 'diagnose') continue
      const d = await mods.agent.findAgentSession(s.session_id)
      // 注意：会话详情把结构化结论放在 result 里（/agent/diagnosis/find 才叫 diagnosis）。
      const diag = d?.result || {}
      if (Array.isArray(diag.memory_used) && diag.memory_used.length > 0) return { session: s.session_id, diag }
    }
    return null
  })
  if (injectCheck.ok) {
    const found = injectCheck.value
    if (!found) {
      console.log('  - 未找到"注入过历史经验"的会话（模型是否召回取决于症状相似度）：本条跳过，已由 Go 验收用例 AC-28 覆盖')
    } else {
      const { session, diag } = found
      check(
        '注入过历史经验的会话：记为 diagnose-v5 且带"经验不是证据"的警示',
        diag.prompt_version === 'diagnose-v5' &&
          (diag.memory_note || '').includes('不计入证据') &&
          (diag.warnings || []).some((w) => w.includes('历史经验')),
        `session=${session} prompt_version=${diag.prompt_version} memory_used=${diag.memory_used?.length} note=${summary(diag.memory_note, 40)}`,
      )
    }
  }
}

await server.close()

// ---------------------------------------------------------------------------
// 阶段 4-B) 任务编排（DAG）只读控制面：api/job.js → /job/dag/validate|find
//
// 这一节的意义不在于"接口能通"，而在于**坏图真的会被拒**：
// 校验通过 = 允许保存并下发到节点执行；校验失败 = 明确告诉运维哪一步错了。
// 因此这里既校验一份合法菱形，也必须校验一份成环的图（期望业务码 1021）。
// ---------------------------------------------------------------------------
{
  const diamond = JSON.stringify({
    max_parallel: 2,
    steps: [
      { index: 0, name: 'verify-extract', command: 'echo extract' },
      { index: 1, name: 'verify-transform', command: 'echo transform', depends_on: '0' },
      { index: 2, name: 'verify-enrich', command: 'echo enrich', depends_on: '0' },
      { index: 3, name: 'verify-load', command: 'echo load', depends_on: '1,2', join: 'all_success' },
    ],
  })

  const okDraft = await step('POST /job/dag/validate（合法菱形）', () =>
    mods.job.validateJobDag({ steps_json: diamond }),
  )
  if (okDraft.ok) {
    const p = okDraft.value?.plan
    check(
      'POST /job/dag/validate 合法图：给出步骤 / 依赖边 / 层深',
      okDraft.value?.valid === true &&
        p?.steps?.length === 4 &&
        p?.edges?.length === 4 &&
        p?.max_depth === 2 &&
        Array.isArray(p.steps.find((s) => s.index === 3)?.depends_on),
      `steps=${p?.steps?.length} edges=${p?.edges?.length} max_depth=${p?.max_depth} load.depends_on=${JSON.stringify(p?.steps?.find((s) => s.index === 3)?.depends_on)}`,
    )
  }

  const cycleDraft = JSON.stringify({
    steps: [
      { index: 0, name: 'verify-a', command: 'echo a', depends_on: '2' },
      { index: 1, name: 'verify-b', command: 'echo b', depends_on: '0' },
      { index: 2, name: 'verify-c', command: 'echo c', depends_on: '1' },
    ],
  })
  const bad = await step('POST /job/dag/validate（成环）', async () => {
    try {
      await mods.job.validateJobDag({ steps_json: cycleDraft })
      return { rejected: false }
    } catch (e) {
      return { rejected: true, code: e?.code, msg: e?.message || String(e) }
    }
  })
  if (bad.ok) {
    check(
      'POST /job/dag/validate 坏图被拒：code=1021 且给出完整环路',
      bad.value?.rejected === true && bad.value?.code === 1021 && /环/.test(bad.value?.msg || ''),
      `code=${bad.value?.code} msg=${summary(bad.value?.msg, 90)}`,
    )
  }

  // 库里的任务：有编排的必须能读出摘要（含"是否可执行"），没有编排的必须是 1022
  const jobs = await step('POST /job/search（编排视图的任务下拉）', () =>
    mods.job.searchJobs({ page: 1, page_size: 100 }),
  )
  if (jobs.ok) {
    const list = jobs.value?.list || []
    const withPlan = list.filter((j) => String(j.steps_json || '').trim())
    const without = list.find((j) => !String(j.steps_json || '').trim())
    check(
      'POST /job/search 返回 steps_json（前端据此区分"有编排/单步"）',
      list.length > 0 && list.every((j) => 'steps_json' in j),
      `任务数=${list.length} 其中有编排=${withPlan.length}`,
    )

    if (withPlan.length === 0) {
      console.log('  - 库里暂无带 steps_json 的任务：/job/dag/find 的"有编排"分支留给 Go 验收用例 AC-32 覆盖')
    } else {
      const found = await step('POST /job/dag/find（有编排的任务）', () => mods.job.findJobDag(withPlan[0].id))
      if (found.ok) {
        const d = found.value || {}
        check(
          'POST /job/dag/find 有编排：steps/edges/层深与可执行标记齐全',
          d.executable === true && d.plan?.steps?.length > 0 && Array.isArray(d.plan?.edges),
          `job=${d.job_id} steps=${d.plan?.steps?.length} edges=${d.plan?.edges?.length} max_depth=${d.plan?.max_depth}`,
        )
      }
    }

    if (without) {
      const none = await step('POST /job/dag/find（单步任务）', async () => {
        try {
          await mods.job.findJobDag(without.id)
          return { code: 200 }
        } catch (e) {
          return { code: e?.code, msg: e?.message || String(e) }
        }
      })
      if (none.ok) {
        check(
          'POST /job/dag/find 单步任务：返回 1022（"本来就没有编排"≠"编排是坏的"）',
          none.value?.code === 1022,
          `job=${without.id} code=${none.value?.code} msg=${summary(none.value?.msg, 60)}`,
        )
      }
    }
  }
}

// ---------------------------------------------------------------------------
// 阶段 5) 回放评测台：走控制台自己的 api/ops.js（与 EvalView 同一条链路）。
//
// 这一节是**只读 + 一次幂等同步 + 一次跑批**：
//   - cases/runs/report 都是只读，直接验契约与口径；
//   - seed/sync 按 case_key upsert 是幂等的：连做两次，第二次必须"无新增、无下线"，
//     且覆盖全部种子样本（这条断言与库内此前有没有种子无关，新库上也成立）；
//   - run 是**回放已录轨迹**：不调用模型，所以在没有模型凭据的机器上也能真跑。
//     它会往 eval_run 落一行（trigger=console-verify 标明来源），这是可回溯的记录而非状态改动。
//
// 要钉住的口径（这三条错了评测台就会变成"看起来在评测"）：
//   1) 计数自洽：passed + failed + skipped === cases；
//   2) 评分分母**不含 skipped**：score === passed / (passed + failed)；
//   3) 列表（/runs）只给汇总行，逐样本明细只在 /report —— 列表带明细会让响应体爆掉。
// ---------------------------------------------------------------------------
{
  const cases = await step('POST /ops/eval/cases（样本枚举）', () => mods.ops.listEvalCases(''))
  if (cases.ok) {
    const d = cases.value || {}
    const list = Array.isArray(d.list) ? d.list : []
    // 期望值必须是合法 JSON 且 id 与 case_key 一致：改键名会让断言静默失效（历史踩过）。
    const badExpect = list.filter((c) => {
      try {
        return JSON.parse(c.expect_json || '').id !== c.case_key
      } catch (e) {
        return true
      }
    })
    check(
      'POST /ops/eval/cases 样本可枚举、期望值可解析且与 case_key 一致',
      list.length > 0 && typeof d.total === 'number' && badExpect.length === 0,
      `total=${d.total} enabled=${d.enabled} seed=${d.seed_version} 期望值有问题的=${badExpect.length}`,
    )
  }

  const sync = await step('POST /ops/eval/seed/sync（幂等同步）', () => mods.ops.syncEvalSeed())
  if (sync.ok && cases.ok) {
    const second = await mods.ops.syncEvalSeed() // 幂等的判据在第二次：不能有新增/下线
    const total = cases.value?.total || 0
    check(
      'POST /ops/eval/seed/sync 幂等：二次同步无新增、无下线，且覆盖全部种子样本',
      second.added === 0 && second.removed === 0 && second.updated === total,
      `第一次 added=${sync.value?.added} updated=${sync.value?.updated} removed=${sync.value?.removed}；` +
        `第二次 added=${second.added} updated=${second.updated} removed=${second.removed}；库内样本=${total}`,
    )
  }

  const run = await step('POST /ops/eval/run（回放一次，不调模型）', () =>
    mods.ops.runEvalBench({ trigger: 'console-verify' }),
  )
  let newRunId = 0
  if (run.ok) {
    const rep = run.value?.report || {}
    newRunId = rep.run_id || 0
    const denom = (rep.passed || 0) + (rep.failed || 0)
    const scoreOK = denom > 0 ? Math.abs(rep.score - rep.passed / denom) < 1e-9 : rep.score === 0
    check(
      'POST /ops/eval/run 是回放（mode=replay）且报告计数自洽、评分分母不含 skipped',
      rep.mode === 'replay' &&
        (rep.results || []).length === rep.cases &&
        rep.passed + rep.failed + rep.skipped === rep.cases &&
        scoreOK,
      `run_id=${rep.run_id} mode=${rep.mode} cases=${rep.cases} ` +
        `${rep.passed}/${rep.failed}/${rep.skipped} score=${rep.score} 判定次数=${rep.sessions}`,
    )
    const skippedNoReason = (rep.results || []).filter((r) => r.status === 'skipped' && !r.reason)
    check(
      '回放报告：跳过的样本必须写明原因（跳过 ≠ 通过）',
      skippedNoReason.length === 0,
      `未写原因的跳过=${skippedNoReason.length}`,
    )
  }

  const report = await step('POST /ops/eval/report（按 run_id 回溯）', () =>
    mods.ops.getEvalReport(newRunId),
  )
  if (report.ok) {
    check(
      'POST /ops/eval/report 可按 run_id 取回同一次报告（可回溯）',
      report.value?.report?.run_id === newRunId && !!report.value?.run && !!report.value?.report?.mode,
      `请求 run_id=${newRunId} 返回 run_id=${report.value?.report?.run_id} mode=${report.value?.report?.mode}`,
    )
  }

  const runs = await step('POST /ops/eval/runs（历次跑批）', () => mods.ops.listEvalRuns(5))
  if (runs.ok) {
    const list = Array.isArray(runs.value?.list) ? runs.value.list : []
    const ordered = list.every((r, i) => i === 0 || list[i - 1].run_id > r.run_id)
    const selfConsistent = list.every((r) => r.passed + r.failed + r.skipped === r.cases)
    const noReportJSON = list.every((r) => !('report_json' in r))
    const containsNew = list.some((r) => r.run_id === newRunId)
    check(
      'POST /ops/eval/runs 返回汇总行：id 倒序、计数自洽、不带逐样本明细，且含刚跑的那次',
      list.length > 0 && ordered && selfConsistent && noReportJSON && containsNew,
      `条数=${list.length} 倒序=${ordered} 计数自洽=${selfConsistent} 无 report_json=${noReportJSON} 含新跑批=${containsNew}`,
    )
  }

  const empty = await step('POST /ops/eval/report（不存在的 run_id）', () =>
    mods.ops.getEvalReport(99999999),
  )
  if (empty.ok) {
    check(
      'POST /ops/eval/report 对不存在的 run_id 返回空报告 + 说明，而不是报错或假数据',
      empty.value?.report === null && !!empty.value?.note,
      `report=${empty.value?.report} note=${summary(empty.value?.note, 60)}`,
    )
  }
}

console.log('\n===== 真实联调结果 =====')
console.log(`通过 ${results.length - failed}/${results.length}，失败 ${failed}`)
for (const r of results.filter((x) => !x.ok)) console.log(`  FAIL ${r.name} — ${r.detail}`)
process.exit(failed === 0 ? 0 : 1)