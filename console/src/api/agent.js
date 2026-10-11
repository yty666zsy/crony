/**
 * Agent 接口 —— **全部为规划中接口（阶段 2）**。
 *
 * 后端目前没有任何 Agent 路由；本控制台的 /agents 页面在阶段 1 只做
 * "信息架构 + 交互形态" 的定义，数据来自 Mock。
 *
 * 规划接口（阶段 2 交付真实 Agent 运行时后启用）：
 *   POST /agent/search    列出 Agent 及其状态/能力
 *   POST /agent/chat      对话（多轮，返回模型回复与工具调用轨迹）
 *   POST /agent/dispatch  把自然语言目标下发给 Agent 执行
 *
 * 相关阶段说明见根目录 AI-AGENT-改造方案.md：
 *   阶段 1 = 任务模型 2.0（job_run / job_run_step）+ 控制台骨架
 *   阶段 2 = Agent 运行时接入（本文件接口）
 *   阶段 3 = 自治分级与审批（变更类动作需人工确认）
 */
import { request, USE_MOCK, mockResponse } from './client'
import * as mock from './mock'

/** 【规划中·阶段 2】POST /agent/search */
export function searchAgents(params = {}) {
  if (USE_MOCK) return mockResponse(mock.agentSearch(params))
  return request.post('/agent/search', params)
}

/** 【规划中·阶段 2】POST /agent/chat */
export function chatWithAgent(payload) {
  if (USE_MOCK)
    return mockResponse(mock.agentChat(payload), {
      // 对话回复稍慢一点，让"思考中"状态可见
      delay: 700,
    })
  return request.post('/agent/chat', payload)
}

/** 【规划中·阶段 2】POST /agent/dispatch */
export function dispatchAgentTask(payload) {
  if (USE_MOCK) return mockResponse(mock.agentDispatch(payload))
  return request.post('/agent/dispatch', payload)
}