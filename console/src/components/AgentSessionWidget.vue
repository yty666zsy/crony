<script setup>
import { reactive, ref, watch } from 'vue'
import { storeToRefs } from 'pinia'
import { useAgentPanelStore } from '@/stores/agentPanel'
import { searchAgentSessions } from '@/api/agent'
import { AGENT_SESSION_KIND } from '@/utils/dict'
import { formatRelative } from '@/utils/format'
import MockBadge from '@/components/MockBadge.vue'
import StatusBadge from '@/components/StatusBadge.vue'

/**
 * 右下角浮动「Agent 会话速查」（紫色按钮 → 展开面板）。
 *
 * 这里刻意不做成聊天框：阶段 2 契约没有 /agent/chat，Agent 的产物是**会话 + 轨迹**（§1/§2）。
 * 面板只调用真实存在的 §1 接口做检索，并把结果链接到 /agents/:session_id。
 */
const panel = useAgentPanelStore()
const { open, runId, jobId } = storeToRefs(panel)

const loading = ref(false)
const rows = ref([])
const total = ref(0)
const err = ref(null)

const form = reactive({ run_id: '', job_id: '', kind: '' })

// 从其它页面带过来的检索条件（例如 /runs 里点"查看 Agent 轨迹"）
watch(
  [runId, jobId, open],
  ([r, j, isOpen]) => {
    if (!isOpen) return
    if (r && r !== form.run_id) form.run_id = r
    if (j && String(j) !== form.job_id) form.job_id = String(j)
  },
  { immediate: true },
)

async function load() {
  loading.value = true
  err.value = null
  try {
    const res = await searchAgentSessions({
      run_id: form.run_id || undefined,
      job_id: form.job_id || undefined,
      kind: form.kind || undefined,
      page: 1,
      page_size: 8,
    })
    rows.value = res?.list || []
    total.value = res?.total || 0
  } catch (e) {
    err.value = e
    rows.value = []
    total.value = 0
  } finally {
    loading.value = false
  }
}

function reset() {
  form.run_id = ''
  form.job_id = ''
  form.kind = ''
  load()
}

async function toggle() {
  panel.toggle()
  if (panel.open) await load()
}
</script>

<template>
  <div class="fixed bottom-6 right-6 z-50 flex flex-col items-end gap-3">
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
        class="flex max-h-[560px] w-[420px] max-w-[calc(100vw-3rem)] flex-col overflow-hidden rounded-2xl border border-navy-600 bg-navy-800 shadow-2xl"
      >
        <header class="flex items-center justify-between border-b border-navy-600 px-4 py-3">
          <div>
            <p class="text-sm font-medium text-ink">Agent 会话速查</p>
            <p class="mt-0.5 text-[10px] text-ink-dim">
              契约 §1 · 阶段 2 没有 /agent/chat，Agent 的产物是会话与轨迹
            </p>
          </div>
          <el-button text size="small" @click="panel.closePanel()">
            <el-icon :size="14"><Close /></el-icon>
          </el-button>
        </header>

        <div class="space-y-2 border-b border-navy-600 px-4 py-3">
          <div class="grid grid-cols-2 gap-2">
            <el-input v-model="form.run_id" size="small" placeholder="run_id（如 j2-t…）" clearable />
            <el-input v-model="form.job_id" size="small" placeholder="job_id" clearable />
          </div>
          <div class="flex items-center gap-2">
            <el-select v-model="form.kind" size="small" placeholder="全部类型" clearable class="flex-1">
              <el-option v-for="k in AGENT_SESSION_KIND" :key="k.key" :label="k.text" :value="k.key" />
            </el-select>
            <el-button size="small" type="primary" :loading="loading" @click="load">检索</el-button>
            <el-button size="small" plain @click="reset">重置</el-button>
          </div>
          <div class="flex items-center gap-2">
            <MockBadge text="Mock 会话数据" />
            <router-link to="/agents" class="text-[10px] text-brand-300 hover:underline" @click="panel.closePanel()">
              打开完整会话列表 →
            </router-link>
          </div>
        </div>

        <div class="scroll-area flex-1 overflow-auto px-4 py-3">
          <p v-if="err" class="rounded-lg border border-danger/50 bg-danger/10 p-2 text-[11px] text-danger">
            检索失败（code={{ err.code }}）：{{ err.message }}
          </p>
          <p v-else-if="loading" class="text-[11px] text-ink-dim">读取中…</p>
          <p v-else-if="!rows.length" class="text-[11px] text-ink-dim">
            没有匹配的会话。可留空条件直接检索最近 8 条。
          </p>
          <ul v-else class="space-y-2">
            <li v-for="s in rows" :key="s.session_id" class="rounded-lg border border-navy-600 bg-navy-900 p-2">
              <div class="flex flex-wrap items-center gap-1.5">
                <span
                  class="chip"
                  :style="{
                    color: AGENT_SESSION_KIND[s.kind]?.color || '#7c8aa8',
                    backgroundColor: (AGENT_SESSION_KIND[s.kind]?.color || '#7c8aa8') + '1f',
                  }"
                >{{ AGENT_SESSION_KIND[s.kind]?.text || s.kind }}</span>
                <StatusBadge kind="session" :value="s.status" size="small" />
                <span v-if="s.warn_count" class="chip border-warn/40 text-warn">警告 {{ s.warn_count }}</span>
                <span class="ml-auto text-[10px] text-ink-dim">{{ formatRelative(s.created_ms || s.created) }}</span>
              </div>
              <p class="mt-1 line-clamp-2 text-[11px] leading-5 text-ink-muted">{{ s.goal || '（无目标描述）' }}</p>
              <div class="mt-1 flex items-center gap-2 text-[10px] text-ink-dim">
                <span class="font-mono">{{ s.provider || '—' }}</span>
                <span>模型调用 {{ s.model_calls }}</span>
                <span>工具调用 {{ s.tool_calls }}</span>
                <span>证据 {{ s.evidence_count }}</span>
                <router-link
                  :to="`/agents/${s.session_id}`"
                  class="ml-auto text-brand-300 hover:underline"
                  @click="panel.closePanel()"
                >看轨迹 →</router-link>
              </div>
            </li>
          </ul>
        </div>

        <footer class="border-t border-navy-600 px-4 py-2 text-[10px] text-ink-dim">
          共 {{ total }} 条匹配会话 · 数据来自 POST /agent/session/search（契约 §1）
        </footer>
      </section>
    </transition>

    <button
      class="flex h-12 w-12 items-center justify-center rounded-full bg-gradient-to-br from-brand-500 to-violet2 text-white shadow-lg transition hover:scale-105"
      title="Agent 会话速查（阶段 2）"
      @click="toggle"
    >
      <el-icon :size="20"><MagicStick /></el-icon>
    </button>
  </div>
</template>