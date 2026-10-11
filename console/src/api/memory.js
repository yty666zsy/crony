/**
 * Agent 经验库（记忆）接口（阶段 4-A）。
 *
 * 这一层是"Agent 记忆"的人侧入口：它不生产经验（经验只由**会话结束后**的旁路
 * 沉淀产生），只负责三件事——**看、试召回、撤下**。
 *
 * 契约（admin/internal/handler/agent_memory.go，恒 HTTP 200 + {code,data,msg}）：
 *   POST /agent/memory/search   { outcome, tool, target_node, keyword, session_id, enabled, page, page_size }
 *   POST /agent/memory/find     { memory_id }
 *   POST /agent/memory/recall   { symptom, agent_kind, target_node, top_k, min_score }
 *   POST /agent/memory/stats    {}
 *   POST /agent/memory/disable  { memory_id, reason }   ← reason 必填
 *   POST /agent/memory/enable   { memory_id }
 *
 * 三条前端必须遵守的规则（都是安全约束，不是风格问题）：
 *
 *  1. **没有"编辑经验"这个动作**。经验库是提示词的输入：能改它的人就能影响模型行为，
 *     等价于提示词注入。所以只有 disable/enable，坏经验**撤下来**而不是改掉；
 *     接口返回的 `source.editable=false` 就是这条约束的机器可读形式。
 *
 *  2. **停用必须写原因**（`source.disable_requires_reason=true`）。没有原因的停用
 *     等于"悄悄让模型少知道一件事"，事后没人能复盘为什么。因此前端把原因做成
 *     必填校验（inputValidator），而不是可选备注。
 *
 *  3. **经验不是证据**。召回结果的 `advisory_only=true` / `usable_as_evidence=false`
 *     必须**照着渲染**（页面上要有明确的"仅线索"标识），不能因为"看起来像结论"
 *     就把它当证据展示。渲染时不要自己拼这句中文——用后端给的 notice，
 *     保证同一约束在所有出口只有一种说法。
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** POST /agent/memory/search 经验检索（enabled 传 undefined 表示"全部"） */
export function searchMemories(params = {}) {
  if (USE_MOCK) return mockResponse(mock.memorySearch(params))
  return request.post('/agent/memory/search', params)
}

/** POST /agent/memory/find 单条经验详情 */
export function findMemory(memoryId) {
  if (USE_MOCK) return mockResponse(mock.memoryFind({ memory_id: memoryId }))
  return request.post('/agent/memory/find', { memory_id: memoryId })
}

/**
 * POST /agent/memory/recall 以症状试召回。
 *
 * 这是排查"为什么这条经验没有被用上"的唯一手段：输入当时的症状，
 * 看命中了什么、相似度多少、命中了哪些词。返回空列表是**正确答案**，
 * 不是错误——它表示"没有足够相似的历史经验"，而不是"查询失败"。
 */
export function recallMemories(params = {}) {
  if (USE_MOCK) return mockResponse(mock.memoryRecall(params))
  return request.post('/agent/memory/recall', params)
}

/** POST /agent/memory/stats 统计口径（成功率怎么算的必须能看见） */
export function memoryStats() {
  if (USE_MOCK) return mockResponse(mock.memoryStats())
  return request.post('/agent/memory/stats', {})
}

/** POST /agent/memory/disable 停用（reason 必填，后端会拒空原因） */
export function disableMemory(memoryId, reason) {
  if (USE_MOCK) return mockResponse(mock.memorySetEnabled({ memory_id: memoryId, enabled: false, reason }))
  return request.post('/agent/memory/disable', { memory_id: memoryId, reason })
}

/** POST /agent/memory/enable 重新启用 */
export function enableMemory(memoryId) {
  if (USE_MOCK) return mockResponse(mock.memorySetEnabled({ memory_id: memoryId, enabled: true }))
  return request.post('/agent/memory/enable', { memory_id: memoryId })
}