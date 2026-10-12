<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { searchJobs, saveJob, deleteJob, runJobOnce, findJob } from '@/api/job'
import { searchNodes } from '@/api/node'
import StatusBadge from '@/components/StatusBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import NlJobWizard from '@/components/NlJobWizard.vue'
import MockBadge from '@/components/MockBadge.vue'
import { concurrencyPolicyOf, CONCURRENCY_POLICY_LIST, JOB_STATUS, JOB_TYPE } from '@/utils/dict'
import { formatRelative, formatSeconds, truncate } from '@/utils/format'

/**
 * /jobs —— 定时任务。
 *
 * 后端接口（全部已实现）：
 *   POST /job/search  列表（分页）
 *   GET  /job/find    详情
 *   POST /job/add     新增/更新（同一个 handler）
 *   POST /job/del     删除
 *   POST /job/once    立即执行一次
 *
 * 并发策略（concurrency_policy）是**已实现字段**：`models.Job.concurrency_policy` 是 int 0..4
 * （见 common/pkg/runpolicy），本页提交 int，后端 `request.ValidConcurrencyPolicy` 校验；
 * node 执行时按它决定跳过 / 排队 / 取消或终止在跑的，并写进 `job_run.policy_snapshot`。
 */

/** 并发策略 → 徽标配色（0=Skip 警告 / 1-2 缓冲 主色 / 3-4 强杀 危险）。 */
function concurrencyClass(value) {
  const p = concurrencyPolicyOf(value)
  if (!p) return ''
  return {
    0: 'border-warn/40 text-warn',
    1: 'border-brand-500/40 text-brand-300',
    2: 'border-brand-500/40 text-brand-300',
    3: 'border-danger/40 text-danger',
    4: 'border-danger/40 text-danger',
  }[p.value]
}
const loading = ref(false)
const rows = ref([])
const total = ref(0)
const nodeOptions = ref([])

const query = reactive({
  name: '',
  run_on: '',
  job_type: '',
  status: '',
  page: 1,
  page_size: 10,
})

const drawer = reactive({
  visible: false,
  mode: 'create', // create | edit
  submitting: false,
})

const form = reactive({
  id: null,
  name: '',
  command: '',
  spec: '0 3 * * *',
  job_type: 1,
  http_method: 2,
  run_on: '',
  allocation: 2,
  status: 1,
  timeout: 600,
  retry_times: 2,
  retry_interval: 5,
  notify_type: 1,
  concurrency_policy: 0,
  note: '',
})

const formRef = ref(null)

const rules = {
  name: [{ required: true, message: '请输入任务名称', trigger: 'blur' }],
  command: [{ required: true, message: '请输入执行命令或 URL', trigger: 'blur' }],
  spec: [{ required: true, message: '请输入 cron 表达式', trigger: 'blur' }],
  job_type: [{ required: true, message: '请选择任务类型', trigger: 'change' }],
}

async function load() {
  loading.value = true
  try {
    const res = await searchJobs({
      name: query.name || undefined,
      run_on: query.run_on || undefined,
      job_type: query.job_type === '' ? undefined : query.job_type,
      status: query.status === '' ? undefined : query.status,
      page: query.page,
      page_size: query.page_size,
    })
    rows.value = res?.list || []
    total.value = res?.total || 0
  } finally {
    loading.value = false
  }
}

async function loadNodes() {
  const res = await searchNodes({ page: 1, page_size: 100 })
  nodeOptions.value = res?.list || []
}

function onSearch() {
  query.page = 1
  load()
}

function reset() {
  query.name = ''
  query.run_on = ''
  query.job_type = ''
  query.status = ''
  onSearch()
}

function openCreate() {
  drawer.mode = 'create'
  Object.assign(form, {
    id: null,
    name: '',
    command: '',
    spec: '0 3 * * *',
    job_type: 1,
    http_method: 2,
    run_on: '',
    allocation: 2,
    status: 1,
    timeout: 600,
    retry_times: 2,
    retry_interval: 5,
    notify_type: 1,
    concurrency_policy: 0,
    note: '',
  })
  drawer.visible = true
}

async function openEdit(row) {
  drawer.mode = 'edit'
  let detail = row
  try {
    detail = await findJob(row.id)
  } catch {
    detail = row // 详情接口失败时退回列表行
  }
  Object.assign(form, {
    id: detail.id,
    name: detail.name,
    command: detail.command,
    spec: detail.spec,
    job_type: detail.job_type || 1,
    http_method: detail.http_method || 2,
    run_on: detail.run_on || '',
    allocation: detail.allocation || 2,
    status: detail.status ?? 1,
    timeout: detail.timeout ?? 0,
    retry_times: detail.retry_times ?? 0,
    retry_interval: detail.retry_interval ?? 0,
    notify_type: detail.notify_type ?? 1,
    concurrency_policy: concurrencyPolicyOf(detail.concurrency_policy)?.value ?? 0,
    note: detail.note || '',
  })
  drawer.visible = true
}

async function submitForm() {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return

  drawer.submitting = true
  try {
    await saveJob({ ...form })
    ElMessage.success(drawer.mode === 'create' ? '任务已创建（Mock）' : '任务已更新（Mock）')
    drawer.visible = false
    load()
  } finally {
    drawer.submitting = false
  }
}

async function onRunOnce(row) {
  const res = await runJobOnce({ job_id: row.id, node_uuid: row.run_on })
  ElMessage.success(`已提交一次性执行：${res?.run_id || '已受理'}`)
}

async function onDelete(row) {
  await deleteJob(row.id)
  ElMessage.success('删除请求已提交（Mock）')
  load()
}

const activeCount = computed(() => rows.value.filter((j) => j.status === 1).length)

/** 自然语言建任务向导（阶段 2 契约 §5）：草案 → 核对 5 次触发时间 → 确认创建 */
const nlOpen = ref(false)

onMounted(() => {
  load()
  loadNodes()
})
</script>

<template>
  <div class="space-y-4">
    <section class="grid-cards">
      <div class="card card-pad">
        <p class="card-sub">任务总数</p>
        <p class="mt-1 text-xl font-semibold text-ink">{{ total }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">POST /job/search 返回的 total</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页已分配节点</p>
        <p class="mt-1 text-xl font-semibold text-ok">{{ activeCount }} / {{ rows.length }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">status=1 表示已绑定 run_on</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">平均成功率</p>
        <p class="mt-1 text-xl font-semibold text-brand-300">
          {{
            rows.length
              ? (rows.reduce((s, j) => s + (j.success_rate || 0), 0) / rows.length).toFixed(1)
              : '—'
          }}%
        </p>
        <p class="mt-1 text-[11px] text-ink-dim">基于任务行返回的 success_rate 聚合</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">未分配任务</p>
        <p class="mt-1 text-xl font-semibold text-warn">
          {{ rows.filter((j) => j.status === 0).length }}
        </p>
        <p class="mt-1 text-[11px] text-ink-dim">高风险任务需人工确认（阶段 3 审批）</p>
      </div>
    </section>

    <section class="card overflow-hidden">
      <div class="panel-head flex-wrap">
        <div class="flex flex-wrap items-center gap-2">
          <el-input
            v-model="query.name"
            placeholder="任务名称"
            clearable
            class="!w-[190px]"
            @keyup.enter="onSearch"
          >
            <template #prefix><el-icon><Search /></el-icon></template>
          </el-input>
          <el-select v-model="query.run_on" placeholder="目标节点" clearable class="!w-[180px]">
            <el-option
              v-for="n in nodeOptions"
              :key="n.uuid"
              :label="`${n.hostname} (${n.ip})`"
              :value="n.uuid"
            />
          </el-select>
          <el-select v-model="query.job_type" placeholder="类型" clearable class="!w-[130px]">
            <el-option v-for="(v, k) in JOB_TYPE" :key="k" :label="v.text" :value="Number(k)" />
          </el-select>
          <el-select v-model="query.status" placeholder="分配状态" clearable class="!w-[130px]">
            <el-option v-for="(v, k) in JOB_STATUS" :key="k" :label="v.text" :value="Number(k)" />
          </el-select>
          <el-button type="primary" @click="onSearch">查询</el-button>
          <el-button @click="reset">重置</el-button>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <MockBadge text="Mock 任务数据" />
          <el-button @click="nlOpen = true">
            <el-icon class="mr-1"><MagicStick /></el-icon>用自然语言建任务
          </el-button>
          <el-button type="primary" @click="openCreate">
            <el-icon class="mr-1"><Plus /></el-icon>新建任务
          </el-button>
        </div>
      </div>

      <el-table v-loading="loading" :data="rows" style="width: 100%">
        <el-table-column label="任务名" min-width="200" fixed="left">
          <template #default="{ row }">
            <div class="min-w-0">
              <p class="truncate text-xs font-medium text-ink">{{ row.name }}</p>
              <p class="truncate text-[10px] text-ink-dim">#{{ row.id }} · {{ formatRelative(row.updated) }} 更新</p>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="命令 / URL" min-width="230">
          <template #default="{ row }">
            <el-tooltip :content="row.command" placement="top" :show-after="400">
              <span class="font-mono text-[11px] text-ink-muted">{{ truncate(row.command, 46) }}</span>
            </el-tooltip>
          </template>
        </el-table-column>

        <el-table-column label="Cron 表达式" width="140">
          <template #default="{ row }">
            <span class="rounded bg-navy-700 px-1.5 py-0.5 font-mono text-[11px] text-brand-300">
              {{ row.spec }}
            </span>
          </template>
        </el-table-column>

        <el-table-column label="目标节点" min-width="150">
          <template #default="{ row }">
            <div v-if="row.run_on" class="min-w-0">
              <p class="truncate text-xs text-ink-muted">{{ row.host_name }}</p>
              <p class="truncate font-mono text-[10px] text-ink-dim">{{ row.ip }}</p>
            </div>
            <span v-else class="chip border-warn/40 text-warn">未分配</span>
          </template>
        </el-table-column>

        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <StatusBadge kind="job" :value="row.status" />
          </template>
        </el-table-column>

        <el-table-column label="类型" width="110">
          <template #default="{ row }">
            <span class="chip">{{ JOB_TYPE[row.job_type]?.text }}</span>
          </template>
        </el-table-column>

        <el-table-column label="重试策略" width="170">
          <template #default="{ row }">
            <p class="text-[11px] text-ink-muted">
              重试 {{ row.retry_times }} 次 /
              <span v-if="row.retry_interval > 0">间隔 {{ row.retry_interval }}s</span>
              <span v-else>立即重试</span>
            </p>
            <p class="text-[10px] text-ink-dim">超时 {{ formatSeconds(row.timeout) }}</p>
          </template>
        </el-table-column>

        <el-table-column label="并发策略" width="150">
          <template #default="{ row }">
            <span
              class="chip"
              :class="concurrencyClass(row.concurrency_policy)"
            >
              {{ concurrencyPolicyOf(row.concurrency_policy)?.text || '未设置' }}
            </span>
            <p class="mt-0.5 text-[10px] text-ink-dim">随任务保存（0=Skip）</p>
          </template>
        </el-table-column>

        <el-table-column label="最近执行" width="130">
          <template #default="{ row }">
            <div v-if="row.last_run_status">
              <StatusBadge kind="run" :value="row.last_run_status" size="small" />
              <p class="mt-1 text-[10px] text-ink-dim">{{ row.success_rate }}% 成功率</p>
            </div>
            <span v-else class="text-[10px] text-ink-dim">暂无记录</span>
          </template>
        </el-table-column>

        <el-table-column label="操作" width="190" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click="onRunOnce(row)">立即执行</el-button>
            <el-button link type="primary" size="small" @click="openEdit(row)">编辑</el-button>
            <el-button link type="danger" size="small" @click="onDelete(row)">删除</el-button>
          </template>
        </el-table-column>

        <template #empty>
          <EmptyState
            icon="Timer"
            title="还没有定时任务"
            desc="创建一个周期任务：填写 cron 表达式、执行命令与目标节点即可。新任务默认需要先被分配节点（status=0 → 1）才会开始调度。"
            action-text="新建第一个任务"
            @action="openCreate"
          />
        </template>
      </el-table>

      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-navy-600 px-5 py-3">
        <p class="text-[11px] text-ink-dim">共 {{ total }} 条</p>
        <el-pagination
          v-model:current-page="query.page"
          v-model:page-size="query.page_size"
          :total="total"
          :page-sizes="[10, 20, 50]"
          layout="sizes, prev, pager, next"
          background
          @current-change="load"
          @size-change="onSearch"
        />
      </div>
    </section>

    <!-- 自然语言建任务向导（阶段 2 契约 §5.1 草案 / §5.2 确认） -->
    <NlJobWizard v-model="nlOpen" @created="load" />

    <!-- 新建 / 编辑抽屉 -->
    <el-drawer v-model="drawer.visible" :size="520" :title="drawer.mode === 'create' ? '新建定时任务' : `编辑任务 #${form.id}`">
      <div class="scroll-area h-full px-5 py-4">
        <el-form ref="formRef" :model="form" :rules="rules" label-width="112px" label-position="right">
          <el-form-item label="任务名称" prop="name">
            <el-input v-model="form.name" placeholder="例如：清理 90 天前日志" maxlength="64" show-word-limit />
          </el-form-item>

          <el-form-item label="任务类型" prop="job_type">
            <el-radio-group v-model="form.job_type">
              <el-radio :value="1">Shell 命令</el-radio>
              <el-radio :value="2">HTTP 请求</el-radio>
            </el-radio-group>
          </el-form-item>

          <el-form-item :label="form.job_type === 2 ? '请求 URL' : '执行命令'" prop="command">
            <el-input
              v-model="form.command"
              type="textarea"
              :rows="3"
              :placeholder="form.job_type === 2 ? 'https://intranet.example.com/api/check' : 'bash /opt/app/check.sh --all'"
            />
            <p class="mt-1 text-[11px] text-ink-dim">
              Shell 命令会在目标节点上以进程组方式启动（阶段 1 已支持整组取消）。
            </p>
          </el-form-item>

          <el-form-item v-if="form.job_type === 2" label="HTTP 方法">
            <el-select v-model="form.http_method" class="w-full">
              <el-option label="GET" :value="1" />
              <el-option label="POST" :value="2" />
            </el-select>
          </el-form-item>

          <el-form-item label="Cron 表达式" prop="spec">
            <el-input v-model="form.spec" placeholder="分 时 日 月 周，例如 0 3 * * *" />
            <p class="mt-1 text-[11px] text-ink-dim">
              例：<code class="text-brand-300">0 3 * * *</code> 每天 03:00；
              <code class="text-brand-300">*/30 * * * *</code> 每 30 分钟。
            </p>
          </el-form-item>

          <el-form-item label="目标节点">
            <el-select v-model="form.run_on" placeholder="选择 crony-node 节点" clearable class="w-full">
              <el-option
                v-for="n in nodeOptions"
                :key="n.uuid"
                :label="`${n.hostname} · ${n.ip}${n.status === 2 ? '（失联）' : ''}`"
                :value="n.uuid"
                :disabled="n.status === 2"
              />
            </el-select>
          </el-form-item>

          <el-form-item label="节点分配">
            <el-radio-group v-model="form.allocation">
              <el-radio :value="2">自动分配</el-radio>
              <el-radio :value="1">手动指定</el-radio>
            </el-radio-group>
          </el-form-item>

          <el-divider content-position="left">
            <span class="text-[11px] text-ink-dim">重试与超时（后端已支持）</span>
          </el-divider>

          <el-form-item label="重试次数">
            <el-input-number v-model="form.retry_times" :min="0" :max="100" />
            <span class="ml-2 text-[11px] text-ink-dim">上限 100（阶段 0 约束，防重试风暴）</span>
          </el-form-item>

          <el-form-item label="重试间隔">
            <el-input-number v-model="form.retry_interval" :min="0" :max="3600" />
            <span class="ml-2 text-[11px] text-ink-dim">秒；0 = 立即重试</span>
          </el-form-item>

          <el-form-item label="超时时间">
            <el-input-number v-model="form.timeout" :min="0" :max="86400" />
            <span class="ml-2 text-[11px] text-ink-dim">秒；0 = 不限制</span>
          </el-form-item>

          <el-divider content-position="left">
            <span class="text-[11px] text-ink-dim">并发策略</span>
          </el-divider>

          <el-form-item label="并发策略">
            <el-select v-model="form.concurrency_policy" class="w-full">
              <el-option
                v-for="p in CONCURRENCY_POLICY_LIST"
                :key="p.value"
                :label="p.text"
                :value="p.value"
              />
            </el-select>
            <p class="mt-1 text-[11px] text-ink-dim">
              随任务保存并生效：node 执行时按该策略决定「跳过 / 排队 / 取消或终止在跑的」（见
              common/pkg/runpolicy）。
            </p>
          </el-form-item>

          <el-form-item label="备注">
            <el-input v-model="form.note" type="textarea" :rows="2" maxlength="512" show-word-limit />
          </el-form-item>
        </el-form>
      </div>

      <template #footer>
        <div class="flex items-center justify-between px-1">
          <span class="text-[11px] text-ink-dim">
            POST {{ form.id ? '/job/add（更新）' : '/job/add（新增）' }}
          </span>
          <div>
            <el-button @click="drawer.visible = false">取消</el-button>
            <el-button type="primary" :loading="drawer.submitting" @click="submitForm">
              保存
            </el-button>
          </div>
        </div>
      </template>
    </el-drawer>
  </div>
</template>