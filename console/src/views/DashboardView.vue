<script setup>
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { fetchOverview } from '@/api/statis'
import { fetchRunOverview } from '@/api/run'
import { searchAgentSessions } from '@/api/agent'
import { searchKnowledge } from '@/api/knowledge'
import StatCard from '@/components/StatCard.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import { ALERT_LEVEL, AGENT_SESSION_KIND, CONCURRENCY_POLICY, TRIGGER_SOURCE } from '@/utils/dict'
import { formatDuration, formatRelative, formatTime, formatNumber } from '@/utils/format'

/**
 * /dashboard —— 总览页。
 *
 * 布局（对齐 Baseline 观测结果）：
 *   1) 一行 4 张 KPI 卡 + 一张更高的 "Latest Alerts" 卡；
 *   2) "Quick Actions" 区块；
 *   3) 三栏：Servers（空状态）/ Online Agents（绿色徽标列表）/ Knowledge Base；
 *   4) "Recent Tasks" 表格（任务名、目标、状态、耗时、触发方式、开始时间、操作）。
 *
 * 数据来源：
 *   - GET /job/run/overview（后端已实现，近 24h 聚合）→ 前两张 KPI 卡（执行次数、成功率）
 *   - GET /statis/overview（**已实现**：节点/耗时/Agent 会话 KPI、告警、最近任务）
 *   - POST /agent/session/search（阶段 2 契约 §1）→ 右下 "Recent Agent Sessions" 栏
 *   - POST /knowledge/search（仍规划中）→ 右侧栏 → 走 Mock
 *
 * 注意：阶段 1 这里显示的是 6 个编造的 "Agent 实体"（名字/角色/今日运行次数）。
 * Go 侧没有 Agent 实体模型，所以阶段 2 换成**真实存在的会话记录**（consumes token、
 * 调只读工具、有 stop_reason 的那种），避免用假实体撑版面。
 */
const router = useRouter()
const loading = ref(true)
const data = ref(null)
const runOverview = ref(null)
const agentSessions = ref([])
const docs = ref([])

onMounted(async () => {
  try {
    const [overview, runOv, sessionRes, kbRes] = await Promise.all([
      // /statis/overview 已实现；Mock 关闭且后端不可达时单独兜底，不拖垮整页。
      fetchOverview().catch(() => null),
      // 该接口后端已实现；Mock 关闭且后端未启动时不应该拖垮整页，因此单独兜底。
      fetchRunOverview().catch(() => null),
      searchAgentSessions({ page: 1, page_size: 5 }).catch(() => ({ list: [] })),
      searchKnowledge({ page: 1, page_size: 4 }).catch(() => ({ list: [] })),
    ])
    data.value = overview
    runOverview.value = runOv
    agentSessions.value = (sessionRes?.list || []).slice(0, 5)
    docs.value = kbRes?.list || []
  } finally {
    loading.value = false
  }
})

const kpi = computed(() => data.value?.kpi || {})
const trends = computed(() => data.value?.trends || {})
const alerts = computed(() => data.value?.alerts || [])
const recentTasks = computed(() => data.value?.recent_tasks || [])

/** 有警告的会话数：会话列表里最需要被看见的信号（模型引用未观测证据、预算耗尽等都会进 warn_count） */
const warnedSessions = computed(() => agentSessions.value.filter((s) => (s.warn_count || 0) > 0).length)

/** 执行概览卡：优先用 /job/run/overview 的真实口径，缺失时回落到概览 Mock */
const runsCard = computed(() => {
  const ov = runOverview.value
  if (ov && Number(ov.total) >= 0 && ov.by_status) {
    return {
      total: ov.total,
      hint: `成功 ${ov.success} · 失败 ${ov.failed} · 超时 ${ov.timeout} · 跳过 ${ov.skipped}`,
      source: '近 24 小时 · GET /job/run/overview',
      successRate: Math.round((ov.success_rate || 0) * 1000) / 10,
      skipRate: Math.round((ov.skip_rate || 0) * 1000) / 10,
      retryTotal: ov.retry_total || 0,
    }
  }
  const k = kpi.value || {}
  return {
    total: k.runs_today ?? 0,
    hint: `成功 ${(k.runs_today || 0) - (k.failed_today || 0) - (k.timeout_today || 0) - (k.skipped_today || 0)} · 失败 ${k.failed_today ?? 0} · 跳过 ${k.skipped_today ?? 0}`,
    source: '今日 · GET /statis/overview',
    successRate: k.success_rate_today ?? 0,
    skipRate: null,
    retryTotal: null,
  }
})

/** 执行成功率的进度条颜色：>=95 绿、>=90 橙、否则红 */
const successColor = computed(() => {
  const v = runsCard.value.successRate || 0
  if (v >= 95) return '#22c55e'
  if (v >= 90) return '#f59e0b'
  return '#ef4444'
})

const quickActions = [
  { key: 'run-job', label: '立即执行任务', desc: '手动触发一次 job_run', icon: 'VideoPlay', to: '/jobs' },
  { key: 'runs', label: '查看执行记录', desc: '按状态检索 run 与步骤', icon: 'DataLine', to: '/runs' },
  { key: 'servers', label: '服务器状态', desc: '节点心跳与任务分布', icon: 'Monitor', to: '/servers' },
  { key: 'terminal', label: '打开终端', desc: '交互式终端（阶段 2）', icon: 'Monitor', to: '/terminal' },
  { key: 'agent', label: '用自然语言建任务', desc: '描述需求 → 核对 5 次触发时间 → 确认', icon: 'MagicStick', to: '/jobs' },
  { key: 'agent-session', label: '查看 Agent 会话', desc: '轨迹回放与只读能力边界', icon: 'MagicStick', to: '/agents' },
  { key: 'kb', label: '检索知识库', desc: 'Runbook / SOP / 复盘', icon: 'Search', to: '/knowledge' },
]

function goRunDetail(row) {
  // 执行详情在 /runs 页以抽屉形式呈现，这里带上 run_id 直接定位
  router.push({ path: '/runs', query: { run_id: row.run_id } })
}
</script>

<template>
  <div v-loading="loading" class="space-y-5">
    <!-- ============ 1. KPI + Latest Alerts ============ -->
    <section class="grid-kpi">
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard
          label="执行次数（24h）"
          :value="formatNumber(runsCard.total)"
          icon="VideoPlay"
          color="#06b6d4"
          :trend="trends.runs_today"
          :hint="runsCard.hint"
          :sub="runsCard.source"
        />
        <StatCard
          label="执行成功率"
          :value="`${runsCard.successRate ?? '—'}%`"
          icon="CircleCheck"
          :color="successColor"
          :trend="trends.success_rate_today"
          :hint="
            runsCard.skipRate === null
              ? '近 12 个时间片段'
              : `跳过率 ${runsCard.skipRate}% · 重试次数 ${runsCard.retryTotal}（跳过不计入失败）`
          "
        />
        <StatCard
          label="在线节点"
          :value="`${kpi.online_nodes ?? 0}/${kpi.total_nodes ?? 0}`"
          icon="Monitor"
          color="#38bdf8"
          :progress="kpi.total_nodes ? Math.round((kpi.online_nodes / kpi.total_nodes) * 100) : 0"
          :delta="kpi.total_nodes ? `离线 ${kpi.total_nodes - kpi.online_nodes} 台` : ''"
          delta-type="down"
          :hint="`Agent 会话 ${kpi.agent_sessions_total || 0} 个（今日 ${kpi.agent_sessions_today || 0}）`"
        />
        <StatCard
          label="平均执行耗时"
          :value="formatDuration(kpi.avg_duration_ms)"
          icon="Timer"
          color="#3b82f6"
          :trend="trends.avg_duration"
          hint="今日终态 run 的平均耗时"
        />
      </div>

      <!-- Latest Alerts（比左侧 KPI 卡更高，列表可滚动） -->
      <div class="card flex flex-col overflow-hidden">
        <div class="panel-head">
          <div class="flex items-center gap-2">
            <el-icon :size="15" class="text-warn"><Warning /></el-icon>
            <span class="card-title">Latest Alerts</span>
          </div>
          <span class="chip">{{ alerts.length }} 条</span>
        </div>
        <div class="scroll-area max-h-[290px] flex-1 divide-y divide-navy-600">
          <button
            v-for="a in alerts"
            :key="a.id"
            class="flex w-full items-start gap-3 px-5 py-3 text-left transition-colors hover:bg-navy-700"
            @click="router.push('/audit')"
          >
            <span
              class="mt-1.5 inline-block h-1.5 w-1.5 shrink-0 rounded-full"
              :style="{ backgroundColor: ALERT_LEVEL[a.level]?.color }"
            />
            <div class="min-w-0 flex-1">
              <p class="text-xs leading-5 text-ink">{{ a.title }}</p>
              <p class="mt-0.5 flex flex-wrap items-center gap-x-2 text-[10px] text-ink-dim">
                <span>{{ a.source }}</span>
                <span class="text-navy-500">·</span>
                <span>{{ formatRelative(a.time) }}</span>
              </p>
              <p class="mt-1 flex items-center gap-1.5">
                <span
                  class="chip"
                  :style="{
                    color: ALERT_LEVEL[a.level]?.color,
                    borderColor: (ALERT_LEVEL[a.level]?.color || '#333') + '55',
                  }"
                >
                  {{ ALERT_LEVEL[a.level]?.text || a.level }}
                </span>
                <span class="chip">{{ a.status === 'firing' ? '未恢复' : a.status === 'ack' ? '已确认' : '已恢复' }}</span>
              </p>
            </div>
          </button>
          <EmptyState
            v-if="!alerts.length"
            compact
            icon="CircleCheck"
            title="暂无告警"
            desc="所有巡检任务均未触发阈值。"
          />
        </div>
      </div>
    </section>

    <!-- ============ 2. Quick Actions ============ -->
    <section class="card card-pad">
      <div class="mb-3.5 flex items-center justify-between">
        <div>
          <h2 class="card-title">Quick Actions</h2>
          <p class="card-sub mt-0.5">常用入口，直接跳转到对应工作区</p>
        </div>
      </div>
      <div class="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
        <router-link
          v-for="act in quickActions"
          :key="act.key"
          :to="act.to"
          class="group flex flex-col gap-2 rounded-xl border border-navy-600 bg-navy-800 p-3.5 transition-all hover:-translate-y-0.5 hover:border-brand-500 hover:bg-navy-700"
        >
          <div
            class="flex h-8 w-8 items-center justify-center rounded-lg bg-brand-500/15 text-brand-300 transition-colors group-hover:bg-brand-500 group-hover:text-white"
          >
            <el-icon :size="15"><component :is="act.icon" /></el-icon>
          </div>
          <div class="min-w-0">
            <p class="truncate text-xs font-medium text-ink">{{ act.label }}</p>
            <p class="mt-0.5 line-clamp-2 text-[10px] leading-4 text-ink-dim">{{ act.desc }}</p>
          </div>
        </router-link>
      </div>
    </section>

    <!-- ============ 3. 三栏 ============ -->
    <section class="grid-3col">
      <!-- Servers：Baseline 中该卡片为空状态 -->
      <div class="card flex flex-col">
        <div class="panel-head">
          <span class="card-title">Servers</span>
          <router-link to="/servers" class="text-[11px] text-brand-300 hover:underline">
            服务器管理
          </router-link>
        </div>
        <div class="flex flex-1 items-center justify-center px-4 py-2">
          <EmptyState
            icon="Monitor"
            title="尚未纳管服务器"
            desc="节点心跳已能上报（当前 6 台已注册），但资产纳管流程（凭据、分组、标签）在阶段 2 交付。"
            action-text="查看已注册节点"
            action-to="/servers"
          />
        </div>
      </div>

      <!-- Recent Agent Sessions（阶段 2 契约 §1：真实会话，不是编造的 Agent 实体） -->
      <div class="card flex flex-col">
        <div class="panel-head">
          <div class="flex items-center gap-2">
            <span class="card-title">Recent Agent Sessions</span>
            <span v-if="warnedSessions" class="chip border-danger/40 text-danger">{{ warnedSessions }} 条有警告</span>
          </div>
          <router-link to="/agents" class="text-[11px] text-brand-300 hover:underline">
            全部会话
          </router-link>
        </div>
        <ul class="divide-y divide-navy-600">
          <li v-for="s in agentSessions" :key="s.session_id" class="px-5 py-3">
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
            <p class="mt-1 truncate text-[11px] text-ink-muted">{{ s.goal || '（无目标描述）' }}</p>
            <div class="mt-1 flex items-center gap-2 text-[10px] text-ink-dim">
              <span class="font-mono">{{ s.provider || '—' }}</span>
              <span>tokens {{ formatNumber(s.total_tokens) }}</span>
              <span>工具 {{ s.tool_calls }}</span>
              <router-link :to="`/agents/${s.session_id}`" class="ml-auto text-brand-300 hover:underline">
                看轨迹 →
              </router-link>
            </div>
          </li>
        </ul>
        <EmptyState
          v-if="!agentSessions.length"
          compact
          icon="MagicStick"
          title="暂无 Agent 会话"
          desc="会话由诊断、自然语言建任务或任务内嵌 Agent 产生。"
        />
        <p class="mt-auto border-t border-navy-600 px-5 py-2.5 text-[10px] text-ink-dim">
          POST /agent/session/search（契约 §1）· 阶段 2 契约接口
        </p>
      </div>

      <!-- Knowledge Base -->
      <div class="card flex flex-col">
        <div class="panel-head">
          <div class="flex items-center gap-2">
            <el-icon :size="15" class="text-violet2"><Notebook /></el-icon>
            <span class="card-title">Knowledge Base</span>
          </div>
          <span class="chip">{{ kpi.kb_docs }} 篇</span>
        </div>
        <ul class="flex-1 divide-y divide-navy-600">
          <li v-for="d in docs" :key="d.id" class="px-5 py-2.5">
            <p class="truncate text-xs text-ink">{{ d.title }}</p>
            <p class="mt-1 flex flex-wrap gap-1">
              <span v-for="t in d.tags.slice(0, 2)" :key="t" class="chip">{{ t }}</span>
              <span class="chip text-ink-dim">{{ formatRelative(d.updated) }}</span>
            </p>
          </li>
        </ul>
        <div class="border-t border-navy-600 p-3">
          <router-link to="/knowledge">
            <el-button class="w-full" size="small" plain>
              <el-icon :size="13" class="mr-1"><Search /></el-icon>
              检索知识库
            </el-button>
          </router-link>
        </div>
      </div>
    </section>

    <!-- ============ 4. Recent Tasks ============ -->
    <section class="card overflow-hidden">
      <div class="panel-head">
        <div>
          <span class="card-title">Recent Tasks</span>
          <p class="card-sub mt-0.5">来自 job_run（阶段 1 任务模型 2.0）</p>
        </div>
        <router-link to="/runs" class="text-[11px] text-brand-300 hover:underline">
          查看全部执行记录
        </router-link>
      </div>

      <el-table :data="recentTasks" style="width: 100%" size="default" @row-click="goRunDetail">
        <el-table-column prop="job_name" label="任务名" min-width="190">
          <template #default="{ row }">
            <div class="min-w-0">
              <p class="truncate text-xs font-medium text-ink">{{ row.job_name }}</p>
              <p class="truncate font-mono text-[10px] text-ink-dim">{{ row.run_id }}</p>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="目标" min-width="150">
          <template #default="{ row }">
            <div class="min-w-0">
              <p class="truncate text-xs text-ink-muted">{{ row.hostname }}</p>
              <p class="truncate font-mono text-[10px] text-ink-dim">{{ row.ip }}</p>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <StatusBadge kind="run" :value="row.status" />
          </template>
        </el-table-column>

        <el-table-column label="耗时" width="100">
          <template #default="{ row }">
            <span class="text-xs text-ink-muted">{{ formatDuration(row.duration_ms) }}</span>
          </template>
        </el-table-column>

        <el-table-column label="触发方式" width="120">
          <template #default="{ row }">
            <span class="chip">{{ TRIGGER_SOURCE[row.trigger_source]?.text }}</span>
          </template>
        </el-table-column>

        <el-table-column label="开始时间" width="170">
          <template #default="{ row }">
            <span class="text-xs text-ink-muted">
              {{ formatTime(row.started_at || row.scheduled_time) }}
            </span>
          </template>
        </el-table-column>

        <el-table-column label="操作" width="150" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click.stop="goRunDetail(row)">
              详情
            </el-button>
            <el-button link type="primary" size="small" @click.stop="router.push('/jobs')">
              执行
            </el-button>
            <el-button
              link
              type="danger"
              size="small"
              @click.stop="router.push('/jobs')"
            >
              取消
            </el-button>
          </template>
        </el-table-column>
      </el-table>

      <p class="border-t border-navy-600 px-5 py-2.5 text-[10px] text-ink-dim">
        并发策略示例：
        <span v-for="(v, k) in CONCURRENCY_POLICY" :key="k" class="ml-2">{{ v.text }}</span>
        —— "已跳过"状态即 Skip 策略生效的结果。
      </p>
    </section>
  </div>
</template>