/**
 * 知识库接口 —— **规划中接口（阶段 2）**。
 *
 * 后端没有知识库路由；阶段 1 仅定义信息架构与检索交互。
 * 规划接口：
 *   POST /knowledge/search  文档检索（阶段 2 为向量检索 + 标签过滤）
 *   GET  /knowledge/find    文档详情
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** 【规划中·阶段 2】POST /knowledge/search */
export function searchKnowledge(params = {}) {
  if (USE_MOCK) return mockResponse(mock.knowledgeSearch(params))
  return request.post('/knowledge/search', params)
}