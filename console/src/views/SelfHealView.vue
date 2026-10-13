<template>
  <div class="page">
    <el-card shadow="never">
      <template #header>
        <div class="head">
          <div>
            <div class="title">自愈调度（失败自动处置）</div>
            <div class="sub">
              这里是<strong>只读</strong>视图 + 一个"手动跑一轮"按钮。系统能做的动作是<strong>白名单</strong>：
              <span v-for="(name, act) in actionNames" :key="act" class="chip">{{ act }}（{{ name }}）</span>
              ——除此之外一律转人工。页面<strong>没有</strong>"直接执行动作"的入口，这是设计，不是遗漏。
            </div>
          </div>
          <div class="head-actions">
            <el-button :loading="loading" @click="loadAll">刷新</el-button>
            <el-button type="primary" :loading="scanning" @click="runScan()">手动跑一轮</el-button>
          </div>
        </div>
      </template>

      <!-- 第一屏永远是"边界"：看不到边界就不该打开开关。 -->
      <el-descriptions v-if="policy" :column="4" border size="small" class="policy">
        <el-descriptions-item label="总开关">
          <el-tag :type="policy.enabled ? 'success' : 'info'" size="small">
            {{ policy.enabled ? '已开启' : '关闭（只分类留痕，不动手）' }}
          </el-tag>
        </el-descriptions-item>
        <el-descriptions-item label="写能力（tools.allow-write）">
          <el-tag :type="writesEnabled ? 'warning' : 'info'" size="small">
            {{ writesEnabled ? '已开：动作可能真的执行' : '关闭：所有写动作都会被拒' }}
          </el-tag>
        </el-descriptions-item>
        <el-descriptions-item label="尝试预算">
          窗口 {{ policy.window_seconds }}s 内 {{ policy.max_attempts_per_job_window }} 次（含成功过的尝试）
        </el-descriptions-item>
        <el-descriptions-item label="冷却">{{ policy.cooldown_seconds }}s（0 = 不冷却）</el-descriptions-item>
        <el-descriptions-item label="超时放大">
          ×{{ policy.extend_timeout_factor }}，天花板 {{ policy.max_extend_timeout_seconds }}s
        </el-descriptions-item>
        <el-descriptions-item label="并发上限">{{ policy.max_concurrent }}（事实来源：executed 行数）</el-descriptions-item>
        <el-descriptions-item label="观测窗口">{{ policy.observe_window_seconds }}s（没观测到 ≠ 成功）</el-descriptions-item>
        <el-descriptions-item label="扫描间隔 / 回看">
          {{ policy.interval_seconds }}s / {{ policy.lookback_seconds }}s
        </el-descriptions-item>
      </el-descriptions>

      <el-alert
        v-if="banner"
        :type="banner.type"
        :title="banner.title"
        :description="banner.description"
        :closable="false"
        show-icon
        class="banner"
      />

      <div class="filters">
        <el-select v-model="filters.status" placeholder="状态" clearable style="width: 190px" @change="loadList">
          <el-option v-for="(name, key) in statusNames" :key="key" :label="name" :value="key" />
        </el-select>
        <el-select v-model="filters.action" placeholder="动作" clearable style="width: 180px" @change="loadList">
          <el-option v-for="(name, key) in actionNames" :key="key" :label="name" :value="key" />
        </el-select>
        <el-input v-model="filters.source_run_id" placeholder="来源执行 run_id" clearable style="width: 230px" @keyup.enter="loadList" />
        <el-input v-model="filters.node_uuid" placeholder="节点 UUID" clearable style="width: 180px" @keyup.enter="loadList" />
        <el-button @click="loadList">查询</el-button>
      </div>

      <el-table :data="rows" size="small" v-loading="loading" @row-click="openDetail" class="tbl">
        <el-table-column prop="heal_id" label="自愈单号" width="230" />
        <el-table-column label="任务" min-width="150">
          <template #default="{ row }">#{{ row.job_id }} {{ row.job_name }}</template>
        </el-table-column>
        <el-table-column label="状态" width="160">
          <template #default="{ row }">
            <el-tag :type="statusType(row.status)" size="small">{{ statusNames[row.status] || row.status }}</el-tag>
            <el-tag v-if="pendingObservation(row)" size="small" type="warning" effect="plain" class="ml">观察中</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="失败分类 / 动作" min-width="180">
          <template #default="{ row }">
            <div>{{ row.failure_class }}</div>
            <div class="dim">{{ actionNames[row.action] || row.action || '无动作' }}</div>
          </template>
        </el-table-column>
        <el-table-column label="第几次 / 结果" width="150">
          <template #default="{ row }">
            <div>attempt {{ row.attempt }}</div>
            <div class="dim">{{ row.new_run_id ? '新执行 ' + row.new_run_id : '无后续执行' }}</div>
          </template>
        </el-table-column>
        <el-table-column label="触发方" width="140" prop="executed_by" />
        <el-table-column label="发生时间" width="160">
          <template #default="{ row }">{{ fmtTs(row.created) }}</template>
        </el-table-column>
      </el-table>

      <div class="page-bar">
        <span class="dim">共 {{ total }} 条</span>
        <el-pagination
          layout="prev, pager, next"
          :total="total"
          :page-size="filters.page_size"
          :current-page="filters.page"
          @current-change="onPage"
        />
      </div>
    </el-card>

    <el-card v-if="detail" shadow="never" class="detail">
      <template #header>
        <div class="head">
          <div class="title">自愈详情 · {{ detail.heal_id }}</div>
          <el-button size="small" @click="detail = null">收起</el-button>
        </div>
      </template>

      <el-descriptions :column="3" border size="small">
        <el-descriptions-item label="状态">
          <el-tag :type="statusType(detail.status)" size="small">{{ statusNames[detail.status] || detail.status }}</el-tag>
        </el-descriptions-item>
        <el-descriptions-item label="任务">#{{ detail.job_id }} {{ detail.job_name }}</el-descriptions-item>
        <el-descriptions-item label="第几次尝试">attempt {{ detail.attempt }}</el-descriptions-item>
        <el-descriptions-item label="来源失败执行">{{ detail.source_run_id }}</el-descriptions-item>
        <el-descriptions-item label="观测到的新执行">{{ detail.new_run_id || '（无）' }}</el-descriptions-item>
        <el-descriptions-item label="观测截止">
          {{ detail.deadline ? fmtTs(detail.deadline) : '—（已终结）' }}
        </el-descriptions-item>
        <el-descriptions-item label="结束时间">
          {{ detail.finished_at ? fmtTs(detail.finished_at) : '未结束' }}
        </el-descriptions-item>
        <el-descriptions-item label="触发方">{{ detail.executed_by }}</el-descriptions-item>
        <el-descriptions-item label="关联经验条目">{{ detail.memory_ids || '（无）' }}</el-descriptions-item>
      </el-descriptions>

      <div class="block">
        <div class="block-title">症状（这就是失败的事实描述）</div>
        <div class="mono">{{ detail.symptom }}</div>
      </div>
      <div class="block">
        <div class="block-title">为什么这么决策</div>
        <div>{{ detail.reason }}</div>
      </div>
      <div class="block">
        <div class="block-title">策略判定（命中哪条规则，安全边界事后可复盘）</div>
        <div class="mono">{{ pretty(detail.policy_json) }}</div>
      </div>
      <div class="block">
        <div class="block-title">动作参数</div>
        <div class="mono">{{ detail.action_json || '（无动作参数：转人工/被拒）' }}</div>
      </div>
      <div class="block">
        <div class="block-title">依据的失败事实（Evidence，只含系统自己观测到的）</div>
        <div class="mono">{{ pretty(detail.evidence_json) }}</div>
      </div>
      <div class="block">
        <div class="block-title">决策时参考的历史经验（只影响候选顺序，不作证据）</div>
        <div class="mono">{{ pretty(detail.memory_json) }}</div>
      </div>
      <div class="block">
        <div class="block-title">结果说明</div>
        <div :class="{ warn: detail.status === 'failed' }">{{ detail.outcome_note || '—' }}</div>
      </div>
    </el-card>

    <el-card shadow="never" class="cases">
      <template #header><div class="title">失败分类 → 允许的动作（白名单映射）</div></template>
      <el-table :data="classes" size="small">
        <el-table-column prop="desc" label="失败分类" width="180" />
        <el-table-column prop="class" label="class" width="200" />
        <el-table-column label="允许的动作">
          <template #default="{ row }">
            <el-tag v-for="a in row.actions" :key="a" size="small" effect="plain" class="ml">
              {{ a }}（{{ actionNames[a] || a }}）
            </el-tag>
          </template>
        </el-table-column>
      </el-table>
      <div class="dim tip">
        除上表之外的情况一律转人工；"转人工"不执行任何写动作，因此不受写能力闸门限制。
      </div>
    </el-card>
  </div>
</template>

<script setup>
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { fetchHealPolicy, findHeal, scanHeals, searchHeals } from '@/api/selfheal'

const loading = ref(false)
const scanning = ref(false)
const rows = ref([])
const total = ref(0)
const policy = ref(null)
const writesEnabled = ref(false)
const statusNames = ref({})
const actionNames = ref({})
const classes = ref([])
const detail = ref(null)
const banner = ref(null)

const filters = reactive({
  status: '',
  action: '',
  source_run_id: '',
  node_uuid: '',
  page: 1,
  page_size: 20,
})

const statusType = (s) => {
  switch (s) {
    case 'succeeded':
      return 'success'
    case 'failed':
      return 'danger'
    case 'escalated':
      return 'warning'
    case 'executed':
      return 'warning'
    case 'denied':
      return 'info'
    default:
      return 'info'
  }
}

const fmtTs = (sec) => (sec ? new Date(sec * 1000).toLocaleString() : '—')

/**
 * 是否"仍在等观测结果"。
 * 后端返回的每一行只有 status（planned/denied/executed/succeeded/failed/escalated），
 * 没有 pending_observation 字段——终态之外的都是还没落定，这里按真实语义推导，
 * 而不是依赖 Mock 独有的字段。
 */
const pendingObservation = (row) => row?.status === 'executed' || row?.status === 'planned'

// JSON 快照可能不是合法 JSON（历史数据/被截断），此时原样显示而不是吞掉。
const pretty = (text) => {
  if (!text) return '（空）'
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch (e) {
    return String(text)
  }
}

async function loadPolicy() {
  const data = await fetchHealPolicy()
  policy.value = data.policy || {}
  writesEnabled.value = Boolean(data.writes_enabled)
  statusNames.value = data.status_names || {}
  actionNames.value = data.action_names || {}
  classes.value = data.classes || []
  if (!writesEnabled.value && policy.value.enabled) {
    banner.value = {
      type: 'warning',
      title: '总开关已开，但写能力关闭',
      description:
        '当前 tools.allow-write=false：扫描会照常分类并留下"被策略拒绝"的记录，但不会执行任何重跑。' +
        '这是默认档位，也是最小爆炸半径的档位。',
    }
  }
}

async function loadList() {
  loading.value = true
  try {
    const params = { page: filters.page, page_size: filters.page_size }
    if (filters.status) params.status = filters.status
    if (filters.action) params.action = filters.action
    if (filters.source_run_id) params.source_run_id = filters.source_run_id
    if (filters.node_uuid) params.node_uuid = filters.node_uuid
    const data = await searchHeals(params)
    rows.value = data.list || []
    total.value = data.total || 0
    if (data.status_names) statusNames.value = data.status_names
    if (data.action_names) actionNames.value = data.action_names
  } finally {
    loading.value = false
  }
}

async function loadAll() {
  await Promise.all([loadPolicy(), loadList()])
}

function onPage(p) {
  filters.page = p
  loadList()
}

async function openDetail(row) {
  // 后端 /agent/heal/find 的响应是 { heal: {...} }，早前直接把整个响应体当明细用，
  // 于是 detail.heal_id / detail.status 全是 undefined，详情面板整块空白。
  const res = await findHeal(row.heal_id)
  detail.value = res?.heal || res || null
}

async function runScan() {
  scanning.value = true
  try {
    const rep = await scanHeals()
    banner.value = {
      type: 'info',
      title: '已手动跑一轮（不绕过闸门）',
      description:
        `扫描 ${rep.scanned} 条失败执行：执行 ${rep.executed}、被拒 ${rep.denied}、` +
        `转人工 ${rep.escalated}、失败 ${rep.failed}、观测到成功 ${rep.succeeded}。` +
        '与自动轮次走的是同一套判定。',
    }
    await loadList()
  } finally {
    scanning.value = false
  }
}

onMounted(loadAll)
</script>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 12px;
}
.title {
  font-size: 15px;
  font-weight: 600;
}
.sub {
  margin-top: 4px;
  color: #909399;
  font-size: 12px;
  line-height: 1.7;
  max-width: 900px;
}
.chip {
  display: inline-block;
  margin: 0 4px;
  padding: 0 6px;
  background: #f4f4f5;
  border-radius: 4px;
  color: #606266;
  font-size: 11px;
}
.head-actions {
  display: flex;
  gap: 8px;
  flex-shrink: 0;
}
.policy {
  margin-bottom: 12px;
}
.banner {
  margin-bottom: 12px;
}
.filters {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}
.tbl :deep(.el-table__row) {
  cursor: pointer;
}
.dim {
  color: #909399;
  font-size: 11px;
}
.ml {
  margin-left: 4px;
}
.page-bar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-top: 12px;
}
.detail .block {
  margin-top: 12px;
}
.block-title {
  font-weight: 600;
  font-size: 12px;
  margin-bottom: 4px;
}
.mono {
  font-family: Consolas, Monaco, monospace;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
  background: #fafafa;
  border-radius: 4px;
  padding: 8px 10px;
}
.warn {
  color: #e6a23c;
}
.tip {
  margin-top: 8px;
  line-height: 1.7;
}
</style>