<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { searchAuditLogs } from '@/api/audit'
import StatusBadge from '@/components/StatusBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import { formatRelative, formatTime } from '@/utils/format'

/**
 * /audit —— 审计追溯。
 *
 * 数据来自后端**真实**接口 `POST /audit/search`，它合并两个事实源：
 *   1. `agent_tool_audit` —— 工具网关每次调用一行（含被拒 / 转审批，append-only）；
 *   2. `approval_request` —— 写动作审批单（谁批的、结果如何）。
 * 每行带 `source` 标注来源；响应的 `notice` 写明合并窗口（每个来源最近 N 行）。
 *
 * 注意：平台**没有**独立的"用户登录审计"表，因此这里不显示登录类事件（不编造）。
 */

const loading = ref(false)
const rows = ref([])
const total = ref(0)
// notice / source_window：后端如实告知"合并了哪些来源、窗口多大"。
const notice = ref('')
const sourceWindow = ref(0)
const detail = reactive({ visible: false, row: null })

const query = reactive({
  operator: '',
  action: '',
  target: '',
  result: '',
  ip: '',
  page: 1,
  page_size: 12,
})

const operatorTypeMeta = {
  user: { text: '用户', color: '#38bdf8' },
  system: { text: '系统', color: '#7c8aa8' },
  agent: { text: 'AI Agent', color: '#3b82f6' },
  anonymous: { text: '匿名', color: '#ef4444' },
}

/** 审计行来源 → 中文（与后端 `source` 字段一一对应）。 */
const sourceMeta = {
  tool_audit: { text: '工具调用', color: '#3b82f6' },
  approval: { text: '写动作审批', color: '#f59e0b' },
}

/**
 * 动作筛选可选项。
 *
 * 后端对 `action` 是**大小写不敏感的子串匹配**（同时匹配 `action` 与 `action_text`），
 * 所以这里给"前缀 / 关键词"而不是穷举值：`tool.` 命中全部工具调用，
 * `approval.` 命中全部审批单，中文关键词命中 action_text。
 */
const actionGroups = [
  { label: '工具调用（前缀）', items: ['tool.'] },
  { label: '写动作审批（前缀）', items: ['approval.'] },
  { label: '决策关键词', items: ['已执行', '被拒绝', '转人工审批', '审批后执行'] },
]

async function load() {
  loading.value = true
  try {
    const res = await searchAuditLogs({
      operator: query.operator || undefined,
      action: query.action || undefined,
      target: query.target || undefined,
      result: query.result || undefined,
      ip: query.ip || undefined,
      page: query.page,
      page_size: query.page_size,
    })
    rows.value = res?.list || []
    total.value = res?.total || 0
    notice.value = res?.notice || ''
    sourceWindow.value = Number(res?.source_window) || 0
  } catch {
    // 后端不可达时展示空结果（不在页面里反复弹错误提示）。
    rows.value = []
    total.value = 0
    notice.value = ''
    sourceWindow.value = 0
  } finally {
    loading.value = false
  }
}

function onSearch() {
  query.page = 1
  load()
}

function reset() {
  query.operator = ''
  query.action = ''
  query.target = ''
  query.result = ''
  query.ip = ''
  onSearch()
}

function openDetail(row) {
  detail.row = row
  detail.visible = true
}

const eventJson = computed(() => (detail.row ? JSON.stringify(detail.row, null, 2) : ''))

const agentEvents = computed(() => rows.value.filter((r) => r.operator_type === 'agent').length)
// 异常结果 = 后端结果枚举里**非成功/非待审**的那些（denied 是"没动手被拦"，也算异常）。
const ABNORMAL_RESULTS = ['failed', 'denied', 'expired', 'canceled', 'blocked']
const failedEvents = computed(() => rows.value.filter((r) => ABNORMAL_RESULTS.includes(r.result)).length)

onMounted(load)
</script>

<template>
  <div class="space-y-4">
    <section class="grid-cards">
      <div class="card card-pad">
        <p class="card-sub">事件总数</p>
        <p class="mt-1 text-xl font-semibold text-ink">{{ total }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">默认按时间倒序</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页 Agent 发起</p>
        <p class="mt-1 text-xl font-semibold text-violet2">{{ agentEvents }} / {{ rows.length }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">AI 动作必须可追溯到具体 Agent</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页异常事件</p>
        <p class="mt-1 text-xl font-semibold text-danger">{{ failedEvents }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">失败 / 被拒绝</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">数据来源</p>
        <p class="mt-1 text-xl font-semibold text-brand-300">2 个事实源合并</p>
        <p class="mt-1 text-[11px] text-ink-dim">
          工具调用审计 + 写动作审批（各取最近 {{ sourceWindow || '—' }} 行）
        </p>
      </div>
    </section>

    <section class="card overflow-hidden">
      <div class="panel-head flex-wrap">
        <div class="flex flex-wrap items-center gap-2">
          <el-input v-model="query.operator" placeholder="操作者" clearable class="!w-[160px]" @keyup.enter="onSearch">
            <template #prefix><el-icon><Search /></el-icon></template>
          </el-input>
          <el-select v-model="query.action" placeholder="动作" clearable filterable class="!w-[170px]">
            <el-option-group v-for="g in actionGroups" :key="g.label" :label="g.label">
              <el-option v-for="a in g.items" :key="a" :label="a" :value="a" />
            </el-option-group>
          </el-select>
          <el-input v-model="query.target" placeholder="目标对象" clearable class="!w-[170px]" @keyup.enter="onSearch" />
          <el-select v-model="query.result" placeholder="结果" clearable class="!w-[130px]">
            <el-option label="成功" value="success" />
            <el-option label="失败" value="failed" />
            <el-option label="被拒绝（未执行）" value="denied" />
            <el-option label="待审批" value="pending" />
            <el-option label="已过期" value="expired" />
            <el-option label="已撤销" value="canceled" />
            <el-option label="复核未通过" value="blocked" />
          </el-select>
          <el-input v-model="query.ip" placeholder="来源 IP" clearable class="!w-[140px]" @keyup.enter="onSearch" />
          <el-button type="primary" @click="onSearch">查询</el-button>
          <el-button @click="reset">重置</el-button>
        </div>
        <el-button plain><el-icon class="mr-1"><Download /></el-icon>导出</el-button>
      </div>

      <el-table
        v-loading="loading"
        :data="rows"
        style="width: 100%"
        row-class-name="cursor-pointer"
        @row-click="openDetail"
      >
        <el-table-column label="时间" width="180" fixed="left">
          <template #default="{ row }">
            <p class="text-xs text-ink-muted">{{ formatTime(row.time) }}</p>
            <p class="text-[10px] text-ink-dim">{{ formatRelative(row.time) }}</p>
          </template>
        </el-table-column>

        <el-table-column label="操作者" min-width="170">
          <template #default="{ row }">
            <div class="flex items-center gap-2">
              <span
                class="chip shrink-0"
                :style="{
                  color: operatorTypeMeta[row.operator_type]?.color,
                  borderColor: (operatorTypeMeta[row.operator_type]?.color || '#333') + '55',
                }"
              >
                {{ operatorTypeMeta[row.operator_type]?.text }}
              </span>
              <span class="truncate font-mono text-[11px] text-ink-muted">{{ row.operator }}</span>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="动作" min-width="190">
          <template #default="{ row }">
            <p class="truncate text-xs text-ink">{{ row.action_text }}</p>
            <p class="truncate font-mono text-[10px] text-ink-dim">{{ row.action }}</p>
          </template>
        </el-table-column>

        <el-table-column label="目标" min-width="190">
          <template #default="{ row }">
            <p class="truncate text-xs text-ink-muted">{{ row.target }}</p>
            <p class="truncate text-[10px] text-ink-dim">{{ row.target_type }}</p>
          </template>
        </el-table-column>

        <el-table-column label="结果" width="105">
          <template #default="{ row }">
            <StatusBadge kind="audit" :value="row.result" />
          </template>
        </el-table-column>

        <el-table-column label="来源" width="110">
          <template #default="{ row }">
            <span
              class="chip"
              :style="{
                color: sourceMeta[row.source]?.color || '#7c8aa8',
                borderColor: (sourceMeta[row.source]?.color || '#7c8aa8') + '55',
              }"
            >
              {{ sourceMeta[row.source]?.text || row.source }}
            </span>
          </template>
        </el-table-column>

        <el-table-column label="详情" min-width="220">
          <template #default="{ row }">
            <span class="text-[11px] text-ink-dim">{{ row.detail }}</span>
          </template>
        </el-table-column>

        <el-table-column label="来源 IP" width="140">
          <template #default="{ row }">
            <span class="font-mono text-[11px] text-ink-muted">{{ row.ip }}</span>
          </template>
        </el-table-column>

        <el-table-column label="trace_id" width="160">
          <template #default="{ row }">
            <span class="font-mono text-[10px] text-brand-300">{{ row.trace_id }}</span>
          </template>
        </el-table-column>

        <template #empty>
          <EmptyState
            icon="Document"
            title="没有匹配的审计事件"
            desc="调整筛选条件，或清空条件查看全部事件。数据来自工具调用审计与写动作审批两个真实来源。"
            action-text="重置筛选"
            @action="reset"
          />
        </template>
      </el-table>

      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-navy-600 px-5 py-3">
        <p class="text-[11px] text-ink-dim">
          共 {{ total }} 条<span v-if="notice"> · {{ notice }}</span>
        </p>
        <el-pagination
          v-model:current-page="query.page"
          v-model:page-size="query.page_size"
          :total="total"
          :page-sizes="[12, 24, 48]"
          layout="sizes, prev, pager, next"
          background
          @current-change="load"
          @size-change="onSearch"
        />
      </div>
    </section>

    <!-- 事件详情 -->
    <el-drawer v-model="detail.visible" :size="560" title="审计事件详情">
      <div v-if="detail.row" class="scroll-area h-full px-5 py-4">
        <div class="card card-pad">
          <div class="flex items-start justify-between gap-3">
            <div>
              <p class="text-sm font-semibold text-ink">{{ detail.row.action_text }}</p>
              <p class="mt-0.5 font-mono text-[11px] text-brand-300">{{ detail.row.action }}</p>
            </div>
            <StatusBadge kind="audit" :value="detail.row.result" />
          </div>

          <el-descriptions :column="2" border size="small" class="mt-4">
            <el-descriptions-item label="事件 ID">
              <span class="font-mono">{{ detail.row.id }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="时间">{{ formatTime(detail.row.time) }}</el-descriptions-item>
            <el-descriptions-item label="操作者">
              <span class="font-mono">{{ detail.row.operator }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="操作者类型">
              {{ operatorTypeMeta[detail.row.operator_type]?.text }}
            </el-descriptions-item>
            <el-descriptions-item label="目标">{{ detail.row.target }}</el-descriptions-item>
            <el-descriptions-item label="目标类型">{{ detail.row.target_type }}</el-descriptions-item>
            <el-descriptions-item label="来源">
              {{ sourceMeta[detail.row.source]?.text || detail.row.source }}
            </el-descriptions-item>
            <el-descriptions-item label="来源 IP">
              <span class="font-mono">{{ detail.row.ip || '—（当前未采集）' }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="trace_id" :span="2">
              <span class="font-mono">{{ detail.row.trace_id }}</span>
            </el-descriptions-item>
          </el-descriptions>

          <div class="mt-3">
            <p class="section-label">说明</p>
            <p class="mt-1 text-[11px] leading-5 text-ink-muted">{{ detail.row.detail }}</p>
          </div>
        </div>

        <div class="card mt-4 card-pad">
          <p class="section-label">原始事件（JSON）</p>
          <pre class="mt-2 max-h-[320px] overflow-auto rounded-lg bg-navy-900 p-3 font-mono text-[10px] leading-5 text-ink-muted">{{ eventJson }}</pre>
        </div>

        <div class="card mt-4 card-pad">
          <p class="section-label">关联追溯</p>
          <div class="mt-2 flex flex-wrap gap-2">
            <el-button size="small" plain @click="$router.push('/runs')">查看关联执行</el-button>
            <el-button size="small" plain @click="$router.push('/agents')">查看发起 Agent</el-button>
            <el-button size="small" plain @click="$router.push('/jobs')">查看任务定义</el-button>
          </div>
          <p class="mt-2 text-[11px] text-ink-dim">
            trace_id 用于串起同一次会话/运行（工具调用取 run_id，审批单取 run_id 或审批号）。
          </p>
        </div>
      </div>
    </el-drawer>
  </div>
</template>