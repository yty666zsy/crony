<script setup>
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { budgetView, findAgentSession, toolStatsView } from '@/api/agent'
import StatusBadge from '@/components/StatusBadge.vue'
import MockBadge from '@/components/MockBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import AgentStepCard from '@/components/AgentStepCard.vue'
import {
  AGENT_SESSION_KIND,
  AGENT_STOP_REASON,
  AGENT_STEP_KIND,
  bizHint,
} from '@/utils/dict'
import { formatDuration, formatNumber, formatTime, prettyJson } from '@/utils/format'

/**
 * /agents/:session_id —— 轨迹回放（阶段 2 契约 §2）。
 *
 * 本页的**唯一职责**是把后端记录的步骤原样渲染出来：
 *   model 步骤 → prompt 预览 + 模型输出原文 + 它请求的工具调用
 *   tool  步骤 → 工具名 + 入参 + 观测结果（观测是"事实"，不是模型说的）
 *   notice 步骤 → 内核的结构化修复提示（例如 schema 不合法重试）
 *
 * 刻意不做的事（契约 §7 的精神）：
 *   · 不渲染任何"模型思考过程/内心独白"——后端只记录 prompt 与输出，没有思维链字段，
 *     编一段出来就是伪造可观测性；
 *   · 不把 tool 步骤的 observation 改写成"模型认为……"，observation 是工具真实返回；
 *   · 不隐藏 notice 与 warnings（失败与重试必须看得见，否则轨迹会显得比实际更顺）。
 */
const route = useRoute()
const router = useRouter()

const loading = ref(false)
const session = ref(null)
const error = ref(null)
const showRaw = ref(false)

const sessionId = computed(() => String(route.params.session_id || ''))

async function load() {
  loading.value = true
  error.value = null
  try {
    session.value = await findAgentSession(sessionId.value)
  } catch (err) {
    error.value = bizHint(err)
    session.value = null
  } finally {
    loading.value = false
  }
}

const kindMeta = computed(() => AGENT_SESSION_KIND[session.value?.kind] || null)
const stopMeta = computed(() => AGENT_STOP_REASON[session.value?.stop_reason] || null)

/**
 * 预算与工具统计一律走归一化视图：
 * 内核的 BudgetSnapshot 用 elapsed(纳秒) + 嵌套 budget.MaxCalls，契约用 duration_ms/max_calls；
 * 归一化后模板只认契约字段，两边都能显示，且缺失就是 undefined（界面显示 "—"）。
 */
const budgetInfo = computed(() => budgetView(session.value?.budget))
const stats = computed(() => toolStatsView(session.value?.tool_stats))

/** 预算消耗比例：只看有上限的维度，没上限的不画进度（避免假装有额度） */
const budgetPct = computed(() => {
  const b = budgetInfo.value
  const pct = (used, max) => {
    const u = Number(used) || 0
    const m = Number(max) || 0
    if (m <= 0) return null
    return Math.min(100, Math.round((u / m) * 100))
  }
  return {
    calls: pct(b.calls, b.maxCalls),
    tokens: pct(b.tokens, b.maxTokens),
    duration: pct(b.durationMs, b.maxDurationMs),
  }
})

const stepsByKind = computed(() => {
  const list = session.value?.steps || []
  return {
    model: list.filter((s) => s.kind === 'model').length,
    tool: list.filter((s) => s.kind === 'tool').length,
    notice: list.filter((s) => s.kind === 'notice').length,
  }
})

onMounted(load)
</script>

<template>
  <div class="space-y-4">
    <!-- 顶部：返回 + 标识 -->
    <div class="flex flex-wrap items-center gap-2">
      <el-button size="small" plain @click="router.push('/agents')">
        <el-icon class="mr-1" :size="13"><ArrowLeft /></el-icon>会话列表
      </el-button>
      <p class="font-mono text-[11px] text-brand-300">{{ sessionId }}</p>
      <MockBadge text="Mock 轨迹数据" />
      <el-button size="small" plain class="ml-auto" :loading="loading" @click="load">
        <el-icon class="mr-1" :size="13"><Refresh /></el-icon>重新读取
      </el-button>
    </div>

    <p v-if="error" class="card card-pad border-danger/50">
      <span class="text-xs text-danger">读取会话失败（code={{ error.code }}）：{{ error.title }}</span>
      <span class="mt-1 block text-[11px] leading-5 text-ink-muted">{{ error.desc }}</span>
      <span v-if="error.msg" class="mt-1 block font-mono text-[10px] text-ink-dim">后端原文：{{ error.msg }}</span>
      <el-button class="mt-3" size="small" plain @click="router.push('/agents')">回到会话列表</el-button>
    </p>

    <template v-else-if="session">
      <!-- 会话概要 -->
      <section class="card card-pad">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div class="min-w-0">
            <div class="flex flex-wrap items-center gap-2">
              <span
                v-if="kindMeta"
                class="chip"
                :style="{ color: kindMeta.color, backgroundColor: kindMeta.color + '1f' }"
              >{{ kindMeta.text }}</span>
              <StatusBadge kind="session" :value="session.status" />
              <span v-if="stopMeta" class="chip" :style="{ color: stopMeta.color }">
                终止：{{ session.stop_reason_text || stopMeta.text }}
              </span>
              <span class="chip" v-if="session.job_id">job #{{ session.job_id }}</span>
            </div>
            <p class="mt-2 text-sm leading-6 text-ink">{{ session.goal || '（无目标描述）' }}</p>
          </div>
          <div class="text-right text-[10px] leading-5 text-ink-dim">
            <p>创建 {{ formatTime(session.created_ms || session.created) }}</p>
            <p>耗时 {{ formatDuration(session.duration_ms) }}</p>
            <p class="font-mono">{{ session.run_id || session.node_uuid || '—' }}</p>
          </div>
        </div>

        <!-- 预算与模型身份 -->
        <div class="mt-4 grid gap-3 md:grid-cols-2">
          <div class="rounded-lg border border-navy-600 bg-navy-900 p-3">
            <p class="section-label">预算消耗（budget）</p>
            <div class="mt-2 space-y-2">
              <div>
                <div class="flex items-center justify-between text-[11px] text-ink-muted">
                  <span>模型调用</span>
                  <span class="font-mono">
                    {{ budgetInfo.calls ?? 0 }} / {{ budgetInfo.maxCalls ?? '—' }}
                  </span>
                </div>
                <div v-if="budgetPct.calls !== null" class="mt-1 h-1.5 rounded bg-navy-700">
                  <div
                    class="h-1.5 rounded"
                    :class="budgetPct.calls >= 80 ? 'bg-danger' : budgetPct.calls >= 50 ? 'bg-warn' : 'bg-brand-500'"
                    :style="{ width: budgetPct.calls + '%' }"
                  />
                </div>
              </div>
              <div>
                <div class="flex items-center justify-between text-[11px] text-ink-muted">
                  <span>token</span>
                  <span class="font-mono">
                    {{ formatNumber(budgetInfo.tokens ?? 0) }} / {{ budgetInfo.maxTokens ? formatNumber(budgetInfo.maxTokens) : '—' }}
                  </span>
                </div>
                <div v-if="budgetPct.tokens !== null" class="mt-1 h-1.5 rounded bg-navy-700">
                  <div
                    class="h-1.5 rounded"
                    :class="budgetPct.tokens >= 80 ? 'bg-danger' : budgetPct.tokens >= 50 ? 'bg-warn' : 'bg-brand-500'"
                    :style="{ width: budgetPct.tokens + '%' }"
                  />
                </div>
              </div>
              <div>
                <div class="flex items-center justify-between text-[11px] text-ink-muted">
                  <span>时长</span>
                  <span class="font-mono">
                    {{ formatDuration(budgetInfo.durationMs) }} / {{ formatDuration(budgetInfo.maxDurationMs) }}
                  </span>
                </div>
                <div v-if="budgetPct.duration !== null" class="mt-1 h-1.5 rounded bg-navy-700">
                  <div
                    class="h-1.5 rounded"
                    :class="budgetPct.duration >= 80 ? 'bg-danger' : budgetPct.duration >= 50 ? 'bg-warn' : 'bg-brand-500'"
                    :style="{ width: budgetPct.duration + '%' }"
                  />
                </div>
              </div>
            </div>
          </div>

          <div class="rounded-lg border border-navy-600 bg-navy-900 p-3">
            <p class="section-label">模型与工具统计</p>
            <div class="mt-2 flex flex-wrap gap-1.5">
              <span class="chip font-mono">provider {{ session.provider || '—' }}</span>
              <!-- 离线 FakeProvider 从不上报 model：显示 "—" 而不是猜一个 -->
              <span class="chip font-mono">model {{ session.model || '—（provider 未上报）' }}</span>
              <span class="chip font-mono">prompt {{ session.prompt_version || '—' }}</span>
            </div>
            <div class="mt-2 flex flex-wrap gap-1.5">
              <span class="chip">工具调用 {{ stats.calls ?? 0 }}</span>
              <span class="chip" :class="stats.denied ? 'border-danger/40 text-danger' : ''">
                被拒绝 {{ stats.denied ?? 0 }}
              </span>
              <span class="chip">被截断 {{ stats.truncated ?? '—' }}</span>
              <span class="chip">剩余额度 {{ stats.remaining ?? '—' }}</span>
            </div>
            <p class="mt-2 text-[10px] leading-5 text-ink-dim">
              「被截断 / 剩余额度」显示 “—” 表示后端这一刻没有返回该字段（内核的 SessionStats 里没有这两项），
              这里是如实留空，不是 0。
            </p>
          </div>
        </div>

        <!-- 警告 -->
        <div v-if="session.warnings?.length" class="mt-3 rounded-lg border border-warn/40 bg-warn/10 p-3">
          <p class="text-xs font-medium text-warn">本次会话有 {{ session.warnings.length }} 条警告</p>
          <ul class="mt-1 space-y-1">
            <li v-for="w in session.warnings" :key="w" class="text-[11px] leading-5 text-ink-muted">· {{ w }}</li>
          </ul>
        </div>

        <!-- system / context（原文可查，但默认折叠） -->
        <div class="mt-3 flex flex-wrap gap-2">
          <el-button size="small" plain @click="showRaw = !showRaw">
            {{ showRaw ? '收起 system / context 原文' : '查看 system / context 原文' }}
          </el-button>
          <span class="self-center text-[10px] text-ink-dim">
            prompt_bytes 是发给模型的字节数（可核对是否被截断）
          </span>
        </div>
        <div v-if="showRaw" class="mt-2 grid gap-2 md:grid-cols-2">
          <div>
            <p class="section-label">system</p>
            <pre class="scroll-area mt-1 max-h-56 overflow-auto rounded-lg bg-navy-900 p-3 text-[10px] leading-5 text-ink-muted">{{ session.system || '（空）' }}</pre>
          </div>
          <div>
            <p class="section-label">context</p>
            <pre class="scroll-area mt-1 max-h-56 overflow-auto rounded-lg bg-navy-900 p-3 text-[10px] leading-5 text-ink-muted">{{ session.context || '（空）' }}</pre>
          </div>
        </div>
      </section>

      <!-- 轨迹回放 -->
      <section class="card card-pad">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <p class="card-title">轨迹回放（trace）</p>
            <p class="card-sub mt-0.5">
              只渲染后端**实际记录**的 {{ session.steps?.length || 0 }} 个步骤；
              没有记录的内容（例如模型的思考过程）这里不会出现。
            </p>
          </div>
          <div class="flex flex-wrap items-center gap-1.5">
            <span
              v-for="(v, k) in stepsByKind"
              :key="k"
              class="chip"
              :style="{ color: AGENT_STEP_KIND[k].color, backgroundColor: AGENT_STEP_KIND[k].color + '1f' }"
            >
              {{ AGENT_STEP_KIND[k].text }} {{ v }}
            </span>
          </div>
        </div>

        <div v-if="session.steps?.length" class="mt-4">
          <AgentStepCard v-for="(s, i) in session.steps" :key="s.seq ?? i" :step="s" :index="i" />
        </div>
        <EmptyState
          v-else
          class="mt-4"
          icon="Remove"
          title="这次会话没有记录任何步骤"
          desc="可能是预算在第一次调用前就被拒（stop_reason=no_provider / llm_budget），或会话被立即取消。"
        />
      </section>

      <!-- 会话级证据 -->
      <section v-if="session.evidence?.length" class="card card-pad">
        <p class="card-title">会话级证据（{{ session.evidence.length }} 条）</p>
        <p class="card-sub mt-0.5">source 说明证据来自哪个工具/日志，ref 是定位，quote 是原文摘录。</p>
        <ul class="mt-3 space-y-2">
          <li
            v-for="(e, i) in session.evidence"
            :key="i"
            class="rounded-lg border border-navy-600 bg-navy-900 p-3"
          >
            <div class="flex flex-wrap items-center gap-2">
              <span class="chip font-mono text-[10px]">{{ e.source || '—' }}</span>
              <span class="font-mono text-[10px] text-brand-300">{{ e.ref }}</span>
            </div>
            <p class="mt-1.5 text-[11px] leading-5 text-ink">{{ e.quote }}</p>
          </li>
        </ul>
      </section>

      <!-- 结论 -->
      <section class="card card-pad">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <p class="card-title">会话结论（result）</p>
            <p class="card-sub mt-0.5">
              只读展示：这是内核返回的结构化结果原文。阶段 2 不会有任何变更动作被执行。
            </p>
          </div>
          <div class="flex items-center gap-2">
            <router-link
              v-if="session.run_id"
              :to="`/runs?run_id=${encodeURIComponent(session.run_id)}`"
              class="text-[11px] text-brand-300 hover:underline"
            >在 /runs 里看这次执行 →</router-link>
          </div>
        </div>
        <pre class="scroll-area mt-3 max-h-80 overflow-auto rounded-lg bg-navy-900 p-3 text-[10px] leading-5 text-ink-muted">{{ session.result ? prettyJson(session.result) : '（本次会话没有结构化结论）' }}</pre>
      </section>
    </template>

    <div v-else-if="loading" class="card card-pad text-[11px] text-ink-dim">正在读取会话轨迹…</div>
  </div>
</template>