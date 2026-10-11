<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { confirmNlJob, draftNlJob } from '@/api/agent'
import { searchNodes } from '@/api/node'
import MockBadge from '@/components/MockBadge.vue'
import { JOB_TYPE, bizHint } from '@/utils/dict'
import { formatSeconds, prettyJson } from '@/utils/format'

/**
 * 自然语言建任务向导（阶段 2 契约 §5.1 草案 → §5.2 确认）。
 *
 * 两步是**契约要求**的，不是 UX 偏好：
 *   §5.1 返回 requires_confirmation=true / confirmed=false —— 草案阶段绝不落库；
 *   §5.2 只有 confirmed=true 才创建，此时 status=0（未分配节点，不会被调度）。
 *
 * 界面必须把三件事讲清楚（否则就是骗人）：
 *   1) 5 次**具体**触发时间（next_triggers / next_trigger_text）与 trigger_echo 原文，
 *      让用户看到"我写的 cron 到底会在什么时候跑"；
 *   2) warnings 要显眼：尤其 5 段表达式会被库按别的字段解释（`0 2 * * *` 不是"每天 2 点"），
 *      这类警告单独用红色块强调，不能淹没在普通提示里；
 *   3) "确认前不会写入调度器" —— 草案阶段所有内容都只在服务端会话里，
 *      关掉向导不会留下任务。
 */
const props = defineProps({
  modelValue: { type: Boolean, default: false },
})
const emit = defineEmits(['update:modelValue', 'created'])

const visible = computed({
  get: () => props.modelValue,
  set: (v) => emit('update:modelValue', v),
})

const nodeOptions = ref([])
const nodesLoaded = ref(false)

const step = ref('draft') // draft | review | done

const draftForm = reactive({
  text: '每天凌晨 2 点清理 /tmp 下 7 天前的文件',
  node_uuid: '',
  timezone: 'Asia/Shanghai',
})

const loading = ref(false)
const error = ref(null)
/** §5.1 的完整返回 */
const draft = ref(null)
/** §5.2 的返回 */
const created = ref(null)

/** 覆盖项（§5.2 overrides，只允许改这四项，其余要重新起草） */
const overrides = reactive({ name: '', spec: '', timeout_seconds: null, retry_times: null })

const examples = [
  '每天凌晨 2 点清理 /tmp 下 7 天前的文件',
  '每小时检查一次健康检查接口 https://api.example.com/healthz',
  '每周一早上 8 点跑一次月度报表脚本 /opt/report/gen.sh',
]

/** 5 段表达式警告单独挑出来：这是最容易让人踩坑的一条 */
const fiveSegWarnings = computed(() =>
  (draft.value?.warnings || []).filter((w) => w.includes('5 段')),
)
const otherWarnings = computed(() =>
  (draft.value?.warnings || []).filter((w) => !w.includes('5 段')),
)

/** job_type 是字符串（'cmd' / 'http'），字典的 key 正好也是这两个值 */
const jobTypeText = computed(
  () => Object.values(JOB_TYPE).find((v) => v.key === draft.value?.job_type)?.text || draft.value?.job_type || '—',
)

async function loadNodes() {
  if (nodesLoaded.value) return
  try {
    const res = await searchNodes({ page: 1, page_size: 100 })
    nodeOptions.value = res?.list || []
    if (!draftForm.node_uuid && nodeOptions.value.length) {
      // 默认选第一个在线节点（status=1），避免用户莫名其妙撞上 1012
      const online = nodeOptions.value.find((n) => n.status === 1)
      draftForm.node_uuid = (online || nodeOptions.value[0]).uuid
    }
  } catch {
    nodeOptions.value = []
  } finally {
    nodesLoaded.value = true
  }
}

// 打开向导时加载节点（懒加载：不打开就不发请求）
watch(visible, (v) => {
  if (v) {
    error.value = null
    loadNodes()
  }
})

async function makeDraft() {
  const text = draftForm.text.trim()
  if (!text) {
    ElMessage.warning('请先用一句话描述你想要的定时任务')
    return
  }
  loading.value = true
  error.value = null
  try {
    draft.value = await draftNlJob({
      text,
      node_uuid: draftForm.node_uuid || undefined,
      timezone: draftForm.timezone || undefined,
    })
    created.value = null
    step.value = 'review'
    // 覆盖项默认跟着草案走，用户不改就是草案值
    overrides.name = draft.value.name || ''
    overrides.spec = draft.value.spec || ''
    overrides.timeout_seconds = draft.value.timeout_seconds ?? null
    overrides.retry_times = draft.value.retry_times ?? null
  } catch (err) {
    error.value = bizHint(err)
    ElMessage.error(`${error.value.title}（code=${error.value.code}）`)
  } finally {
    loading.value = false
  }
}

async function doConfirm() {
  if (!draft.value) return
  loading.value = true
  error.value = null
  try {
    created.value = await confirmNlJob({
      session_id: draft.value.session_id,
      confirmed: true,
      overrides: {
        name: overrides.name || undefined,
        spec: overrides.spec || undefined,
        timeout_seconds: overrides.timeout_seconds ?? undefined,
        retry_times: overrides.retry_times ?? undefined,
      },
    })
    step.value = 'done'
    ElMessage.success(`任务已创建：#${created.value.job_id}（status=0，未分配节点）`)
    emit('created')
  } catch (err) {
    error.value = bizHint(err)
    ElMessage.error(`创建失败：${error.value.title}（code=${error.value.code}）`)
  } finally {
    loading.value = false
  }
}

function restart() {
  step.value = 'draft'
  draft.value = null
  created.value = null
  error.value = null
}

function close() {
  visible.value = false
}
</script>

<template>
  <el-drawer v-model="visible" :size="720" title="用自然语言建任务（阶段 2 · 契约 §5）">
    <div class="scroll-area h-full px-5 py-4">
      <!-- 步骤指示 -->
      <div class="mb-4 flex flex-wrap items-center gap-2">
        <span class="chip" :class="step === 'draft' ? 'border-brand-500/60 text-brand-300' : 'text-ink-dim'">
          1 描述需求 → 生成草案
        </span>
        <span class="text-ink-dim">→</span>
        <span class="chip" :class="step === 'review' ? 'border-brand-500/60 text-brand-300' : 'text-ink-dim'">
          2 核对触发时间与警告 → 确认
        </span>
        <span class="text-ink-dim">→</span>
        <span class="chip" :class="step === 'done' ? 'border-ok/60 text-ok' : 'text-ink-dim'">3 已创建</span>
        <MockBadge class="ml-auto" text="Mock 草案" />
      </div>

      <p v-if="error" class="mb-3 rounded-lg border border-danger/50 bg-danger/10 p-3">
        <span class="text-xs text-danger">失败（code={{ error.code }}）：{{ error.title }}</span>
        <span class="mt-1 block text-[11px] leading-5 text-ink-muted">{{ error.desc }}</span>
        <span v-if="error.msg" class="mt-1 block font-mono text-[10px] text-ink-dim">后端原文：{{ error.msg }}</span>
      </p>

      <!-- ==================== 第 1 步：草案 ==================== -->
      <div v-if="step === 'draft'" class="space-y-3">
        <div class="card card-pad">
          <p class="card-title">用一句话描述任务</p>
          <p class="card-sub mt-0.5">
            不需要写 cron，也不用写命令。内核会把它翻成结构化草案（job_type / spec / argv / 超时 / 重试），
            然后由你确认。模型可能理解错，所以下一步会明确列出它到底准备创建什么。
          </p>
          <el-input
            v-model="draftForm.text"
            type="textarea"
            :rows="3"
            maxlength="2000"
            show-word-limit
            class="mt-3"
            placeholder="例：每天凌晨 2 点清理 /tmp 下 7 天前的文件"
          />
          <div class="mt-2 flex flex-wrap gap-1.5">
            <button
              v-for="e in examples"
              :key="e"
              class="chip hover:border-brand-500/60"
              type="button"
              @click="draftForm.text = e"
            >
              {{ e }}
            </button>
          </div>

          <div class="mt-3 grid gap-2 sm:grid-cols-2">
            <div>
              <p class="section-label">目标节点</p>
              <el-select v-model="draftForm.node_uuid" placeholder="选择节点" class="mt-1 w-full">
                <el-option
                  v-for="n in nodeOptions"
                  :key="n.uuid"
                  :label="`${n.hostname}（${n.status === 1 ? '在线' : '失联'}）`"
                  :value="n.uuid"
                />
              </el-select>
              <p class="mt-1 text-[10px] text-ink-dim">
                节点离线/未注册会让接口直接返回 1012，采集不到只读证据。
              </p>
            </div>
            <div>
              <p class="section-label">时区</p>
              <el-input v-model="draftForm.timezone" class="mt-1" placeholder="Asia/Shanghai" />
              <p class="mt-1 text-[10px] text-ink-dim">
                触发时间按这个时区计算，草案里会把 5 次具体时间列出来。
              </p>
            </div>
          </div>
        </div>

        <div class="flex flex-wrap items-center gap-2">
          <el-button type="primary" :loading="loading" @click="makeDraft">
            <el-icon class="mr-1" :size="14"><MagicStick /></el-icon>生成草案
          </el-button>
          <el-button plain @click="close">取消</el-button>
          <span class="text-[10px] text-ink-dim">
            生成草案**不会**创建任务，只会在服务端留下一个待确认的会话（§5.1）。
          </span>
        </div>
      </div>

      <!-- ==================== 第 2 步：核对确认 ==================== -->
      <div v-else-if="step === 'review' && draft" class="space-y-3">
        <!-- 五段表达式警告：最醒目的位置 -->
        <div v-if="fiveSegWarnings.length" class="rounded-lg border border-danger/60 bg-danger/10 p-3">
          <p class="text-xs font-medium text-danger">⚠ cron 表达式只有 5 段，含义可能和你想的不一样</p>
          <ul class="mt-1 space-y-1">
            <li v-for="w in fiveSegWarnings" :key="w" class="text-[11px] leading-5 text-danger">{{ w }}</li>
          </ul>
          <p class="mt-1 text-[10px] leading-5 text-ink-muted">
            建议改成 6 段（秒 分 时 日 月 周）后重新起草，或者确认下面列出的 5 次触发时间确实是你想要的。
          </p>
        </div>

        <!-- 触发时间：必须列出 5 次具体时间 -->
        <div class="card card-pad">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <p class="card-title">接下来 5 次触发时间</p>
            <span class="chip font-mono">{{ draft.spec }}</span>
          </div>
          <ol class="mt-2 space-y-1">
            <li
              v-for="(t, i) in draft.next_triggers"
              :key="i"
              class="flex items-center gap-2 rounded-lg bg-navy-900 px-3 py-1.5"
            >
              <span class="text-[10px] text-ink-dim">{{ i + 1 }}</span>
              <span class="font-mono text-[11px] text-ink">{{ draft.next_trigger_text?.[i] || t }}</span>
            </li>
          </ol>
          <div class="mt-3 rounded-lg border border-navy-600 bg-navy-900 p-3">
            <p class="section-label">trigger_echo（内核原文）</p>
            <pre class="mt-1 whitespace-pre-wrap text-[10px] leading-5 text-ink-muted">{{ draft.trigger_echo }}</pre>
          </div>
          <p class="mt-2 text-[10px] leading-5 text-ink-dim">
            requires_confirmation={{ draft.requires_confirmation }} · confirmed={{ draft.confirmed }} ——
            草案阶段任务**没有**写入调度器；只有点下面的"确认创建"（§5.2 confirmed=true）才会创建，
            且创建出来是 status=0（未分配节点，不会被调度执行）。
          </p>
        </div>

        <!-- 草案内容 -->
        <div class="card card-pad">
          <p class="card-title">内核准备创建的任务</p>
          <div class="mt-2 flex flex-wrap gap-1.5">
            <span class="chip">{{ jobTypeText }}</span>
            <span class="chip">超时 {{ formatSeconds(draft.timeout_seconds) }}</span>
            <span class="chip">重试 {{ draft.retry_times }} 次 / 间隔 {{ draft.retry_interval }}s</span>
            <span class="chip border-warn/40 text-warn">
              并发：{{ draft.concurrency_policy_text || draft.concurrency_policy }}
            </span>
            <span v-if="draft.concurrency_value" class="chip">并发值 {{ draft.concurrency_value }}</span>
            <span class="chip">时区 {{ draft.timezone }}</span>
            <span v-if="draft.repairs?.length" class="chip border-warn/40 text-warn">
              内核自动修正 {{ draft.repairs.length }} 处
            </span>
          </div>

          <div class="mt-3 space-y-2">
            <div>
              <p class="section-label">任务名</p>
              <p class="mt-0.5 text-[11px] text-ink">{{ draft.name }}</p>
            </div>
            <div>
              <p class="section-label">说明</p>
              <p class="mt-0.5 text-[11px] leading-5 text-ink-muted">{{ draft.description }}</p>
            </div>
            <div v-if="draft.command?.length">
              <p class="section-label">命令（argv，不含 shell 展开）</p>
              <pre class="scroll-area mt-1 max-h-32 overflow-auto whitespace-pre-wrap rounded-lg bg-navy-900 p-3 font-mono text-[11px] leading-5 text-ink-muted">{{ draft.command.join(' ') }}</pre>
              <p class="mt-1 text-[10px] text-ink-dim">
                raw_argv：<span class="font-mono">{{ prettyJson(draft.raw_argv) }}</span>
              </p>
            </div>
            <div v-if="draft.http">
              <p class="section-label">HTTP 请求</p>
              <pre class="mt-1 overflow-x-auto rounded-lg bg-navy-900 p-3 font-mono text-[11px] leading-5 text-ink-muted">{{ prettyJson(draft.http) }}</pre>
            </div>
            <div>
              <p class="section-label">内核给出的理由（rationale）</p>
              <p class="mt-0.5 text-[11px] leading-5 text-ink-muted">{{ draft.rationale }}</p>
            </div>
            <div v-if="draft.repairs?.length">
              <p class="section-label text-warn">内核自动修正（repairs）</p>
              <ul class="mt-0.5 space-y-1">
                <li v-for="r in draft.repairs" :key="r" class="text-[11px] leading-5 text-warn">· {{ r }}</li>
              </ul>
            </div>
            <div>
              <p class="section-label">模型身份</p>
              <p class="mt-0.5 font-mono text-[10px] text-ink-dim">
                provider={{ draft.provider || '—' }} · model={{ draft.model || '—（provider 未上报）' }} ·
                prompt={{ draft.prompt_version || '—' }} · stop={{ draft.stop_reason_text || draft.stop_reason }}
              </p>
            </div>
          </div>
        </div>

        <!-- 其它警告 -->
        <div v-if="otherWarnings.length" class="rounded-lg border border-warn/40 bg-warn/10 p-3">
          <p class="text-xs font-medium text-warn">其它警告（{{ otherWarnings.length }} 条）</p>
          <ul class="mt-1 space-y-1">
            <li v-for="w in otherWarnings" :key="w" class="text-[11px] leading-5 text-ink-muted">· {{ w }}</li>
          </ul>
        </div>

        <!-- 覆盖项 -->
        <div class="card card-pad">
          <p class="card-title">确认前可改的 4 个字段（§5.2 overrides）</p>
          <p class="card-sub mt-0.5">
            后端只接受 name / spec / timeout_seconds / retry_times 四项覆盖，其余字段要靠重新起草；
            改完的 spec 会由后端**再次本地复验**，不合法就返回 1009，不会创建出坏任务。
          </p>
          <div class="mt-3 grid gap-2 sm:grid-cols-2">
            <div>
              <p class="section-label">任务名</p>
              <el-input v-model="overrides.name" size="small" maxlength="64" class="mt-1" />
            </div>
            <div>
              <p class="section-label">cron 表达式（6 段更保险）</p>
              <el-input v-model="overrides.spec" size="small" class="mt-1" placeholder="0 0 2 * * *" />
            </div>
            <div>
              <p class="section-label">超时（秒）</p>
              <el-input-number v-model="overrides.timeout_seconds" :min="1" :max="86400" size="small" class="mt-1 w-full" />
            </div>
            <div>
              <p class="section-label">重试次数</p>
              <el-input-number v-model="overrides.retry_times" :min="0" :max="10" size="small" class="mt-1 w-full" />
            </div>
          </div>
        </div>

        <div class="flex flex-wrap items-center gap-2">
          <el-button type="primary" :loading="loading" @click="doConfirm">
            <el-icon class="mr-1" :size="14"><Select /></el-icon>确认创建（confirmed=true）
          </el-button>
          <el-button plain @click="restart">重新起草</el-button>
          <el-button plain @click="close">关闭（不创建）</el-button>
          <span class="text-[10px] text-ink-dim">
            关闭不会留下任何任务：草案还在服务端会话里，未确认就不会写入调度器。
          </span>
        </div>
      </div>

      <!-- ==================== 第 3 步：已完成 ==================== -->
      <div v-else-if="step === 'done' && created" class="space-y-3">
        <div class="card card-pad">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <p class="card-title">已创建任务 #{{ created.job_id }}</p>
            <span class="chip border-ok/40 text-ok">status={{ created.status }}（未分配节点）</span>
          </div>
          <p class="card-sub mt-1">
            status=0 表示还没有绑定节点，**不会**被调度。请到任务列表里编辑并分配节点后才会开始跑。
          </p>
          <div class="mt-3">
            <p class="section-label">cron 表达式</p>
            <p class="mt-0.5 font-mono text-[11px] text-brand-300">{{ created.spec }}</p>
          </div>
          <ol class="mt-3 space-y-1">
            <li
              v-for="(t, i) in created.next_triggers"
              :key="i"
              class="rounded-lg bg-navy-900 px-3 py-1.5 font-mono text-[11px] text-ink-muted"
            >
              {{ i + 1 }}. {{ t }}
            </li>
          </ol>
          <div class="mt-3 rounded-lg border border-navy-600 bg-navy-900 p-3">
            <p class="section-label">trigger_echo</p>
            <pre class="mt-1 whitespace-pre-wrap text-[10px] leading-5 text-ink-muted">{{ created.trigger_echo }}</pre>
          </div>
          <p class="mt-2 text-[10px] text-ink-dim">创建时间：{{ created.created }}</p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <el-button type="primary" plain @click="close">回到任务列表</el-button>
          <el-button plain @click="restart">再建一个</el-button>
        </div>
      </div>
    </div>
  </el-drawer>
</template>