<template>
  <div class="page">
    <el-card shadow="never">
      <template #header>
        <div class="head">
          <div>
            <div class="title">任务编排（多步骤 DAG）</div>
            <div class="sub">
              读取任务的 <code>steps_json</code>：按层深渲染步骤与依赖边，校验结果直接内联显示。
              这里是**只读**视图——写入口仍是任务编辑（后端保存时会拒绝非法图）。
            </div>
          </div>
          <div class="head-actions">
            <el-button :loading="loading" @click="loadJobs">刷新任务</el-button>
            <el-button type="primary" :loading="validating" @click="validateDraft">校验草稿</el-button>
          </div>
        </div>
      </template>

      <div class="filters">
        <el-select v-model="selectedJobId" placeholder="选择任务" filterable style="width: 320px" @change="loadPlan(selectedJobId)">
          <el-option v-for="j in jobs" :key="j.id" :label="`#${j.id} ${j.name}`" :value="j.id">
            <span>#{{ j.id }} {{ j.name }}</span>
            <el-tag v-if="hasPlan(j)" size="small" type="success" style="margin-left: 8px">有编排</el-tag>
            <el-tag v-else size="small" type="info" style="margin-left: 8px">单步</el-tag>
          </el-option>
        </el-select>
        <el-input v-model="keyword" placeholder="按名称筛选任务" clearable style="width: 220px" @keyup.enter="loadJobs" />
        <el-button @click="loadJobs">查询</el-button>
      </div>

      <el-alert
        v-if="banner"
        :type="banner.type"
        :title="banner.title"
        :description="banner.description"
        :closable="false"
        show-icon
        style="margin: 12px 0"
      />

      <template v-if="plan">
        <div class="meta">
          <el-tag size="small">并发上限 {{ plan.max_parallel }}</el-tag>
          <el-tag size="small" :type="plan.fail_fast ? 'danger' : 'info'">
            {{ plan.fail_fast ? '失败即急停（fail_fast）' : '分支独立（不急停）' }}
          </el-tag>
          <el-tag size="small" type="info">层深 {{ plan.max_depth }}（同层可并行）</el-tag>
          <el-tag size="small" type="info">{{ (plan.steps || []).length }} 步 / {{ (plan.edges || []).length }} 条依赖边</el-tag>
        </div>

        <div class="lanes">
          <div v-for="lane in lanes" :key="lane.depth" class="lane">
            <div class="lane-head">第 {{ lane.depth }} 层</div>
            <div class="lane-body">
              <div v-for="s in lane.steps" :key="s.index" class="node" :class="{ warn: (s.warnings || []).length }">
                <div class="node-head">
                  <span class="node-idx">#{{ s.index }}</span>
                  <span class="node-name">{{ s.name }}</span>
                  <el-tag size="small" effect="plain">{{ s.kind }}</el-tag>
                </div>
                <div class="node-cmd">{{ s.command }}</div>
                <div class="node-meta">
                  <span>依赖：{{ (s.depends_on || []).length ? s.depends_on.join(', ') : '无（起点）' }}</span>
                  <span>汇聚：{{ s.join }}</span>
                  <span>条件：{{ s.condition }}</span>
                  <span v-if="s.retry_times">重试：{{ s.retry_times }} 次</span>
                  <span v-if="s.timeout_seconds">超时：{{ s.timeout_seconds }}s</span>
                </div>
                <div v-for="(w, i) in (s.warnings || [])" :key="i" class="node-warn">⚠ {{ w }}</div>
              </div>
            </div>
            <div v-if="lane.depth < plan.max_depth" class="lane-arrow">↓</div>
          </div>
        </div>

        <el-table :data="plan.steps" size="small" style="margin-top: 16px">
          <el-table-column prop="index" label="#" width="60" />
          <el-table-column prop="name" label="步骤" min-width="140" />
          <el-table-column prop="kind" label="类型" width="90" />
          <el-table-column label="依赖" width="120">
            <template #default="{ row }">{{ (row.depends_on || []).length ? row.depends_on.join(',') : '—' }}</template>
          </el-table-column>
          <el-table-column prop="join" label="汇聚" width="120" />
          <el-table-column prop="condition" label="条件" width="110" />
          <el-table-column prop="depth" label="层深" width="80" />
          <el-table-column label="命令" min-width="240" show-overflow-tooltip>
            <template #default="{ row }">{{ row.command }}</template>
          </el-table-column>
        </el-table>

        <el-table :data="plan.edges" size="small" style="margin-top: 12px">
          <el-table-column label="依赖边（上游 → 下游）" min-width="240">
            <template #default="{ row }">{{ edgeText(row) }}</template>
          </el-table-column>
        </el-table>
      </template>

      <el-empty v-else-if="!loading" description="选择左侧任务查看它的编排，或粘贴一份编排定义做校验" />

      <el-divider content-position="left">草稿校验（不落库）</el-divider>
      <el-input
        v-model="draft"
        type="textarea"
        :rows="10"
        placeholder='粘贴 steps_json，例如 {"steps":[{"index":0,"command":"/opt/a.sh"},{"index":1,"command":"/opt/b.sh","depends_on":"0"}]}'
      />
      <div class="draft-actions">
        <el-button @click="fillFromSelectedJob" :disabled="!selectedJob">用选中任务的 steps_json 填充</el-button>
        <el-button @click="fillDiamond">填一份菱形样例</el-button>
        <el-button type="primary" :loading="validating" @click="validateDraft">校验</el-button>
      </div>
      <el-alert
        v-if="draftResult"
        :type="draftResult.ok ? 'success' : 'error'"
        :title="draftResult.title"
        :description="draftResult.description"
        :closable="false"
        show-icon
        style="margin-top: 12px"
      />
    </el-card>
  </div>
</template>

<script setup>
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { searchJobs, findJobDag, validateJobDag } from '../api/job'

const jobs = ref([])
const keyword = ref('')
const selectedJobId = ref(null)
const plan = ref(null)
const loading = ref(false)
const validating = ref(false)
const draft = ref('')
const draftResult = ref(null)
const banner = ref(null)

const selectedJob = computed(() => jobs.value.find((j) => Number(j.id) === Number(selectedJobId.value)) || null)

/** 有编排 = steps_json 非空（后端把这一步叫"是否走 DAG 引擎"的判据）。 */
function hasPlan(job) {
  return Boolean(String(job?.steps_json ?? '').trim())
}

/** 按层深分道：同一层的步骤可以并行，这正是控制台要让人一眼看到的事。 */
const lanes = computed(() => {
  if (!plan.value) return []
  const byDepth = new Map()
  for (const s of plan.value.steps || []) {
    if (!byDepth.has(s.depth)) byDepth.set(s.depth, [])
    byDepth.get(s.depth).push(s)
  }
  return [...byDepth.entries()].sort((a, b) => a[0] - b[0]).map(([depth, steps]) => ({ depth, steps }))
})

function edgeText(edge) {
  const steps = plan.value?.steps || []
  const from = steps.find((s) => s.index === edge.from)
  const to = steps.find((s) => s.index === edge.to)
  return `${edge.from} ${from?.name || ''} → ${edge.to} ${to?.name || ''}`
}

function describe(res) {
  const s = res.plan || {}
  const bits = [`${(s.steps || []).length} 步 / ${(s.edges || []).length} 条依赖边 / 层深 ${s.max_depth ?? 0}`]
  const warns = (s.steps || []).flatMap((x) => (x.warnings || []).map((w) => `#${x.index} ${w}`))
  return bits.concat(warns).join('；')
}

async function loadJobs() {
  loading.value = true
  try {
    const res = await searchJobs({ page: 1, page_size: 100, name: keyword.value })
    jobs.value = res.list || []
    if (!selectedJobId.value) {
      const first = jobs.value.find(hasPlan)
      if (first) {
        selectedJobId.value = first.id
        await loadPlan(first.id)
      }
    }
  } finally {
    loading.value = false
  }
}

async function loadPlan(jobId) {
  if (!jobId) return
  plan.value = null
  banner.value = null
  loading.value = true
  try {
    const res = await findJobDag(jobId)
    plan.value = res.plan
    banner.value = {
      type: 'success',
      title: `#${res.job_id} ${res.name} 的编排可执行`,
      description: describe({ plan: res.plan }),
    }
  } catch (e) {
    // 1022 = 该任务没有编排；1021 = 存着的定义非法（仍要展示出来，否则用户看不到自己存了什么）
    if (e?.code === 1022) {
      banner.value = { type: 'info', title: '该任务是单步任务', description: '没有 steps_json：它按老的单步语义执行，行为与升级前完全一致。' }
    } else {
      banner.value = { type: 'error', title: `编排不可执行（code=${e?.code ?? '?'}）`, description: e?.message || '未知错误' }
      try {
        const raw = selectedJob.value?.steps_json
        if (raw) draft.value = raw
      } catch {
        /* 忽略 */
      }
    }
  } finally {
    loading.value = false
  }
}

async function validateDraft() {
  validating.value = true
  draftResult.value = null
  try {
    const res = await validateJobDag({ steps_json: draft.value })
    draftResult.value = { ok: true, title: '校验通过：可以保存并下发给节点执行', description: describe(res) }
    ElMessage.success('编排校验通过')
  } catch (e) {
    // 内联展示具体原因（哪个步骤、哪个环）：只弹 toast 会把定位信息丢掉
    draftResult.value = { ok: false, title: `校验未通过（code=${e?.code ?? '?'}）`, description: e?.message || '未知错误' }
  } finally {
    validating.value = false
  }
}

function fillFromSelectedJob() {
  draft.value = selectedJob.value?.steps_json || ''
}

function fillDiamond() {
  draft.value = JSON.stringify(
    {
      max_parallel: 2,
      steps: [
        { index: 0, name: 'extract', command: '/opt/extract.sh' },
        { index: 1, name: 'transform', command: '/opt/transform.sh', depends_on: '0' },
        { index: 2, name: 'enrich', command: '/opt/enrich.sh', depends_on: '0' },
        { index: 3, name: 'load', command: '/opt/load.sh', depends_on: '1,2', join: 'all_success' },
      ],
    },
    null,
    2,
  )
}

onMounted(loadJobs)
</script>

<style scoped>
.page {
  padding: 16px;
}
.head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 12px;
}
.title {
  font-size: 16px;
  font-weight: 600;
}
.sub {
  margin-top: 6px;
  color: #909399;
  font-size: 12px;
  line-height: 1.6;
}
.head-actions {
  display: flex;
  gap: 8px;
}
.filters {
  display: flex;
  gap: 8px;
  align-items: center;
}
.meta {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin: 12px 0;
}
.lanes {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.lane-head {
  font-size: 12px;
  color: #606266;
  margin-bottom: 6px;
}
.lane-body {
  display: flex;
  gap: 12px;
  flex-wrap: wrap;
}
.node {
  min-width: 240px;
  max-width: 320px;
  border: 1px solid #e4e7ed;
  border-left: 3px solid #409eff;
  border-radius: 6px;
  padding: 8px 10px;
  background: #fafafa;
}
.node.warn {
  border-left-color: #e6a23c;
}
.node-head {
  display: flex;
  align-items: center;
  gap: 6px;
  font-weight: 600;
}
.node-idx {
  color: #909399;
  font-size: 12px;
}
.node-name {
  font-size: 13px;
}
.node-cmd {
  margin-top: 6px;
  font-family: Consolas, Monaco, monospace;
  font-size: 12px;
  color: #303133;
  word-break: break-all;
}
.node-meta {
  margin-top: 6px;
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  color: #909399;
  font-size: 11px;
}
.node-warn {
  margin-top: 4px;
  color: #e6a23c;
  font-size: 11px;
}
.lane-arrow {
  color: #c0c4cc;
  text-align: center;
}
.draft-actions {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}
</style>