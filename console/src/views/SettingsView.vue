<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { fetchSettings } from '@/api/settings'
import { fetchHealth } from '@/api/statis'
import { changePassword } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'
import { HEALTH_STATUS, BIZ_CODE } from '@/utils/dict'
import { formatTime } from '@/utils/format'

/**
 * /settings —— 系统设置。
 *
 * 重要区分（不要混淆）：
 *   - **只读展示**：鉴权、调度默认值、通知渠道、Agent 运行时快照，
 *     这些值来自后端配置（admin/conf/app.ini + 代码默认值），
 *     控制台在阶段 1 **不提供写接口**，后端也没有对应的 update 路由，
 *     因此这里一律渲染为只读，避免给出"能改但其实改不了"的假象。
 *   - **可写**：修改密码 —— 对应真实接口 POST /user/change_pw。
 *
 * 主题切换（深色/浅色）在顶栏，这里也放一个开关，因为它是用户级偏好而非系统配置。
 */
const auth = useAuthStore()
const app = useAppStore()

const loading = ref(false)
const snapshot = ref(null)
const health = ref(null)
const tab = ref('security')

const pwFormRef = ref(null)
const pwSubmitting = ref(false)
const pwForm = reactive({ old_pw: '', new_pw: '', confirm_pw: '' })

const pwRules = {
  old_pw: [{ required: true, message: '请输入当前口令', trigger: 'blur' }],
  new_pw: [
    { required: true, message: '请输入新口令', trigger: 'blur' },
    { min: 8, message: '新口令至少 8 位（后端 password_min_length=8）', trigger: 'blur' },
  ],
  confirm_pw: [
    { required: true, message: '请再次输入新口令', trigger: 'blur' },
    {
      validator: (_rule, value, cb) =>
        value === pwForm.new_pw ? cb() : cb(new Error('两次输入的新口令不一致')),
      trigger: 'blur',
    },
  ],
}

async function load() {
  loading.value = true
  try {
    const [s, h] = await Promise.all([
      // 规划中接口（后端未实现）：缺失时对应区块显示占位，不弹错。
      fetchSettings().catch(() => null),
      fetchHealth().catch(() => null),
    ])
    snapshot.value = s
    health.value = h
  } finally {
    loading.value = false
  }
}

async function submitPassword() {
  const valid = await pwFormRef.value?.validate().catch(() => false)
  if (!valid) return
  pwSubmitting.value = true
  try {
    await changePassword({
      username: auth.user?.username || 'admin',
      old_pw: pwForm.old_pw,
      new_pw: pwForm.new_pw,
    })
    ElMessage.success('口令已修改（Mock 模式下不会真正变更）')
    pwFormRef.value?.resetFields()
  } finally {
    pwSubmitting.value = false
  }
}

const authConf = computed(() => snapshot.value?.auth || {})
const schedulerConf = computed(() => snapshot.value?.scheduler || {})
const notifyConf = computed(() => snapshot.value?.notify || { channels: [] })
const agentConf = computed(() => snapshot.value?.agent || {})

const healthSummary = computed(() => {
  const list = health.value?.components || []
  return {
    total: list.length,
    ok: list.filter((c) => c.status === 'ok').length,
    bad: list.filter((c) => c.status === 'degraded' || c.status === 'down').length,
  }
})

onMounted(load)
</script>

<template>
  <div v-loading="loading" class="space-y-4">
    <!-- 顶部健康度 -->
    <section class="grid-cards">
      <div class="card card-pad">
        <p class="card-sub">后端健康度</p>
        <div class="mt-1 flex items-baseline gap-2">
          <span
            class="text-xl font-semibold"
            :class="
              health?.status === 'ok' ? 'text-ok' : health?.status === 'degraded' ? 'text-warn' : 'text-danger'
            "
          >
            {{ health?.status === 'ok' ? '正常' : health?.status === 'degraded' ? '降级' : '异常' }}
          </span>
          <span class="text-[11px] text-ink-dim">
            {{ healthSummary.ok }}/{{ healthSummary.total }} 组件正常
          </span>
        </div>
        <p class="mt-1 text-[11px] text-ink-dim">
          检查时间 {{ health?.checked_at ? formatTime(health.checked_at) : '—' }}
        </p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">鉴权</p>
        <p class="mt-1 text-sm font-semibold text-ink">JWT · {{ authConf.jwt_ttl_hours }} 小时</p>
        <p class="mt-1 text-[11px] text-ink-dim">issuer {{ authConf.jwt_issuer }}</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">默认并发策略</p>
        <p class="mt-1 text-sm font-semibold text-warn">{{ schedulerConf.default_concurrency_policy }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">阶段 1 任务模型 2.0 规划值</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">Agent 运行时</p>
        <p class="mt-1 text-sm font-semibold" :class="agentConf.enabled ? 'text-ok' : 'text-warn'">
          {{ agentConf.enabled ? '已启用' : '未启用（阶段 2）' }}
        </p>
        <p class="mt-1 text-[11px] text-ink-dim">{{ agentConf.autonomy_level }}</p>
      </div>
    </section>

    <section class="card overflow-hidden">
      <el-tabs v-model="tab" class="px-5 pt-3">
        <!-- 安全 -->
        <el-tab-pane label="安全与鉴权" name="security">
          <div class="space-y-4 pb-5">
            <div class="card card-pad">
              <p class="card-title">鉴权配置（只读）</p>
              <p class="card-sub mt-0.5">
                来源：admin/conf/app.ini 与代码默认值。阶段 1 后端没有 update 路由，控制台不提供修改入口。
              </p>
              <el-descriptions :column="2" border size="small" class="mt-3">
                <el-descriptions-item label="JWT issuer">{{ authConf.jwt_issuer }}</el-descriptions-item>
                <el-descriptions-item label="JWT 有效期">{{ authConf.jwt_ttl_hours }} 小时</el-descriptions-item>
                <el-descriptions-item label="密钥来源">{{ authConf.jwt_secret_source }}</el-descriptions-item>
                <el-descriptions-item label="口令最小长度">{{ authConf.password_min_length }}</el-descriptions-item>
                <el-descriptions-item label="强制改密">{{ authConf.force_password_change ? '是' : '否' }}</el-descriptions-item>
                <el-descriptions-item label="RBAC">{{ authConf.rbac }}</el-descriptions-item>
                <el-descriptions-item label="引导管理员" :span="2">
                  {{ authConf.bootstrap_admin }}
                </el-descriptions-item>
              </el-descriptions>
            </div>

            <div class="card card-pad">
              <p class="card-title">修改口令</p>
              <p class="card-sub mt-0.5">
                对应真实接口 <code class="text-brand-300">POST /user/change_pw</code>
                （需 JWT；业务错误仍返回 HTTP 200，code=1000 时为原口令错误）。
              </p>
              <el-form
                ref="pwFormRef"
                :model="pwForm"
                :rules="pwRules"
                label-width="100px"
                class="mt-3 max-w-lg"
              >
                <el-form-item label="当前口令" prop="old_pw">
                  <el-input v-model="pwForm.old_pw" type="password" show-password placeholder="当前登录口令" />
                </el-form-item>
                <el-form-item label="新口令" prop="new_pw">
                  <el-input v-model="pwForm.new_pw" type="password" show-password placeholder="至少 8 位" />
                </el-form-item>
                <el-form-item label="确认新口令" prop="confirm_pw">
                  <el-input v-model="pwForm.confirm_pw" type="password" show-password placeholder="再次输入" />
                </el-form-item>
                <el-form-item>
                  <el-button type="primary" :loading="pwSubmitting" @click="submitPassword">
                    提交修改
                  </el-button>
                  <el-button @click="pwFormRef?.resetFields()">重置</el-button>
                </el-form-item>
              </el-form>
              <p class="text-[11px] text-warn">
                Mock 模式下仅校验表单并回显成功，不会真的修改口令。
              </p>
            </div>
          </div>
        </el-tab-pane>

        <!-- 调度 -->
        <el-tab-pane label="调度与任务默认值" name="scheduler">
          <div class="space-y-4 pb-5">
            <div class="card card-pad">
              <p class="card-title">任务默认值（只读 · 阶段 1 规划）</p>
              <p class="card-sub mt-0.5">
                这些是"任务模型 2.0"要引入的调度侧默认值；当前 models.Job 只有 timeout /
                retry_times / retry_interval，尚无并发策略列。
              </p>
              <div class="mt-3 grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
                <div class="rounded-lg bg-navy-900 p-3">
                  <p class="text-[10px] text-ink-dim">默认并发策略</p>
                  <p class="mt-1 text-sm text-warn">{{ schedulerConf.default_concurrency_policy }}</p>
                  <p class="mt-1 text-[10px] text-ink-dim">并发触发时跳过本轮，并产生 status=5 的 run</p>
                </div>
                <div class="rounded-lg bg-navy-900 p-3">
                  <p class="text-[10px] text-ink-dim">默认重试</p>
                  <p class="mt-1 text-sm text-ink">{{ schedulerConf.default_retry_times }} 次 / 间隔 {{ schedulerConf.default_retry_interval }}s</p>
                  <p class="mt-1 text-[10px] text-ink-dim">上限 {{ schedulerConf.max_retry_times }} 次</p>
                </div>
                <div class="rounded-lg bg-navy-900 p-3">
                  <p class="text-[10px] text-ink-dim">默认超时</p>
                  <p class="mt-1 text-sm text-ink">{{ schedulerConf.default_timeout_seconds }}s（0=不限）</p>
                  <p class="mt-1 text-[10px] text-ink-dim">超时后按进程组取消</p>
                </div>
                <div class="rounded-lg bg-navy-900 p-3">
                  <p class="text-[10px] text-ink-dim">默认分配方式</p>
                  <p class="mt-1 text-sm text-ink">{{ schedulerConf.default_allocation }}</p>
                  <p class="mt-1 text-[10px] text-ink-dim">auto = 调度器选取可用节点</p>
                </div>
              </div>
            </div>

            <div class="card card-pad">
              <p class="card-title">组件状态</p>
              <div class="mt-3 space-y-2">
                <div
                  v-for="c in health?.components || []"
                  :key="c.name"
                  class="flex flex-wrap items-center gap-3 rounded-lg bg-navy-900 px-3.5 py-2.5"
                >
                  <span
                    class="inline-block h-2 w-2 shrink-0 rounded-full"
                    :style="{ backgroundColor: HEALTH_STATUS[c.status]?.color }"
                  />
                  <span class="text-xs text-ink">{{ c.name }}</span>
                  <span
                    class="chip"
                    :style="{
                      color: HEALTH_STATUS[c.status]?.color,
                      borderColor: (HEALTH_STATUS[c.status]?.color || '#333') + '55',
                    }"
                  >
                    {{ HEALTH_STATUS[c.status]?.text }}
                  </span>
                  <span class="text-[11px] text-ink-dim">{{ c.detail }}</span>
                  <span v-if="c.latency_ms" class="ml-auto text-[11px] text-ink-dim">
                    {{ c.latency_ms }} ms
                  </span>
                </div>
              </div>
              <p class="mt-2 text-[11px] text-ink-dim">
                GET /statis/health 为规划中接口（阶段 1）。
              </p>
            </div>
          </div>
        </el-tab-pane>

        <!-- 通知 -->
        <el-tab-pane label="通知渠道" name="notify">
          <div class="space-y-4 pb-5">
            <div class="card card-pad">
              <p class="card-title">渠道（只读）</p>
              <p class="card-sub mt-0.5">
                实际渠道配置在 node/admin 的配置文件里（钉钉、SMTP）。控制台暂不提供写接口。
              </p>
              <div class="mt-3 space-y-2">
                <div
                  v-for="ch in notifyConf.channels"
                  :key="ch.key"
                  class="flex items-center gap-3 rounded-lg bg-navy-900 px-3.5 py-3"
                >
                  <el-icon :size="15" class="text-ink-dim"><Bell /></el-icon>
                  <div class="min-w-0 flex-1">
                    <p class="text-xs text-ink">{{ ch.label }}</p>
                    <p class="text-[10px] text-ink-dim">
                      {{ ch.supported ? '后端已支持' : '后端未实现（规划中）' }}
                    </p>
                  </div>
                  <el-tag :type="ch.enabled ? 'success' : 'info'" size="small" effect="dark">
                    {{ ch.enabled ? '已启用' : '未启用' }}
                  </el-tag>
                </div>
              </div>
            </div>

            <div class="card card-pad">
              <p class="card-title">通知中的错误码含义</p>
              <p class="card-sub mt-0.5">用于排查"操作失败"提示的真实原因（来自后端 resp/rsp.go）</p>
              <div class="mt-3 grid gap-2 sm:grid-cols-2">
                <div v-for="(text, code) in BIZ_CODE" :key="code" class="flex items-center gap-3 rounded-lg bg-navy-900 px-3 py-2">
                  <span class="font-mono text-[11px] text-brand-300">{{ code }}</span>
                  <span class="text-[11px] text-ink-muted">{{ text }}</span>
                </div>
              </div>
            </div>
          </div>
        </el-tab-pane>

        <!-- Agent -->
        <el-tab-pane label="AI Agent（阶段 2 · 只读）" name="agent">
          <div class="space-y-4 pb-5">
            <div class="card card-pad">
              <div class="flex flex-wrap items-center justify-between gap-3">
                <div>
                  <p class="card-title">Agent 能力与预算</p>
                  <p class="card-sub mt-0.5">
                    阶段 2 的内核已落地（会话 / 轨迹 / 诊断 / 自然语言建任务），但本控制台的数据仍来自 Mock；
                    **没有**真实的模型凭据，因此不声明任何准确率。
                  </p>
                </div>
                <el-tag :type="agentConf.enabled ? 'success' : 'warning'" effect="dark">
                  {{ agentConf.enabled ? '后端配置已启用' : '后端配置未启用' }}
                </el-tag>
              </div>

              <el-descriptions :column="2" border size="small" class="mt-3">
                <el-descriptions-item label="provider">
                  {{ agentConf.provider || '—' }}
                </el-descriptions-item>
                <el-descriptions-item label="model">
                  <span class="font-mono">{{ agentConf.model || '—（provider 未上报）' }}</span>
                </el-descriptions-item>
                <el-descriptions-item label="自治等级">{{ agentConf.autonomy_level }}</el-descriptions-item>
                <el-descriptions-item label="advisory_only">
                  <span class="font-mono">{{ agentConf.advisory_only }}</span>
                </el-descriptions-item>
                <el-descriptions-item label="只读工具数">{{ agentConf.readonly_tools }}</el-descriptions-item>
                <el-descriptions-item label="prompt 版本">
                  {{ (agentConf.prompt_versions || []).join('、') }}
                </el-descriptions-item>
                <el-descriptions-item label="单会话工具调用上限">
                  {{ agentConf.max_tool_calls_per_session }} 次
                </el-descriptions-item>
                <el-descriptions-item label="预算（调用 / token / 时长）">
                  {{ agentConf.budget?.calls }} / {{ agentConf.budget?.tokens }} / {{ agentConf.budget?.duration }}
                </el-descriptions-item>
              </el-descriptions>

              <ul class="mt-4 space-y-1.5 text-[11px] leading-5 text-ink-dim">
                <li>· 阶段 2 的工具**全部只读**（shell_readonly / http_get / node_status / log_search / mysql_query_readonly），
                  allow_hosts 为空时 http_get 会被直接拒绝。</li>
                <li>· 诊断结果 advisory_only=true：只输出建议，**没有**执行/应用按钮；唯一能做的动作是复制文本。</li>
                <li>· 阶段 3 才会出现变更类动作与审批流；届时每次工具调用写入 audit_event，审批通过后才由 Agent 触发 job_run。</li>
                <li>· 数据口径：Agent 的所有产物都必须能回到某个 session_id / run_id，否则不写知识库。</li>
                <li>· 「需要审批的动作」当前列表为占位文案：阶段 2 根本不产生变更动作，所以这些审批项永远不会被触发。</li>
              </ul>
            </div>
          </div>
        </el-tab-pane>

        <!-- 外观 -->
        <el-tab-pane label="外观" name="appearance">
          <div class="space-y-4 pb-5">
            <div class="card card-pad">
              <p class="card-title">主题</p>
              <p class="card-sub mt-0.5">
                默认深色（观测站基线为深海军蓝）；浅色主题为可用但未做逐页视觉校验的骨架实现。
              </p>
              <div class="mt-3 flex items-center gap-3">
                <el-radio-group :model-value="app.theme" @change="app.applyTheme($event)">
                  <el-radio-button value="dark">深色</el-radio-button>
                  <el-radio-button value="light">浅色</el-radio-button>
                </el-radio-group>
                <span class="text-[11px] text-ink-dim">当前：{{ app.theme }}</span>
              </div>
            </div>

            <div class="card card-pad">
              <p class="card-title">布局</p>
              <div class="mt-3 flex flex-wrap items-center gap-3">
                <el-button @click="app.collapseAll()">收起全部菜单</el-button>
                <el-button @click="app.expandAll()">展开全部菜单</el-button>
                <span class="text-[11px] text-ink-dim">
                  视口 ≤1024px 时侧边栏自动收为图标栏（硬性响应式要求）
                </span>
              </div>
            </div>
          </div>
        </el-tab-pane>
      </el-tabs>
    </section>
  </div>
</template>