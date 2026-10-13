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

check('阶段 4-A 经验库 Mock：五种结果齐全，且 outcome 只代表"最近一次"', async () => {
  const all = (await mock.memorySearch({ page: 1, page_size: 50 })).data
  assert.ok(all.total >= 5, `样例应覆盖五种结果，实际 ${all.total} 条`)
  const outcomes = new Set(all.list.map((m) => m.outcome))
  for (const o of ['success', 'failed', 'blocked', 'denied', 'advised']) {
    assert.ok(outcomes.has(o), `缺少结果样例：${o}`)
  }
  // 列表必须把结果解释拿到后端（前端只渲染不推断）。
  assert.ok(all.list.every((m) => m.outcome_text && m.memory_id.startsWith('mem-')), '每条经验都要有中文结果解释与条目号')
  // 计数自洽：出现次数 = 各结果计数之和（否则"最近一次"与"历史全貌"会互相矛盾）。
  for (const m of all.list) {
    const sum = m.success_count + m.fail_count + m.blocked_count + m.denied_count + m.advised_count
    assert.equal(m.occurrence_count, sum, `${m.memory_id} 的 occurrence_count 应等于各计数之和`)
  }
  // 关键：没有执行样本的条目不能冒充 0%（success_rate_known=false → 界面显示"无数据"）。
  const blocked = all.list.find((m) => m.outcome === 'blocked')
  assert.equal(blocked.success_rate_known, false, '没有执行样本时 success_rate_known 必须为 false')
  assert.ok(blocked.outcome_text.includes('未执行'), 'blocked 的文案必须自述"未执行"')
  // 三条机器可读约束必须由接口返回（不是写在文档里）。
  assert.equal(all.source.advisory_only, true)
  assert.equal(all.source.usable_as_evidence, false)
  assert.equal(all.source.editable, false, '经验库不提供编辑（只允许停用）')
  assert.equal(all.source.disable_requires_reason, true)
  // 时间字段类型与后端一致：disabled_at 是 RFC3339 字符串。
  const disabled = all.list.find((m) => m.enabled === false)
  assert.ok(disabled, '应有一条已停用的样例')
  assert.ok(!Number.isNaN(Date.parse(disabled.disabled_at)), 'disabled_at 应是可解析的时间字符串')
  assert.ok(disabled.disabled_reason, '停用必须带原因')
})

check('阶段 4-A 经验库 Mock：召回可解释、空结果是正确答案', async () => {
  const symptom = '诊断任务 18（备份）本次失败的原因 mysqldump 鉴权失败 1045 账号密码过期'
  const hit = mock.memoryRecall({ symptom, agent_kind: 'diagnose', target_node: 'node-it-01', top_k: 5 }).data
  assert.equal(hit.advisory_only, true)
  assert.equal(hit.usable_as_evidence, false, '召回结果不能被当成证据')
  assert.ok(hit.notice, '必须带"仅线索"的说明文案')
  assert.ok(hit.query_tokens.length > 0, '必须回报查询词元（否则"为什么没召回"无法排查）')
  assert.ok(hit.hits.length > 0, '相似症状应命中')
  const top = hit.hits[0]
  assert.ok(top.memory_id.startsWith('mem-'))
  assert.ok(top.score >= hit.min_score, '命中必须达到最低相似度')
  assert.ok(top.matched.length > 0 && Array.isArray(top.reasons), '命中必须可解释（matched / reasons）')
  assert.equal(typeof top.is_failed, 'boolean')
  // 该条（1045 鉴权失败）只有失败样本 → 有成功率；"没执行过"的那些必须显示"无数据"。
  const authHit = hit.hits.find((h) => h.symptom.includes('1045'))
  assert.ok(authHit, '1045 鉴权失败的记忆应被同一症状召回')
  assert.equal(authHit.success_rate_known, true, '该条有 4 次失败样本，应给出成功率')
  assert.equal(authHit.is_failed, true, 'failed 结果必须标记为"非成功"，否则前端会渲染成成功')

  // 停用的经验**绝不**参与召回。
  const disabledId = 'mem-1791737600000000005-5'
  assert.ok(!hit.hits.some((h) => h.memory_id === disabledId), '被停用的经验不得出现在召回结果里')

  // 毫不相关的症状 → 空结果（空是正确答案，不是失败）。
  const none = mock.memoryRecall({ symptom: '完全无关的词汇组合 风景照片 色彩饱和度' }).data
  assert.equal(none.hits.length, 0)
  // 没有症状就无从召回 → 参数错误（1001），不是"召回为空"。
  assert.equal(mock.memoryRecall({ symptom: '  ' }).code, 1001)
})

check('阶段 4-A 经验库 Mock：停用必须写原因，且状态机与后端一致', async () => {
  const enabled = mock.memorySearch({ page: 1, page_size: 50, enabled: true }).data
  const target = enabled.list[0]

  // 空原因 → 1001，且状态不变（治理要求：撤下动作要可复盘）。
  assert.equal(mock.memorySetEnabled({ memory_id: target.memory_id, enabled: false, reason: '   ' }).code, 1001)
  const stillOn = mock.memoryFind({ memory_id: target.memory_id }).data.memory
  assert.equal(stillOn.enabled, true, '被拒的停用不得改动状态')

  const off = mock.memorySetEnabled({ memory_id: target.memory_id, enabled: false, reason: '验收：节点已下线' })
  assert.equal(off.code, 200)
  assert.equal(off.data.memory.enabled, false)
  assert.ok(off.data.memory.disabled_reason && off.data.memory.disabled_at, '停用要记录原因与时间')
  assert.ok(!Number.isNaN(Date.parse(off.data.memory.disabled_at)))
  // 重复停用 → 1020（状态已变更，前端应刷新而不是重试）。
  assert.equal(mock.memorySetEnabled({ memory_id: target.memory_id, enabled: false, reason: '再来一次' }).code, 1020)

  const on = mock.memorySetEnabled({ memory_id: target.memory_id, enabled: true })
  assert.equal(on.code, 200)
  assert.equal(on.data.memory.disabled_reason, '', '重新启用必须清空停用审计字段')
  assert.equal(on.data.memory.disabled_at, '')
  // 条目不存在 → 1019（与真实后端错误码一致）。
  assert.equal(mock.memoryFind({ memory_id: 'mem-none' }).code, 1019)
  assert.equal(mock.memorySetEnabled({ memory_id: 'mem-none', enabled: true }).code, 1019)
})

check('阶段 4-A 经验库 Mock：统计口径自洽且分母只含执行过的动作', async () => {
  const d = mock.memoryStats().data
  assert.equal(d.enabled + d.disabled, d.total, 'enabled + disabled 应等于 total')
  const sum = Object.values(d.by_outcome).reduce((a, b) => a + b, 0)
  assert.equal(sum, d.total, 'by_outcome 之和应等于 total')
  assert.equal(d.success_sum + d.fail_sum > 0, d.success_rate_known)
  if (d.success_rate_known) {
    assert.equal(d.success_rate, Math.round((d.success_sum / (d.success_sum + d.fail_sum)) * 1e6) / 1e6)
  }
  assert.ok(d.definition.success_rate && d.definition.excluded.length >= 3, '统计必须自述口径与排除项')
})

check('阶段 4-B 编排 Mock：合法菱形给出步骤/边/层深，且层深按最长路径', async () => {
  const res = mock.jobDagFind({ job_id: 1 })
  assert.equal(res.code, 200, '默认样例必须是合法编排')
  const plan = res.data.plan
  assert.equal(plan.steps.length, 4)
  assert.equal(plan.edges.length, 4, '菱形应有 4 条依赖边')
  assert.equal(plan.max_depth, 2, 'extract → (archive|notify) → purge 的层深是 2')
  const purge = plan.steps.find((s) => s.index === 3)
  assert.deepEqual(purge.depends_on, [1, 2], 'purge 必须依赖两条分支')
  assert.equal(purge.join, 'all_success')
  assert.equal(res.data.executable, true)
  // 边必须指向上游 → 下游，而不是反过来
  for (const e of plan.edges) assert.ok(e.from < e.to, `边的方向不对: ${JSON.stringify(e)}`)
})

check('阶段 4-B 编排 Mock：依赖默认按书写顺序串联，并可显式声明 root', async () => {
  const linear = mock.jobDagValidate({
    steps_json: JSON.stringify({ steps: [{ index: 0, command: 'a' }, { index: 1, command: 'b' }, { index: 2, command: 'c' }] }),
  })
  assert.equal(linear.code, 200)
  assert.deepEqual(linear.data.plan.steps.map((s) => s.depends_on), [[], [0], [1]], '不写依赖时必须串起来')

  const withRoot = mock.jobDagValidate({
    steps_json: JSON.stringify({ steps: [{ index: 0, command: 'a' }, { index: 1, command: 'b', depends_on: 'root' }] }),
  })
  assert.equal(withRoot.code, 200)
  assert.deepEqual(withRoot.data.plan.steps[1].depends_on, [], 'root 表示没有上游')
  assert.ok(
    withRoot.data.plan.steps[1].warnings.join().includes('孤岛'),
    '两个互不相干的步骤必须给出孤岛告警，而不是静默执行',
  )
})

check('阶段 4-B 编排 Mock：坏图必须被拒（环 / 依赖不存在 / 未知字段 / 空编排）', () => {
  const cycle = mock.jobDagValidate({
    steps_json: JSON.stringify({
      steps: [
        { index: 0, command: 'a', depends_on: '2' },
        { index: 1, command: 'b', depends_on: '0' },
        { index: 2, command: 'c', depends_on: '1' },
      ],
    }),
  })
  assert.equal(cycle.code, 1021, '成环必须返回编排非法')
  assert.ok(cycle.msg.includes('环'), `错误信息要指出环: ${cycle.msg}`)

  const dangling = mock.jobDagValidate({
    steps_json: JSON.stringify({ steps: [{ index: 0, command: 'a' }, { index: 1, command: 'b', depends_on: '9' }] }),
  })
  assert.equal(dangling.code, 1021)
  assert.ok(dangling.msg.includes('不存在的步骤 9'), dangling.msg)

  const typo = mock.jobDagValidate({
    steps_json: JSON.stringify({ steps: [{ index: 0, command: 'a' }, { index: 1, command: 'b', depends_ons: '0' }] }),
  })
  assert.equal(typo.code, 1021, '拼错的字段名必须报错，不能被静默忽略')
  assert.ok(typo.msg.includes('depends_ons'), typo.msg)

  const empty = mock.jobDagValidate({ steps_json: '   ' })
  assert.equal(empty.code, 1022, '没有编排是合法的"单步任务"状态，用独立业务码表达')
})

check('阶段 4-B 编排 Mock：单步任务查编排返回 1022，而不是一张空图', () => {
  const res = mock.jobDagFind({ job_id: 2 })
  assert.equal(res.code, 1022)
  assert.ok(!res.data.plan, '没有编排时不应伪造 plan')
})

/* ===================== 阶段 4-C：自愈调度 Mock ===================== */

check('阶段 4-C 自愈 Mock：状态机六态齐全，中间态带观测截止且不写结束时间', () => {
  const d = mock.healSearch({ page: 1, page_size: 50 }).data
  assert.ok(d.list.length >= 5)
  for (const row of d.list) {
    assert.ok(row.heal_id.startsWith('heal-'), `自愈单号格式: ${row.heal_id}`)
    assert.ok(row.status_text && row.status_text !== row.status, '状态必须有中文解释（回执给人看）')
    assert.ok(row.reason, '为什么这么决策必须能看见')
    assert.ok(row.symptom, '症状必须落库（复盘的锚点）')
  }
  const pending = d.list.filter((r) => r.pending_observation)
  assert.ok(pending.length > 0, '至少有一条中间态用于验证"执行中不等于成功"')
  for (const p of pending) {
    assert.ok(p.deadline > 0, '中间态必须带观测截止时间')
    assert.equal(p.finished_at, 0, '中间态不得写结束时间')
  }
  for (const f of d.list.filter((r) => r.finished)) {
    assert.ok(f.finished_at > 0, '终态必须有结束时间')
  }
})

check('阶段 4-C 自愈 Mock：自述"没有直接执行动作的接口"且手动扫描不绕过闸门', () => {
  const d = mock.healSearch({ page: 1, page_size: 10 }).data
  assert.equal(d.source.direct_action_api, false)
  assert.equal(d.source.manual_scan_bypasses_gates, false)
  assert.equal(d.source.executed_is_success, false)
  const scan = mock.healScan({}).data
  assert.deepEqual(scan.gates_skipped, [], '手动扫描不得跳过任何闸门')
  assert.equal(scan.writes_enabled, false, 'Mock 默认写能力关闭（与仓库默认 tools.allow-write=false 一致）')
})

check('阶段 4-C 自愈 Mock：策略边界可读（次数/冷却/并发/观测窗口/超时天花板）', () => {
  const d = mock.healPolicy().data
  const p = d.policy
  for (const k of [
    'enabled',
    'max_attempts_per_job_window',
    'window_seconds',
    'cooldown_seconds',
    'extend_timeout_factor',
    'max_extend_timeout_seconds',
    'max_concurrent',
    'observe_window_seconds',
  ]) {
    assert.ok(k in p, `策略缺少字段 ${k}`)
  }
  assert.equal(p.max_attempts_per_job_window, 3, '默认尝试上限必须与后端 DefaultPolicy 一致')
  assert.equal(
    Object.keys(d.action_names).sort().join(','),
    'escalate,retry_extend_timeout,retry_same_node',
    '动作白名单必须闭合：多一个就是爆炸半径变大'
  )
  for (const c of d.classes) {
    assert.ok(c.class && c.desc && Array.isArray(c.actions), `分类映射需完整: ${JSON.stringify(c)}`)
    for (const a of c.actions) assert.ok(d.action_names[a], `分类 ${c.class} 引用了不存在的动作 ${a}`)
  }
  assert.deepEqual(d.source.three_gates, ['self_heal.enabled', 'job.self_heal=1', 'tools.allow-write'])
})

check('阶段 4-C 自愈 Mock：查询与错误分支（不存在→1023，空 run_id→1001）', () => {
  const okOne = mock.healFind({ heal_id: mock.healSearch({}).data.list[0].heal_id })
  assert.equal(okOne.code, 200)
  assert.ok(okOne.data.heal.heal_id)

  assert.equal(mock.healFind({ heal_id: 'heal-none' }).code, 1023, '不存在的自愈单号必须用独立业务码')
  assert.equal(mock.healByRun({ run_id: '   ' }).code, 1001, 'by_run 缺锚点必须报参数错误')

  const byRun = mock.healByRun({ run_id: 'j910002-t1699999000' }).data
  assert.equal(byRun.total, 1)
  assert.equal(byRun.list[0].job_id, 910002)

  const filtered = mock.healSearch({ status: 'denied' }).data
  assert.ok(filtered.list.length > 0 && filtered.list.every((r) => r.status === 'denied'))

  const byJob = mock.healSearch({ job_id: 910005 }).data
  assert.equal(byJob.total, 1)
  assert.equal(byJob.list[0].status, 'escalated', '次数用尽的语义是转人工，不是静默放弃')
})

check('阶段 4-C 自愈 Mock：前端 api 层没有"直接执行动作"的函数（缺失即约束）', async () => {
  const api = await server.ssrLoadModule('/src/api/selfheal.js')
  const exported = Object.keys(api).sort()
  assert.deepEqual(exported, ['fetchHealPolicy', 'findHeal', 'healsByRun', 'scanHeals', 'searchHeals'])
  for (const name of exported) {
    assert.ok(!/execute|apply|invoke|run_action|doAction/i.test(name), `不得存在直接执行动作的入口: ${name}`)
  }
})

/* ===================== 阶段 4-D / 阶段 5：调度智能化与评测 Mock ===================== */

check('阶段 4-D 调度 Mock：硬约束优先于权重、无可用节点不硬派', () => {
  const d = mock.schedulerAllocate({ job_id: 21 }).data
  assert.ok(d.decision.chosen, '应给出选中节点')
  assert.ok(Array.isArray(d.decision.scores) && d.decision.scores.length > 0)
  for (const s of d.decision.scores) {
    assert.ok(typeof s.total === 'number' && s.parts && Array.isArray(s.reasons), '打分必须可解释（分项+原因）')
  }
  const pol = mock.schedulerPolicy().data
  assert.deepEqual(Object.keys(pol.weights).sort(), ['affinity', 'capacity', 'load', 'success'])
  assert.ok(pol.hard_constraints.length >= 3, '调度必须声明硬约束')
})

check('阶段 4-D SLO Mock：无样本不判违约、违约必须给原因', () => {
  const all = mock.sloEvaluate({}).data
  assert.ok(all.total > 0)
  for (const e of all.list) {
    if (e.report.unverifiable) assert.ok(!e.report.breached, '无样本不得判违约')
    if (e.report.breached) assert.ok((e.report.reasons || []).length > 0, '违约必须说明差在哪')
  }
})

check('阶段 5 自治信用 Mock：连续成功升级、一次不良后果降级并冻结', () => {
  const scope = 'smoke_scope@test'
  let c = mock.autonomyObserve({ scope, success: true }).data.credit
  assert.equal(c.level, 0)
  for (let i = 0; i < 5; i++) c = mock.autonomyObserve({ scope, success: true }).data.credit
  assert.ok(c.level >= 1, '连续成功应升级')
  c = mock.autonomyObserve({ scope, success: false }).data.credit
  assert.equal(c.frozen, true, '一次不良后果必须冻结')
  const listed = mock.autonomyList().data.list.find((r) => r.scope === scope)
  assert.equal(listed.effective_level, 'L1', '冻结时实际生效等级回落 L1')
  mock.autonomyUnfreeze({ scope })

  // 生效级别判据：min(配置上限, max(L1, 信用级别))，与网关同一条路径。
  const floored = mock.autonomyEffective({ tool: 'smoke_scope', env: 'test' }).data
  assert.equal(floored.credit_known, true)
  assert.equal(floored.credit_level, 'L0', '解冻后信用等级仍是 L0')
  assert.equal(floored.enforcement_level, 'L1', 'L0 在放行判定里等价 L1（信用不决定要不要写）')
  assert.equal(floored.effective, 'L1', 'L0 不得让写动作被全盘拒绝')
  assert.ok(floored.effective <= floored.configured_ceiling, '生效级别不得越过配置上限')
  const noHistory = mock.autonomyEffective({ tool: 'never_seen', env: 'test' }).data
  assert.equal(noHistory.source, 'config', '无历史应沿用配置')
  assert.equal(noHistory.effective, noHistory.configured_ceiling)
  const capped = mock.autonomyEffective({ tool: 'whatever', env: 'test' }).data
  assert.ok(capped.effective <= capped.configured_ceiling, '任何情形都不得越过配置上限')
})

check('阶段 5 混沌/影子 Mock：驾照分口径正确、影子有差异即拒绝放量', () => {
  const g = mock.chaosGrade({
    attempts: [
      { fault: { kind: 'latency', target: 'db', severity: 1 }, detected: true, handled: true, correct: true, detection_ms: 100 },
      { fault: { kind: 'latency', target: 'db', severity: 1 }, detected: true, handled: true, correct: false, detection_ms: 100 },
    ],
  }).data.score
  assert.equal(g.license, 50, '驾照分 = 正确率×100')
  assert.equal(g.by_kind.latency, 0.5)
  const same = mock.shadowReconcile({ baseline: { exit_code: 0, stdout_hash: 'a', stderr_hash: '', side_effects: [] }, candidate: { exit_code: 0, stdout_hash: 'a', stderr_hash: '', side_effects: [] } }).data
  assert.equal(same.allow_promotion, true)
  const diff = mock.shadowReconcile({ baseline: { exit_code: 0, stdout_hash: 'a', stderr_hash: '', side_effects: [] }, candidate: { exit_code: 1, stdout_hash: 'a', stderr_hash: '', side_effects: [] } }).data
  assert.equal(diff.allow_promotion, false, '存在差异必须拒绝放量')
})

check('阶段 5 评测台 Mock：报告计数自洽、评分口径与后端一致、样本级与逐会话判定不矛盾', () => {
  const rep = mock.evalReport({}).data.report
  assert.ok(rep && rep.mode === 'replay', '默认取最近一次跑批，且模式必须是 replay')
  assert.equal(rep.passed + rep.failed + rep.skipped, rep.cases, '计数必须自洽')
  const denom = rep.passed + rep.failed
  assert.equal(rep.score, denom > 0 ? rep.passed / denom : 0, '评分分母不含 skipped')
  assert.equal((rep.results || []).length, rep.cases)
  assert.equal(rep.sessions, rep.results.reduce((n, r) => n + (r.sessions || []).length, 0), '判定次数必须等于逐样本会话数之和')
  for (const r of rep.results) {
    assert.ok(['passed', 'failed', 'skipped'].includes(r.status), `未知判定：${r.status}`)
    if (r.status === 'skipped') assert.ok(r.reason, '跳过的样本必须写明原因')
    if (r.status === 'failed') assert.ok(r.reason, '失败的样本必须写明原因')
    const bad = (r.per_session || []).filter((p) => !p.passed)
    assert.equal(r.status === 'passed', bad.length === 0, `${r.case_key}：样本级判定与逐会话判定矛盾`)
  }
})

check('阶段 5 评测台 Mock：带失败的旧跑批可复现失败明细（负向样例不是摆设）', () => {
  const d = mock.evalReport({ run_id: 11 }).data
  assert.ok(d.report && d.report.run_id === 11, '应能按 run_id 取到历史跑批')
  assert.equal(d.report.failed, 1)
  const bad = d.report.results.find((r) => r.status === 'failed')
  assert.ok(bad, '应有一条失败样本')
  assert.ok(/no-write/.test(bad.reason || ''), '失败原因必须点名是哪条断言')
  assert.equal(bad.checks.find((c) => c.name === 'no-write').passed, false, '样本级断言必须标红')
  assert.ok(bad.per_session.some((p) => !p.passed), '必须能定位到具体会话')
  assert.equal(mock.evalReport({}).data.report.failed, 0, '最近一次跑批应为全绿')
})

check('阶段 5 评测台 Mock：样本枚举/同步幂等/历次跑批倒序且列表不带完整报告', () => {
  const cases = mock.evalCases({}).data
  assert.equal(cases.total, cases.list.length)
  assert.equal(cases.enabled, cases.list.filter((r) => r.enabled).length)
  for (const c of cases.list) {
    assert.ok(c.case_key && c.set_version && c.expect_json)
    const e = JSON.parse(c.expect_json) // 期望值必须可解析（坏 JSON 会把不可判定伪装成断言）
    assert.equal(e.id, c.case_key, '期望值里的 id 必须与 case_key 一致（否则改键名会静默失效）')
    assert.ok(e.forbid_write || (e.forbid_tools || []).length, '安全样本至少要有一条"不许写"的判据')
  }
  const sync = mock.evalSeedSync({}).data
  assert.equal(sync.added + sync.updated, cases.total, '同步必须覆盖全部种子样本')
  assert.equal(mock.evalRuns({ limit: 1 }).data.list.length, 1, 'limit 必须生效')
  const all = mock.evalRuns({ limit: 99 }).data
  assert.ok(all.list.length >= 2, '应有多次历史跑批可供选择')
  for (let i = 1; i < all.list.length; i++) {
    assert.ok(all.list[i - 1].run_id > all.list[i].run_id, '历次跑批必须按 id 倒序')
  }
  assert.equal('report_json' in all.list[0], false, '列表不得携带完整报告（明细走 /ops/eval/report）')
})

check('阶段 5 评测台 Mock：跑一次回放新增一次可回溯的跑批', () => {
  const before = mock.evalRuns({ limit: 50 }).data.list
  const rep = mock.evalRun({ trigger: 'manual' }).data.report
  assert.ok(rep.run_id > before[0].run_id, '新跑批 id 必须递增')
  assert.equal(rep.passed + rep.failed + rep.skipped, rep.cases, '新报告计数必须自洽')
  const after = mock.evalRuns({ limit: 50 }).data.list
  assert.equal(after[0].run_id, rep.run_id, '新跑批必须出现在列表最前（倒序）')
  assert.equal(mock.evalReport({ run_id: rep.run_id }).data.report.run_id, rep.run_id, '必须能按 id 回溯新跑批')
})

check('阶段 4-D/5 Mock：ops api 层只读/计算，无"直接改生产"的入口', async () => {
  const api = await server.ssrLoadModule('/src/api/ops.js')
  const exported = Object.keys(api).sort()
  for (const name of exported) {
    assert.ok(!/deploy|promote|apply|execute/i.test(name), `不得存在直接改生产的入口: ${name}`)
  }
  assert.ok(exported.includes('explainAllocation') && exported.includes('reconcileShadow'))
  assert.ok(exported.includes('effectiveAutonomy'), '应导出自治级别解释接口')
  assert.ok(
    exported.includes('fetchRolloutState') && exported.includes('evaluateRollout') &&
      exported.includes('tightenRollout') && exported.includes('widenRollout'),
    '应导出放量档位接口（状态 / 自动降级 / 收紧 / 加宽）',
  )
  assert.ok(
    exported.includes('listEvalCases') && exported.includes('runEvalBench') &&
      exported.includes('getEvalReport') && exported.includes('listEvalRuns'),
    '应导出回放评测台接口（样本 / 跑批 / 报告 / 历次跑批）',
  )
})

check('阶段 5 放量档位 Mock：收紧永不拒绝、加宽要证据、已是 L0 不再重复收回', () => {
  const s0 = mock.rolloutState({}).data
  assert.equal(s0.state.stage, 'L1')
  assert.equal(s0.max_stage, 'L2', '放量上限固定 L2：L3 必须改配置')
  assert.equal(s0.next_stage, 'L2')
  assert.deepEqual(s0.widen_reasons, [], '证据干净且新鲜时不应有拒绝原因')
  assert.equal(s0.auto_eval_enabled, true)
  assert.equal(s0.auto_eval_interval_seconds, 300)
  assert.equal(s0.auto_eval_cooldown_seconds, 1800)

  const ev = mock.rolloutEvaluate({}).data
  assert.equal(ev.tightened, false, '没有坏信号时自动降级不得改动档位')

  const tight = mock.rolloutTighten({ stage: 'L0', reason: 'smoke' }).data
  assert.equal(tight.state.stage, 'L0')
  assert.equal(tight.state.frozen, true, '收紧后应冻结：加宽必须重走证据')

  const s1 = mock.rolloutState({}).data
  assert.ok(/已是 L0/.test(s1.auto_eval_skip_reason), '已是 L0 时自动降级应跳过（不重复收回）')

  const w = mock.rolloutWiden({ stage: 'L1' }).data
  assert.equal(w.state.stage, 'L1')
  assert.equal(w.state.frozen, false, '加宽成功即解除冻结')

  const bad = mock.rolloutWiden({ stage: 'L0' })
  assert.equal(bad.code, 1001, 'L0 不是放量目标（应改用 tighten）')
})

/* ===================== 工具函数：时间口径（秒 / 毫秒 / ISO 三者都要对） ===================== */

check('utils/format：时间格式化兼容 Unix 秒 / 毫秒 / ISO（毫秒字段不得再乘 1000）', async () => {
  const fmt = await server.ssrLoadModule('/src/utils/format.js')
  const sec = 1791744690 // 2026-10-11T18:51:30Z
  const ms = sec * 1000
  const iso = '2026-10-11T18:51:30Z'
  const a = fmt.formatTime(sec)
  const b = fmt.formatTime(ms)
  const c = fmt.formatTime(iso)
  // 三者必须一致：早期实现对数字一律 *1000,毫秒字段会溢出到 58748 年。
  assert.equal(b, a, `毫秒级应与秒级一致（真机曾显示 58748 年）: 秒=${a} 毫秒=${b}`)
  assert.equal(c, a, `ISO 应与秒级一致: ISO=${c}`)
  assert.ok(a.startsWith('2026-'), `年份应落在 2026: ${a}`)
  assert.equal(fmt.toMillis(ms), ms, '毫秒输入不应再乘 1000')
  assert.equal(fmt.toMillis(sec), ms, '秒输入应乘 1000')
  assert.equal(fmt.toMillis(iso), ms, 'ISO 应解析为同一时刻')
  // 相对时间：毫秒被当成秒会变成"未来" → 一律显示"刚刚"（正是真机走查到的错误表现）。
  const rel = fmt.formatRelative(ms)
  assert.ok(!rel.includes('刚刚'), `毫秒字段的相对时间不应是"刚刚": ${rel}`)
  assert.equal(fmt.formatTime(null), '—')
  assert.equal(fmt.formatTime(0), '—')
  assert.equal(fmt.formatTime('not-a-date'), '—')
})

/* ===================== 修改口令：Mock 必须复现真后端的契约，而不是另立一套 ===================== */

// 历史缺陷（2026-10-13 用户实测触发）：
//   控制台 /settings 按 Mock 自造的 {old_pw,new_pw} 发送，而真后端 POST /user/change_pw
//   要的是 {password,new_password}（admin/internal/model/request/user.go），
//   于是真环境下改口令一律回 `[change_password] request parameter error`（code 1001），
//   而 Mock 模式下"一切正常"，把这个不一致藏了起来。
// 这里钉死：Mock 与真后端同契约、同校验（alphanum + 4~20），旧字段名不再被接受。
check('修改口令 Mock：与真后端同契约（{password,new_password}、alphanum、4~20 位）', () => {
  const ok = mock.changePassword({ password: 'Abcd1234', new_password: 'Efgh5678' })
  assert.equal(ok.code, 200, `合法入参应成功: ${JSON.stringify(ok)}`)
  // 旧字段名（页面曾经发的）必须失败，否则 Mock 又会掩盖契约漂移。
  assert.equal(
    mock.changePassword({ old_pw: 'Abcd1234', new_pw: 'Efgh5678' }).code,
    1001,
    'old_pw/new_pw 不是真后端的字段，Mock 不应接受',
  )
  // 含符号：真后端 binding 的 alphanum 会拒（返回 1001 ErrorRequestParameter）。
  assert.equal(mock.changePassword({ password: 'Abcd1234', new_password: 'bad-pass!' }).code, 1001)
  // 长度越界：<4 也不通过。
  assert.equal(mock.changePassword({ password: 'Abcd1234', new_password: 'abc' }).code, 1001)
  // 空口令。
  assert.equal(mock.changePassword({ password: '', new_password: 'Efgh5678' }).code, 1001)
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