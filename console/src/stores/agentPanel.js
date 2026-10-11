import { defineStore } from 'pinia'

/**
 * 右下角「Agent 会话速查」面板的状态。
 *
 * 为什么不是聊天面板：阶段 2 的契约里**没有** /agent/chat —— Agent 不是一个可以聊天的对象，
 * 而是受预算约束的会话（diagnose / nljob / job，契约 §1）。
 * 所以这个面板只做两件真实存在的事：
 *   1) 按 run_id / job_id 过滤会话列表（§1）；
 *   2) 跳到会话详情看轨迹回放（§2）。
 * 它自己不发起任何"对话"，也不在前端维护会话历史（历史在服务端）。
 */
export const useAgentPanelStore = defineStore('agentPanel', {
  state: () => ({
    open: false,
    /** 预填的检索条件（从 /runs 之类的地方跳过来时用得上） */
    runId: '',
    jobId: '',
  }),

  actions: {
    toggle() {
      this.open = !this.open
    },

    openPanel(payload = {}) {
      if (payload.runId !== undefined) this.runId = payload.runId
      if (payload.jobId !== undefined) this.jobId = payload.jobId
      this.open = true
    },

    closePanel() {
      this.open = false
    },
  },
})