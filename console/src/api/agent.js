/**
 * Agent 接口 —— **阶段 2 真实契约**（《PHASE2-接口契约.md》§1–§5）。
 *
 * 历史说明（为什么这个文件被重写）：
 *   阶段 1 这里写的是 `POST /agent/search | /agent/chat | /agent/dispatch`，那是**设想**，
 *   后端从未实现过这三个路由。阶段 2 的 Agent 内核（common/pkg/agent/*）落地后，
 *   真实接口收敛为下面的 7 个（§1 会话列表 / §2 会话详情 / §3.1 读诊断 / §3.2 触发诊断 /
 *   §4 只读工具清单 / §5.1 草案 / §5.2 确认创建）。
 *   阶段 1 的三个函数（searchAgents / chatWithAgent / dispatchAgentTask）**已删除**：
 *   留着一个指向不存在路由的函数，风险是某天被误调用后只在运行期报 404；
 *   调用点（AgentsView / DashboardView / stores/chat.js）已同步改为契约接口。
 *
 * 通用约定（契约 §0，与阶段 1 一致）：
 *   - 全部 POST + JSON；HTTP 状态码恒为 200，业务结果看信封 { code, data, msg }
 *   - 信封由 api/client.js 的 unwrap() 拆掉，Mock 与真实请求共用同一条路径
 *   - 业务错误码：1007 未配置模型 / 1008 会话不存在 / 1009 草案复验不通过 /
 *     1010 运行记录不存在 / 1011 能力关闭 / 1012 节点离线（人话提示见 utils/dict.js）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/* ------------------------------------------------------------------ *
 * §1 会话列表  POST /agent/session/search
 * 入参（全部可选）：{ job_id, run_id, kind, status, page, page_size }
 * kind ∈ diagnose | nljob | job；status ∈ completed | failed | canceled
 * 返回：PageResult { list, total, page, page_size }
 * ------------------------------------------------------------------ */
export function searchAgentSessions(params = {}) {
  if (USE_MOCK) return mockResponse(mock.agentSessionSearch(params))
  return request.post('/agent/session/search', params)
}

/* ------------------------------------------------------------------ *
 * §2 会话详情（轨迹回放）  POST /agent/session/find
 * 入参：{ session_id }
 * 返回：会话概要 + system/context/budget/warnings/evidence/tool_stats/steps/result
 * ------------------------------------------------------------------ */
export function findAgentSession(sessionId) {
  const payload = typeof sessionId === 'string' ? { session_id: sessionId } : sessionId || {}
  if (USE_MOCK) return mockResponse(mock.agentSessionFind(payload))
  return request.post('/agent/session/find', payload)
}

/* ------------------------------------------------------------------ *
 * §3.1 读取已存诊断  POST /agent/diagnosis/find
 * 入参：{ run_id }
 * 返回：{ found, session_id, diagnosis }；found=false 是**正常状态**（还没诊断过），不是错误码
 * ------------------------------------------------------------------ */
export function findAgentDiagnosis(runId) {
  const payload = typeof runId === 'string' ? { run_id: runId } : runId || {}
  if (USE_MOCK) return mockResponse(mock.agentDiagnosisFind(payload))
  return request.post('/agent/diagnosis/find', payload)
}

/* ------------------------------------------------------------------ *
 * §3.2 触发一次诊断  POST /agent/diagnosis/run
 * 入参：{ run_id, force }
 *   force=false：已有诊断直接返回旧的（幂等，不烧 token）
 *   force=true ：重新诊断并新增一条会话记录（会消耗 token，UI 需二次确认）
 * 返回：同 §3.1，found 恒为 true
 * 失败：1010 运行记录不存在 / 1007 未配置模型 / 1012 节点离线 / 1011 能力关闭
 * ------------------------------------------------------------------ */
export function runAgentDiagnosis(runId, { force = false } = {}) {
  const payload = typeof runId === 'string' ? { run_id: runId, force } : { ...(runId || {}), force }
  if (USE_MOCK)
    return mockResponse(mock.agentDiagnosisRun(payload), {
      // 触发诊断是"真的会调模型"的动作，Mock 也给足够长的延迟，让 loading 态可见
      delay: force ? 900 : 600,
    })
  return request.post('/agent/diagnosis/run', payload)
}

/* ------------------------------------------------------------------ *
 * §4 只读工具清单  POST /agent/tool/list
 * 返回：{ tools: [{ name, description, readonly, timeout_ms, schema }], policy: {...} }
 * 用途：/agents 的"当前能力边界"区块——只展示真实注册的只读工具与配额，不写宣传文案
 * ------------------------------------------------------------------ */
export function listAgentTools(params = {}) {
  if (USE_MOCK) return mockResponse(mock.agentToolList(params))
  return request.post('/agent/tool/list', params)
}

/* ------------------------------------------------------------------ *
 * §5.1 自然语言 → 任务草案  POST /agent/nljob/draft
 * 入参：{ text, node_uuid, timezone }
 * 返回：草案字段 + next_triggers / next_trigger_text / trigger_echo
 *       + requires_confirmation（恒 true）/ confirmed（恒 false，本接口不落库）
 *
 * ⚠️ 真实后端的 data 是**多一层**的：{ draft: {...}, error?, session_id }，
 *    而 Mock 与调用方按扁平结构读（draft.value.name / .spec / ...）。
 *    这里统一抹平：有 draft 就把它摊平，并保留外层 error/session_id；
 *    draft 自己的同名字段优先（避免外层覆盖真实草案字段）。
 *    这个差异是 2026-10-11 真实联调（VITE_USE_MOCK=false）发现的：
 *    不抹平则向导页所有字段显示为空、确认按钮拿不到 session_id。
 * ------------------------------------------------------------------ */
export function normalizeDraft(data) {
  if (!data || typeof data !== 'object' || Array.isArray(data)) return data
  if (!data.draft || typeof data.draft !== 'object') return data
  return { ...data, ...data.draft }
}

export function draftNlJob(payload) {
  if (USE_MOCK)
    return mockResponse(mock.agentNlJobDraft(payload), {
      delay: 800, // 起草要跑模型，Mock 也慢一点，避免让人以为"没调模型"
    })
  return request.post('/agent/nljob/draft', payload).then(normalizeDraft)
}

/* ------------------------------------------------------------------ *
 * §5.2 确认创建  POST /agent/nljob/confirm
 * 入参：{ session_id, confirmed: true, overrides: { name, spec, timeout_seconds, retry_times } }
 *   overrides 只允许这四个字段；空串 / timeout_seconds<=0 / retry_times<0 表示"不改"，
 *   服务端会重新复验（见 admin/internal/handler/agent.go 的 applyDraftOverrides）。
 * 返回：{ job_id, spec, status, next_triggers, trigger_echo, created }
 *   注意 status 一律 0（未分配）：确认只落成任务定义，不会自动派发到节点
 * ------------------------------------------------------------------ */
export function confirmNlJob(payload) {
  if (USE_MOCK) return mockResponse(mock.agentNlJobConfirm(payload), { delay: 500 })
  return request.post('/agent/nljob/confirm', payload)
}

/* ================================================================== *
 * 契约字段 ↔ Agent 内核结构的差异兜底（只读，不改变契约字段名）
 *
 * 背景：契约 §2 的示例里，步骤是 `seq` / `created` / `total_tokens` / `error`，
 * 而内核 `agent.Step`（common/pkg/agent/trace.go）的 JSON 是
 * `index` / `at` / 嵌套的 `usage.total_tokens` / `err`。
 * 契约是唯一契约，前端**按契约字段渲染**；下面的读取函数只在契约字段缺失时
 * 回落到内核字段，避免后端 handler 少包一层时整页显示空白。
 * 这些差异已作为"契约问题"上报，不会反过来改契约（见 console/README.md 第 9 节）。
 * ================================================================== */

/** 步骤序号：契约 `seq` → 内核 `index` */
export const stepSeq = (step) => step?.seq ?? step?.index ?? 0

/** 步骤时间：契约 `created` → 内核 `at` */
export const stepTime = (step) => step?.created ?? step?.at ?? ''

/** 步骤错误：契约 `error` → 内核 `err` */
export const stepError = (step) => step?.error ?? step?.err ?? ''

/** 步骤 token：契约 `total_tokens` → 内核 `usage.total_tokens` */
export const stepTokens = (step) => step?.total_tokens ?? step?.usage?.total_tokens ?? 0

const num = (v) => (Number.isFinite(Number(v)) ? Number(v) : undefined)

/**
 * §2 budget 归一化（只读展示）。
 * 契约：{ calls, tokens, duration_ms, max_calls, max_tokens, max_duration_ms, ... }
 * 内核：{ calls, tokens, elapsed(纳秒), remaining_calls, remaining_tokens, budget:{MaxCalls,…} }
 */
export function budgetView(budget) {
  const b = budget || {}
  const inner = b.budget || {}
  const elapsedMs = num(b.elapsed) !== undefined ? Math.round(num(b.elapsed) / 1e6) : undefined
  const maxDurationNs = num(inner.MaxDuration)
  return {
    calls: num(b.calls),
    maxCalls: num(b.max_calls ?? inner.MaxCalls ?? inner.max_calls),
    tokens: num(b.tokens),
    maxTokens: num(b.max_tokens ?? inner.MaxTokens ?? inner.max_tokens),
    durationMs: num(b.duration_ms) ?? elapsedMs,
    maxDurationMs: num(b.max_duration_ms ?? maxDurationNs / 1e6),
    remainingCalls: num(b.remaining_calls),
    remainingTokens: num(b.remaining_tokens),
    exceeded: b.exceeded === true,
    reason: b.reason || '',
  }
}

/**
 * §2 tool_stats 归一化。
 * 契约：{ calls, denied, truncated, remaining }
 * 内核 SessionStats 只有 { calls, denied, evidence, per_tool, … }，没有 truncated/remaining。
 * 缺失时返回 undefined，由界面显示"—"，**不编造数字**。
 */
export function toolStatsView(stats) {
  const s = stats || {}
  return {
    calls: num(s.calls),
    denied: num(s.denied),
    truncated: num(s.truncated),
    remaining: num(s.remaining),
    evidence: num(s.evidence),
    perTool: s.per_tool || null,
  }
}