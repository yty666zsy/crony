/**
 * 自愈调度接口（阶段 4-C）。
 *
 * 这一层是"系统自己动手"的人侧入口：它只负责**看**与**手动催一轮**。
 * 契约（admin/internal/handler/job_heal.go，恒 HTTP 200 + {code,data,msg}）：
 *   POST /agent/heal/search   { job_id, status, failure_class, action, source_run_id, node_uuid, page, page_size }
 *   POST /agent/heal/find     { heal_id }
 *   POST /agent/heal/by_run   { run_id }
 *   POST /agent/heal/policy   {}                         ← 只读：当前生效的边界
 *   POST /agent/heal/scan     { run_id? }                ← 跑一轮（不传 run_id 则全量扫）
 *
 * 三条前端必须遵守的规则（都是安全约束，不是风格问题）：
 *
 *  1. **没有"直接执行动作"的接口**，本文件也**不提供**对应的前端函数。
 *     自愈动作能不能做，只由后端闸门（白名单 + 次数 + 冷却 + 并发 + 写能力）决定；
 *     在闸门旁边开一扇"点一下就动手"的门，等于把"为什么它能自动动手"的论证作废。
 *     手动催一轮走的是 scan，它**不绕过**任何闸门——只是"现在就跑一轮"。
 *
 *  2. **策略边界必须先看得见**（`fetchHealPolicy`）。运维看到"能重试几次、冷却多久、
 *     超时最多放大到多少、写能力是否已开"之后才敢打开总开关；因此页面上必须先渲染
 *     这道边界，再渲染任何动作结果。
 *
 *  3. **"没有观测到结果" ≠ 成功**。executed（已执行待观测）是中间态：
 *     页面上必须显示它的观测截止时间，不能染成绿色，也不能把 attempt 当成"成功次数"。
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** POST /agent/heal/search 分页查询自愈记录 */
export function searchHeals(params = {}) {
  if (USE_MOCK) return mockResponse(mock.healSearch(params))
  return request.post('/agent/heal/search', params)
}

/** POST /agent/heal/find 单条自愈详情（含策略判定、证据快照、关联经验） */
export function findHeal(healId) {
  if (USE_MOCK) return mockResponse(mock.healFind({ heal_id: healId }))
  return request.post('/agent/heal/find', { heal_id: healId })
}

/** POST /agent/heal/by_run 某次失败执行对应的自愈记录（执行详情页跳转用） */
export function healsByRun(runId) {
  if (USE_MOCK) return mockResponse(mock.healByRun({ run_id: runId }))
  return request.post('/agent/heal/by_run', { run_id: runId })
}

/**
 * POST /agent/heal/policy 当前生效的策略边界（只读）。
 *
 * 返回 policy（次数/冷却/并发/观测窗口等）、writes_enabled（写动作是否可能执行）、
 * classes（每类失败允许哪些动作）、status_names/action_names（中文名）。
 */
export function fetchHealPolicy() {
  if (USE_MOCK) return mockResponse(mock.healPolicy())
  return request.post('/agent/heal/policy', {})
}

/**
 * POST /agent/heal/scan 手动触发一轮扫描；传 run_id 则只处置那一次失败。
 *
 * 注意：这不是"执行某个动作"，而是"现在跑一轮判定"。传 run_id 时后端仍会校验
 * 该执行确实是 failed/timeout，否则返回 1025。
 */
export function scanHeals(params = {}) {
  if (USE_MOCK) return mockResponse(mock.healScan(params))
  return request.post('/agent/heal/scan', params)
}