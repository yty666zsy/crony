<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { searchNodes, deleteNode } from '@/api/node'
import StatusBadge from '@/components/StatusBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import { formatNumber, formatRelative, formatSeconds } from '@/utils/format'

/**
 * /servers —— 服务器管理。
 *
 * 后端接口：POST /node/search（已实现，需要 JWT）
 *          POST /node/del（已实现，需要管理员）
 * 响应元素 = models.Node + job_count（resp.RspNodeSearch）。
 */
const loading = ref(false)
const rows = ref([])
const total = ref(0)

const query = reactive({
  hostname: '',
  ip: '',
  status: '',
  page: 1,
  page_size: 10,
})

/** 与后端一致的 PageResult：{ list, total, page, page_size } */
async function load() {
  loading.value = true
  try {
    const res = await searchNodes({
      hostname: query.hostname || undefined,
      ip: query.ip || undefined,
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

function reset() {
  query.hostname = ''
  query.ip = ''
  query.status = ''
  query.page = 1
  load()
}

function onSearch() {
  query.page = 1
  load()
}

async function onDelete(row) {
  try {
    await ElMessageBox.confirm(
      `确认删除节点 ${row.hostname}（${row.uuid}）？该操作需要管理员权限，且会影响其上任务的目标节点解析。`,
      '删除节点',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return // 用户取消
  }
  await deleteNode(row.uuid)
  ElMessage.success('删除请求已提交')
  load()
}

const onlineCount = computed(() => rows.value.filter((n) => n.status === 1).length)
const totalJobs = computed(() => rows.value.reduce((sum, n) => sum + (n.job_count || 0), 0))

onMounted(load)
</script>

<template>
  <div class="space-y-4">
    <!-- 概览条 -->
    <section class="grid-cards">
      <div class="card card-pad">
        <p class="card-sub">节点总数</p>
        <p class="mt-1 text-xl font-semibold text-ink">{{ formatNumber(total) }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">来自 POST /node/search</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页在线</p>
        <p class="mt-1 text-xl font-semibold text-ok">{{ onlineCount }} / {{ rows.length }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">心跳正常（status=1）</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页承载任务</p>
        <p class="mt-1 text-xl font-semibold text-brand-300">{{ totalJobs }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">job_count 字段汇总</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">纳管状态</p>
        <p class="mt-1 text-xl font-semibold text-warn">未纳管</p>
        <p class="mt-1 text-[11px] text-ink-dim">资产纳管（凭据/分组）为阶段 2</p>
      </div>
    </section>

    <!-- 筛选 + 表格 -->
    <section class="card overflow-hidden">
      <div class="panel-head flex-wrap">
        <div class="flex flex-wrap items-center gap-2">
          <el-input
            v-model="query.hostname"
            placeholder="主机名"
            clearable
            size="default"
            class="!w-[190px]"
            @keyup.enter="onSearch"
          >
            <template #prefix><el-icon><Search /></el-icon></template>
          </el-input>
          <el-input
            v-model="query.ip"
            placeholder="IP 地址"
            clearable
            size="default"
            class="!w-[160px]"
            @keyup.enter="onSearch"
          />
          <el-select v-model="query.status" placeholder="连接状态" clearable size="default" class="!w-[140px]">
            <el-option label="在线" :value="1" />
            <el-option label="失联" :value="2" />
          </el-select>
          <el-button type="primary" @click="onSearch">查询</el-button>
          <el-button @click="reset">重置</el-button>
        </div>
        <div class="flex items-center gap-2">
          <el-button plain @click="load">
            <el-icon class="mr-1"><Refresh /></el-icon>刷新
          </el-button>
          <el-button type="primary" plain>
            <el-icon class="mr-1"><Download /></el-icon>导出
          </el-button>
        </div>
      </div>

      <el-table v-loading="loading" :data="rows" style="width: 100%">
        <el-table-column label="主机名" min-width="180">
          <template #default="{ row }">
            <div class="flex items-center gap-2.5">
              <div
                class="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg"
                :class="row.status === 1 ? 'bg-ok/15 text-ok' : 'bg-danger/15 text-danger'"
              >
                <el-icon :size="14"><Monitor /></el-icon>
              </div>
              <div class="min-w-0">
                <p class="truncate text-xs font-medium text-ink">{{ row.hostname }}</p>
                <p class="truncate text-[10px] text-ink-dim">PID {{ row.pid }} · {{ row.version }}</p>
              </div>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="IP" width="140">
          <template #default="{ row }">
            <span class="font-mono text-xs text-ink-muted">{{ row.ip }}</span>
          </template>
        </el-table-column>

        <el-table-column label="UUID" min-width="170">
          <template #default="{ row }">
            <span class="font-mono text-[11px] text-ink-dim">{{ row.uuid }}</span>
          </template>
        </el-table-column>

        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <StatusBadge kind="node" :value="row.status" />
          </template>
        </el-table-column>

        <el-table-column label="最后心跳" width="150">
          <template #default="{ row }">
            <div>
              <p class="text-xs text-ink-muted">{{ formatRelative(row.status === 1 ? row.up : row.down) }}</p>
              <p class="text-[10px] text-ink-dim">
                已运行 {{ formatSeconds(Math.floor(Date.now() / 1000) - row.up) }}
              </p>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="任务数" width="90" align="center">
          <template #default="{ row }">
            <span class="chip" :class="row.job_count ? 'text-brand-300' : ''">{{ row.job_count }}</span>
          </template>
        </el-table-column>

        <el-table-column label="资源水位" width="150">
          <template #default="{ row }">
            <div v-if="row.status === 1" class="space-y-1">
              <div class="flex items-center gap-2">
                <span class="w-8 text-[10px] text-ink-dim">CPU</span>
                <div class="h-1 flex-1 overflow-hidden rounded-full bg-navy-600">
                  <div class="h-full rounded-full bg-brand-500" :style="{ width: row.cpu + '%' }" />
                </div>
                <span class="w-8 text-right text-[10px] text-ink-dim">{{ row.cpu }}%</span>
              </div>
              <div class="flex items-center gap-2">
                <span class="w-8 text-[10px] text-ink-dim">DISK</span>
                <div class="h-1 flex-1 overflow-hidden rounded-full bg-navy-600">
                  <div
                    class="h-full rounded-full"
                    :style="{
                      width: row.disk + '%',
                      backgroundColor: row.disk > 85 ? '#ef4444' : row.disk > 70 ? '#f59e0b' : '#22c55e',
                    }"
                  />
                </div>
                <span class="w-8 text-right text-[10px] text-ink-dim">{{ row.disk }}%</span>
              </div>
            </div>
            <span v-else class="text-[10px] text-ink-dim">失联，无采集</span>
          </template>
        </el-table-column>

        <el-table-column label="标签" min-width="150">
          <template #default="{ row }">
            <div class="flex flex-wrap gap-1">
              <span v-for="t in row.tags || []" :key="t" class="chip">{{ t }}</span>
              <span v-if="row.agent_attached" class="chip border-brand-500/40 text-brand-300">Agent 已挂载</span>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="操作" width="170" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click="$router.push('/terminal')">终端</el-button>
            <el-button link type="primary" size="small" @click="$router.push('/runs')">记录</el-button>
            <el-button link type="danger" size="small" @click="onDelete(row)">删除</el-button>
          </template>
        </el-table-column>

        <template #empty>
          <EmptyState
            icon="Connection"
            title="未接入任何服务器节点"
            :desc="
              query.hostname || query.ip || query.status !== ''
                ? '当前筛选条件下没有匹配的节点，请调整条件后重试。'
                : '在目标机器上启动 crony-node 并指向 admin 服务即可自动注册；节点首次注册后需要管理员确认纳管（阶段 2）。'
            "
            action-text="了解接入方式"
          />
        </template>
      </el-table>

      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-navy-600 px-5 py-3">
        <p class="text-[11px] text-ink-dim">
          共 {{ total }} 条 · 当前第 {{ query.page }} 页 · 每页 {{ query.page_size }} 条
        </p>
        <el-pagination
          v-model:current-page="query.page"
          v-model:page-size="query.page_size"
          :total="total"
          :page-sizes="[10, 20, 50]"
          layout="sizes, prev, pager, next, jumper"
          background
          @current-change="load"
          @size-change="onSearch"
        />
      </div>
    </section>

    <p class="text-[11px] text-ink-dim">
      数据来源：POST /node/search（后端已实现）。响应为
      <code class="text-brand-300">{ list, total, page, page_size }</code>；业务错误同样是 HTTP 200，
      由 api/client.js 的响应拦截按 <code class="text-brand-300">code !== 200</code> 统一弹出提示。
      页面上的资源水位/标签/纳管状态为阶段 2 规划字段（Mock）。
    </p>
  </div>
</template>