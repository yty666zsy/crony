<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { fetchWeekStatistics, fetchTodayStatistics, fetchOverview, fetchHealth } from '@/api/statis'
import { searchNodes } from '@/api/node'
import { ALERT_LEVEL, HEALTH_STATUS } from '@/utils/dict'
import { formatNumber, formatDuration } from '@/utils/format'

/**
 * /big-screen —— 值班大屏（meta.bare = true，不带侧边栏/顶栏）。
 *
 * 数据全部来自**真实**统计接口：
 *   POST /statis/week（已实现）、POST /statis/today（已实现）、
 *   GET  /statis/overview（已实现）、GET /statis/health（已实现）、
 *   POST /node/search（已实现）。
 * 没有引入图表库：柱状图用 div 高度实现，保证离线可用、构建体积可控。
 */
const router = useRouter()

const week = ref(null)
const today = ref(null)
const overview = ref(null)
const health = ref(null)
const nodes = ref([])
const clock = ref(new Date().toLocaleTimeString('zh-CN', { hour12: false }))
let timer = null

onMounted(async () => {
  const [w, t, o, h, n] = await Promise.all([
    fetchWeekStatistics().catch(() => null),
    fetchTodayStatistics().catch(() => null),
    // 规划中接口（后端未实现）：缺失时大屏对应区块显示占位，不弹错。
    fetchOverview().catch(() => null),
    fetchHealth().catch(() => null),
    searchNodes({ page: 1, page_size: 20 }).catch(() => ({ list: [] })),
  ])
  week.value = w
  today.value = t
  overview.value = o
  health.value = h
  nodes.value = n?.list || []
  timer = setInterval(() => {
    clock.value = new Date().toLocaleTimeString('zh-CN', { hour12: false })
  }, 1000)
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})

/** 周趋势的最大值，用来归一化柱高 */
const weekMax = computed(() => {
  if (!week.value) return 1
  const all = [
    ...(week.value.success || []),
    ...(week.value.fail || []),
    ...(week.value.timeout || []),
    ...(week.value.skipped || []),
  ]
  return Math.max(...all, 1)
})

const kpi = computed(() => overview.value?.kpi || {})
const alerts = computed(() => (overview.value?.alerts || []).slice(0, 4))
const ticker = computed(() =>
  (overview.value?.alerts || []).map((a) => `${a.level === 'critical' ? '【严重】' : a.level === 'warning' ? '【警告】' : '【提示】'}${a.title}`).join('　　　'),
)

const onlineNodes = computed(() => nodes.value.filter((n) => n.status === 1))
</script>

<template>
  <div class="min-h-screen bg-[#070b16] px-6 py-5 text-ink">
    <!-- 头部 -->
    <header class="flex flex-wrap items-center gap-4 border-b border-navy-600 pb-4">
      <div class="flex items-center gap-3">
        <div
          class="flex h-11 w-11 items-center justify-center rounded-xl bg-gradient-to-br from-brand-500 to-violet2 text-white shadow-glow"
        >
          <el-icon :size="22"><Cpu /></el-icon>
        </div>
        <div>
          <h1 class="text-lg font-semibold tracking-wide">crony 值班大屏</h1>
          <p class="text-[11px] text-ink-dim">分布式定时任务 · AI Agent 运维</p>
        </div>
      </div>

      <div class="ml-auto flex items-center gap-4">
        <div class="flex items-center gap-2 rounded-lg border border-ok/25 bg-ok/10 px-3 py-1.5">
          <span class="relative flex h-2 w-2">
            <span class="absolute inline-flex h-full w-full rounded-full bg-ok opacity-70 animate-pulse-ring" />
            <span class="relative inline-flex h-2 w-2 rounded-full bg-ok" />
          </span>
          <span class="text-[11px] text-ok/90">
            {{ health?.status === 'degraded' ? 'System Degraded' : 'System Normal' }}
          </span>
        </div>
        <span class="font-mono text-lg text-brand-300">{{ clock }}</span>
        <el-button size="small" plain @click="router.push('/dashboard')">
          <el-icon class="mr-1"><Back /></el-icon>退出大屏
        </el-button>
      </div>
    </header>

    <!-- 核心指标 -->
    <section class="mt-5 grid grid-cols-2 gap-4 lg:grid-cols-5">
      <div class="rounded-2xl border border-navy-600 bg-navy-850 p-4">
        <p class="text-[11px] text-ink-dim">今日执行</p>
        <p class="mt-1 text-3xl font-semibold text-brand-300">{{ formatNumber(kpi.runs_today) }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">
          成功 {{ today?.job_success ?? '—' }} · 失败 {{ today?.job_fail ?? '—' }}
        </p>
      </div>
      <div class="rounded-2xl border border-navy-600 bg-navy-850 p-4">
        <p class="text-[11px] text-ink-dim">成功率</p>
        <p class="mt-1 text-3xl font-semibold text-ok">{{ kpi.success_rate_today ?? 0 }}%</p>
        <p class="mt-1 text-[11px] text-ink-dim">超时 {{ today?.job_timeout ?? '—' }} · 运行中 {{ today?.job_running ?? '—' }}</p>
      </div>
      <div class="rounded-2xl border border-navy-600 bg-navy-850 p-4">
        <p class="text-[11px] text-ink-dim">节点在线</p>
        <p class="mt-1 text-3xl font-semibold text-[#60a5fa]">
          {{ today?.node_online ?? kpi.online_nodes ?? 0 }}<span class="text-base text-ink-dim">/{{ today?.node_total ?? kpi.total_nodes ?? 0 }}</span>
        </p>
        <p class="mt-1 text-[11px] text-ink-dim">离线 {{ (today?.node_total ?? 0) - (today?.node_online ?? 0) }} 台</p>
      </div>
      <div class="rounded-2xl border border-navy-600 bg-navy-850 p-4">
        <p class="text-[11px] text-ink-dim">Agent 会话</p>
        <p class="mt-1 text-3xl font-semibold text-violet2">
          {{ kpi.agent_sessions_today || 0 }}<span class="text-base text-ink-dim">/{{ kpi.agent_sessions_total || 0 }}</span>
        </p>
        <p class="mt-1 text-[11px] text-ink-dim">今日 / 累计 · 今日失败 {{ kpi.agent_sessions_failed_today || 0 }}</p>
      </div>
      <div class="rounded-2xl border border-navy-600 bg-navy-850 p-4">
        <p class="text-[11px] text-ink-dim">平均耗时</p>
        <p class="mt-1 text-3xl font-semibold text-warn">{{ formatDuration(kpi.avg_duration_ms) }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">跳过 {{ kpi.skipped_today ?? 0 }} 次（并发 Skip）</p>
      </div>
    </section>

    <div class="mt-5 grid gap-4 xl:grid-cols-3">
      <!-- 周趋势 -->
      <section class="rounded-2xl border border-navy-600 bg-navy-850 p-4 xl:col-span-2">
        <div class="flex items-center justify-between">
          <h2 class="text-sm font-semibold text-ink">近 7 日执行分布</h2>
          <div class="flex items-center gap-3 text-[10px] text-ink-dim">
            <span class="flex items-center gap-1"><span class="h-2 w-2 rounded-sm bg-ok" />成功</span>
            <span class="flex items-center gap-1"><span class="h-2 w-2 rounded-sm bg-danger" />失败</span>
            <span class="flex items-center gap-1"><span class="h-2 w-2 rounded-sm bg-warn" />超时</span>
            <span class="flex items-center gap-1"><span class="h-2 w-2 rounded-sm bg-[#7c8aa8]" />跳过</span>
          </div>
        </div>

        <div class="mt-5 flex h-[240px] items-end gap-4">
          <div v-for="(d, i) in week?.date || []" :key="d" class="flex flex-1 flex-col items-center gap-2">
            <div class="flex h-[210px] w-full flex-col justify-end gap-0.5">
              <div
                class="w-full rounded-t bg-warn/80"
                :style="{ height: ((week?.timeout?.[i] || 0) / weekMax) * 100 + '%' }"
                :title="`超时 ${week?.timeout?.[i]}`"
              />
              <div
                class="w-full bg-danger/80"
                :style="{ height: ((week?.fail?.[i] || 0) / weekMax) * 100 + '%' }"
                :title="`失败 ${week?.fail?.[i]}`"
              />
              <div
                class="w-full bg-[#7c8aa8]/70"
                :style="{ height: ((week?.skipped?.[i] || 0) / weekMax) * 100 + '%' }"
                :title="`跳过 ${week?.skipped?.[i]}`"
              />
              <div
                class="w-full rounded-t bg-gradient-to-t from-brand-600 to-ok"
                :style="{ height: ((week?.success?.[i] || 0) / weekMax) * 100 + '%' }"
                :title="`成功 ${week?.success?.[i]}`"
              />
            </div>
            <span class="text-[11px] text-ink-dim">{{ d }}</span>
            <span class="text-[10px] text-ink-muted">{{ week?.success?.[i] }}</span>
          </div>
        </div>
      </section>

      <!-- 告警 + 组件健康 -->
      <section class="space-y-4">
        <div class="rounded-2xl border border-navy-600 bg-navy-850 p-4">
          <h2 class="text-sm font-semibold text-ink">实时告警</h2>
          <ul class="mt-3 space-y-2">
            <li v-for="a in alerts" :key="a.id" class="flex gap-2.5 rounded-lg bg-navy-900 px-3 py-2">
              <span
                class="mt-1.5 inline-block h-1.5 w-1.5 shrink-0 rounded-full"
                :style="{ backgroundColor: ALERT_LEVEL[a.level]?.color }"
              />
              <div class="min-w-0">
                <p class="truncate text-[11px] text-ink">{{ a.title }}</p>
                <p class="text-[10px] text-ink-dim">{{ a.node }} · {{ a.source }}</p>
              </div>
            </li>
          </ul>
        </div>

        <div class="rounded-2xl border border-navy-600 bg-navy-850 p-4">
          <h2 class="text-sm font-semibold text-ink">组件健康</h2>
          <ul class="mt-3 space-y-2">
            <li v-for="c in health?.components || []" :key="c.name" class="flex items-center gap-2.5">
              <span
                class="inline-block h-2 w-2 shrink-0 rounded-full"
                :style="{ backgroundColor: HEALTH_STATUS[c.status]?.color }"
              />
              <span class="flex-1 truncate text-[11px] text-ink-muted">{{ c.name }}</span>
              <span class="text-[10px]" :style="{ color: HEALTH_STATUS[c.status]?.color }">
                {{ HEALTH_STATUS[c.status]?.text }}
              </span>
            </li>
          </ul>
        </div>
      </section>
    </div>

    <!-- 节点水位 -->
    <section class="mt-5 rounded-2xl border border-navy-600 bg-navy-850 p-4">
      <div class="flex items-center justify-between">
        <h2 class="text-sm font-semibold text-ink">节点水位（{{ onlineNodes.length }} 台在线）</h2>
        <span class="text-[11px] text-ink-dim">数据来源 POST /node/search</span>
      </div>
      <div class="mt-4 grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <div
          v-for="n in nodes"
          :key="n.uuid"
          class="rounded-xl border border-navy-600 bg-navy-900 p-3.5"
          :class="n.status === 2 ? 'opacity-60' : ''"
        >
          <div class="flex items-center justify-between">
            <p class="truncate text-xs font-medium text-ink">{{ n.hostname }}</p>
            <span class="text-[10px]" :class="n.status === 1 ? 'text-ok' : 'text-danger'">
              {{ n.status === 1 ? '在线' : '失联' }}
            </span>
          </div>
          <p class="mt-0.5 font-mono text-[10px] text-ink-dim">{{ n.ip }} · {{ n.version }}</p>

          <div class="mt-3 space-y-1.5">
            <div class="flex items-center gap-2">
              <span class="w-9 text-[10px] text-ink-dim">CPU</span>
              <div class="h-1.5 flex-1 overflow-hidden rounded-full bg-navy-700">
                <div class="h-full rounded-full bg-brand-500" :style="{ width: (n.cpu || 0) + '%' }" />
              </div>
              <span class="w-8 text-right text-[10px] text-ink-muted">{{ n.cpu }}%</span>
            </div>
            <div class="flex items-center gap-2">
              <span class="w-9 text-[10px] text-ink-dim">DISK</span>
              <div class="h-1.5 flex-1 overflow-hidden rounded-full bg-navy-700">
                <div
                  class="h-full rounded-full"
                  :style="{
                    width: (n.disk || 0) + '%',
                    backgroundColor: n.disk > 85 ? '#ef4444' : n.disk > 70 ? '#f59e0b' : '#22c55e',
                  }"
                />
              </div>
              <span class="w-8 text-right text-[10px] text-ink-muted">{{ n.disk }}%</span>
            </div>
          </div>
          <p class="mt-2.5 text-[10px] text-ink-dim">承载任务 {{ n.job_count }} 个</p>
        </div>
      </div>
    </section>

    <!-- 底部滚动告警条 -->
    <footer class="mt-5 overflow-hidden rounded-xl border border-navy-600 bg-navy-850 px-4 py-2.5">
      <p class="truncate text-[11px] text-ink-muted">
        <span class="mr-2 text-warn">● 告警轮播</span>{{ ticker }}
      </p>
    </footer>
  </div>
</template>