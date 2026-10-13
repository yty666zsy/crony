<template>
  <div class="page">
    <el-card shadow="never">
      <template #header>
        <div class="head">
          <div>
            <div class="title">运维智能化</div>
            <div class="sub">
              调度智能化（多目标加权打分）、SLO 契约、指标看板、自治信用（灰度与自动降级）、
              放量档位（证据驱动的平台级上限）、混沌考核、影子对账。这里以<strong>只读/计算</strong>为主：
              能动的按钮只有"记录一次动作结果"（推进<strong>信任分</strong>）与"收紧/加宽放量档位"
              （改的是<strong>能不能自动动手的上限</strong>）——两者都不直接触发生产动作。
            </div>
          </div>
          <el-button :loading="loading" @click="loadAll">刷新</el-button>
        </div>
      </template>

      <el-row :gutter="12">
        <el-col v-for="c in cards" :key="c.label" :span="6">
          <div class="kpi">
            <div class="kpi-label">{{ c.label }}</div>
            <div class="kpi-value">{{ c.value }}</div>
          </div>
        </el-col>
      </el-row>

      <div class="dim" style="margin-top: 14px">
        方案指标（近 {{ metrics?.metrics_window_days ?? 7 }} 天）——比率的"无样本"与"0%"是两回事：
        没有观测到样本时一律显示<strong>无样本</strong>，不显示 0%。
      </div>
      <el-row :gutter="12" style="margin-top: 8px">
        <el-col v-for="c in planCards" :key="c.label" :span="6">
          <div class="kpi">
            <div class="kpi-label">{{ c.label }}</div>
            <div class="kpi-value">{{ c.value }}</div>
            <div class="kpi-sub">{{ c.sub }}</div>
          </div>
        </el-col>
      </el-row>

      <div v-if="loadIssues.length" class="dim">
        ⚠ 本次刷新有区块未拿到数据：{{ loadIssues.join('、') }}（其余区块已正常显示；原因见右下角提示）
      </div>
    </el-card>

    <el-card shadow="never">
      <template #header><div class="title">多目标调度解释（只读：不真正改派）</div></template>
      <div class="row">
        <el-input v-model.number="allocJobId" placeholder="任务 ID" style="width: 160px" />
        <el-button type="primary" @click="doAllocate">解释分配</el-button>
        <span class="dim">硬约束不满足即拒绝；无可用节点时宁可不派发。</span>
      </div>
      <el-table v-if="alloc.scores" :data="alloc.scores" size="small" class="tbl">
        <el-table-column label="节点" width="180">
          <template #default="{ row }">
            <span :class="{ chosen: row.uuid === alloc.chosen }">
              {{ row.uuid }}<span v-if="row.uuid === alloc.chosen" class="badge">选中</span>
            </span>
          </template>
        </el-table-column>
        <el-table-column prop="total" label="总分" width="90" />
        <el-table-column label="分项" min-width="260">
          <template #default="{ row }">
            <span v-for="(v, k) in row.parts" :key="k" class="chip">{{ k }} {{ v }}</span>
          </template>
        </el-table-column>
        <el-table-column label="说明" min-width="200">
          <template #default="{ row }">{{ (row.reasons || []).join('；') }}</template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-card shadow="never">
      <template #header><div class="title">SLO 契约（成功率 / 准时率）</div></template>
      <el-table :data="sloList" size="small" class="tbl">
        <el-table-column prop="name" label="契约" width="180" />
        <el-table-column label="目标" min-width="200">
          <template #default="{ row }">
            成功率 ≥ {{ (row.target_success_rate * 100).toFixed(0) }}% ·
            准时率 ≥ {{ (row.target_on_time_rate * 100).toFixed(0) }}%
          </template>
        </el-table-column>
        <el-table-column label="窗口 / 宽限" width="180">
          <template #default="{ row }">{{ row.window_seconds }}s / {{ row.grace_seconds }}s</template>
        </el-table-column>
        <el-table-column label="状态" width="120">
          <template #default="{ row }">
            <el-tag size="small" :type="row.enabled ? 'success' : 'info'">{{ row.enabled ? '启用' : '停用' }}</el-tag>
          </template>
        </el-table-column>
      </el-table>
      <div class="row">
        <el-button @click="doEvaluateSlo">评估全部契约</el-button>
        <span v-if="sloEval" class="dim">共 {{ sloEval.total }} 条，违约 {{ sloEval.breached }} 条（违约只回报事实，不触发动作）</span>
      </div>
      <el-table v-if="sloEval && sloEval.list && sloEval.list.length" :data="sloEval.list" size="small" class="tbl">
        <el-table-column label="契约" width="160">
          <template #default="{ row }">{{ row.policy.name }}</template>
        </el-table-column>
        <el-table-column label="成功率" width="130">
          <template #default="{ row }">{{ pct(row.report.success_rate) }}</template>
        </el-table-column>
        <el-table-column label="准时率" width="130">
          <template #default="{ row }">{{ pct(row.report.on_time_rate) }}</template>
        </el-table-column>
        <el-table-column label="判定" min-width="220">
          <template #default="{ row }">
            <el-tag v-if="row.report.unverifiable" size="small" type="info">无样本（不判违约）</el-tag>
            <el-tag v-else-if="row.report.breached" size="small" type="danger">{{ (row.report.reasons || []).join('；') }}</el-tag>
            <el-tag v-else size="small" type="success">达标</el-tag>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-card shadow="never">
      <template #header><div class="title">自治信用（连续成功升级 / 一次不良后果降级并冻结）</div></template>
      <el-table :data="autonomy" size="small" class="tbl">
        <el-table-column prop="scope" label="动作@环境" min-width="180" />
        <el-table-column label="等级" width="120">
          <template #default="{ row }">{{ row.level_name }}（生效 {{ row.effective_level }}）</template>
        </el-table-column>
        <el-table-column label="连续成功 / 累计" width="220">
          <template #default="{ row }">{{ row.streak }} / 成功 {{ row.total_up }} · 不良 {{ row.total_down }}</template>
        </el-table-column>
        <el-table-column label="状态" min-width="240">
          <template #default="{ row }">
            <el-tag v-if="row.frozen" size="small" type="warning">已冻结：{{ row.frozen_reason }}</el-tag>
            <el-tag v-else size="small" type="success">正常</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="150">
          <template #default="{ row }">
            <el-button v-if="row.frozen" size="small" @click="doUnfreeze(row.scope)">人工解冻</el-button>
          </template>
        </el-table-column>
      </el-table>
      <div class="row">
        <el-input v-model="obsScope" placeholder="scope（如 job_retry@prod）" style="width: 240px" />
        <el-button size="small" @click="doObserve(true)">记录一次成功</el-button>
        <el-button size="small" @click="doObserve(false)">记录一次不良后果</el-button>
      </div>
      <div class="row">
        <el-input v-model="effTool" placeholder="工具名（如 job_retry）" style="width: 200px" />
        <el-input v-model="effEnv" placeholder="环境（默认 default）" style="width: 160px" />
        <el-button size="small" @click="doEffective">解释生效级别</el-button>
        <span v-if="effResult" class="dim">
          {{ effResult.scope }}：配置上限 {{ effResult.configured_ceiling }} ∩ 信用
          {{ effResult.credit_known ? `${effResult.credit_level}（放行 ${effResult.enforcement_level}）` : '无历史' }}
          → 生效 {{ effResult.effective }}
          （来源 {{ effResult.source }}{{ effResult.frozen ? '，已冻结' : '' }}，信用接入 {{ effResult.credit_enabled ? '开' : '关' }}）
        </span>
      </div>
    </el-card>

    <el-card shadow="never">
      <template #header><div class="title">放量档位（证据驱动的平台级上限：加宽要证据，坏信号自动收回 L0）</div></template>
      <div class="row">
        <el-input v-model="rolloutEnv" placeholder="环境（默认 default）" style="width: 200px" />
        <el-button size="small" @click="loadRollout">读取状态</el-button>
        <el-button size="small" @click="doRolloutEvaluate">自动降级评估一次</el-button>
        <el-button size="small" type="warning" @click="doRolloutTighten">人工收紧到 L0</el-button>
        <el-button size="small" type="primary" :disabled="!rollout.next_stage" @click="doRolloutWiden(rollout.next_stage)">
          加宽到 {{ rollout.next_stage || '—' }}
        </el-button>
      </div>
      <template v-if="rollout.state !== undefined">
        <div class="dim">
          环境 {{ rollout.env }} ·
          {{ rollout.enabled ? '放量已接入放行判定（min(配置, 信用, 放量)）' : '放量未接入（不表态，沿用配置）' }} ·
          当前档位
          <strong>{{ rollout.state ? rollout.state.stage + (rollout.state.frozen ? '（已冻结）' : '') : '无记录' }}</strong> ·
          上限 {{ rollout.max_stage }}
        </div>
        <div class="dim">{{ rollout.note }}</div>
        <div class="row">
          <span class="chip">评测 run_id {{ rollout.evidence.eval_run_id }}</span>
          <span class="chip">通过 {{ rollout.evidence.eval_passed }}</span>
          <span class="chip">失败 {{ rollout.evidence.eval_failed }}</span>
          <span class="chip">跳过 {{ rollout.evidence.eval_skipped }}（跳过不等于通过）</span>
          <span class="chip">得分 {{ rollout.evidence.eval_score }}</span>
          <span class="chip">SLO 契约 {{ rollout.evidence.slo_total }}</span>
          <span class="chip">SLO 违约 {{ rollout.evidence.slo_breaches }}</span>
        </div>
        <div v-if="rollout.evidence_error" class="dim">证据采集失败：{{ rollout.evidence_error }}</div>
        <div class="row">
          <template v-if="rollout.widen_reasons && rollout.widen_reasons.length">
            <span class="dim">现在不能加宽到 {{ rollout.next_stage }}：</span>
            <el-tag v-for="r in rollout.widen_reasons" :key="r" size="small" type="danger">{{ r }}</el-tag>
          </template>
          <span v-else class="dim">证据齐全且干净：可以加宽到 {{ rollout.next_stage }}</span>
        </div>
        <div class="dim">
          自动降级循环：{{ rollout.auto_eval_enabled ? '开' : '关' }}
          （每 {{ rollout.auto_eval_interval_seconds }}s，冷却 {{ rollout.auto_eval_cooldown_seconds }}s）——
          {{ rollout.auto_eval_skip_reason ? '下轮会跳过：' + rollout.auto_eval_skip_reason : '下轮到点会真的评估一次' }}
        </div>
        <div class="dim" v-if="rollout.state">档位依据：{{ rollout.state.reason }}</div>
      </template>
    </el-card>

    <el-card shadow="never">
      <template #header><div class="title">混沌考核（驾照分）/ 影子对账</div></template>
      <div class="row">
        <el-button @click="doChaosDemo">示例考核打分</el-button>
        <span v-if="chaosScore" class="dim">
          驾照分 {{ chaosScore.license }}（正确率 {{ pct(chaosScore.correct_rate) }}，检测率 {{ pct(chaosScore.detect_rate) }}）
        </span>
      </div>
      <div class="row">
        <el-button @click="doShadowDemo(true)">影子对账（一致）</el-button>
        <el-button @click="doShadowDemo(false)">影子对账（有差异）</el-button>
        <span v-if="shadowResult" class="dim">
          {{ shadowResult.allow_promotion ? '零差异：允许放量' : '存在意外差异：拒绝放量 —— ' + (shadowResult.diff.differences || []).join('；') }}
        </span>
      </div>
    </el-card>
  </div>
</template>

<script setup>
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import {
  effectiveAutonomy, evaluateRollout, explainAllocation, evaluateSlo, fetchMetricsOverview,
  fetchRolloutState, gradeChaos, listAutonomy, observeAutonomy, reconcileShadow, searchSlo,
  tightenRollout, unfreezeAutonomy, widenRollout,
} from '@/api/ops'

const loading = ref(false)
const metrics = ref(null)
const allocJobId = ref(0)
const alloc = ref({})
const sloList = ref([])
const sloEval = ref(null)
const autonomy = ref([])
const obsScope = ref('job_retry@prod')
const effTool = ref('job_retry')
const effEnv = ref('prod')
const effResult = ref(null)
const chaosScore = ref(null)
const shadowResult = ref(null)
const rolloutEnv = ref('')
const rollout = ref({})
const loadIssues = ref([])

const pct = (r) => (r == null || r < 0 ? '无样本' : (r * 100).toFixed(1) + '%')

const cards = computed(() => {
  const m = metrics.value || {}
  return [
    { label: '今日执行', value: m.runs_today ?? '—' },
    { label: '今日成功率', value: pct(m.success_rate_today) },
    { label: '在线节点', value: `${m.nodes_up ?? '—'}/${m.nodes_total ?? '—'}` },
    { label: '待审批', value: m.approvals_pending ?? '—' },
  ]
})

// 方案 §指标看板 要求的 7 项，除"今日成功率"（在上排）外都在这里。
//
// 每一项都把**分子/分母**一起摊出来当自证材料：只给一个比率时，人没法判断它是
// "全坏"还是"没数据"；而分母写出来，"无样本"与"0%"就永远分得开。
// 三个复现率的代理口径（尤其"误动作率")必须在界面上写明，不能让人按方案原意去解读。
const planCards = computed(() => {
  const m = metrics.value || {}
  const decided = m.heal_decided ?? '—'
  const dash = (v) => (v == null ? '—' : v)
  return [
    {
      label: 'MTTR（自愈平均耗时）',
      value: m.mttr_seconds == null || m.mttr_seconds < 0 ? '无样本' : `${Math.round(m.mttr_seconds)}s`,
      sub: `已成功自愈 ${dash(m.heal_succeeded)} 单（从创建到终态）`,
    },
    {
      label: '自动修复率',
      value: pct(m.auto_repair_rate),
      sub: `${dash(m.heal_succeeded)}/${decided} 已出结论的处置`,
    },
    {
      label: '人工接管率',
      value: pct(m.human_takeover_rate),
      sub: `${dash(m.heal_escalated)}/${decided} 系统判定"没把握"转人`,
    },
    {
      label: '误动作率',
      value: pct(m.misaction_rate),
      sub: `${dash(m.heal_failed)}/${decided} 动手后仍失败（可观测代理口径）`,
    },
    {
      label: '单次会话 token 成本',
      value:
        m.session_avg_tokens == null || m.session_avg_tokens < 0
          ? '无样本'
          : `${Math.round(m.session_avg_tokens)} token`,
      sub: `${dash(m.session_count)} 次会话 / 共 ${dash(m.session_tokens_total)} token（未记账不计入）`,
    },
    {
      label: '审批通过率',
      value: pct(m.approval_pass_rate),
      sub: `${dash(m.approval_approved)}/${dash(m.approval_decided)} 有结论的单子（待审批/超时/撤回不计入）`,
    },
  ]
})

// loadAll 各区块**互相独立**（任一接口失败，其余区块照常显示、失败项写在页面上），
// 但**顺序**发请求。两条都是从实测里来的，不是风格偏好：
//
//   1. 独立：2026-10-13 实测到一次 `POST /ops/metrics/overview` 502，而当时是顺序 await +
//      单个 try/finally，结果是整页 KPI / SLO / 信用 / 放量**全部空白**、页面却"不报错"。
//      这正是 F1–F10 那一类"静默失败"，所以逐项兜底。
//   2. 顺序：本机浏览器/代理层在**并发**发起同源请求时会丢掉第一批的第一个
//      （实测响应码：并发 2 条 [502,200]、3 条 [502,200,200]、4 条 [502,200,200,200]；
//      而**串行**连跑两轮 8 次全是 200）。四个只读请求差不了几十毫秒，
//      "并发换来一次随机丢包"不划算。
async function loadAll() {
  loading.value = true
  try {
    const issues = []
    const tasks = [
      ['指标看板', async () => { metrics.value = await fetchMetricsOverview() }],
      ['SLO 契约', async () => { sloList.value = (await searchSlo(0)).list || [] }],
      ['自治信用', async () => { autonomy.value = (await listAutonomy()).list || [] }],
      ['放量档位', loadRollout],
    ]
    for (const [name, fn] of tasks) {
      try {
        await fn()
      } catch {
        // 具体原因由 client.js 统一弹提示；这里只记"哪个区块这次没拿到"。
        issues.push(name)
      }
    }
    loadIssues.value = issues
  } finally {
    loading.value = false
  }
}

// loadRollout 读放量档位状态：档位 + 依据 + "再放一档"卡在哪 + 自动降级循环配置。
async function loadRollout() {
  const res = await fetchRolloutState(rolloutEnv.value)
  rollout.value = res || {}
}

// doRolloutEvaluate 手动敲一次"自动降级"（与常驻循环走的是同一条判定路径）。
async function doRolloutEvaluate() {
  const res = await evaluateRollout(rolloutEnv.value)
  ElMessage.success(res && res.tightened ? '命中坏信号，已自动收回 L0' : '没有坏信号，档位未改动')
  await loadRollout()
}

async function doRolloutTighten() {
  await tightenRollout(rolloutEnv.value, 'L0', '控制台人工收紧')
  ElMessage.success('已收紧到 L0（收紧不需要证据）')
  await loadRollout()
}

// doRolloutWiden 加宽：证据不成立时后端返回 1026 + 逐条原因。
// 拒绝原因由 client 统一弹出、并在面板的"现在不能加宽"里常驻显示，这里不再重复弹一次。
async function doRolloutWiden(stage) {
  try {
    await widenRollout(rolloutEnv.value, stage)
    ElMessage.success(`已加宽到 ${stage}`)
  } catch (e) {
    /* 已拒绝：原因见下方"现在不能加宽到 Lx"清单 */
  }
  await loadRollout()
}

async function doAllocate() {
  const res = await explainAllocation(allocJobId.value)
  alloc.value = res.decision || {}
}

async function doEvaluateSlo() {
  sloEval.value = await evaluateSlo(0)
}

async function doObserve(success) {
  await observeAutonomy(obsScope.value, success)
  ElMessage.success(success ? '已记录一次成功' : '已记录一次不良后果（已降级并冻结）')
  autonomy.value = (await listAutonomy()).list || []
}

async function doUnfreeze(scope) {
  await unfreezeAutonomy(scope)
  autonomy.value = (await listAutonomy()).list || []
}

// doEffective 展示"这个写动作为什么要审批/为什么自动放行"：
// 实际生效级别 = min(配置上限, 信用级别)。它走的是与工具网关同一条判据。
async function doEffective() {
  effResult.value = await effectiveAutonomy(effTool.value, effEnv.value)
}

async function doChaosDemo() {
  const attempts = [
    { fault: { kind: 'latency', target: 'db', severity: 1 }, detected: true, handled: true, correct: true, detection_ms: 120 },
    { fault: { kind: 'disk_full', target: 'node', severity: 2 }, detected: true, handled: true, correct: false, detection_ms: 400 },
  ]
  chaosScore.value = (await gradeChaos(attempts)).score || null
}

async function doShadowDemo(same) {
  const base = { exit_code: 0, stdout_hash: 'aaa', stderr_hash: 'bbb', side_effects: [{ kind: 'file', key: '/x', change: 'modified' }] }
  const cand = same
    ? { ...base, side_effects: [...base.side_effects] }
    : { exit_code: 1, stdout_hash: 'zzz', stderr_hash: 'bbb', side_effects: [{ kind: 'process', key: 'nginx', change: 'stopped' }] }
  shadowResult.value = await reconcileShadow(base, cand)
}

onMounted(loadAll)
</script>

<style scoped>
.page { display: flex; flex-direction: column; gap: 12px; }
.head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; }
.title { font-size: 15px; font-weight: 600; }
.sub { margin-top: 4px; color: #909399; font-size: 12px; line-height: 1.7; max-width: 960px; }
.kpi { background: #fafafa; border-radius: 6px; padding: 12px 14px; }
.kpi-label { color: #909399; font-size: 12px; }
.kpi-value { font-size: 22px; font-weight: 600; margin-top: 4px; }
.kpi-sub { color: #a8abb2; font-size: 12px; margin-top: 4px; line-height: 1.4; }
.row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; margin: 10px 0; }
.tbl { margin-top: 8px; }
.dim { color: #909399; font-size: 12px; }
.chip { display: inline-block; margin: 0 4px 2px 0; padding: 0 6px; background: #f4f4f5; border-radius: 4px; font-size: 11px; color: #606266; }
.badge { margin-left: 6px; color: #67c23a; font-size: 11px; }
.chosen { font-weight: 600; }
</style>
