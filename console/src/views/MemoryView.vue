<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  disableMemory,
  enableMemory,
  findMemory,
  memoryStats,
  recallMemories,
  searchMemories,
} from '@/api/memory'
import { formatRelative, formatTime } from '@/utils/format'

/**
 * /memory —— Agent 经验库（记忆）（阶段 4-A）。
 *
 * 这一页回答三个问题，顺序不能乱：
 *
 *  1. **Agent 到底记住了什么？**
 *     列表把每条经验摊开成「症状 → 当时做了什么 → 结果如何（出现几次）」。
 *     不摊开的话，"经验库"就是个黑盒，人没法判断模型是不是被一条过期的经验带偏。
 *
 *  2. **这条经验会被用在什么样的症状上？**
 *     「试召回」输入一段症状，看命中了哪些条目、相似度多少、命中了哪些词。
 *     **返回空是正确答案**：它表示"没有足够相似的历史"，不是"查询失败"。
 *     这是排查"为什么这条经验没被用上"的唯一手段。
 *
 *  3. **坏经验怎么撤下来？**
 *     只有停用（必须写原因），**没有编辑**。原因见下面 MEMORY_RULES——
 *     经验库是提示词的输入，能改它的人就能影响模型行为，等价于提示词注入；
 *     所以治理动作只允许"撤下 + 记录为什么"，而不是"改一改继续用"。
 *
 * 三个必须照着后端渲染、不许前端自己编的地方：
 *  - 结果的区分：blocked/denied（**没执行**）与 failed（执行了但失败）是两件事，
 *    成功率的分母只含后者；
 *  - 成功率三态：没有执行样本时显示"无数据"，**绝不能显示 0%**；
 *  - 召回结果上的"仅线索、不是证据"标识，文案用后端给的 notice，
 *    保证这个约束在所有出口只有一种说法。
 */

// 业务码（utils/dict.js 的 BIZ_CODE 是「码 → 文案」，没有符号常量）：
//  1001 参数错误（这里是"停用没写原因"）；1019 条目不存在；1020 状态已变更。
const PARAM_ERROR = 1001
const MEMORY_NOT_FOUND = 1019
const MEMORY_CONFLICT = 1020

/** 后端 source 块里的约束（机器可读）——页面直接渲染，不自己拼这句中文。 */
const MEMORY_RULES = [
  { key: 'advisory_only', text: '经验只作排查线索，不计入本次结论的证据' },
  { key: 'editable', text: '不提供编辑：坏经验只能停用（撤下），不能改掉' },
  { key: 'disable_requires_reason', text: '停用必须写原因：撤下动作要可复盘' },
]

const loading = ref(false)
const statsLoading = ref(false)
const rows = ref([])
const total = ref(0)
const source = ref(null)
const stats = ref(null)
const busy = ref('')
const detail = reactive({ visible: false, row: null })

const query = reactive({
  outcome: '',
  action_kind: '',
  tool: '',
  target_node: '',
  keyword: '',
  enabled: '', // '' 全部 / true 启用 / false 已停用（后端是 *bool 三态）
  page: 1,
  page_size: 10,
})

const outcomeOptions = [
  { value: '', label: '全部结果' },
  { value: 'success', label: '成功' },
  { value: 'failed', label: '执行了但失败' },
  { value: 'blocked', label: '未执行（转人工/复核未过）' },
  { value: 'denied', label: '未执行（被闸门拒绝）' },
  { value: 'advised', label: '只给建议' },
]

/** outcome → 标签颜色。刻意让"没执行"两态是 info（灰），不要染成红色的"失败"。 */
const outcomeColor = {
  success: 'success',
  failed: 'danger',
  blocked: 'warning',
  denied: 'info',
  advised: 'info',
}

const enabledOptions = [
  { value: '', label: '全部' },
  { value: true, label: '启用中' },
  { value: false, label: '已停用' },
]

// ---------- 召回（试召回抽屉） ----------
const recall = reactive({
  visible: false,
  loading: false,
  symptom: '',
  agent_kind: 'diagnose',
  target_node: '',
  top_k: 5,
  min_score: 0.34,
  result: null,
})

async function load() {
  loading.value = true
  try {
    const res = await searchMemories({
      outcome: query.outcome || undefined,
      action_kind: query.action_kind || undefined,
      tool: query.tool || undefined,
      target_node: query.target_node || undefined,
      keyword: query.keyword || undefined,
      // 三态：'' 表示不传（后端按"全部"处理）
      enabled: query.enabled === '' ? undefined : query.enabled,
      page: query.page,
      page_size: query.page_size,
    })
    rows.value = res?.list || []
    total.value = res?.total || 0
    source.value = res?.source || null
  } catch {
    /* 业务错误由 client.js 统一提示，这里保持列表为空 */
  } finally {
    loading.value = false
  }
}

async function loadStats() {
  statsLoading.value = true
  try {
    stats.value = await memoryStats()
  } catch {
    stats.value = null
  } finally {
    statsLoading.value = false
  }
}

function onSearch() {
  query.page = 1
  load()
}

function reset() {
  query.outcome = ''
  query.action_kind = ''
  query.tool = ''
  query.target_node = ''
  query.keyword = ''
  query.enabled = ''
  onSearch()
}

function onPage(page) {
  query.page = page
  load()
}

async function openDetail(row) {
  detail.row = row
  detail.visible = true
  // 详情以**接口**为准（列表可能是上一页的快照）：经验详情里有更长的结果原文。
  try {
    const res = await findMemory(row.memory_id)
    if (res?.memory) detail.row = res.memory
  } catch (err) {
    if (err?.code === MEMORY_NOT_FOUND) {
      ElMessage.info('这条经验已经不在了（可能已停用或数据被清理），已刷新列表')
      detail.visible = false
      await load()
    }
  }
}

/**
 * 停用：原因**必填**（后端也会拒空原因，这里提前拦住是为了少一次无效往返，
 * 而不是把校验只放在前端）。
 */
async function onDisable(row) {
  let reason = ''
  try {
    const input = await ElMessageBox.prompt(
      '停用后这条经验**不再参与召回**（条目本身保留，历史仍可复盘）。\n请写清楚为什么撤下它——将来有人问"模型怎么不知道这件事"时，这条原因就是答案。',
      '停用经验',
      {
        confirmButtonText: '停用',
        cancelButtonText: '取消',
        inputPlaceholder: '例：该节点已下线，这条经验会误导模型去查一台不存在的机器',
        inputValue: '',
        inputValidator: (v) => (String(v || '').trim().length >= 4 ? true : '必须填写原因（至少 4 个字）'),
        type: 'warning',
        dangerouslyUseHTMLString: false,
      },
    )
    reason = String(input.value || '').trim()
  } catch {
    return // 用户取消
  }

  busy.value = row.memory_id
  try {
    await disableMemory(row.memory_id, reason)
    ElMessage.success('已停用：该经验不再参与召回（未删除，历史仍可复盘）')
    detail.visible = false
    await Promise.all([load(), loadStats()])
  } catch (err) {
    if (err?.code === MEMORY_CONFLICT) {
      ElMessage.info('这条经验的状态已经变了（可能已停用），已为你刷新')
      await load()
    } else if (err?.code === PARAM_ERROR) {
      ElMessage.warning('停用必须填写原因')
    }
    // 1019 等其它码由 client.js 统一提示
  } finally {
    busy.value = ''
  }
}

async function onEnable(row) {
  busy.value = row.memory_id
  try {
    await enableMemory(row.memory_id)
    ElMessage.success('已重新启用：会重新参与召回')
    detail.visible = false
    await Promise.all([load(), loadStats()])
  } catch (err) {
    if (err?.code === MEMORY_CONFLICT) {
      ElMessage.info('这条经验已经是启用状态，已为你刷新')
      await load()
    }
  } finally {
    busy.value = ''
  }
}

// ---------- 试召回 ----------
function openRecall() {
  recall.visible = true
  recall.result = null
}

async function runRecall() {
  const symptom = String(recall.symptom || '').trim()
  if (!symptom) {
    ElMessage.warning('请先输入当时的症状（没有症状就无从召回）')
    return
  }
  recall.loading = true
  try {
    recall.result = await recallMemories({
      symptom,
      agent_kind: recall.agent_kind || undefined,
      target_node: recall.target_node || undefined,
      top_k: recall.top_k,
      min_score: recall.min_score,
    })
  } catch {
    recall.result = null
  } finally {
    recall.loading = false
  }
}

/** 命中的相似度条（百分比）：只做视觉提示，数值本身来自后端。 */
function scorePercent(score) {
  return `${Math.round(Number(score || 0) * 100)}%`
}

const recallHits = computed(() => recall.result?.hits || [])

const enabledCount = computed(() => stats.value?.enabled ?? null)
const disabledCount = computed(() => stats.value?.disabled ?? null)

/** 计数构成文案：「出现 6 次（成功 5 / 失败 1）」。让 outcome 只是"最近一次"这件事看得见。 */
function compositionText(row) {
  const parts = []
  if (row.success_count) parts.push(`成功 ${row.success_count}`)
  if (row.fail_count) parts.push(`失败 ${row.fail_count}`)
  if (row.blocked_count) parts.push(`未执行(转人工) ${row.blocked_count}`)
  if (row.denied_count) parts.push(`未执行(被拒) ${row.denied_count}`)
  if (row.advised_count) parts.push(`只建议 ${row.advised_count}`)
  const n = Number(row.occurrence_count || 0)
  if (!parts.length) return `出现 ${n} 次`
  return `出现 ${n} 次（${parts.join(' / ')}）`
}

onMounted(() => {
  load()
  loadStats()
})
</script>

<template>
  <div class="page">
    <el-card shadow="never">
      <template #header>
        <div class="head">
          <div>
            <div class="title">Agent 经验库（记忆）</div>
            <div class="sub">
              Agent 每次会话结束后，把「症状 → 当时做了什么 → 结果如何」沉淀成一条经验；
              下次遇到相似症状时**只作为线索**注入提示词。
              <b>经验不是证据</b>：模型引用经验条目号会被判为"未观测"。本页没有编辑功能——坏经验只能停用。
            </div>
          </div>
          <div class="head-right">
            <el-tag v-if="source && source.advisory_only" type="info" effect="dark">仅线索 · 非证据</el-tag>
            <el-button size="small" @click="openRecall">试召回</el-button>
            <el-button size="small" :loading="loading || statsLoading" @click="() => { load(); loadStats() }">刷新</el-button>
          </div>
        </div>
      </template>

      <div v-if="source" class="rules">
        <el-tag v-for="r in MEMORY_RULES" :key="r.key" size="small" effect="plain" :type="r.key === 'advisory_only' ? 'info' : 'warning'">
          {{ r.text }}
        </el-tag>
      </div>

      <div class="stats">
        <div class="stat">
          <div class="stat-label">经验条目</div>
          <div class="stat-value">{{ stats ? stats.total : '—' }}</div>
          <div class="stat-sub">启用 {{ enabledCount ?? '—' }} / 停用 {{ disabledCount ?? '—' }}</div>
        </div>
        <div class="stat">
          <div class="stat-label">成功率（只算执行过的动作）</div>
          <div class="stat-value">{{ stats && stats.success_rate_known ? stats.success_rate_text : '无执行样本' }}</div>
          <div class="stat-sub">
            成功 {{ stats?.success_sum ?? '—' }} / 失败 {{ stats?.fail_sum ?? '—' }}；
            被拒与未执行**不计入分母**
          </div>
        </div>
        <div class="stat">
          <div class="stat-label">结果分布（最近一次口径）</div>
          <div class="stat-value small">
            <template v-if="stats">
              <span v-for="(v, k) in stats.by_outcome" :key="k" class="chip">{{ k }} {{ v }}</span>
            </template>
            <template v-else>—</template>
          </div>
          <div class="stat-sub">单条的 outcome 只是最近一次，全部历史看"计数构成"</div>
        </div>
      </div>

      <div class="filters">
        <el-select v-model="query.outcome" size="small" style="width: 190px" @change="onSearch">
          <el-option v-for="o in outcomeOptions" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
        <el-select v-model="query.action_kind" size="small" style="width: 150px" @change="onSearch">
          <el-option label="全部类型" value="" />
          <el-option label="工具动作" value="tool" />
          <el-option label="只给建议" value="advice" />
        </el-select>
        <el-select v-model="query.enabled" size="small" style="width: 130px" @change="onSearch">
          <el-option v-for="o in enabledOptions" :key="String(o.value)" :label="o.label" :value="o.value" />
        </el-select>
        <el-input v-model="query.tool" size="small" placeholder="工具名" clearable style="width: 160px" @keyup.enter="onSearch" />
        <el-input v-model="query.target_node" size="small" placeholder="目标节点 UUID" clearable style="width: 170px" @keyup.enter="onSearch" />
        <el-input v-model="query.keyword" size="small" placeholder="关键词（症状/动作/结果）" clearable style="width: 220px" @keyup.enter="onSearch" />
        <el-button size="small" type="primary" @click="onSearch">查询</el-button>
        <el-button size="small" @click="reset">重置</el-button>
      </div>

      <el-table v-loading="loading" :data="rows" size="small" style="width: 100%" @row-click="openDetail">
        <el-table-column label="状态" width="96">
          <template #default="{ row }">
            <el-tag :type="row.enabled ? 'success' : 'info'" size="small" effect="plain">
              {{ row.enabled ? '启用中' : '已停用' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="结果" width="190">
          <template #default="{ row }">
            <el-tag :type="outcomeColor[row.outcome] || 'info'" size="small">{{ row.outcome_text || row.outcome }}</el-tag>
            <div class="muted">{{ compositionText(row) }}</div>
          </template>
        </el-table-column>
        <el-table-column label="症状（什么时候用得上）" min-width="260">
          <template #default="{ row }">
            <div class="symptom">{{ row.symptom }}</div>
            <div class="muted">{{ row.action_kind === 'advice' ? '只给建议，没有动作' : row.action_text || '—' }}</div>
          </template>
        </el-table-column>
        <el-table-column label="成功率" width="150">
          <template #default="{ row }">
            <!-- 三态：没有执行样本时显示"无数据"，绝不显示 0% -->
            <span v-if="row.success_rate_known">{{ row.success_rate_text || `${(row.success_rate * 100).toFixed(1)}%` }}</span>
            <span v-else class="muted">无执行样本</span>
          </template>
        </el-table-column>
        <el-table-column label="目标节点" width="120" prop="target_node" />
        <el-table-column label="最近出现" width="130">
          <template #default="{ row }">{{ formatRelative(row.last_seen) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="110" fixed="right">
          <template #default="{ row }">
            <el-button
              v-if="row.enabled"
              size="small"
              text
              type="warning"
              :loading="busy === row.memory_id"
              @click.stop="onDisable(row)"
            >停用</el-button>
            <el-button
              v-else
              size="small"
              text
              type="primary"
              :loading="busy === row.memory_id"
              @click.stop="onEnable(row)"
            >启用</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div class="pager">
        <el-pagination
          layout="total, prev, pager, next"
          :total="total"
          :current-page="query.page"
          :page-size="query.page_size"
          @current-change="onPage"
        />
      </div>
    </el-card>

    <!-- ---------- 详情抽屉 ---------- -->
    <el-drawer v-model="detail.visible" title="经验详情" size="520px">
      <div v-if="detail.row" class="detail">
        <div class="detail-row"><span class="k">条目号</span><span class="v mono">{{ detail.row.memory_id }}</span></div>
        <div class="detail-row"><span class="k">来源会话</span><span class="v mono">{{ detail.row.session_id || '—' }}</span></div>
        <div class="detail-row"><span class="k">目标节点</span><span class="v mono">{{ detail.row.target_node || '—' }}</span></div>
        <div class="detail-row"><span class="k">结果</span>
          <span class="v">
            <el-tag :type="outcomeColor[detail.row.outcome] || 'info'" size="small">{{ detail.row.outcome_text }}</el-tag>
            <span class="muted"> {{ compositionText(detail.row) }}</span>
          </span>
        </div>
        <div class="detail-row"><span class="k">成功率</span>
          <span class="v">
            {{ detail.row.success_rate_known ? detail.row.success_rate_text : '无执行样本（被拒/未执行不计入成功率）' }}
          </span>
        </div>
        <div class="detail-row"><span class="k">症状</span><span class="v">{{ detail.row.symptom }}</span></div>
        <div class="detail-row"><span class="k">当时动作</span><span class="v">{{ detail.row.action_text || '（只给了建议）' }}</span></div>
        <div class="detail-row"><span class="k">结果摘要</span><span class="v">{{ detail.row.result_text || '—' }}</span></div>
        <div class="detail-row" v-if="detail.row.result_detail"><span class="k">结果原文</span><span class="v code">{{ detail.row.result_detail }}</span></div>
        <div class="detail-row"><span class="k">分词</span><span class="v mono small-text">{{ detail.row.symptom_tokens || '—' }}</span></div>
        <div class="detail-row"><span class="k">最近出现</span><span class="v">{{ formatTime(detail.row.last_seen) }}（{{ formatRelative(detail.row.last_seen) }}）</span></div>
        <div class="detail-row" v-if="!detail.row.enabled">
          <span class="k">停用审计</span>
          <span class="v">
            {{ detail.row.disabled_reason || '（未记录原因）' }}
            <div class="muted">由 {{ detail.row.disabled_by || '未知' }} 于 {{ formatTime(detail.row.disabled_at) }} 停用</div>
          </span>
        </div>
        <el-alert
          type="info"
          :closable="false"
          show-icon
          title="这条经验只会作为线索注入提示词，不计入任何结论的证据"
          description="模型若在结论里引用它（mem- 开头的条目号），会被判定为「未观测」并降级为需人工复核。"
        />
        <div class="detail-actions">
          <el-button v-if="detail.row.enabled" type="warning" :loading="busy === detail.row.memory_id" @click="onDisable(detail.row)">
            停用（需写原因）
          </el-button>
          <el-button v-else type="primary" :loading="busy === detail.row.memory_id" @click="onEnable(detail.row)">
            重新启用
          </el-button>
        </div>
      </div>
    </el-drawer>

    <!-- ---------- 试召回抽屉 ---------- -->
    <el-drawer v-model="recall.visible" title="试召回：这条经验会被用在什么症状上？" size="620px">
      <div class="recall-form">
        <el-input
          v-model="recall.symptom"
          type="textarea"
          :rows="3"
          placeholder="输入当时的症状，例如：诊断任务 18（备份）本次失败的原因 mysqldump 鉴权失败 1045"
        />
        <div class="recall-row">
          <el-input v-model="recall.target_node" size="small" placeholder="目标节点 UUID（可选，同节点会加权）" style="width: 260px" />
          <el-input-number v-model="recall.top_k" size="small" :min="1" :max="20" />
          <el-input-number v-model="recall.min_score" size="small" :min="0" :max="1" :step="0.02" :precision="2" />
          <el-button size="small" type="primary" :loading="recall.loading" @click="runRecall">召回</el-button>
        </div>
        <div class="muted">
          最低相似度默认 0.34；返回空列表是**正确答案**（没有足够相似的历史），不是查询失败。
        </div>
      </div>

      <template v-if="recall.result">
        <el-alert
          class="mt"
          type="info"
          :closable="false"
          show-icon
          :title="recall.result.notice || '经验只作排查线索，不计入证据'"
        />
        <div class="muted mt">
          比较了 {{ recall.result.considered }} 条启用中的经验，命中 {{ recallHits.length }} 条；
          查询词元 {{ (recall.result.query_tokens || []).length }} 个。
        </div>

        <el-empty v-if="!recallHits.length" description="没有足够相似的历史经验（这是正确答案）" />

        <div v-for="h in recallHits" :key="h.memory_id" class="hit">
          <div class="hit-head">
            <span class="mono">{{ h.memory_id }}</span>
            <el-tag size="small" :type="h.is_failed ? 'danger' : 'success'" effect="dark">
              相似度 {{ scorePercent(h.score) }}
            </el-tag>
            <el-tag v-if="h.same_kind" size="small" effect="plain">同类任务</el-tag>
            <el-tag v-if="h.same_target" size="small" effect="plain">同节点</el-tag>
          </div>
          <div class="hit-symptom">{{ h.symptom }}</div>
          <div class="muted">当时动作：{{ h.action_text || '（只给建议）' }}｜结果：{{ h.outcome_text }}｜{{ compositionText(h) }}</div>
          <div class="muted">
            成功率：{{ h.success_rate_known ? h.success_rate_text : '无执行样本' }}
          </div>
          <div class="muted small-text">
            命中词元：{{ (h.matched || []).join('、') || '—' }}
            <template v-if="(h.reasons || []).length">｜加权：{{ h.reasons.join('、') }}</template>
          </div>
        </div>
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
  max-width: 780px;
}
.rules {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 12px;
}
.stats {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 12px;
  margin-bottom: 12px;
}
.stat {
  padding: 10px 12px;
  border: 1px solid rgba(124, 138, 168, 0.18);
  border-radius: 6px;
}
.stat-label {
  font-size: 12px;
  color: #7c8aa8;
}
.stat-value {
  margin-top: 4px;
  font-size: 18px;
  font-weight: 600;
}
.stat-value.small {
  font-size: 13px;
  font-weight: 500;
}
.stat-sub {
  margin-top: 4px;
  font-size: 11px;
  color: #7c8aa8;
}
.chip {
  display: inline-block;
  margin: 0 6px 4px 0;
  padding: 1px 6px;
  border-radius: 4px;
  background: rgba(124, 138, 168, 0.12);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}
.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.symptom {
  font-size: 13px;
  line-height: 1.45;
}
.muted {
  color: #7c8aa8;
  font-size: 12px;
}
.small-text {
  font-size: 11px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
  word-break: break-all;
}
.code {
  display: block;
  margin: 4px 0 0;
  padding: 8px;
  background: rgba(124, 138, 168, 0.08);
  border-radius: 4px;
  white-space: pre-wrap;
  word-break: break-all;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 12px;
}
.detail {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.detail-row {
  display: flex;
  gap: 10px;
  font-size: 13px;
}
.detail-row .k {
  flex: 0 0 78px;
  color: #7c8aa8;
}
.detail-row .v {
  flex: 1;
  word-break: break-all;
}
.detail-actions {
  display: flex;
  gap: 8px;
  margin-top: 6px;
}
.recall-form {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.recall-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.mt {
  margin-top: 12px;
}
.hit {
  margin-top: 12px;
  padding: 10px 12px;
  border: 1px solid rgba(124, 138, 168, 0.18);
  border-radius: 6px;
}
.hit-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.hit-symptom {
  font-size: 13px;
  margin-bottom: 4px;
}
</style>