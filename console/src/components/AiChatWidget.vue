<script setup>
import { nextTick, ref, watch } from 'vue'
import { storeToRefs } from 'pinia'
import { useChatStore } from '@/stores/chat'
import { formatTime } from '@/utils/format'

/**
 * 右下角浮动 AI 聊天入口（紫色圆形按钮 → 展开聊天面板）。
 *
 * 阶段 1：消息与回复全部来自 Mock（见 src/api/agent.js），面板上明确标注
 * "不产生真实变更"，避免误以为已经能自动执行运维动作。
 */
const chat = useChatStore()
const { open, messages, sending } = storeToRefs(chat)
const draft = ref('')
const listRef = ref(null)

const quickPrompts = [
  'node-edge-01 磁盘为什么到 91%',
  'MySQL 慢查询巡检失败的根因',
  '给容器健康检查加失败告警',
]

async function scrollToBottom() {
  await nextTick()
  const el = listRef.value
  if (el) el.scrollTop = el.scrollHeight
}

watch(messages, scrollToBottom, { deep: true, flush: 'post' })
watch(open, (v) => {
  if (v) scrollToBottom()
})

async function submit() {
  const text = draft.value
  if (!text || !text.trim()) return
  draft.value = ''
  await chat.send(text)
}

function useQuick(p) {
  draft.value = p
}
</script>

<template>
  <div class="fixed bottom-6 right-6 z-50 flex flex-col items-end gap-3">
    <!-- 聊天面板 -->
    <transition
      enter-active-class="transition duration-200 ease-out"
      enter-from-class="translate-y-3 scale-95 opacity-0"
      enter-to-class="translate-y-0 scale-100 opacity-100"
      leave-active-class="transition duration-150 ease-in"
      leave-from-class="translate-y-0 scale-100 opacity-100"
      leave-to-class="translate-y-3 scale-95 opacity-0"
    >
      <section
        v-if="open"
        class="flex h-[520px] w-[380px] max-w-[calc(100vw-3rem)] flex-col overflow-hidden rounded-2xl border border-navy-600 bg-navy-800 shadow-2xl"
      >
        <!-- 头部 -->
        <header
          class="flex shrink-0 items-center gap-3 border-b border-navy-600 bg-gradient-to-r from-brand-700/40 to-violet2/25 px-4 py-3"
        >
          <div
            class="flex h-8 w-8 items-center justify-center rounded-lg bg-gradient-to-br from-brand-500 to-violet2 text-white"
          >
            <el-icon :size="15"><MagicStick /></el-icon>
          </div>
          <div class="min-w-0 flex-1">
            <p class="truncate text-xs font-semibold leading-4 text-ink">AI 运维助手</p>
            <p class="truncate text-[10px] leading-4 text-ink-dim">
              Mock 模式 · 不产生真实变更
            </p>
          </div>
          <el-select v-model="chat.agentId" size="small" class="!w-[120px]">
            <el-option label="观测 Agent" value="agent-observe-01" />
            <el-option label="根因 Agent" value="agent-rca-01" />
            <el-option label="修复 Agent" value="agent-remediate-01" />
          </el-select>
          <button
            class="flex h-6 w-6 items-center justify-center rounded text-ink-dim hover:bg-navy-700 hover:text-ink"
            title="清空对话"
            @click="chat.clear()"
          >
            <el-icon :size="13"><Delete /></el-icon>
          </button>
          <button
            class="flex h-6 w-6 items-center justify-center rounded text-ink-dim hover:bg-navy-700 hover:text-ink"
            title="关闭"
            @click="chat.closePanel()"
          >
            <el-icon :size="13"><Close /></el-icon>
          </button>
        </header>

        <!-- 消息列表 -->
        <div ref="listRef" class="scroll-area flex-1 space-y-3 px-4 py-4">
          <div
            v-for="m in messages"
            :key="m.id"
            class="flex gap-2.5"
            :class="m.role === 'user' ? 'flex-row-reverse' : ''"
          >
            <div
              class="mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-[10px] font-semibold"
              :class="
                m.role === 'user'
                  ? 'bg-navy-600 text-ink-muted'
                  : 'bg-gradient-to-br from-brand-500 to-violet2 text-white'
              "
            >
              {{ m.role === 'user' ? '我' : 'AI' }}
            </div>
            <div class="min-w-0 max-w-[78%]">
              <div
                class="whitespace-pre-wrap break-words rounded-xl px-3 py-2 text-xs leading-5"
                :class="
                  m.role === 'user'
                    ? 'bg-brand-500/20 text-ink'
                    : m.error
                      ? 'bg-danger/15 text-danger'
                      : 'bg-navy-700 text-ink-muted'
                "
              >
                {{ m.text }}
              </div>
              <p class="mt-1 px-1 text-[10px] text-ink-dim">
                <span v-if="m.agent">{{ m.agent }} · </span>{{ formatTime(Math.floor(m.time / 1000)) }}
              </p>
            </div>
          </div>

          <div v-if="sending" class="flex items-center gap-2 px-1 text-[11px] text-ink-dim">
            <el-icon class="is-loading" :size="13"><Loading /></el-icon>
            正在思考（Mock 延迟）…
          </div>
        </div>

        <!-- 快捷问题 -->
        <div v-if="messages.length <= 1" class="flex flex-wrap gap-1.5 px-4 pb-2">
          <button
            v-for="p in quickPrompts"
            :key="p"
            class="rounded-full border border-navy-600 px-2.5 py-1 text-[10px] text-ink-muted transition-colors hover:border-brand-500 hover:text-ink"
            @click="useQuick(p)"
          >
            {{ p }}
          </button>
        </div>

        <!-- 输入区 -->
        <footer class="shrink-0 border-t border-navy-600 p-3">
          <div class="flex items-end gap-2">
            <el-input
              v-model="draft"
              type="textarea"
              :rows="2"
              resize="none"
              maxlength="500"
              placeholder="描述你的运维目标，例如：帮我定位 node-edge-01 磁盘告警的根因"
              @keydown.enter.exact.prevent="submit"
            />
            <el-button type="primary" :loading="sending" class="!h-[54px]" @click="submit">
              <el-icon v-if="!sending" :size="15"><Promotion /></el-icon>
            </el-button>
          </div>
          <p class="mt-1.5 text-[10px] text-ink-dim">
            Enter 发送 · 阶段 2 接入真实 Agent 运行时（POST /agent/chat）
          </p>
        </footer>
      </section>
    </transition>

    <!-- 浮动入口按钮 -->
    <el-tooltip content="AI 运维助手（Mock）" placement="left">
      <button
        class="relative flex h-14 w-14 items-center justify-center rounded-full bg-gradient-to-br from-brand-500 to-violet2 text-white shadow-glow transition-transform hover:scale-105"
        @click="chat.toggle()"
      >
        <span
          v-if="!open"
          class="absolute inset-0 rounded-full bg-violet2/50 animate-pulse-ring"
          aria-hidden="true"
        />
        <el-icon :size="22"><component :is="open ? 'ArrowDown' : 'ChatDotRound'" /></el-icon>
      </button>
    </el-tooltip>
  </div>
</template>