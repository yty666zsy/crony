<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  approveApproval,
  cancelApproval,
  findApproval,
  rejectApproval,
  searchApprovals,
  searchToolAudit,
} from '@/api/approval'
import { formatRelative, formatTime } from '@/utils/format'

/**
 * 两个业务码要单独判断（utils/dict.js 的 BIZ_CODE 是「码 → 文案」的映射表，
 * 并没有符号常量，所以这里定义具名常量而不是写魔法数字）。
 *   1014 状态已变更：单子已被别人处理 / 已执行 → 刷新列表，不要重试；
 *   1015 已过期：写动作的合理性依赖当时的现场 → 必须重新发起。
 */
const APPROVAL_CONFLICT = 1014
const APPROVAL_EXPIRED = 1015

/**
 * /approvals —— 写动作审批台（阶段 4.2）。
 *
 * 这是「人工确认闭环」里人这一侧的界面。设计上刻意坚持三件事：
 *
 * 1. **先说清楚要做什么，再让人点按钮**
 *    每行都摊开：工具、风险级别、模型自述理由、影响面、目标节点、剩余有效期。
 *    审批不是"点同意"，是**在有限时间内做一次安全判断**，信息不够就没法判断。
 *
 * 2. **状态文案必须能区分「执行了」「执行失败」「从没执行」**
 *    blocked（执行前复核未通过）与 execution_failed（执行了但失败）是两件事：
 *    前者 executed_at 为空、动作从未发生。页面直接渲染后端给的 status_text，
 *    不自己拼中文，避免同一状态在两处出现两种说法。
 *
 * 3. **批准是危险操作，重复点击不危险但要说清楚**
 *    一次批准 = 至多一次执行（控制面 CAS）。第二次点击会拿到 1014，
 *    页面据此提示"已被处理过"并刷新，而不是重试或报一个吓人的错。
 *
 * 页脚还有「本会话的审计流水」：每一次工具调用都有一条记录，
 * 包括被拒绝的和转审批的——被拒也要留痕，这是可解释性的底线。
 */

const loading = ref(false)
const rows = ref([])
const total = ref(0)
const busy = ref('')
const detail = reactive({ visible: false, row: null })
const audit = reactive({ loading: false, rows: [], total: 0, decision: '', page: 1, page_size: 8 })

const query = reactive({ status: '', tool: '', target_node: '', page: 1, page_size: 10 })

/** 状态过滤项。pending 放第一位：这是人真正要处理的东西。 */
const statusOptions = [
  { value: '', label: '全部' },
  { value: 'pending', label: '待审批' },
  { value: 'executed', label: '已执行' },
  { value: 'execution_failed', label: '已执行但失败' },
  { value: 'blocked', label: '已拦截（未执行）' },
  { value: 'rejected', label: '已驳回' },
  { value: 'expired', label: '已过期' },
  { value: 'canceled', label: '已撤销' },
]

const statusColor = {
  pending: 'warning',
  approved: 'primary',
  executed: 'success',
  execution_failed: 'danger',
  blocked: 'info',
  rejected: 'info',
  expired: 'info',
  canceled: 'info',
}

const riskColor = { write: 'warning', destructive: 'danger', read: 'info' }

const decisionOptions = [
  { value: '', label: '全部决策' },
  { value: 'allowed', label: '已放行' },
  { value: 'denied', label: '已拒绝' },
  { value: 'approval_required', label: '转人工审批' },
  { value: 'approval_executed', label: '已批准并执行' },
]

/** 剩余有效期：只在 pending 时有意义，过期的必须显示"已过期"而不是负数。 */
function remainText(row) {
  if (row.status !== 'pending') return '—'
  const s = Number(row.remain_seconds || 0)
  if (s <= 0) return '已过期'
  if (s < 60) return `${s} 秒后过期`
  return `${Math.floor(s / 60)} 分 ${s % 60} 秒后过期`
}

/** 是否可决策：只有待审批。前端不替代后端判断，只是别把按钮点成全红。 */
function canDecide(row) {
  return row.status === 'pending'
}

async function load() {
  loading.value = true
  try {
    const res = await searchApprovals({
      status: query.status || undefined,
      tool: query.tool || undefined,
      target_node: query.target_node || undefined,
      page: query.page,
      page_size: query.page_size,
    })
    rows.value = res?.list || []
    total.value = res?.total || 0
  } catch {
    /* 业务错误已由 client.js 统一提示，这里保持列表为空即可 */
  } finally {
    loading.value = false
  }
}

function onSearch() {
  query.page = 1
  load()
}

function reset() {
  query.status = ''
  query.tool = ''
  query.target_node = ''
  onSearch()
}

async function openDetail(row) {
  detail.visible = true
  detail.row = row
  // 详情单独再拉一次：列表是快照，人点开时状态可能已经变了（例如被别人批了）。
  try {
    const res = await findApproval(row.approval_id)
    if (res?.approval) detail.row = res.approval
  } catch {
    /* 保留列表里的那一份 */
  }
}

/**
 * 决策（批准 / 驳回 / 撤销）共用一条路径。
 *
 * 为什么要先把 note 收齐：驳回与撤销的**理由**是审计里唯一能回答
 * "为什么没做"的信息，后端会把它原样存进 decision_note。
 */
async function decide(row, action) {
  const isApprove = action === 'approve'
  const title = isApprove ? '批准并执行' : action === 'reject' ? '驳回' : '撤销'
  const tip = isApprove
    ? `批准后**立即执行一次**：${row.tool}（${row.impact || '影响面未知'}）。\n一次批准至多执行一次，重复点击不会重复执行。`
    : '不会执行任何动作，理由会写进审计。'
  let note = ''
  try {
    const input = await ElMessageBox.prompt(tip, title, {
      confirmButtonText: title,
      cancelButtonText: '再想想',
      inputPlaceholder: isApprove ? '可选：为什么同意（写进审计）' : '建议填写：为什么不做',
      inputValue: '',
      type: isApprove ? 'warning' : 'info',
      dangerouslyUseHTMLString: false,
    })
    note = input.value || ''
  } catch {
    return // 用户取消
  }

  busy.value = row.approval_id
  try {
    const res = isApprove
      ? await approveApproval(row.approval_id, note)
      : action === 'reject'
        ? await rejectApproval(row.approval_id, note)
        : await cancelApproval(row.approval_id, note)
    const approval = res?.approval
    if (isApprove && approval && approval.status === 'execution_failed') {
      // 执行真的发生了但失败：不能报"成功"，也不能报"没执行"。
      ElMessage.warning('已执行但失败：动作已经发生，请看详情里的执行结果')
    } else if (isApprove && approval && approval.status === 'blocked') {
      ElMessage.warning('已拦截：动作**从未执行**（执行前复核未通过），详情里有原因')
    } else if (isApprove) {
      ElMessage.success('已批准并执行一次')
    } else {
      ElMessage.success(`${title}成功（未执行任何动作）`)
    }
    detail.visible = false
    await Promise.all([load(), loadAudit()])
  } catch (err) {
    if (err?.code === APPROVAL_CONFLICT || err?.code === APPROVAL_EXPIRED) {
      ElMessage.info('这张单子已经处理过了（或被系统作废），已为你刷新列表')
      await load()
    }
    // 1016 / 1017 是安全拦截而不是故障：提示由 client.js 统一给出，这里不重试。
  } finally {
    busy.value = ''
  }
}

async function loadAudit() {
  audit.loading = true
  try {
    const res = await searchToolAudit({
      session_id: detail.row?.session_id || undefined,
      decision: audit.decision || undefined,
      page: audit.page,
      page_size: audit.page_size,
    })
    audit.rows = res?.list || []
    audit.total = res?.total || 0
  } catch {
    audit.rows = []
    audit.total = 0
  } finally {
    audit.loading = false
  }
}

const pendingCount = computed(() => rows.value.filter((r) => r.status === 'pending').length)

let timer = null
onMounted(() => {
  load()
  // 审批是"有时效的待办"：不刷新会让人看着已经过期的单子做判断。
  timer = setInterval(load, 15000)
})
onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <div class="page">
    <el-card shadow="never">
      <template #header>
        <div class="head">
          <div>
            <div class="title">写动作审批台</div>
            <div class="sub">
              Agent 想动手时不会自己动手：先落单，人批准后才执行，且
              <b>一次批准至多执行一次</b>。本页数据每 15 秒自动刷新。
            </div>
          </div>
          <div class="head-right">
            <el-tag v-if="pendingCount" type="warning" effect="dark">本页待处理 {{ pendingCount }}</el-tag>
            <el-button size="small" @click="load">刷新</el-button>
          </div>
        </div>
      </template>

      <div class="filters">
        <el-radio-group v-model="query.status" size="small" @change="onSearch">
          <el-radio-button v-for="s in statusOptions" :key="s.value" :value="s.value">{{ s.label }}</el-radio-button>
        </el-radio-group>
        <el-input v-model="query.tool" size="small" placeholder="工具名（如 job_retry）" clearable style="width: 190px" @keyup.enter="onSearch" />
        <el-input v-model="query.target_node" size="small" placeholder="目标节点 UUID" clearable style="width: 190px" @keyup.enter="onSearch" />
        <el-button size="small" type="primary" @click="onSearch">查询</el-button>
        <el-button size="small" @click="reset">重置</el-button>
      </div>

      <el-table v-loading="loading" :data="rows" size="small" style="width: 100%" @row-click="openDetail">
        <el-table-column label="状态" width="150">
          <template #default="{ row }">
            <el-tag :type="statusColor[row.status] || 'info'" size="small">{{ row.status_text || row.status }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="工具 / 风险" width="170">
          <template #default="{ row }">
            <div>{{ row.tool }}</div>
            <el-tag :type="riskColor[row.risk] || 'info'" size="small" effect="plain">{{ row.risk_text || row.risk }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="要做什么（模型自述理由）" min-width="240">
          <template #default="{ row }">
            <div class="reason">{{ row.reason || '—' }}</div>
            <div class="muted">{{ row.impact || '影响面未知' }}</div>
          </template>
        </el-table-column>
        <el-table-column label="目标节点" width="130" prop="target_node" />
        <el-table-column label="有效期" width="130">
          <template #default="{ row }">
            <span :class="{ warn: row.status === 'pending' && row.remain_seconds < 180 }">{{ remainText(row) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="发起" width="130">
          <template #default="{ row }">
            <div>{{ row.requested_by || 'agent' }}</div>
            <div class="muted">{{ formatRelative(row.created) }}</div>
          </template>
        </el-table-column>
        <el-table-column label="决策人" width="100">
          <template #default="{ row }">{{ row.decided_by || '—' }}</template>
        </el-table-column>
        <el-table-column label="操作" width="210" fixed="right">
          <template #default="{ row }">
            <template v-if="canDecide(row)">
              <el-button size="small" type="warning" :loading="busy === row.approval_id" @click.stop="decide(row, 'approve')">批准并执行</el-button>
              <el-button size="small" :disabled="busy === row.approval_id" @click.stop="decide(row, 'reject')">驳回</el-button>
              <el-button size="small" text :disabled="busy === row.approval_id" @click.stop="decide(row, 'cancel')">撤销</el-button>
            </template>
            <el-button v-else size="small" text @click.stop="openDetail(row)">详情</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div class="pager">
        <el-pagination
          v-model:current-page="query.page"
          v-model:page-size="query.page_size"
          :total="total"
          :page-sizes="[10, 20, 50]"
          layout="total, sizes, prev, pager, next"
          size="small"
          @current-change="load"
          @size-change="onSearch"
        />
      </div>
    </el-card>

    <el-drawer v-model="detail.visible" title="审批单详情" size="620px" @open="loadAudit">
      <template v-if="detail.row">
        <el-descriptions :column="1" border size="small">
          <el-descriptions-item label="单号">{{ detail.row.approval_id }}</el-descriptions-item>
          <el-descriptions-item label="状态">
            <el-tag :type="statusColor[detail.row.status] || 'info'" size="small">{{ detail.row.status_text || detail.row.status }}</el-tag>
            <span v-if="detail.row.executed" class="muted">（动作已发生）</span>
            <span v-else class="muted">（动作未发生）</span>
          </el-descriptions-item>
          <el-descriptions-item label="工具 / 风险">{{ detail.row.tool }} · {{ detail.row.risk_text || detail.row.risk }}</el-descriptions-item>
          <el-descriptions-item label="会话 / 运行">
            {{ detail.row.session_id }}<span v-if="detail.row.run_id"> · {{ detail.row.run_id }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="影响面">{{ detail.row.impact || '未知' }}</el-descriptions-item>
          <el-descriptions-item label="模型自述理由">{{ detail.row.reason || '—' }}</el-descriptions-item>
          <el-descriptions-item label="存档参数（脱敏）">
            <pre class="code">{{ detail.row.args_text || '{}' }}</pre>
          </el-descriptions-item>
          <el-descriptions-item label="参数哈希">
            <span class="mono">{{ detail.row.args_hash }}</span>
            <div class="muted">
              批准后执行前会再算一次哈希：对不上就拒绝执行（防止"批准 A、执行 B"）。
            </div>
          </el-descriptions-item>
          <el-descriptions-item label="有效期">
            {{ formatTime(detail.row.expires_at) }}（Unix 秒） · {{ remainText(detail.row) }}
          </el-descriptions-item>
          <el-descriptions-item label="决策">
            <span v-if="detail.row.decided_by">{{ detail.row.decided_by }} · {{ formatTime(detail.row.decided_at) }}</span>
            <span v-else class="muted">尚未决策</span>
            <div v-if="detail.row.decision_note" class="muted">{{ detail.row.decision_note }}</div>
          </el-descriptions-item>
          <el-descriptions-item label="执行">
            <span v-if="detail.row.executed_at">已执行于 {{ formatTime(detail.row.executed_at) }}</span>
            <span v-else class="muted">没有任何执行时间戳（动作未发生）</span>
            <div v-if="detail.row.execution_result" class="code">{{ detail.row.execution_result }}</div>
          </el-descriptions-item>
        </el-descriptions>

        <div class="audit-head">
          <span>本会话的工具调用审计</span>
          <el-radio-group v-model="audit.decision" size="small" @change="loadAudit">
            <el-radio-button v-for="d in decisionOptions" :key="d.value" :value="d.value">{{ d.label }}</el-radio-button>
          </el-radio-group>
        </div>
        <el-table v-loading="audit.loading" :data="audit.rows" size="small" style="width: 100%">
          <el-table-column label="决策" width="140">
            <template #default="{ row }">{{ row.decision_text || row.decision }}</template>
          </el-table-column>
          <el-table-column label="工具" width="150" prop="tool" />
          <el-table-column label="原因" min-width="200">
            <template #default="{ row }">
              <span :class="{ muted: !row.reason }">{{ row.reason || '—' }}</span>
            </template>
          </el-table-column>
          <el-table-column label="耗时" width="80">
            <template #default="{ row }">{{ row.duration_ms }}ms</template>
          </el-table-column>
        </el-table>
        <div v-if="!audit.total" class="muted empty">本次会话还没有工具调用记录（转审批、被拒都会留下记录）。</div>
      </template>
    </el-drawer>
  </div>
</template>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
}
.head-right {
  display: flex;
  align-items: center;
  gap: 8px;
}
.title {
  font-size: 16px;
  font-weight: 600;
}
.sub {
  margin-top: 4px;
  font-size: 12px;
  color: #7c8aa8;
  max-width: 720px;
}
.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.reason {
  font-size: 13px;
}
.muted {
  color: #7c8aa8;
  font-size: 12px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
  word-break: break-all;
}
.code {
  margin: 4px 0 0;
  padding: 8px;
  background: rgba(124, 138, 168, 0.08);
  border-radius: 4px;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}
.warn {
  color: #e6a23c;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 12px;
}
.audit-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin: 18px 0 8px;
  font-size: 13px;
  font-weight: 600;
}
.empty {
  margin-top: 8px;
}
</style>