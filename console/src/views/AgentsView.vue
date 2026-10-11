<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { storeToRefs } from 'pinia'
import { ElMessage } from 'element-plus'
import { searchAgents, dispatchAgentTask } from '@/api/agent'
import { useChatStore } from '@/stores/chat'
import StatusBadge from '@/components/StatusBadge.vue'
import { formatRelative } from '@/utils/format'

/**
 * /agents —— AI Agent 控制台。
 *
 * ⚠️ 阶段 2 交付项：Agent 运行时（多 Agent 编排、工具调用、L1/L2/L3 自治等级）尚未实现。
 *   本页的接口（POST /agent/search、POST /agent/dispatch）均为规划中，数据来自 Mock。
 *   目的：把"多 Agent 自动化平台"的信息架构、交互与审计口径先固化下来，
 *   等运行时落地后直接替换数据源，页面结构不用返工。
 */
const chat = useChatStore()
const { open: chatOpen } = storeToRefs(chat)

const loading = ref(false)
const agents = ref([])
const total = ref(0)

const query = reactive({ name: '', role: '', status: '', page: 1, page_size: 12 })

const dispatch = reactive({
  agentId: 'agent-observe-01',
  task: '',
  submitting: false,
  result: null,
})

const roleMeta = {
  observe: { text: '观测', color: '#38bdf8' },
  rca: { text: '根因分析', color: '#8b5cf6' },
  remediate: { text: '自动修复', color: '#f59e0b' },
  report: { text: '报表', color: '#22c55e' },
  knowledge: { text: '知识沉淀', color: '#6366f1' },
  compliance: { text: '安全合规', color: '#ef4444' },
}

async function load() {
  loading.value = true
  try {
    const res = await searchAgents({
      name: query.name || undefined,
      role: query.role || undefined,
      status: query.status || undefined,
      page: query.page,
      page_size: query.page_size,
    })
    agents.value = res?.list || []
    total.value = res?.total || 0
  } finally {
    loading.value = false
  }
}

async function submitDispatch() {
  if (!dispatch.task.trim()) {
    ElMessage.warning('请先用自然语言描述运维目标')
    return
  }
  dispatch.submitting = true
  try {
    dispatch.result = await dispatchAgentTask({ agent_id: dispatch.agentId, task: dispatch.task })
    ElMessage.success('任务已受理（Mock，不会真实执行）')
  } finally {
    dispatch.submitting = false
  }
}

function talkTo(agent) {
  chat.agentId = agent.id
  chat.openPanel()
}

const onlineCount = computed(() => agents.value.filter((a) => a.status === 'online' || a.status === 'busy').length)
const avgSuccess = computed(() => {
  const list = agents.value.filter((a) => a.runs_today > 0)
  if (!list.length) return '—'
  return (list.reduce((s, a) => s + a.success_rate, 0) / list.length).toFixed(1)
})
const runsToday = computed(() => agents.value.reduce((s, a) => s + (a.runs_today || 0), 0))

onMounted(load)
</script>

<template>
  <div class="space-y-4">
    <!-- 顶部：自然语言下发 -->
    <section class="card card-pad">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 class="card-title">自然语言下发任务</h2>
          <p class="card-sub mt-0.5">
            描述目标（不需要写命令），由 Agent 规划工具调用序列；变更类动作受自治等级约束
          </p>
        </div>
        <span class="chip border-warn/40 text-warn">阶段 2 运行时未上线 · 仅受理</span>
      </div>

      <div class="mt-4 grid gap-3 lg:grid-cols-[220px_1fr_auto]">
        <el-select v-model="dispatch.agentId" placeholder="选择 Agent">
          <el-option
            v-for="a in agents"
            :key="a.id"
            :label="`${a.name}（${roleMeta[a.role]?.text || a.role}）`"
            :value="a.id"
            :disabled="a.status === 'offline'"
          />
        </el-select>

        <el-input
          v-model="dispatch.task"
          type="textarea"
          :rows="2"
          maxlength="500"
          show-word-limit
          placeholder="例如：node-edge-01 的 /data 已经 91%，先确认是哪个目录增长最快，给出清理建议但不要自动删除"
        />

        <el-button type="primary" :loading="dispatch.submitting" class="!h-auto" @click="submitDispatch">
          <el-icon class="mr-1"><Promotion /></el-icon>下发
        </el-button>
      </div>

      <div v-if="dispatch.result" class="mt-3 rounded-lg border border-brand-500/30 bg-brand-500/10 p-3">
        <p class="text-xs text-ink">
          已受理 · Agent
          <span class="font-mono text-brand-300">{{ dispatch.result.agent_id }}</span>
          · run_id
          <span class="font-mono text-brand-300">{{ dispatch.result.run_id }}</span>
        </p>
        <p class="mt-1 text-[11px] leading-5 text-ink-dim">
          任务内容：{{ dispatch.result.task }}
        </p>
        <p class="mt-1 text-[11px] text-warn">
          该受理结果为 Mock（POST /agent/dispatch 规划中），不会真正执行任何操作。
        </p>
      </div>
    </section>

    <!-- 概览 -->
    <section class="grid-cards">
      <div class="card card-pad">
        <p class="card-sub">Agent 在线</p>
        <p class="mt-1 text-xl font-semibold text-ok">{{ onlineCount }} / {{ total }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">online + busy</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">今日 Agent 执行</p>
        <p class="mt-1 text-xl font-semibold text-brand-300">{{ runsToday }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">含只读工具调用</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">平均成功率</p>
        <p class="mt-1 text-xl font-semibold text-ink">{{ avgSuccess }}%</p>
        <p class="mt-1 text-[11px] text-ink-dim">仅统计今日有执行的 Agent</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">待审批（L2）</p>
        <p class="mt-1 text-xl font-semibold text-warn">1</p>
        <p class="mt-1 text-[11px] text-ink-dim">变更类动作需人工确认（阶段 3）</p>
      </div>
    </section>

    <!-- 筛选 -->
    <section class="card card-pad">
      <div class="flex flex-wrap items-center gap-2">
        <el-input v-model="query.name" placeholder="Agent 名称" clearable class="!w-[190px]" @keyup.enter="load">
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <el-select v-model="query.role" placeholder="角色" clearable class="!w-[150px]">
          <el-option v-for="(v, k) in roleMeta" :key="k" :label="v.text" :value="k" />
        </el-select>
        <el-select v-model="query.status" placeholder="状态" clearable class="!w-[130px]">
          <el-option label="在线" value="online" />
          <el-option label="工作中" value="busy" />
          <el-option label="空闲" value="idle" />
          <el-option label="离线" value="offline" />
        </el-select>
        <el-button type="primary" @click="load">查询</el-button>
        <el-button @click="query.name = ''; query.role = ''; query.status = ''; load()">重置</el-button>
      </div>
    </section>

    <!-- Agent 卡片网格 -->
    <section v-loading="loading" class="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
      <article
        v-for="a in agents"
        :key="a.id"
        class="card card-pad flex flex-col transition-colors hover:border-brand-500/60"
      >
        <div class="flex items-start gap-3">
          <div
            class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl"
            :style="{ backgroundColor: (roleMeta[a.role]?.color || '#6366f1') + '22', color: roleMeta[a.role]?.color }"
          >
            <el-icon :size="18"><MagicStick /></el-icon>
          </div>
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <p class="truncate text-sm font-semibold text-ink">{{ a.name }}</p>
              <StatusBadge kind="agent" :value="a.status" size="small" />
            </div>
            <p class="mt-0.5 truncate font-mono text-[10px] text-ink-dim">{{ a.id }}</p>
          </div>
          <span
            class="chip shrink-0"
            :style="{
              color: roleMeta[a.role]?.color,
              borderColor: (roleMeta[a.role]?.color || '#333') + '55',
            }"
          >
            {{ roleMeta[a.role]?.text }}
          </span>
        </div>

        <div class="mt-3 rounded-lg bg-navy-900 px-3 py-2">
          <p class="text-[10px] text-ink-dim">当前任务</p>
          <p class="mt-0.5 truncate text-[11px] text-ink-muted">{{ a.current_task }}</p>
        </div>

        <div class="mt-3 space-y-2">
          <div>
            <p class="text-[10px] text-ink-dim">能力</p>
            <div class="mt-1 flex flex-wrap gap-1">
              <span v-for="c in a.capabilities" :key="c" class="chip">{{ c }}</span>
            </div>
          </div>
          <div>
            <p class="text-[10px] text-ink-dim">可用工具</p>
            <div class="mt-1 flex flex-wrap gap-1">
              <span v-for="t in a.tools" :key="t" class="chip font-mono text-brand-300">{{ t }}</span>
            </div>
          </div>
        </div>

        <div class="mt-3 space-y-2">
          <div class="flex items-center justify-between text-[11px]">
            <span class="text-ink-dim">今日执行 {{ a.runs_today }} 次</span>
            <span class="text-ink-muted">{{ a.success_rate }}%</span>
          </div>
          <div class="h-1.5 w-full overflow-hidden rounded-full bg-navy-600">
            <div
              class="h-full rounded-full"
              :style="{
                width: a.success_rate + '%',
                backgroundColor: a.success_rate >= 95 ? '#22c55e' : a.success_rate >= 88 ? '#f59e0b' : '#ef4444',
              }"
            />
          </div>
          <p class="text-[10px] text-ink-dim">
            模型 {{ a.model }} · 最后活跃 {{ formatRelative(a.last_active) }}
          </p>
          <p v-if="a.guardrail" class="text-[10px] text-warn">{{ a.guardrail }}</p>
        </div>

        <div class="mt-4 flex items-center gap-2 border-t border-navy-600 pt-3">
          <el-button size="small" type="primary" plain :disabled="a.status === 'offline'" @click="talkTo(a)">
            <el-icon class="mr-1" :size="13"><ChatDotRound /></el-icon>对话
          </el-button>
          <el-button
            size="small"
            plain
            :disabled="a.status === 'offline'"
            @click="dispatch.agentId = a.id"
          >
            选为执行者
          </el-button>
          <router-link to="/audit" class="ml-auto text-[11px] text-brand-300 hover:underline">
            审计记录
          </router-link>
        </div>
      </article>
    </section>

    <p class="text-[11px] text-ink-dim">
      数据来源：POST /agent/search（规划中 · 阶段 2）。Agent 运行时、工具网关（ssh_exec / k8s_scale 等）
      与 L1-L3 自治等级均在后端尚未实现；本页为交互与信息架构的前置设计。
      <span :class="chatOpen ? 'text-ok' : ''">AI 对话面板状态：{{ chatOpen ? '已展开' : '收起' }}</span>
    </p>
  </div>
</template>