<script setup>
import { computed, ref } from 'vue'
import { stepError, stepTime } from '@/api/agent'
import { AGENT_STEP_KIND } from '@/utils/dict'
import { formatDuration, formatTime, prettyJson } from '@/utils/format'

/**
 * 单个轨迹步骤（契约 §2 的 steps[]）。
 *
 * 三种 kind 的呈现必须**一眼可分**（这是契约的硬要求，也是审计的前提）：
 *   model  蓝色系：prompt 预览 + 模型输出原文 + 它请求的工具调用
 *   tool   橙色系：工具名 + 入参 + 观测结果（事实）+ 证据定位
 *   notice 红色系：内核的结构化修复提示（例如输出不符合 schema 后重试）
 *
 * 字段取值用 api/agent.js 里的容错取值函数：Go 内核的 Step 用的是 index/at/err/usage.total_tokens，
 * 契约写的是 seq/created/error/total_tokens，两边都要能渲染，且**不伪造**缺失字段。
 */
const props = defineProps({
  step: { type: Object, required: true },
  index: { type: Number, default: 0 },
})

const meta = computed(() => AGENT_STEP_KIND[props.step.kind] || AGENT_STEP_KIND.notice)
const seq = computed(() => props.step?.seq ?? props.step?.index ?? props.index)
const at = computed(() => stepTime(props.step))
const err = computed(() => stepError(props.step))
/** token：契约 total_tokens → 内核 usage.total_tokens；两边都没有就返回 null（界面不显示，不写成 0） */
const tokens = computed(() => props.step?.total_tokens ?? props.step?.usage?.total_tokens ?? null)
const showArgs = ref(false)

const hasModelBody = computed(
  () => props.step.kind === 'model' && (props.step.content || props.step.prompt_preview || props.step.tool_calls?.length),
)

/** 配色条：不同 kind 用不同左边框颜色，扫一眼就能区分 */
const barStyle = computed(() => ({ borderLeftColor: meta.value.color, borderLeftWidth: '3px' }))
</script>

<template>
  <article class="mb-3 rounded-lg border border-navy-600 bg-navy-900 p-3" :style="barStyle">
    <header class="flex flex-wrap items-center gap-2">
      <span
        class="rounded px-1.5 py-0.5 text-[10px] font-medium"
        :style="{ color: meta.color, backgroundColor: meta.color + '1f' }"
      >
        {{ meta.text }}
      </span>
      <span class="font-mono text-[10px] text-ink-dim">seq {{ seq }}</span>
      <span class="text-[10px] text-ink-dim">{{ formatTime(at) }}</span>

      <span v-if="step.kind === 'tool'" class="font-mono text-[11px] text-warn">
        {{ step.tool_name || '—' }}
      </span>
      <span v-else-if="step.kind === 'model'" class="font-mono text-[10px] text-ink-muted">
        {{ step.provider || '—' }} / {{ step.model || '—（未上报）' }}
      </span>

      <span v-if="step.latency_ms" class="ml-auto text-[10px] text-ink-dim">
        延迟 {{ formatDuration(step.latency_ms) }}
      </span>
      <span v-if="step.duration_ms" class="text-[10px] text-ink-dim">
        耗时 {{ formatDuration(step.duration_ms) }}
      </span>
      <span v-if="tokens !== null" class="text-[10px] text-ink-dim">tokens {{ tokens }}</span>
      <span v-if="step.finish_reason" class="chip text-[10px]">finish {{ step.finish_reason }}</span>
    </header>

    <!-- ============ notice：内核提示（必须显眼） ============ -->
    <p
      v-if="step.kind === 'notice'"
      class="mt-2 rounded-md border border-danger/40 bg-danger/10 p-2 text-[11px] leading-5 text-danger"
    >
      {{ step.notice || '（内核记录了提示但没带文本）' }}
    </p>

    <!-- ============ model：prompt 与输出 ============ -->
    <template v-else-if="hasModelBody">
      <div v-if="step.prompt_preview" class="mt-2">
        <p class="text-[10px] text-ink-dim">
          prompt 预览<span v-if="step.prompt_bytes"> · 实际发送 {{ step.prompt_bytes }} 字节</span>
        </p>
        <pre
          class="scroll-area mt-1 max-h-40 overflow-auto whitespace-pre-wrap rounded-md bg-navy-950 p-2.5 font-mono text-[10px] leading-5 text-ink-muted"
        >{{ step.prompt_preview }}</pre>
      </div>

      <div v-if="step.truncated" class="mt-2 rounded-md border border-warn/40 bg-warn/10 p-2 text-[10px] text-warn">
        ⚠ 这一步的 prompt 被预算/长度限制**截断**过，模型看到的不是完整上下文。
      </div>

      <div v-if="step.content" class="mt-2">
        <p class="text-[10px] text-ink-dim">模型输出原文（模型自己写的内容，未必正确）</p>
        <pre
          class="mt-1 max-h-72 overflow-auto whitespace-pre-wrap rounded-md bg-navy-950 p-2.5 text-[11px] leading-5 text-ink"
        >{{ step.content }}</pre>
      </div>

      <div v-if="step.tool_calls?.length" class="mt-2">
        <p class="text-[10px] text-ink-dim">模型请求的工具调用（{{ step.tool_calls.length }} 个，实际执行结果见后面的 tool 步骤）</p>
        <div class="mt-1 space-y-1.5">
          <div v-for="c in step.tool_calls" :key="c.id" class="rounded-md border border-navy-600 bg-navy-950 p-2">
            <div class="flex flex-wrap items-center gap-2">
              <span class="font-mono text-[11px] text-warn">{{ c.name }}</span>
              <span v-if="c.id" class="font-mono text-[10px] text-ink-dim">id {{ c.id }}</span>
            </div>
            <pre
              class="scroll-area mt-1 max-h-32 overflow-auto whitespace-pre-wrap font-mono text-[10px] leading-5 text-ink-muted"
            >{{ c.arguments || '（无入参）' }}</pre>
          </div>
        </div>
      </div>
    </template>

    <!-- ============ tool：入参 + 观测结果 ============ -->
    <template v-else-if="step.kind === 'tool'">
      <div class="mt-2">
        <p class="text-[10px] text-ink-dim">工具入参（tool_args）</p>
        <pre
          class="scroll-area mt-1 max-h-32 overflow-auto whitespace-pre-wrap rounded-md bg-navy-950 p-2.5 font-mono text-[10px] leading-5 text-ink-muted"
        >{{ showArgs ? prettyJson(step.tool_args) : (step.tool_args || '（无入参）') }}</pre>
      </div>

      <div class="mt-2">
        <p class="text-[10px] text-ink-dim">观测结果（observation，工具真实返回，不是模型转述）</p>
        <pre
          class="scroll-area mt-1 max-h-72 overflow-auto whitespace-pre-wrap rounded-md bg-navy-950 p-2.5 text-[11px] leading-5 text-ink"
        >{{ step.observation || '（工具没有返回内容）' }}</pre>
      </div>

      <div v-if="step.evidence?.length" class="mt-2">
        <p class="text-[10px] text-ink-dim">这一步产生的证据定位</p>
        <ul class="mt-1 space-y-1">
          <li v-for="(e, i) in step.evidence" :key="i" class="rounded-md bg-navy-950 p-2">
            <span class="font-mono text-[10px] text-brand-300">{{ e.ref || e.source || '—' }}</span>
            <p v-if="e.quote" class="mt-1 text-[10px] leading-5 text-ink-muted">{{ e.quote }}</p>
          </li>
        </ul>
      </div>
    </template>

    <p v-else class="mt-2 text-[11px] text-ink-dim">这一步没有可展示的内容（后端只记录了元信息）。</p>

    <!-- 错误：任何 kind 都可能带 -->
    <p
      v-if="err"
      class="mt-2 rounded-md border border-danger/40 bg-danger/10 p-2 font-mono text-[10px] leading-5 text-danger"
    >
      error: {{ err }}
    </p>

    <div v-if="step.kind === 'tool'" class="mt-2">
      <el-button link size="small" @click="showArgs = !showArgs">
        {{ showArgs ? '收起格式化入参' : '格式化查看入参' }}
      </el-button>
    </div>
  </article>
</template>