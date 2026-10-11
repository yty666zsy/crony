<script setup>
import { computed, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import MockBadge from '@/components/MockBadge.vue'
import { DIAGNOSIS_CATEGORY, DIAGNOSIS_RISK } from '@/utils/dict'
import { formatConfidence, formatDuration, formatTime, prettyJson } from '@/utils/format'

/**
 * AI 诊断面板（契约 §3.1 读 + §3.2 触发）。
 *
 * 三条硬约束写进了渲染逻辑里，不是文案：
 *  1) advisory_only=true → **只给建议**：本组件不提供任何"执行 / 应用 / 一键修复"按钮，
 *     唯一可做的动作是"复制建议文本"（复制不会改任何东西）；
 *  2) confidence 必须标注"模型自评"：它是模型自己给的数，不是准确率，
 *     所以既显示数字也显示这句话，且不参与任何排序/阈值判断；
 *  3) unverified_refs 非空 → 用红色告警块醒目提示"引用了未观测到的证据定位"，
 *     因为这些定位无法回溯，结论不能直接采信。
 *  4) unverified_node_claims 非空 → 同样红色告警块：ref 都是真的，但结论断言了
 *     只有目标机器才知道的事实（端口未监听 / 服务未运行 / 磁盘已满……），
 *     而本次会话在 admin 侧没有观测过。这类问题 grounding 校验查不出来，
 *     由服务端的"结论句守卫"抓出，前端必须显式呈现，不能只显示证据充分的"否"。
 */
const props = defineProps({
  /** §3.1 的 diagnosis 对象；found=false 时为 null */
  diagnosis: { type: Object, default: null },
  found: { type: Boolean, default: false },
  loading: { type: Boolean, default: false },
  /** 触发诊断请求进行中（按钮 loading） */
  busy: { type: Boolean, default: false },
  /** 诊断所属会话，用于跳 /agents/:session_id */
  sessionId: { type: String, default: '' },
  /** 触发失败时的错误（含 code），由父组件传入以便展示人话提示 */
  error: { type: Object, default: null },
})

const emit = defineEmits(['run'])

const showRaw = ref(false)

const cat = computed(() => DIAGNOSIS_CATEGORY[props.diagnosis?.category] || null)
const confidenceText = computed(() => formatConfidence(props.diagnosis?.confidence))
const unverified = computed(() => props.diagnosis?.unverified_refs || [])
/**
 * unverified_node_claims：结论句里"本应观测却靠推测"的节点侧断言类型。
 *
 * 与 unverified_refs 的区别（两者都要展示，不能合并）：
 *   - unverified_refs：引用了**假 ref**（证据定位无法回溯）；
 *   - 本字段：ref **全真**，但结论断言了只有目标机器才知道的事实——
 *     grounding 校验查不出这类问题，是服务端的"结论句守卫"抓出来的。
 */
const unverifiedClaims = computed(() => props.diagnosis?.unverified_node_claims || [])
const warnings = computed(() => props.diagnosis?.warnings || [])
const actions = computed(() => props.diagnosis?.suggested_actions || [])

/** 二次确认：force=true 会新增会话并消耗 token，不能一键就发出去 */
async function askForce() {
  try {
    await ElMessageBox.confirm(
      '重新诊断会**新增一条会话**并真实消耗模型预算（默认 20 次调用 / 200000 token / 30 分钟）。' +
        '已有诊断不会被覆盖，历史记录保留以便对比。确定要重新诊断吗？',
      '二次确认：重新诊断（消耗 token）',
      { type: 'warning', confirmButtonText: '确认重新诊断', cancelButtonText: '取消', dangerouslyUseHTMLString: false },
    )
  } catch {
    return // 用户取消：什么都不做
  }
  emit('run', true)
}

async function copyDiagnosis() {
  const d = props.diagnosis
  if (!d) return
  const text = [
    `结论：${d.summary}`,
    `分类：${d.category_text || d.category}｜置信度：${confidenceText.value}（模型自评）｜证据充分：${d.evidence_sufficient ? '是' : '否'}`,
    `原因：${d.cause}`,
    '',
    '证据链：',
    ...(d.evidence || []).map((e, i) => `${i + 1}. [${e.ref}] ${e.quote}（作用：${e.why}）`),
    '',
    '建议动作（需人工执行）：',
    ...(d.suggested_actions || []).map(
      (a, i) => `${i + 1}. [${a.risk_text || a.risk}] ${a.title}：${a.detail}｜回滚：${a.rollback}`,
    ),
    ...(unverified.value.length
      ? ['', '警告：引用了未观测到的证据定位：', ...unverified.value.map((r) => `- ${r}`)]
      : []),
    ...(unverifiedClaims.value.length
      ? [
          '',
          '警告：结论含未观测的节点侧断言（只有目标机器自己知道，本次未观测）：',
          `- ${unverifiedClaims.value.join('、')}`,
        ]
      : []),
  ].join('\n')
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('诊断文本已复制（复制不会执行任何动作）')
  } catch {
    ElMessage.warning('浏览器拒绝了剪贴板访问，请手动选中文本复制')
  }
}
</script>

<template>
  <div class="space-y-3">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <div class="flex items-center gap-2">
        <p class="card-title">AI 诊断</p>
        <MockBadge text="Mock 诊断数据" />
      </div>
      <div class="flex items-center gap-2">
        <router-link
          v-if="sessionId"
          :to="`/agents/${sessionId}`"
          class="text-[11px] text-brand-300 hover:underline"
        >
          查看这次诊断的完整轨迹 →
        </router-link>
        <el-button
          v-if="found"
          size="small"
          :loading="busy"
          @click="askForce"
        >
          <el-icon class="mr-1" :size="13"><Refresh /></el-icon>重新诊断
        </el-button>
      </div>
    </div>

    <!-- 还没诊断过：found=false 是正常状态，不是错误 -->
    <div v-if="!found" class="rounded-lg border border-navy-600 bg-navy-900 p-3">
      <p class="text-xs text-ink">这条执行记录还没有诊断记录（<span class="font-mono">found=false</span>）。</p>
      <p class="mt-1 text-[11px] leading-5 text-ink-dim">
        诊断会读取该 run 的日志与节点状态等**只读**证据，输出一份只供参考的结论；
        所有工具都是只读的，不会重启服务、不会改配置。
      </p>
      <div v-if="error" class="mt-2 rounded-lg border border-danger/40 bg-danger/10 p-2">
        <p class="text-[11px] text-danger">触发失败（code={{ error.code }}）：{{ error.title }}</p>
        <p class="mt-0.5 text-[11px] leading-5 text-ink-muted">{{ error.desc }}</p>
        <p v-if="error.msg" class="mt-0.5 font-mono text-[10px] text-ink-dim">后端原文：{{ error.msg }}</p>
      </div>
      <el-button class="mt-3" type="primary" size="small" :loading="busy" @click="emit('run', false)">
        <el-icon class="mr-1" :size="13"><MagicStick /></el-icon>立即诊断
      </el-button>
    </div>

    <!-- 已有诊断 -->
    <template v-else-if="diagnosis">
      <!-- advisory_only：只读只建议 -->
      <div class="rounded-lg border border-brand-500/40 bg-brand-500/10 p-3">
        <p class="text-xs font-medium text-brand-200">
          仅建议，不执行（<span class="font-mono">advisory_only=true</span>）
        </p>
        <p class="mt-1 text-[11px] leading-5 text-ink-muted">
          阶段 2 的工具全部是只读的，诊断结果只供人工参考。本面板**没有**执行/应用/一键修复按钮；
          唯一能做的动作是复制文本。
        </p>
      </div>

      <!-- 引用未观测证据：必须醒目 -->
      <div v-if="unverified.length" class="rounded-lg border border-danger/60 bg-danger/10 p-3">
        <p class="text-xs font-medium text-danger">
          ⚠ 引用了未观测到的证据定位（{{ unverified.length }} 处）
        </p>
        <p class="mt-1 text-[11px] leading-5 text-ink-muted">
          下面的定位在本次会话的观测集合里**不存在**，无法回溯。这份结论不能直接采信，
          需要人工核对后再使用。
        </p>
        <ul class="mt-2 space-y-1">
          <li v-for="r in unverified" :key="r" class="font-mono text-[11px] text-danger">· {{ r }}</li>
        </ul>
      </div>

      <!-- 结论含未观测的节点侧断言：ref 全真但结论越界，同样必须醒目 -->
      <div v-if="unverifiedClaims.length" class="rounded-lg border border-danger/60 bg-danger/10 p-3">
        <p class="text-xs font-medium text-danger">
          ⚠ 结论含未观测的节点侧断言（{{ unverifiedClaims.join('、') }}）
        </p>
        <p class="mt-1 text-[11px] leading-5 text-ink-muted">
          这些事实只有目标机器自己知道（端口是否监听 / 服务是否运行 / 资源水位 / 文件与进程是否存在），
          而本次会话在 admin 侧**没有**观测过它们——结论里的这部分是推测，不是观测。
          已在服务端强制标记为"证据不足"，请先在节点上确认再据此决策。
        </p>
      </div>

      <!-- 证据不足：把"还要采集什么"提到最显眼处 -->
      <div
        v-if="diagnosis.evidence_sufficient === false"
        class="rounded-lg border border-warn/50 bg-warn/10 p-3"
      >
        <p class="text-xs font-medium text-warn">证据不足（<span class="font-mono">evidence_sufficient=false</span>）</p>
        <p class="mt-1 text-[11px] leading-5 text-ink-muted">
          模型没有拿到足够证据，本次**没有**给出根因断言。要继续排查，请先采集：
        </p>
        <ul class="mt-2 space-y-1">
          <li v-for="n in diagnosis.next_data_to_collect" :key="n" class="text-[11px] text-ink-muted">· {{ n }}</li>
        </ul>
      </div>

      <div class="card card-pad">
        <p class="text-sm leading-6 text-ink">{{ diagnosis.summary }}</p>

        <div class="mt-2 flex flex-wrap items-center gap-2">
          <span
            v-if="cat"
            class="chip"
            :style="{ color: cat.color, backgroundColor: cat.color + '1f' }"
          >{{ cat.text }}</span>
          <span class="chip">
            置信度 {{ confidenceText }}
            <span class="ml-1 text-ink-dim">（模型自评，不是准确率）</span>
          </span>
          <span class="chip">证据充分：{{ diagnosis.evidence_sufficient ? '是' : '否' }}</span>
          <span class="chip">终止原因：{{ diagnosis.stop_reason_text || diagnosis.stop_reason }}</span>
        </div>

        <div class="mt-3 rounded-lg bg-navy-900 p-3">
          <p class="section-label">原因分析</p>
          <p class="mt-1 text-[11px] leading-5 text-ink-muted">{{ diagnosis.cause }}</p>
        </div>

        <div v-if="diagnosis.evidence?.length" class="mt-3">
          <p class="section-label">证据链（{{ diagnosis.evidence.length }} 条，逐条可回溯）</p>
          <ul class="mt-2 space-y-2">
            <li v-for="(e, i) in diagnosis.evidence" :key="i" class="rounded-lg border border-navy-600 bg-navy-900 p-2">
              <p class="font-mono text-[10px] text-brand-300">[{{ e.ref }}]</p>
              <p class="mt-1 text-[11px] text-ink">{{ e.quote }}</p>
              <p class="mt-1 text-[10px] text-ink-dim">作用：{{ e.why }}</p>
            </li>
          </ul>
        </div>

        <div class="mt-3 grid gap-3 md:grid-cols-2">
          <div v-if="diagnosis.assumptions?.length" class="rounded-lg bg-navy-900 p-3">
            <p class="section-label">未验证的假设</p>
            <ul class="mt-1 space-y-1">
              <li v-for="a in diagnosis.assumptions" :key="a" class="text-[11px] text-ink-muted">· {{ a }}</li>
            </ul>
          </div>
          <div v-if="diagnosis.ruled_out?.length" class="rounded-lg bg-navy-900 p-3">
            <p class="section-label">已排除的可能</p>
            <ul class="mt-1 space-y-1">
              <li v-for="r in diagnosis.ruled_out" :key="r.hypothesis" class="text-[11px] text-ink-muted">
                · {{ r.hypothesis }} —— {{ r.reason }}
              </li>
            </ul>
          </div>
        </div>

        <div v-if="actions.length" class="mt-3">
          <p class="section-label">建议动作（需人工执行，面板只提供复制）</p>
          <div class="mt-2 space-y-2">
            <div v-for="(a, i) in actions" :key="i" class="rounded-lg border border-navy-600 bg-navy-900 p-3">
              <div class="flex flex-wrap items-center gap-2">
                <p class="text-xs font-medium text-ink">{{ a.title }}</p>
                <span
                  class="chip"
                  :style="{
                    color: DIAGNOSIS_RISK[a.risk]?.color || '#7c8aa8',
                    backgroundColor: (DIAGNOSIS_RISK[a.risk]?.color || '#7c8aa8') + '1f',
                  }"
                >
                  {{ a.risk_text || DIAGNOSIS_RISK[a.risk]?.text || a.risk }}
                </span>
              </div>
              <p class="mt-1 text-[11px] leading-5 text-ink-muted">{{ a.detail }}</p>
              <p class="mt-1 text-[10px] text-ink-dim">回滚：{{ a.rollback || '未说明' }}</p>
            </div>
          </div>
        </div>

        <div v-if="warnings.length" class="mt-3 rounded-lg border border-warn/40 bg-warn/10 p-3">
          <p class="section-label text-warn">本次诊断的警告</p>
          <ul class="mt-1 space-y-1">
            <li v-for="w in warnings" :key="w" class="text-[11px] text-ink-muted">· {{ w }}</li>
          </ul>
        </div>

        <div class="mt-3 flex flex-wrap items-center gap-2 text-[10px] text-ink-dim">
          <span class="font-mono">provider={{ diagnosis.provider || '—' }}</span>
          <span class="font-mono">model={{ diagnosis.model || '（provider 未上报）' }}</span>
          <span class="font-mono">prompt={{ diagnosis.prompt_version || '—' }}</span>
          <span>耗时 {{ formatDuration(diagnosis.duration_ms) }}</span>
          <span>工具调用 {{ diagnosis.tool_calls }}</span>
          <span>tokens {{ diagnosis.total_tokens }}</span>
          <span>{{ formatTime(diagnosis.created) }}</span>
        </div>

        <div class="mt-3 flex flex-wrap items-center gap-2 border-t border-navy-600 pt-3">
          <el-button size="small" @click="copyDiagnosis">
            <el-icon class="mr-1" :size="13"><DocumentCopy /></el-icon>复制诊断文本
          </el-button>
          <el-button size="small" plain @click="showRaw = !showRaw">
            {{ showRaw ? '收起' : '查看' }}诊断原文（explain）
          </el-button>
        </div>

        <pre
          v-if="showRaw"
          class="scroll-area mt-2 max-h-72 overflow-auto rounded-lg bg-navy-900 p-3 text-[10px] leading-5 text-ink-muted"
        >{{ diagnosis.explain || prettyJson(diagnosis) }}</pre>
      </div>
    </template>

    <div v-else-if="loading" class="rounded-lg border border-navy-600 bg-navy-900 p-3 text-[11px] text-ink-dim">
      正在读取诊断记录…
    </div>
  </div>
</template>