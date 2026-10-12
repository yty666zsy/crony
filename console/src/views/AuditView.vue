<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { searchAuditLogs } from '@/api/audit'
import StatusBadge from '@/components/StatusBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import { formatRelative, formatTime } from '@/utils/format'

/**
 * /audit —— 审计追溯。
 *
 * ⚠️ 阶段 2 规划页：POST /audit/search 尚未实现。
 *   阶段 0/1 的审计事实来源是 admin 的访问日志 + job 操作记录；
 *   阶段 2 会引入统一 audit_event 表（operator 可以是 user / system / agent），
 *   并保证 Agent 的每一次工具调用都留痕（AI 运维可解释性的硬要求）。
 */

const loading = ref(false)
const rows = ref([])
const total = ref(0)
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

const actionGroups = [
  { label: '任务', items: ['job.once', 'job.failed', 'job.success', 'job.skip', 'job.kill'] },
  { label: '用户', items: ['user.login', 'user.change_pw', 'user.register'] },
  { label: '节点', items: ['node.delete', 'node.register'] },
  { label: 'Agent', items: ['baseline.scan', 'agent.dispatch', 'kb.write'] },
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
const failedEvents = computed(() => rows.value.filter((r) => r.result === 'failed' || r.result === 'forbidden').length)

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
        <p class="card-sub">保留策略</p>
        <p class="mt-1 text-xl font-semibold text-brand-300">180 天</p>
        <p class="mt-1 text-[11px] text-ink-dim">阶段 2 支持导出与冷备</p>
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
            <el-option label="已跳过" value="skipped" />
            <el-option label="被拒绝" value="forbidden" />
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
            desc="调整筛选条件，或清空条件查看全部事件。审计事件在阶段 2 由统一 audit_event 表提供。"
            action-text="重置筛选"
            @action="reset"
          />
        </template>
      </el-table>

      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-navy-600 px-5 py-3">
        <p class="text-[11px] text-ink-dim">
          共 {{ total }} 条 · 接口 POST /audit/search 规划中（阶段 2）
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
            <el-descriptions-item label="来源 IP">
              <span class="font-mono">{{ detail.row.ip }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="trace_id">
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
            阶段 2：以 trace_id 串联 job_run → job_run_step → audit_event，支持一键回放。
          </p>
        </div>
      </div>
    </el-drawer>
  </div>
</template>