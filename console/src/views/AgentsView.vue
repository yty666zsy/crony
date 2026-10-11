<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { listAgentTools, runAgentDiagnosis, searchAgentSessions } from '@/api/agent'
import StatusBadge from '@/components/StatusBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import MockBadge from '@/components/MockBadge.vue'
import DiagnosisPanel from '@/components/DiagnosisPanel.vue'
import {
  AGENT_SESSION_KIND,
  AGENT_SESSION_STATUS,
  AGENT_STOP_REASON,
  bizHint,
} from '@/utils/dict'
import { formatDuration, formatNumber, formatRelative, formatTime } from '@/utils/format'

/**
 * /agents —— Agent 会话列表 + 当前能力边界（阶段 2 契约 §1 / §4 / §3.2）。
 *
 * 与阶段 1 的这页最本质的区别：
 *   阶段 1 展示的是 6 个"Agent 实体"（名字、角色、今日运行次数…），而 Go 侧**没有** Agent 实体模型，
 *   那些数字全是编的。阶段 2 的内核里 Agent 是**受预算约束的会话**：会消耗 token、调只读工具、
 *   产出可回溯的轨迹。所以这页改成：上面写清"现在到底能做什么"（§4 真实工具与配额），
 *   下面列出真实存在的会话记录（§1），并允许就地发起一次诊断（§3.2）。
 *
 * 三条刻意的自我约束：
 *   1) 不写"支持 XX 种自治运维能力"这类话——能力边界只列后端**真实注册**的只读工具与硬配额；
 *   2) provider=offline / model="" 是真实情况（离线 FakeProvider 从不上报 model），一律显示 "—" 而不是编一个模型名；
 *   3) 诊断结果只读只建议（advisory_only=true），页面上没有任何执行/应用按钮。
 */

/* ------------------------------ §1 会话列表 ------------------------------ */

const loading = ref(false)
const rows = ref([])
const total = ref(0)

const query = reactive({
  job_id: '',
  run_id: '',
  kind: '',
  status: '',
  page: 1,
  page_size: 10,
})

async function load() {
  loading.value = true
  try {
    const res = await searchAgentSessions({
      job_id: query.job_id || undefined,
      run_id: query.run_id || undefined,
      kind: query.kind || undefined,
      status: query.status || undefined,
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
  query.job_id = ''
  query.run_id = ''
  query.kind = ''
  query.status = ''
  onSearch()
}

/** 本页概览：只统计"当前页"，避免让人误以为是全局统计（阶段 1 的教训：数字口径必须写清） */
const pageStats = computed(() => {
  const list = rows.value
  return {
    tokens: list.reduce((s, r) => s + (r.total_tokens || 0), 0),
    toolCalls: list.reduce((s, r) => s + (r.tool_calls || 0), 0),
    warned: list.filter((r) => (r.warn_count || 0) > 0).length,
    diagnose: list.filter((r) => r.kind === 'diagnose').length,
  }
})

/* --------------------------- §4 当前能力边界 --------------------------- */

const toolLoading = ref(false)
const tools = ref([])
const policy = ref(null)
const toolError = ref(null)

async function loadTools() {
  toolLoading.value = true
  toolError.value = null
  try {
    const res = await listAgentTools()
    tools.value = res?.tools || []
    policy.value = res?.policy || null
  } catch (err) {
    toolError.value = bizHint(err)
    tools.value = []
    policy.value = null
  } finally {
    toolLoading.value = false
  }
}

const allowHostsText = computed(() => {
  const hosts = policy.value?.allow_hosts || []
  return hosts.length ? hosts.join('、') : '（空：http_get 会被直接拒绝，不做域名放行）'
})

const shellPathsText = computed(() => {
  const paths = policy.value?.shell_read_paths || []
  return paths.length ? paths.join('、') : '（空：shell_readonly 的路径白名单为空）'
})

/* ------------------------ §3.2 立即诊断（就地触发） ------------------------ */

const diag = reactive({
  runId: '',
  sessionId: '',
  found: false,
  diagnosis: null,
  loading: false,
  busy: false,
  error: null,
})

async function diagnose(force = false) {
  const runId = diag.runId.trim()
  if (!runId) {
    ElMessage.warning('请先填写 run_id（在 /runs 页面复制）')
    return
  }
  diag.busy = true
  diag.error = null
  if (!force) {
    // force=false：清掉旧结果，重新读一次（已有诊断会原样返回，不消耗 token）
    diag.found = false
    diag.diagnosis = null
    diag.sessionId = ''
  }
  try {
    const res = await runAgentDiagnosis(runId, { force })
    diag.found = !!res?.found
    diag.diagnosis = res?.diagnosis || null
    diag.sessionId = res?.session_id || res?.diagnosis?.session_id || ''
    ElMessage.success(force ? '已重新诊断（新增一条会话，token 已消耗）' : '诊断完成（只读工具采集 + 模型结论，仅供参考）')
    load()
  } catch (err) {
    diag.error = bizHint(err)
    ElMessage.error(`${diag.error.title}（code=${diag.error.code}）`)
  } finally {
    diag.busy = false
  }
}

function resetDiag() {
  diag.runId = ''
  diag.sessionId = ''
  diag.found = false
  diag.diagnosis = null
  diag.error = null
}

function openRun(row) {
  diag.runId = row.run_id || ''
  diag.sessionId = ''
  diag.found = false
  diag.diagnosis = null
  diag.error = null
  diagnose()
}

onMounted(() => {
  load()
  loadTools()
})
</script>

<template>
  <div class="space-y-4">
    <!-- ==================== 当前能力边界（契约 §4 真实数据） ==================== -->
    <section class="card card-pad">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 class="card-title">当前能力边界</h2>
          <p class="card-sub mt-0.5">
            直接来自 POST /agent/tool/list（契约 §4）：下面就是**现在真实注册**的只读工具与硬配额，
            没有列出来的能力就是没有。
          </p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <MockBadge text="Mock 工具清单" />
          <span class="chip border-ok/40 text-ok">阶段 2：全部工具只读</span>
          <el-button size="small" plain :loading="toolLoading" @click="loadTools">
            <el-icon class="mr-1" :size="13"><Refresh /></el-icon>刷新
          </el-button>
        </div>
      </div>

      <p v-if="toolError" class="mt-3 rounded-lg border border-danger/50 bg-danger/10 p-3 text-[11px] text-danger">
        读取工具清单失败（code={{ toolError.code }}）：{{ toolError.title }} —— {{ toolError.desc }}
      </p>

      <div v-else class="mt-4 grid gap-3 lg:grid-cols-[1.6fr_1fr]">
        <div class="rounded-lg border border-navy-600">
          <el-table v-loading="toolLoading" :data="tools" size="small" style="width: 100%">
            <el-table-column label="工具名" min-width="150">
              <template #default="{ row }">
                <p class="font-mono text-[11px] text-brand-300">{{ row.name }}</p>
                <p class="mt-0.5 text-[10px] text-ink-dim">超时 {{ formatDuration(row.timeout_ms) }}</p>
              </template>
            </el-table-column>
            <el-table-column label="说明（后端原文）" min-width="280">
              <template #default="{ row }">
                <p class="text-[11px] leading-5 text-ink-muted">{{ row.description }}</p>
              </template>
            </el-table-column>
            <el-table-column label="只读" width="80" align="center">
              <template #default="{ row }">
                <span class="chip" :class="row.readonly ? 'border-ok/40 text-ok' : 'border-danger/40 text-danger'">
                  {{ row.readonly ? '是' : '否' }}
                </span>
              </template>
            </el-table-column>
            <template #empty>
              <EmptyState
                compact
                icon="Tools"
                title="没有可用工具"
                desc="后端返回的工具清单为空。NewReadonlyRegistry 会静默跳过 source 为 nil 的工具，因此实际可用工具可能少于代码里定义的数量。"
              />
            </template>
          </el-table>
        </div>

        <div class="space-y-2 rounded-lg border border-navy-600 bg-navy-900 p-3">
          <p class="section-label">硬配额（policy）</p>
          <template v-if="policy">
            <div class="flex flex-wrap gap-1.5">
              <span class="chip">单会话最多 {{ policy.max_calls_per_session }} 次工具调用</span>
              <span class="chip">单工具最多 {{ policy.max_calls_per_tool }} 次</span>
              <span class="chip">单次输出 ≤ {{ formatNumber(policy.max_output_bytes) }} 字节</span>
              <span class="chip">SQL 最多返回 {{ policy.sql_max_rows }} 行</span>
            </div>
            <div class="space-y-1 pt-1 text-[10px] leading-5 text-ink-dim">
              <p>允许访问的主机（allow_hosts）：<span class="text-ink-muted">{{ allowHostsText }}</span></p>
              <p>shell_readonly 可读路径：<span class="text-ink-muted">{{ shellPathsText }}</span></p>
            </div>
            <p class="border-t border-navy-600 pt-2 text-[10px] leading-5 text-ink-dim">
              配额是**硬上限**：达到上限时内核会停止调用工具并以 stop_reason 记录（tool_budget / llm_budget），
              不是无限重试。阶段 2 不存在变更类工具，因此也不需要审批流。
            </p>
          </template>
          <p v-else class="text-[11px] text-ink-dim">配额未取到（接口失败或后端未返回 policy）。</p>
        </div>
      </div>
    </section>

    <!-- ==================== 立即诊断（契约 §3.2） ==================== -->
    <section class="card card-pad">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 class="card-title">立即诊断一次执行</h2>
          <p class="card-sub mt-0.5">
            填入 /runs 里的 run_id（job_id 也可），Agent 会用上表中的只读工具采集证据并给出结论。
            force=false：同一条 run 已有诊断时直接返回那一条，不重复消耗 token。
          </p>
        </div>
        <MockBadge text="Mock 诊断数据" />
      </div>

      <div class="mt-3 flex flex-wrap items-center gap-2">
        <el-input
          v-model="diag.runId"
          placeholder="run_id（例：j2-t3-…，或直接填 job_id）"
          clearable
          class="!w-[320px]"
          @keyup.enter="diagnose"
        />
        <el-button type="primary" :loading="diag.busy" @click="diagnose">
          <el-icon class="mr-1" :size="14"><MagicStick /></el-icon>立即诊断
        </el-button>
        <el-button plain @click="resetDiag">清空</el-button>
      </div>

      <div v-if="diag.found || diag.error || diag.busy" class="mt-4 border-t border-navy-600 pt-3">
        <DiagnosisPanel
          :diagnosis="diag.diagnosis"
          :found="diag.found"
          :loading="false"
          :busy="diag.busy"
          :session-id="diag.sessionId"
          :error="diag.error"
          @run="(force) => diagnose(force)"
        />
      </div>
      <p v-else class="mt-3 text-[10px] leading-5 text-ink-dim">
        提示：也可以在 /runs 里点任意一行，在详情抽屉里直接诊断——那里的 run_id 一定是真实的。
      </p>
    </section>

    <!-- ==================== 会话列表（契约 §1） ==================== -->
    <section class="grid-cards">
      <div class="card card-pad">
        <p class="card-sub">本页会话</p>
        <p class="mt-1 text-xl font-semibold text-ink">{{ rows.length }}</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页 token 合计</p>
        <p class="mt-1 text-xl font-semibold text-brand-300">{{ formatNumber(pageStats.tokens) }}</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页工具调用</p>
        <p class="mt-1 text-xl font-semibold text-warn">{{ pageStats.toolCalls }}</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页有警告的会话</p>
        <p class="mt-1 text-xl font-semibold" :class="pageStats.warned ? 'text-danger' : 'text-ok'">
          {{ pageStats.warned }}
        </p>
      </div>
    </section>

    <section class="card overflow-hidden">
      <div class="panel-head flex-wrap">
        <div class="flex flex-wrap items-center gap-2">
          <el-input v-model="query.run_id" placeholder="run_id" clearable class="!w-[190px]" @keyup.enter="onSearch">
            <template #prefix><el-icon><Search /></el-icon></template>
          </el-input>
          <el-input v-model="query.job_id" placeholder="job_id" clearable class="!w-[120px]" @keyup.enter="onSearch" />
          <el-select v-model="query.kind" placeholder="会话类型" clearable class="!w-[150px]">
            <el-option v-for="k in AGENT_SESSION_KIND" :key="k.key" :label="k.text" :value="k.key" />
          </el-select>
          <el-select v-model="query.status" placeholder="会话状态" clearable class="!w-[130px]">
            <el-option v-for="k in AGENT_SESSION_STATUS" :key="k.key" :label="k.text" :value="k.key" />
          </el-select>
          <el-button type="primary" @click="onSearch">查询</el-button>
          <el-button @click="reset">重置</el-button>
        </div>
        <div class="flex items-center gap-2">
          <MockBadge text="Mock 会话数据" />
          <el-button plain @click="load"><el-icon class="mr-1"><Refresh /></el-icon>刷新</el-button>
        </div>
      </div>

      <el-table v-loading="loading" :data="rows" style="width: 100%">
        <el-table-column label="会话" min-width="230" fixed="left">
          <template #default="{ row }">
            <router-link :to="`/agents/${row.session_id}`" class="font-mono text-[11px] text-brand-300 hover:underline">
              {{ row.session_id }}
            </router-link>
            <p class="mt-0.5 truncate text-[10px] text-ink-dim">{{ row.goal || '（无目标描述）' }}</p>
          </template>
        </el-table-column>

        <el-table-column label="类型 / 状态" width="150">
          <template #default="{ row }">
            <div class="flex flex-col gap-1">
              <span
                class="chip w-fit"
                :style="{
                  color: AGENT_SESSION_KIND[row.kind]?.color || '#7c8aa8',
                  backgroundColor: (AGENT_SESSION_KIND[row.kind]?.color || '#7c8aa8') + '1f',
                }"
              >
                {{ AGENT_SESSION_KIND[row.kind]?.text || row.kind }}
              </span>
              <StatusBadge kind="session" :value="row.status" size="small" />
            </div>
          </template>
        </el-table-column>

        <el-table-column label="对象" min-width="130">
          <template #default="{ row }">
            <p class="text-[11px] text-ink-muted">
              <span v-if="row.job_id">#{{ row.job_id }}</span>
              <span v-else class="text-ink-dim">—</span>
            </p>
            <p class="truncate font-mono text-[10px] text-ink-dim">{{ row.run_id || row.node_uuid || '—' }}</p>
          </template>
        </el-table-column>

        <el-table-column label="provider / 模型" min-width="150">
          <template #default="{ row }">
            <p class="font-mono text-[11px] text-ink-muted">{{ row.provider || '—' }}</p>
            <!-- offline FakeProvider 从不上报 model，这里就显示 "—"，绝不编造模型名 -->
            <p class="truncate font-mono text-[10px] text-ink-dim">{{ row.model || '—（provider 未上报）' }}</p>
          </template>
        </el-table-column>

        <el-table-column label="prompt" width="110">
          <template #default="{ row }">
            <span class="font-mono text-[10px] text-ink-dim">{{ row.prompt_version || '—' }}</span>
          </template>
        </el-table-column>

        <el-table-column label="模型/工具调用" width="120" align="center">
          <template #default="{ row }">
            <p class="text-[11px] text-ink-muted">{{ row.model_calls }} / {{ row.tool_calls }}</p>
            <p class="text-[10px] text-ink-dim">tokens {{ formatNumber(row.total_tokens) }}</p>
          </template>
        </el-table-column>

        <el-table-column label="耗时" width="95">
          <template #default="{ row }">
            <span class="text-[11px] text-ink-muted">{{ formatDuration(row.duration_ms) }}</span>
          </template>
        </el-table-column>

        <el-table-column label="警告/证据" width="105" align="center">
          <template #default="{ row }">
            <span class="chip" :class="row.warn_count ? 'border-danger/40 text-danger' : 'text-ink-dim'">
              警告 {{ row.warn_count }}
            </span>
            <p class="mt-0.5 text-[10px] text-ink-dim">证据 {{ row.evidence_count }}</p>
          </template>
        </el-table-column>

        <el-table-column label="终止原因" min-width="130">
          <template #default="{ row }">
            <p class="text-[11px]" :style="{ color: AGENT_STOP_REASON[row.stop_reason]?.color || '#7c8aa8' }">
              {{ row.stop_reason_text || AGENT_STOP_REASON[row.stop_reason]?.text || row.stop_reason || '—' }}
            </p>
          </template>
        </el-table-column>

        <el-table-column label="创建时间" width="150">
          <template #default="{ row }">
            <p class="text-[11px] text-ink-muted">{{ formatTime(row.created_ms || row.created) }}</p>
            <p class="text-[10px] text-ink-dim">{{ formatRelative(row.created_ms || row.created) }}</p>
          </template>
        </el-table-column>

        <el-table-column label="操作" width="120" fixed="right">
          <template #default="{ row }">
            <router-link :to="`/agents/${row.session_id}`" class="text-[11px] text-brand-300 hover:underline">
              看轨迹
            </router-link>
            <el-button
              v-if="row.kind === 'diagnose' && row.run_id"
              link
              type="primary"
              size="small"
              class="ml-2"
              @click="openRun(row)"
            >
              诊断
            </el-button>
          </template>
        </el-table-column>

        <template #empty>
          <EmptyState
            icon="MagicStick"
            title="暂无 Agent 会话"
            desc="会话由诊断（§3.2）、自然语言建任务（§5.1）或任务内嵌 Agent 产生；也可以在上面直接发起一次诊断。"
          />
        </template>
      </el-table>

      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-navy-600 px-5 py-3">
        <p class="text-[11px] text-ink-dim">
          共 {{ total }} 条 · 数据来源：POST /agent/session/search（契约 §1，阶段 2 契约接口）
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
  </div>
</template>