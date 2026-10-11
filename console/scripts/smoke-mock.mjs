/**
 * Mock 数据层冒烟测试：`pnpm smoke`
 *
 * 目的：证明"前端在没有后端的情况下能独立运行"不是一句声明，而是可重复执行的检查。
 *
 * 做法：用 Vite 自己的 SSR 模块加载器（createServer + ssrLoadModule）在 Node 里
 * **真实执行** src/api/mock 与 src/api/*.js 的调用链——这条链路与浏览器里跑的是
 * 同一份代码、同一套 '@' 别名解析、同一套 define 替换，只是渲染层换成了 Node。
 *
 * 覆盖：
 *   - runOverviewQuery 的字段与口径（对齐后端 service.RunOverview / SummarizeRuns）
 *   - job/run/node/script/knowledge/audit/statis/settings 各 Mock 返回结构
 *   - login 的业务错误分支（code != 200 也走 HTTP 200 信封）
 *
 * 退出码 0 表示全部通过；任何断言失败都会抛错并以非 0 退出。
 */
import { register } from 'node:module'
import { join } from 'node:path'
import assert from 'node:assert/strict'
import './child-process-sandbox-shim.mjs'

// 与 run-vite.mjs 一致：受限环境下关闭依赖预打包，并顶替 esbuild（define 替换用）
process.env.CRONY_NO_DEP_OPT = '1'
register('./esbuild-shim-loader.mjs', import.meta.url)

const { createServer } = await import('vite')

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  logLevel: 'error',
  resolve: { alias: { '@': join(process.cwd(), 'src') } },
  server: { middlewareMode: true },
  appType: 'custom',
  esbuild: false,
  optimizeDeps: { noDiscovery: true, include: [] },
})

const checks = []
function check(name, fn) {
  checks.push({ name, fn })
}

const mock = await server.ssrLoadModule('/src/api/mock/index.js')

check('runOverview 字段与后端 RunOverview 对齐', () => {
  const d = mock.runOverviewQuery({}).data
  const keys = ['total', 'success', 'failed', 'timeout', 'canceled', 'skipped', 'running', 'pending', 'retry_total', 'success_rate', 'skip_rate', 'by_status', 'since']
  for (const k of keys) assert.ok(k in d, `缺少字段 ${k}`)
  assert.equal(d.total, Object.values(d.by_status).reduce((a, b) => a + b, 0), 'by_status 之和应等于 total')
  const executed = d.total - d.skipped
  assert.equal(d.success_rate, executed > 0 ? Math.round((d.success / executed) * 10000) / 10000 : 0, 'success_rate 分母应是 total - skipped')
  assert.equal(d.skip_rate, d.total > 0 ? Math.round((d.skipped / d.total) * 10000) / 10000 : 0, 'skip_rate 分母应是 total')
})

check('job/run 检索返回 PageResult 结构', async () => {
  const d = (await mock.runSearch({ page: 1, page_size: 3 })).data
  assert.deepEqual(Object.keys(d).sort(), ['list', 'page', 'page_size', 'total'])
  assert.equal(d.list.length, 3)
  assert.ok(d.total > 3)
  assert.ok(d.list[0].run_id && d.list[0].policy_snapshot, 'run 应带幂等键与策略快照')
})

check('分页契约与后端 PageInfo.Check 一致（page<=0 → 1，page_size<=0 → 20）', async () => {
  const d = (await mock.runSearch({ page: 0, page_size: 0 })).data
  assert.equal(d.page, 1)
  assert.equal(d.page_size, 20)
})

check('run/overview 的步骤时间线可查', async () => {
  const list = (await mock.runSearch({ page: 1, page_size: 20 })).data.list
  const steps = (await mock.runStepsQuery({ run_id: list[0].run_id })).data
  assert.ok(steps.total >= 1, '首个 run 应至少有一个步骤')
  assert.ok(Array.isArray(steps.list))
})

check('statis today/week/system 与 health 结构可用', async () => {
  const today = (await mock.statisTodayQuery()).data
  const week = (await mock.statisWeekQuery()).data
  const health = (await mock.statisHealth()).data
  assert.ok(today && week, 'today/week 统计不应为空')
  assert.ok(Array.isArray(health.components) && health.components.length > 0, 'health.components 应为非空数组')
  assert.ok(health.components.every((c) => c.name && c.status), '每个健康组件应有 name/status')
})

check('节点/任务/脚本/知识库/审计 Mock 均返回分页数据', async () => {
  for (const [name, res] of [
    ['node', await mock.nodeSearch({ page: 1, page_size: 5 })],
    ['job', await mock.jobSearch({ page: 1, page_size: 5 })],
    ['script', await mock.scriptSearch({ page: 1, page_size: 5 })],
    ['knowledge', await mock.knowledgeSearch({ page: 1, page_size: 5 })],
    ['audit', await mock.auditSearch({ page: 1, page_size: 5 })],
  ]) {
    assert.equal(res.code, 200, `${name} 应返回 code 200`)
    assert.ok(res.data.total > 0, `${name} 应有数据`)
  }
})

check('Agent 对话/下发返回可读文案', async () => {
  const chat = (await mock.agentChat({ agent_id: 'agent-ops-01', message: '检查磁盘水位' })).data
  assert.ok(chat && chat.reply && chat.reply.length > 0, 'agentChat 应返回 reply')
  const dispatch = (await mock.agentDispatch({ agent_id: 'agent-ops-01', goal: '清理日志' })).data
  assert.ok(dispatch, 'agentDispatch 应返回任务信息')
})

check('鉴权：口令长度不足走业务错误信封，正常口令返回 token', async () => {
  const bad = mock.login({ username: 'admin', password: 'short' })
  assert.equal(bad.code, 1000, '业务错误码应为 1000（HTTP 仍为 200）')
  const ok = mock.login({ username: 'admin', password: 'crony@2024' })
  assert.equal(ok.code, 200)
  assert.ok(ok.data.token && ok.data.is_admin === true)
})

check('阶段 4.2 审批 Mock：六种状态齐全，且与后端字段同形', async () => {
  const all = (await mock.approvalSearch({ page: 1, page_size: 50 })).data
  assert.equal(all.total, 6, '样例应覆盖 6 张单子')
  const statuses = new Set(all.list.map((r) => r.status))
  for (const s of ['pending', 'executed', 'execution_failed', 'blocked', 'rejected', 'expired']) {
    assert.ok(statuses.has(s), `缺少状态样例：${s}`)
  }
  // 时间字段类型必须与后端一致：created/expires_at 是 Unix 秒，decided_at/executed_at 是 RFC3339。
  const executed = all.list.find((r) => r.status === 'executed')
  assert.equal(typeof executed.created, 'number')
  assert.equal(typeof executed.expires_at, 'number')
  assert.ok(!Number.isNaN(Date.parse(executed.decided_at)), 'decided_at 应是可解析的时间字符串')
  assert.ok(!Number.isNaN(Date.parse(executed.executed_at)))
  // blocked 的核心事实：动作从未发生（没有执行时间戳）。
  const blocked = all.list.find((r) => r.status === 'blocked')
  assert.equal(blocked.executed_at, '', 'blocked 不得有执行时间戳')
  assert.equal(blocked.executed, false)
  assert.ok(blocked.status_text.includes('未执行'), 'blocked 的文案必须自述"未执行"')
  // pending 的剩余有效期必须是"剩下的秒数"，不能是负数或天文数字。
  const pending = all.list.find((r) => r.status === 'pending')
  assert.ok(pending.remain_seconds > 0 && pending.remain_seconds <= 15 * 60, `剩余有效期异常：${pending.remain_seconds}`)
})

check('阶段 4.2 审批 Mock：状态过滤与"已处理不能再决策"的错误码', async () => {
  const pendingOnly = (await mock.approvalSearch({ page: 1, page_size: 50, status: 'pending' })).data
  assert.ok(pendingOnly.list.every((r) => r.status === 'pending'), 'status 过滤必须生效')

  const pending = pendingOnly.list[0]
  const first = mock.approvalDecide({ approval_id: pending.approval_id, action: 'approve', note: '同意' })
  assert.equal(first.code, 200)
  assert.equal(first.data.executed, true, '批准应标记为已执行')
  assert.equal(first.data.approval.status, 'executed')

  // 第二次决策必须被拒（复刻后端 CAS：只有 pending 能被决策）。
  const second = mock.approvalDecide({ approval_id: pending.approval_id, action: 'approve' })
  assert.equal(second.code, 1014, '已处理的单子再决策应返回 1014')
  // 单号不存在 → 1013；这与真实后端的错误码必须一致，否则前端提示会走错分支。
  assert.equal(mock.approvalDecide({ approval_id: 'ap-none' }).code, 1013)
  assert.equal(mock.approvalFind({ approval_id: 'ap-none' }).code, 1013)
})

check('阶段 4.2 工具审计 Mock：四种决策齐全且都有中文解释', async () => {
  const all = (await mock.toolAuditSearch({ page: 1, page_size: 50 })).data
  const decisions = new Set(all.list.map((r) => r.decision))
  for (const d of ['allowed', 'denied', 'approval_required', 'approval_executed']) {
    assert.ok(decisions.has(d), `缺少决策样例：${d}`)
  }
  assert.ok(
    all.list.every((r) => r.decision_text && r.risk && r.created_ms > 0),
    '每条审计都要有中文决策文案 / 风险级别 / 毫秒时间戳',
  )
  const filtered = (await mock.toolAuditSearch({ page: 1, page_size: 50, decision: 'approval_required' })).data
  assert.ok(filtered.list.length > 0 && filtered.list.every((r) => r.decision === 'approval_required'))
})

let failed = 0
for (const { name, fn } of checks) {
  try {
    await fn()
    console.log(`  ✓ ${name}`)
  } catch (err) {
    failed++
    console.log(`  ✗ ${name}\n      ${err.message}`)
  }
}

await server.close()

console.log(`\nMock 冒烟：${checks.length - failed}/${checks.length} 通过`)
process.exitCode = failed ? 1 : 0