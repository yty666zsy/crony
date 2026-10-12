<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { searchRuns, fetchRunSteps, findRun } from '@/api/run'
import { killRun } from '@/api/run'
import { openRunStream } from '@/api/stream'
import { USE_MOCK } from '@/api/client'
import { findAgentDiagnosis, runAgentDiagnosis } from '@/api/agent'
import StatusBadge from '@/components/StatusBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import DiagnosisPanel from '@/components/DiagnosisPanel.vue'
import MockBadge from '@/components/MockBadge.vue'
import { CONCURRENCY_POLICY, RUN_STATUS, TRIGGER_SOURCE, bizHint } from '@/utils/dict'
import { formatDuration, formatRelative, formatTime } from '@/utils/format'

/**
 * /runs —— 执行记录（Recent Tasks 的完整版）。
 *
 * job_run / job_run_step 列表：阶段 1 的规划接口（POST /job/run/search 等），数据来自 Mock。
 * 详情抽屉里的「AI 诊断」面板：**阶段 2 契约接口**（§3.1 读 / §3.2 触发），
 * 只读、只给建议（advisory_only=true），没有执行按钮。
 * 详情抽屉里的「实时进度」：**阶段 2 收尾的执行事实订阅**（§9，GET /job/run/stream）。
 * 它只消费推送、不做任何写操作；抽屉关闭即断开连接。
 */
const route = useRoute()

const loading = ref(false)
const rows = ref([])
const total = ref(0)

const query = reactive({
  job_name: '',
  run_id: '',
  node_uuid: '',
  trigger_source: '',
  status: '',
  page: 1,
  page_size: 10,
})

const detail = reactive({
  visible: false,
  loading: false,
  run: null,
  steps: [],
})

/** AI 诊断面板状态（契约 §3.1/§3.2） */
const diag = reactive({
  runId: '',
  sessionId: '',
  found: false,
  diagnosis: null,
  loading: false,
  busy: false,
  error: null,
})

const stepStatusText = {
  0: '待执行',
  1: '执行中',
  2: '成功',
  3: '失败',
  4: '已取消',
  5: '已跳过',
  6: '超时',
}

/**
 * 实时进度订阅状态（契约 §9）。
 * 语义边界写在界面上而不是注释里：
 *   - connected：连接是否活着（断线会自动退避重连）；
 *   - ended：服务端已发 end（终态或超时）——此时**不再**重连；
 *   - lastAt：最近一次收到事件的时间，用来显示"多久没动静了"（心跳可见性）。
 */
const live = reactive({
  on: false,
  connected: false,
  ended: false,
  endReason: '',
  endStatus: '',
  lastAt: 0,
  retry: 0,
  error: '',
  events: 0,
})

let liveHandle = null

function stopLive(reason = 'client') {
  if (liveHandle) {
    liveHandle.close()
    liveHandle = null
  }
  live.on = false
  live.connected = false
  if (reason !== 'silent') live.endReason = live.endReason || reason
}

/** 终态执行没必要订阅（服务端会立刻 end，不如直接读一次 REST）。 */
function isTerminal(status) {
  return [2, 3, 4, 5, 6].includes(Number(status))
}

function applyLiveEvent({ event, data }) {
  live.events += 1
  live.lastAt = Date.now()
  if (event === 'heartbeat') return
  if (event === 'snapshot') {
    // snapshot 是**重置**语义：清空既有步骤再应用。
    // job_id 模式下任务被再次触发时服务端会再发一次 snapshot，
    // 前端若只是按 step_index 合并，两条执行的步骤就会混在一条时间线上。
    if (data?.run) {
      detail.steps = []
      detail.run = { ...detail.run, ...data.run }
      const row = rows.value.find((r) => r.run_id === data.run.run_id)
      if (row) Object.assign(row, data.run)
    }
    return
  }
  if (event === 'run') {
    if (data?.run) {
      detail.run = { ...detail.run, ...data.run }
      const row = rows.value.find((r) => r.run_id === data.run.run_id)
      if (row) Object.assign(row, data.run)
    }
    return
  }
  if (event === 'step' && Array.isArray(data?.steps)) {
    for (const s of data.steps) {
      const idx = detail.steps.findIndex((x) => Number(x.step_index) === Number(s.step_index))
      if (idx === -1) detail.steps.push(s)
      else detail.steps[idx] = { ...detail.steps[idx], ...s }
    }
    detail.steps.sort((a, b) => Number(a.step_index) - Number(b.step_index))
    return
  }
  if (event === 'error') {
    live.error = data?.detail || '订阅出错'
  }
}

/**
 * 开始订阅。默认只在执行非终态时自动开启（终态执行直接读 REST 更省事），
 * 但允许手动开启——运维想看一条历史执行"怎么跑过来的"时，服务端会先给快照与既有步骤。
 */
function startLive({ auto = false } = {}) {
  if (!detail.run?.run_id) return
  if (auto && isTerminal(detail.run.status)) return
  stopLive('silent')
  live.on = true
  live.connected = false
  live.ended = false
  live.endReason = ''
  live.endStatus = ''
  live.error = ''
  live.events = 0
  live.lastAt = 0
  live.retry = 0

  liveHandle = openRunStream(detail.run.run_id, {
    intervalMs: 1000,
    maxSeconds: 600,
    onEvent: applyLiveEvent,
    onOpen: () => {
      live.connected = true
      live.error = ''
    },
    onRetry: (attempt) => {
      live.connected = false
      live.retry = attempt
    },
    onError: (err) => {
      live.connected = false
      live.error = err?.message || '订阅失败'
    },
    onClose: (data, reason) => {
      live.connected = false
      live.on = false
      liveHandle = null
      if (reason === 'end' || data?.reason) {
        live.ended = true
        live.endReason = data?.reason || reason || ''
        live.endStatus = data?.status || ''
      }
      // 结束时用一次 REST 读兜底，保证界面与库一致（推送是增量，可能有并发写入）
      refreshDetail()
    },
  })
}

/** 订阅结束后重新读一次 run + steps，避免增量推送与库产生任何偏差。 */
async function refreshDetail() {
  const runId = detail.run?.run_id
  if (!runId) return
  try {
    const fresh = await findRun(runId)
    if (fresh && fresh.run_id === runId) {
      detail.run = { ...detail.run, ...fresh }
      const row = rows.value.find((r) => r.run_id === runId)
      if (row) Object.assign(row, fresh)
    }
  } catch {
    /* 读不到就保留已有数据，不打扰用户 */
  }
  try {
    const res = await fetchRunSteps(runId)
    const list = res?.list || []
    if (list.length) detail.steps = list
  } catch {
    /* 同上 */
  }
}

const stepKindMeta = {
  shell: { text: 'Shell', color: '#06b6d4' },
  http: { text: 'HTTP', color: '#38bdf8' },
  sql: { text: 'SQL', color: '#3b82f6' },
  python: { text: 'Python', color: '#22c55e' },
  agent: { text: 'Agent 工具', color: '#f59e0b' },
}

const conditionText = {
  on_success: '成功时执行',
  on_failure: '失败时执行',
  always: '总是执行',
}

async function load() {
  loading.value = true
  try {
    const res = await searchRuns({
      job_name: query.job_name || undefined,
      run_id: query.run_id || undefined,
      node_uuid: query.node_uuid || undefined,
      trigger_source: query.trigger_source === '' ? undefined : query.trigger_source,
      status: query.status === '' ? undefined : query.status,
      page: query.page,
      page_size: query.page_size,
    })
    rows.value = res?.list || []
    total.value = res?.total || 0
  } finally {
    loading.value = false
  }
}

function onSearch() {
  query.page = 1
  load()
}

function reset() {
  query.job_name = ''
  query.run_id = ''
  query.node_uuid = ''
  query.trigger_source = ''
  query.status = ''
  onSearch()
}

/** 打开详情：先取 job_run，再取步骤时间线（两个接口，并行不了——步骤依赖 run_id） */
async function openDetail(row) {
  detail.visible = true
  detail.loading = true
  detail.run = row
  detail.steps = []
  stopLive('silent')
  openDiag(row.run_id)
  try {
    try {
      detail.run = await findRun(row.run_id)
    } catch {
      detail.run = row // findRun 不可用时用列表行兜底
    }
    const stepRes = await fetchRunSteps(row.run_id)
    detail.steps = stepRes?.list || []
  } finally {
    detail.loading = false
  }
  // 非终态执行自动开始订阅；终态执行不订阅（手动点"实时进度"仍可回放）
  startLive({ auto: true })
}

/* ---------------------- AI 诊断（契约 §3.1 读 / §3.2 触发） ---------------------- */

/**
 * 打开抽屉时先读已有诊断（§3.1）。
 * found=false 是**正常返回**（这条 run 还没诊断过），不是错误——所以不弹 toast，
 * 只在面板里显示"立即诊断"入口；只有真出错（1007/1010/1012…）才提示。
 */
async function openDiag(runId) {
  diag.runId = runId || ''
  diag.found = false
  diag.diagnosis = null
  diag.error = null
  diag.loading = true
  try {
    const res = await findAgentDiagnosis(runId)
    diag.found = !!res?.found
    diag.diagnosis = res?.diagnosis || null
    diag.sessionId = res?.session_id || res?.diagnosis?.session_id || ''
  } catch (err) {
    diag.error = bizHint(err)
  } finally {
    diag.loading = false
  }
}

/**
 * 触发诊断（§3.2）。
 * @param {boolean} force 面板二次确认后传 true，表示"重新诊断"（会新增会话并真实消耗 token）
 */
async function triggerDiagnosis(force = false) {
  if (!diag.runId) return
  diag.busy = true
  diag.error = null
  try {
    const res = await runAgentDiagnosis(diag.runId, { force })
    diag.found = true
    diag.diagnosis = res?.diagnosis || null
    diag.sessionId = res?.session_id || res?.diagnosis?.session_id || ''
    ElMessage.success(force ? '已重新诊断（新增一条会话，token 已消耗）' : '诊断完成')
    // 诊断会产生新的 Agent 会话，列表里的会话数列需要刷新
    load()
  } catch (err) {
    diag.error = bizHint(err)
    ElMessage.error(`${diag.error.title}（code=${diag.error.code}）`)
  } finally {
    diag.busy = false
  }
}

async function onKill(row) {
  await killRun({ job_id: row.job_id, node_uuid: row.node_uuid })
  ElMessage.warning('取消信号已下发（Mock；真实 /job/kill 路由当前被注释，阶段 1 恢复）')
  load()
}

const policy = computed(() => {
  try {
    return JSON.parse(detail.run?.policy_snapshot || '{}')
  } catch {
    return {}
  }
})

const stats = computed(() => {
  const list = rows.value
  return {
    success: list.filter((r) => r.status === 2).length,
    failed: list.filter((r) => r.status === 3).length,
    timeout: list.filter((r) => r.status === 6).length,
    skipped: list.filter((r) => r.status === 5).length,
  }
})

onMounted(async () => {
  await load()
  // 支持从 Dashboard 的 "详情" 直接定位到某次执行（?run_id=...）
  const runId = route.query.run_id
  if (runId) {
    query.run_id = String(runId)
    await load()
    const hit = rows.value.find((r) => r.run_id === runId)
    if (hit) openDetail(hit)
  }
})

// 离开页面必须断开订阅：否则留下一条对着数据库轮询的连接（后端还有 10 分钟寿命上限兜底）。
onBeforeUnmount(() => stopLive('silent'))
</script>

<template>
  <div class="space-y-4">
    <!-- 状态分布（四种终态都在 Mock 数据里出现） -->
    <section class="grid-cards">
      <div class="card card-pad">
        <p class="card-sub">本页成功</p>
        <p class="mt-1 text-xl font-semibold text-ok">{{ stats.success }}</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页失败</p>
        <p class="mt-1 text-xl font-semibold text-danger">{{ stats.failed }}</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页超时</p>
        <p class="mt-1 text-xl font-semibold text-warn">{{ stats.timeout }}</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页被跳过（并发 Skip）</p>
        <p class="mt-1 text-xl font-semibold text-warn">{{ stats.skipped }}</p>
      </div>
    </section>

    <section class="card overflow-hidden">
      <div class="panel-head flex-wrap">
        <div class="flex flex-wrap items-center gap-2">
          <el-input v-model="query.job_name" placeholder="任务名" clearable class="!w-[170px]" @keyup.enter="onSearch">
            <template #prefix><el-icon><Search /></el-icon></template>
          </el-input>
          <el-input v-model="query.run_id" placeholder="run_id / trace_id" clearable class="!w-[200px]" @keyup.enter="onSearch" />
          <el-select v-model="query.status" placeholder="执行状态" clearable class="!w-[140px]">
            <el-option v-for="(v, k) in RUN_STATUS" :key="k" :label="v.text" :value="Number(k)" />
          </el-select>
          <el-select v-model="query.trigger_source" placeholder="触发来源" clearable class="!w-[150px]">
            <el-option v-for="(v, k) in TRIGGER_SOURCE" :key="k" :label="v.text" :value="Number(k)" />
          </el-select>
          <el-button type="primary" @click="onSearch">查询</el-button>
          <el-button @click="reset">重置</el-button>
        </div>
        <el-button plain @click="load"><el-icon class="mr-1"><Refresh /></el-icon>刷新</el-button>
      </div>

      <el-table
        v-loading="loading"
        :data="rows"
        style="width: 100%"
        row-class-name="cursor-pointer"
        @row-click="openDetail"
      >
        <el-table-column label="run_id" min-width="180" fixed="left">
          <template #default="{ row }">
            <p class="truncate font-mono text-[11px] text-brand-300">{{ row.run_id }}</p>
            <p class="truncate text-[10px] text-ink-dim">trace {{ row.trace_id }}</p>
          </template>
        </el-table-column>

        <el-table-column label="任务名" min-width="180">
          <template #default="{ row }">
            <p class="truncate text-xs font-medium text-ink">{{ row.job_name }}</p>
            <p class="truncate text-[10px] text-ink-dim">#{{ row.job_id }} · {{ row.spec }}</p>
          </template>
        </el-table-column>

        <el-table-column label="目标" min-width="150">
          <template #default="{ row }">
            <p class="truncate text-xs text-ink-muted">{{ row.hostname }}</p>
            <p class="truncate font-mono text-[10px] text-ink-dim">{{ row.ip }}</p>
          </template>
        </el-table-column>

        <el-table-column label="状态" width="115">
          <template #default="{ row }">
            <StatusBadge kind="run" :value="row.status" />
          </template>
        </el-table-column>

        <el-table-column label="步骤" width="90" align="center">
          <template #default="{ row }">
            <span class="chip" :class="row.failed_steps ? 'border-danger/40 text-danger' : 'text-ok'">
              {{ row.success_steps }}/{{ row.step_count }}
            </span>
          </template>
        </el-table-column>

        <el-table-column label="耗时" width="95">
          <template #default="{ row }">
            <span class="text-xs text-ink-muted">{{ formatDuration(row.duration_ms) }}</span>
          </template>
        </el-table-column>

        <el-table-column label="触发方式" width="115">
          <template #default="{ row }">
            <span class="chip">{{ TRIGGER_SOURCE[row.trigger_source]?.text }}</span>
          </template>
        </el-table-column>

        <el-table-column label="开始时间" width="165">
          <template #default="{ row }">
            <p class="text-xs text-ink-muted">{{ formatTime(row.started_at || row.scheduled_time) }}</p>
            <p class="text-[10px] text-ink-dim">{{ formatRelative(row.started_at || row.scheduled_time) }}</p>
          </template>
        </el-table-column>

        <el-table-column label="操作" width="150" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click.stop="openDetail(row)">详情</el-button>
            <el-button
              link
              type="danger"
              size="small"
              :disabled="row.status !== 1 && row.status !== 0"
              @click.stop="onKill(row)"
            >
              取消
            </el-button>
          </template>
        </el-table-column>

        <template #empty>
          <EmptyState
            icon="DataLine"
            title="暂无执行记录"
            desc="任务被调度或手动触发后，这里会出现对应的 job_run；点击任意一行可查看步骤时间线。"
          />
        </template>
      </el-table>

      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-navy-600 px-5 py-3">
        <p class="text-[11px] text-ink-dim">
          共 {{ total }} 条 · 接口规划中（阶段 1），当前数据来自 Mock
        </p>
        <el-pagination
          v-model:current-page="query.page"
          v-model:page-size="query.page_size"
          :total="total"
          :page-sizes="[10, 20, 50]"
          layout="sizes, prev, pager, next"
          background
          @current-change="load"
          @size-change="onSearch"
        />
      </div>
    </section>

    <!-- ============ 执行详情抽屉 ============ -->
    <el-drawer
      v-model="detail.visible"
      :size="720"
      title="执行详情"
      @closed="stopLive('client')"
    >
      <div v-loading="detail.loading" class="scroll-area h-full px-5 py-4">
        <template v-if="detail.run">
          <!-- job_run 概要 -->
          <div class="card card-pad">
            <div class="flex flex-wrap items-start justify-between gap-3">
              <div>
                <p class="text-sm font-semibold text-ink">{{ detail.run.job_name }}</p>
                <p class="mt-0.5 font-mono text-[11px] text-brand-300">{{ detail.run.run_id }}</p>
              </div>
              <StatusBadge kind="run" :value="detail.run.status" />
            </div>

            <el-descriptions :column="3" border size="small" class="mt-4">
              <el-descriptions-item label="任务 ID">#{{ detail.run.job_id }}</el-descriptions-item>
              <el-descriptions-item label="触发来源">
                {{ TRIGGER_SOURCE[detail.run.trigger_source]?.text }}
              </el-descriptions-item>
              <el-descriptions-item label="触发者">{{ detail.run.triggered_by }}</el-descriptions-item>

              <el-descriptions-item label="目标节点">{{ detail.run.hostname }}</el-descriptions-item>
              <el-descriptions-item label="节点 IP">
                <span class="font-mono">{{ detail.run.ip }}</span>
              </el-descriptions-item>
              <el-descriptions-item label="尝试次数">
                第 {{ detail.run.attempt }} 次
              </el-descriptions-item>

              <el-descriptions-item label="调度时间">
                {{ formatTime(detail.run.scheduled_time) }}
              </el-descriptions-item>
              <el-descriptions-item label="开始时间">
                {{ formatTime(detail.run.started_at) }}
              </el-descriptions-item>
              <el-descriptions-item label="结束时间">
                {{ formatTime(detail.run.finished_at) }}
              </el-descriptions-item>

              <el-descriptions-item label="耗时">
                {{ formatDuration(detail.run.duration_ms) }}
              </el-descriptions-item>
              <el-descriptions-item label="退出码">
                <span
                  class="font-mono"
                  :class="detail.run.exit_code ? 'text-danger' : 'text-ok'"
                >
                  {{ detail.run.exit_code }}
                </span>
              </el-descriptions-item>
              <el-descriptions-item label="步骤">
                {{ detail.run.success_steps }} 成功 / {{ detail.run.failed_steps }} 失败 /
                共 {{ detail.run.step_count }}
              </el-descriptions-item>

              <el-descriptions-item label="cron 表达式">
                <span class="font-mono">{{ detail.run.spec }}</span>
              </el-descriptions-item>
              <el-descriptions-item label="trace_id">
                <span class="font-mono text-[11px]">{{ detail.run.trace_id }}</span>
              </el-descriptions-item>
              <el-descriptions-item label="重试次数">
                {{ detail.run.retry_count }}
              </el-descriptions-item>
            </el-descriptions>

            <div class="mt-4 space-y-2">
              <div>
                <p class="section-label">执行命令</p>
                <pre class="mt-1 overflow-x-auto rounded-lg bg-navy-900 p-3 font-mono text-[11px] leading-5 text-ink-muted">{{ detail.run.command }}</pre>
              </div>

              <div v-if="detail.run.error_msg">
                <p class="section-label text-danger">错误信息</p>
                <p class="mt-1 rounded-lg border border-danger/30 bg-danger/10 p-3 text-[11px] leading-5 text-danger">
                  {{ detail.run.error_msg }}
                </p>
              </div>

              <div v-if="detail.run.output">
                <p class="section-label">输出片段</p>
                <pre class="mt-1 overflow-x-auto rounded-lg bg-navy-900 p-3 font-mono text-[11px] leading-5 text-ink-muted">{{ detail.run.output }}</pre>
              </div>

              <div v-if="Object.keys(policy).length">
                <p class="section-label">策略快照（policy_snapshot）</p>
                <div class="mt-1 flex flex-wrap gap-1.5">
                  <span v-if="policy.concurrency" class="chip border-warn/40 text-warn">
                    并发：{{ CONCURRENCY_POLICY[policy.concurrency]?.text || policy.concurrency }}
                  </span>
                  <span class="chip">重试 {{ policy.retry_times ?? 0 }} 次</span>
                  <span class="chip">间隔 {{ policy.retry_interval ?? 0 }}s</span>
                  <span class="chip">超时 {{ policy.timeout ?? 0 }}s</span>
                </div>
              </div>
            </div>
          </div>

          <!-- ============ AI 诊断（契约 §3.1 读 + §3.2 触发） ============ -->
          <div class="card mt-4 card-pad">
            <DiagnosisPanel
              :diagnosis="diag.diagnosis"
              :found="diag.found"
              :loading="diag.loading"
              :busy="diag.busy"
              :session-id="diag.sessionId"
              :error="diag.error"
              @run="triggerDiagnosis"
            />
            <p class="mt-3 border-t border-navy-600 pt-3 text-[10px] leading-5 text-ink-dim">
              数据来源：POST /agent/diagnosis/find（§3.1，找不到时 found=false）与
              POST /agent/diagnosis/run（§3.2，会真实消耗模型 token）。
              Agent 的完整工具调用轨迹在
              <router-link v-if="diag.sessionId" :to="`/agents/${diag.sessionId}`" class="text-brand-300 hover:underline">
                会话详情
              </router-link>
              <span v-else>会话详情</span>
              里可逐条回放。
            </p>
          </div>

          <!-- 实时进度（契约 §9：GET /job/run/stream，SSE） -->
          <div class="card mt-4 card-pad">
            <div class="flex flex-wrap items-start justify-between gap-3">
              <div class="min-w-0">
                <p class="card-title flex items-center gap-2">
                  实时进度
                  <MockBadge v-if="USE_MOCK" text="Mock 模拟回放" />
                </p>
                <p class="card-sub mt-0.5">
                  服务端按 1s 对比执行事实并推送变化（首帧全量快照 + 增量步骤）；
                  终态后自动 end，不做历史事件重放。
                </p>
              </div>
              <div class="flex shrink-0 items-center gap-2">
                <span
                  class="chip"
                  :class="live.connected ? 'border-ok/40 text-ok' : 'text-ink-dim'"
                  :title="live.lastAt ? `最近事件：${formatTime(Math.floor(live.lastAt / 1000))}` : '尚无事件'"
                >
                  <span
                    class="mr-1.5 inline-block h-1.5 w-1.5 rounded-full align-middle"
                    :style="{ backgroundColor: live.connected ? '#22c55e' : '#7c8aa8' }"
                  />
                  {{ live.connected ? '已连接' : live.on ? '重连中…' : '未连接' }}
                </span>
                <el-button
                  size="small"
                  :type="live.on ? 'default' : 'primary'"
                  @click="live.on ? stopLive('client') : startLive()"
                >
                  {{ live.on ? '停止' : '开始订阅' }}
                </el-button>
              </div>
            </div>

            <div class="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-4">
              <div class="rounded-md bg-navy-900 px-2.5 py-1.5">
                <p class="text-[10px] text-ink-dim">收到事件</p>
                <p class="font-mono text-[11px] text-ink">{{ live.events }}</p>
              </div>
              <div class="rounded-md bg-navy-900 px-2.5 py-1.5">
                <p class="text-[10px] text-ink-dim">最近事件</p>
                <p class="font-mono text-[11px] text-ink">
                  {{ live.lastAt ? formatRelative(Math.floor(live.lastAt / 1000)) : '—' }}
                </p>
              </div>
              <div class="rounded-md bg-navy-900 px-2.5 py-1.5">
                <p class="text-[10px] text-ink-dim">重连次数</p>
                <p class="font-mono text-[11px]" :class="live.retry ? 'text-warn' : 'text-ink'">
                  {{ live.retry }}
                </p>
              </div>
              <div class="rounded-md bg-navy-900 px-2.5 py-1.5">
                <p class="text-[10px] text-ink-dim">结束原因</p>
                <p class="font-mono text-[11px] text-ink">
                  {{ live.ended ? live.endReason || 'end' : '—' }}
                </p>
              </div>
            </div>

            <p
              v-if="live.ended"
              class="mt-2 rounded-lg border border-navy-600 bg-navy-900 px-3 py-2 text-[11px] leading-5 text-ink-muted"
            >
              订阅已结束（reason={{ live.endReason || 'end' }}
              <span v-if="live.endStatus">，status={{ live.endStatus }}</span>）。
              增量推送结束后已用一次 REST 读取校正界面，故此处与库内一致。
            </p>
            <p
              v-if="live.error"
              class="mt-2 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-[11px] leading-5 text-danger"
            >
              {{ live.error }}
            </p>
            <p class="mt-2 border-t border-navy-600 pt-2 text-[10px] leading-5 text-ink-dim">
              鉴权：EventSource 不能带 Authorization 头，因此前端用 fetch 流式读取（src/api/stream.js）。
              未验证项：真实浏览器下的断线重连与心跳可见性尚未人工点验（Mock 模式为模拟回放，不是实时能力）。
            </p>
          </div>

          <!-- 步骤时间线 -->
          <div class="card mt-4 card-pad">
            <div class="mb-4 flex items-center justify-between">
              <div>
                <p class="card-title">步骤时间线（job_run_step）</p>
                <p class="card-sub mt-0.5">
                  每一步含：名称、类型、状态、退出码、耗时、重试次数、输出片段、结构化产物
                </p>
              </div>
              <span class="chip">{{ detail.steps.length }} 步</span>
            </div>

            <el-timeline v-if="detail.steps.length">
              <el-timeline-item
                v-for="s in detail.steps"
                :key="s.id"
                :timestamp="`step ${s.step_index} · ${formatTime(s.started_at)}`"
                placement="top"
                :color="RUN_STATUS[s.status]?.color"
                :hollow="s.status === 0"
              >
                <div class="card border-navy-600 bg-navy-800 p-3.5">
                  <div class="flex flex-wrap items-start justify-between gap-2">
                    <div class="min-w-0">
                      <div class="flex flex-wrap items-center gap-2">
                        <span class="text-xs font-medium text-ink">{{ s.name }}</span>
                        <span
                          class="chip"
                          :style="{
                            color: stepKindMeta[s.kind]?.color,
                            borderColor: (stepKindMeta[s.kind]?.color || '#333') + '55',
                          }"
                        >
                          {{ stepKindMeta[s.kind]?.text || s.kind }}
                        </span>
                        <span class="chip text-ink-dim">{{ conditionText[s.condition] || s.condition }}</span>
                      </div>
                    </div>
                    <span
                      class="shrink-0 rounded-md px-2 py-0.5 text-[11px] font-medium"
                      :style="{
                        backgroundColor: (RUN_STATUS[s.status]?.color || '#7c8aa8') + '1f',
                        color: RUN_STATUS[s.status]?.color,
                      }"
                    >
                      {{ stepStatusText[s.status] }}
                    </span>
                  </div>

                  <div class="mt-2.5 grid grid-cols-2 gap-2 sm:grid-cols-4">
                    <div class="rounded-md bg-navy-900 px-2.5 py-1.5">
                      <p class="text-[10px] text-ink-dim">退出码</p>
                      <p class="font-mono text-[11px]" :class="s.exit_code ? 'text-danger' : 'text-ok'">
                        {{ s.exit_code }}
                      </p>
                    </div>
                    <div class="rounded-md bg-navy-900 px-2.5 py-1.5">
                      <p class="text-[10px] text-ink-dim">耗时</p>
                      <p class="text-[11px] text-ink-muted">{{ formatDuration(s.duration_ms) }}</p>
                    </div>
                    <div class="rounded-md bg-navy-900 px-2.5 py-1.5">
                      <p class="text-[10px] text-ink-dim">重试次数</p>
                      <p class="text-[11px]" :class="s.retry_count ? 'text-warn' : 'text-ink-muted'">
                        {{ s.retry_count }}
                      </p>
                    </div>
                    <div class="rounded-md bg-navy-900 px-2.5 py-1.5">
                      <p class="text-[10px] text-ink-dim">开始</p>
                      <p class="text-[11px] text-ink-muted">{{ formatTime(s.started_at) }}</p>
                    </div>
                  </div>

                  <div class="mt-2.5">
                    <p class="text-[10px] text-ink-dim">命令</p>
                    <pre class="mt-1 overflow-x-auto rounded-md bg-navy-900 p-2.5 font-mono text-[10px] leading-5 text-ink-muted">{{ s.command }}</pre>
                  </div>

                  <div v-if="s.output" class="mt-2.5">
                    <p class="text-[10px] text-ink-dim">输出片段</p>
                    <pre class="mt-1 max-h-40 overflow-auto whitespace-pre-wrap rounded-md bg-navy-900 p-2.5 font-mono text-[10px] leading-5 text-ink-muted">{{ s.output }}</pre>
                  </div>

                  <div v-if="s.error_msg" class="mt-2.5">
                    <p class="text-[10px] text-danger">错误/退避说明</p>
                    <p class="mt-1 rounded-md border border-danger/30 bg-danger/10 p-2.5 text-[10px] leading-5 text-danger">
                      {{ s.error_msg }}
                    </p>
                  </div>

                  <div v-if="s.artifacts && s.artifacts !== '{}'" class="mt-2.5">
                    <p class="text-[10px] text-ink-dim">结构化产物（artifacts）</p>
                    <pre class="mt-1 overflow-x-auto rounded-md bg-navy-900 p-2.5 font-mono text-[10px] leading-5 text-brand-300">{{ s.artifacts }}</pre>
                  </div>
                </div>
              </el-timeline-item>
            </el-timeline>

            <EmptyState
              v-else
              compact
              icon="Remove"
              title="本次执行没有产生步骤"
              desc="状态为「已跳过」的 run 由并发策略 Skip 直接跳过，不会生成 job_run_step 记录。"
            />
          </div>
        </template>
      </div>
    </el-drawer>
  </div>
</template>