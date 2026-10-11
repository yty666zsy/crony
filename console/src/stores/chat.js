import { defineStore } from 'pinia'
import { chatWithAgent } from '@/api/agent'

let seq = 0
const nextId = () => `m${++seq}`

/**
 * AI 聊天面板状态（Mock）。
 *
 * 阶段 1：消息在内存里，刷新即丢失；回复来自 @/api/agent.js 的 Mock（明确标注不会真正执行）。
 * 阶段 2：切换到 POST /agent/chat，回复里会带上工具调用轨迹与审批请求。
 */
export const useChatStore = defineStore('chat', {
  state: () => ({
    open: false,
    sending: false,
    agentId: 'agent-observe-01',
    messages: [
      {
        id: nextId(),
        role: 'assistant',
        agent: 'agent-observe-01',
        text:
          '你好，我是 crony 控制台的 AI 助手（当前为 Mock，所有回复不代表真实执行）。\n' +
          '可以试着问我：\n' +
          '· 帮我看看 node-edge-01 磁盘为什么到 91%\n' +
          '· MySQL 慢查询巡检失败了几次，根因是什么\n' +
          '· 给"容器健康检查"任务加一条失败告警',
        time: Date.now(),
      },
    ],
  }),

  actions: {
    toggle() {
      this.open = !this.open
    },

    openPanel() {
      this.open = true
    },

    closePanel() {
      this.open = false
    },

    async send(text) {
      const content = String(text || '').trim()
      if (!content || this.sending) return

      this.messages.push({ id: nextId(), role: 'user', text: content, time: Date.now() })
      this.sending = true
      try {
        const res = await chatWithAgent({ agent_id: this.agentId, message: content })
        this.messages.push({
          id: nextId(),
          role: 'assistant',
          agent: res?.agent_id || this.agentId,
          text: res?.reply || '（Mock 未返回内容）',
          time: Date.now(),
        })
      } catch (err) {
        this.messages.push({
          id: nextId(),
          role: 'assistant',
          agent: this.agentId,
          text: `调用失败：${err?.message || '未知错误'}`,
          time: Date.now(),
          error: true,
        })
      } finally {
        this.sending = false
      }
    },

    clear() {
      this.messages.splice(1)
    },
  },
})