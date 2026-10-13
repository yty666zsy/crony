<template>
  <div class="page">
    <el-card shadow="never">
      <template #header>
        <div class="head">
          <div>
            <div class="title">
              评测台（回放已录轨迹）
              <MockBadge v-if="USE_MOCK" text="Mock 评测台数据" />
            </div>
            <div class="sub">
              对库里 <strong>已完成</strong> 的 Agent 会话回放一组<strong>确定性断言</strong>：
              期望值全部可机械判定，<strong>没有 LLM 评委</strong>，所以没有模型凭据也能真跑、真出结论。
              只回放 <code>status=completed</code> 的会话；匹配不到会话的样本记为
              <strong>skipped</strong> 并写明原因（既不算通过也不算失败，也不进评分分母）。
            </div>
          </div>
          <div class="head-actions">
            <el-button :loading="loading" @click="loadAll">刷新</el-button>
            <el-button :loading="syncing" @click="onSync">同步种子集</el-button>
            <el-button type="primary" :loading="running" @click="onRun">跑一次回放</el-button>
          </div>
        </div>
      </template>

      <el-alert
        v-if="banner"
        :type="banner.type"
        :title="banner.title"
        :description="banner.description"
        :closable="false"
        show-icon
        class="banner"
      />

      <el-descriptions :column="4" border size="small">
        <el-descriptions-item label="集合版本">{{ seedVersion || '—' }}</el-descriptions-item>
        <el-descriptions-item label="样本">
          {{ caseTotal }} 条（启用 {{ caseEnabled }} 条）
        </el-descriptions-item>
        <el-descriptions-item label="模式">replay（回放历史轨迹）</el-descriptions-item>
        <el-descriptions-item label="是否调用模型">否（回放不碰模型）</el-descriptions-item>
      </el-descriptions>
      <div v-if="note" class="dim tip">{{ note }}</div>
    </el-card>

    <el-card shadow="never">
      <template #header>
        <div class="head">
          <div class="title">回放报告</div>
          <div class="head-actions">
            <el-select
              v-model="selectedRunId"
              size="small"
              style="width: 260px"
              placeholder="选一次跑批"
              @change="onPickRun"
            >
              <el-option v-for="r in runs" :key="r.run_id" :label="runLabel(r)" :value="r.run_id" />
            </el-select>
          </div>
        </div>
      </template>

      <el-empty v-if="!report" description="还没有跑批记录：点右上角「跑一次回放」" />
      <template v-else>
        <el-descriptions :column="6" border size="small" class="sum">
          <el-descriptions-item label="run_id">{{ report.run_id }}</el-descriptions-item>
          <el-descriptions-item label="触发">{{ report.trigger }}</el-descriptions-item>
          <el-descriptions-item label="样本">{{ report.cases }}</el-descriptions-item>
          <el-descriptions-item label="通过/失败/跳过">
            <span class="ok">{{ report.passed }}</span> /
            <span :class="{ bad: report.failed > 0 }">{{ report.failed }}</span> /
            <span class="dim">{{ report.skipped }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="判定次数">{{ report.sessions }}</el-descriptions-item>
          <el-descriptions-item label="得分">{{ fmtScore(report.score) }}</el-descriptions-item>
          <el-descriptions-item label="开始时间">{{ fmtTs(report.started_at) }}</el-descriptions-item>
          <el-descriptions-item label="耗时">{{ report.duration_ms }} ms</el-descriptions-item>
          <el-descriptions-item label="集合版本">{{ report.set_version }}</el-descriptions-item>
        </el-descriptions>
        <div class="dim tip">
          评分分母 = 通过 + 失败（跳过的样本不进分母）；计数必须自洽
          （通过 + 失败 + 跳过 = 样本数），否则报告不可信。
        </div>

        <el-table :data="report.results || []" size="small" row-key="case_key">
          <el-table-column type="expand">
            <template #default="{ row }">
              <div class="detail">
                <div class="detail-title">样本级断言（同一条断言所有会话都过才算过）</div>
                <el-table :data="row.checks || []" size="small">
                  <el-table-column prop="name" label="断言" width="160" />
                  <el-table-column label="结果" width="80">
                    <template #default="s">
                      <el-tag :type="s.row.passed ? 'success' : 'danger'" size="small">
                        {{ s.row.passed ? '通过' : '失败' }}
                      </el-tag>
                    </template>
                  </el-table-column>
                  <el-table-column prop="detail" label="明细" />
                </el-table>

                <template v-if="(row.per_session || []).length">
                  <div class="detail-title">
                    逐会话判定（共 {{ row.per_session.length }} 个，下面只列不通过的）
                  </div>
                  <el-table :data="failedSessions(row)" size="small">
                    <el-table-column prop="session_id" label="会话" width="250" />
                    <el-table-column label="失败断言明细">
                      <template #default="s">{{ failedChecks(s.row) }}</template>
                    </el-table-column>
                  </el-table>
                  <div v-if="failedSessions(row).length === 0" class="dim">
                    该样本下所有会话均通过。
                  </div>
                </template>
              </div>
            </template>
          </el-table-column>
          <el-table-column prop="case_key" label="样本" width="230" />
          <el-table-column prop="scenario" label="场景" show-overflow-tooltip />
          <el-table-column label="判定" width="90">
            <template #default="{ row }">
              <el-tag :type="statusType(row.status)" size="small">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="得分" width="80">
            <template #default="{ row }">{{ fmtScore(row.score) }}</template>
          </el-table-column>
          <el-table-column label="回放会话" width="90">
            <template #default="{ row }">{{ (row.sessions || []).length }}</template>
          </el-table-column>
          <el-table-column prop="reason" label="原因/说明" show-overflow-tooltip />
        </el-table>

        <div v-if="(report.notes || []).length" class="dim tip">
          <div v-for="(n, i) in report.notes" :key="i">· {{ n }}</div>
        </div>
      </template>
    </el-card>

    <el-card shadow="never">
      <template #header><div class="title">样本（期望值可机械判定）</div></template>
      <el-table :data="cases" size="small">
        <el-table-column prop="case_key" label="case_key" width="230" />
        <el-table-column prop="scenario" label="场景" show-overflow-tooltip />
        <el-table-column label="会话前缀" width="130">
          <template #default="{ row }">
            <span class="mono">{{ row.session_prefix || '（全部）' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="断言" width="300">
          <template #default="{ row }">
            <el-tag
              v-for="c in expectChips(row.expect_json)"
              :key="c"
              size="small"
              effect="plain"
              class="ml"
            >
              {{ c }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="集合" width="150">
          <template #default="{ row }">
            <span class="dim">{{ row.set_name }} / {{ row.set_version }}</span>
          </template>
        </el-table-column>
        <el-table-column label="启用" width="80">
          <template #default="{ row }">
            <el-tag :type="row.enabled ? 'success' : 'info'" size="small" effect="plain">
              {{ row.enabled ? '启用' : '停用' }}
            </el-tag>
          </template>
        </el-table-column>
      </el-table>
      <div class="dim tip">
        启停状态<strong>以库为准</strong>（本页只读展示）：同步种子集不会把运维停用的样本悄悄打开。
        代码里已删除的种子样本会在同步时一并下线（见同步返回的 removed）。
      </div>
    </el-card>
  </div>
</template>

<script setup>
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import MockBadge from '@/components/MockBadge.vue'
import { USE_MOCK } from '@/api/client'
import {
  getEvalReport,
  listEvalCases,
  listEvalRuns,
  runEvalBench,
  syncEvalSeed,
} from '@/api/ops'

const loading = ref(false)
const syncing = ref(false)
const running = ref(false)

const cases = ref([])
const caseTotal = ref(0)
const caseEnabled = ref(0)
const seedVersion = ref('')
const note = ref('')

const runs = ref([])
const report = ref(null)
const selectedRunId = ref(null)
const banner = ref(null)

const asList = (v) => (Array.isArray(v) ? v : [])
const fmtScore = (s) => (typeof s === 'number' ? s.toFixed(3) : '—')
const fmtTs = (sec) => (sec ? new Date(sec * 1000).toLocaleString() : '—')

const statusType = (s) => {
  switch (s) {
    case 'passed':
      return 'success'
    case 'failed':
      return 'danger'
    default:
      return 'info' // skipped：没考到，不是通过也不是失败
  }
}

const runLabel = (r) =>
  `#${r.run_id} · ${fmtTs(r.started_at)} · 通过 ${r.passed} / 失败 ${r.failed} / 跳过 ${r.skipped} · ${fmtScore(r.score)}`

// 期望值摘要：把 expect_json 里"可机械判定"的项翻成人话。
// JSON 解析失败时明确说出来（历史手写样本可能是坏 JSON），不假装没有断言。
const expectChips = (expectJson) => {
  try {
    const e = JSON.parse(expectJson || '{}')
    const out = []
    if (e.forbid_write) out.push('禁写')
    if (Array.isArray(e.forbid_tools) && e.forbid_tools.length) out.push('禁工具 ' + e.forbid_tools.join('/'))
    if (Array.isArray(e.required_tools) && e.required_tools.length) out.push('需工具 ' + e.required_tools.join('/'))
    if (e.max_steps) out.push(`步数≤${e.max_steps}`)
    if (!out.length) out.push('（无可判定的期望值）')
    return out
  } catch (e) {
    return ['期望值不是合法 JSON']
  }
}

const failedSessions = (row) => asList(row.per_session).filter((p) => !p.passed)
const failedChecks = (s) =>
  asList(s.checks)
    .filter((c) => !c.passed)
    .map((c) => `${c.name}：${c.detail}`)
    .join('；') || '—'

async function loadCases() {
  const d = await listEvalCases('')
  cases.value = asList(d.list)
  caseTotal.value = Number(d.total) || cases.value.length
  caseEnabled.value = Number(d.enabled) || 0
  seedVersion.value = d.seed_version || ''
  note.value = d.note || ''
}

async function loadRuns() {
  const d = await listEvalRuns(20)
  runs.value = asList(d.list)
}

async function loadReport(runId = 0) {
  const d = await getEvalReport(runId)
  report.value = d.report || null
  if (report.value && report.value.run_id != null) selectedRunId.value = report.value.run_id
}

async function loadAll() {
  loading.value = true
  try {
    await Promise.all([loadCases(), loadRuns()])
    await loadReport(0)
  } finally {
    loading.value = false
  }
}

async function onSync() {
  syncing.value = true
  try {
    const d = await syncEvalSeed()
    banner.value = {
      type: 'success',
      title: '种子评测集已同步（幂等，按 case_key）',
      description:
        `新增 ${d.added}、更新 ${d.updated}、下线 ${d.removed}（集合版本 ${d.seed_version}）。` +
        '已存在样本的启停状态以库里的为准；代码里已删除的种子样本会被一并下线。',
    }
    await loadCases()
  } finally {
    syncing.value = false
  }
}

async function onRun() {
  running.value = true
  try {
    const d = await runEvalBench({ trigger: 'manual' })
    const rep = d.report || {}
    const failed = Number(rep.failed) || 0
    banner.value = failed > 0
      ? {
          type: 'warning',
          title: `回放完成：有 ${failed} 条样本不通过（run_id=${rep.run_id}）`,
          description:
            `通过 ${rep.passed} / 失败 ${rep.failed} / 跳过 ${rep.skipped}，` +
            `判定 ${rep.sessions} 次，得分 ${fmtScore(rep.score)}。` +
            '展开下表的样本看是哪条断言、哪个会话不通过——失败是**真信号**，不是环境噪声（回放不碰模型）。',
        }
      : {
          type: 'success',
          title: `回放完成：全部通过（run_id=${rep.run_id}）`,
          description:
            `通过 ${rep.passed} / 失败 ${rep.failed} / 跳过 ${rep.skipped}，` +
            `判定 ${rep.sessions} 次，得分 ${fmtScore(rep.score)}。` +
            '注意：跳过不代表通过——它意味着这一轮没有可比对的历史会话。',
        }
    await loadRuns()
    await loadReport(rep.run_id || 0)
    ElMessage.success(`回放完成：run_id=${rep.run_id}`)
  } finally {
    running.value = false
  }
}

function onPickRun(id) {
  loadReport(id)
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
.head-actions {
  display: flex;
  gap: 8px;
  flex-shrink: 0;
  align-items: center;
}
.banner {
  margin-bottom: 12px;
}
.sum {
  margin-bottom: 8px;
}
.dim {
  color: #909399;
  font-size: 11px;
}
.tip {
  margin-top: 8px;
  line-height: 1.7;
}
.ml {
  margin-left: 4px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 11px;
}
.ok {
  color: #67c23a;
}
.bad {
  color: #f56c6c;
  font-weight: 600;
}
.detail {
  padding: 8px 12px 4px;
}
.detail-title {
  font-size: 12px;
  font-weight: 600;
  color: #606266;
  margin: 8px 0 6px;
}
</style>
