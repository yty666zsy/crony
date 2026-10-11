<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { searchKnowledge } from '@/api/knowledge'
import EmptyState from '@/components/EmptyState.vue'
import { formatRelative, formatNumber } from '@/utils/format'

/**
 * /knowledge —— 知识库。
 *
 * ⚠️ 阶段 2 规划页：POST /knowledge/search 尚未实现（后端只有 script/search）。
 *   阶段 2 计划做成"向量检索 + Runbook 沉淀"：文档切分为 chunk 后入库，
 *   Agent 通过 kb_search 工具检索，命中记录写回 audit。
 *
 * 当前 Mock 返回 8 篇运维文档（Runbook / SOP / 复盘 / 规范），
 * 展示元数据（标签、切片数、来源、命中次数），正文切片留待阶段 2 接口。
 */
const loading = ref(false)
const docs = ref([])
const total = ref(0)
const detail = reactive({ visible: false, doc: null })

const query = reactive({ keyword: '', tag: '', type: '', page: 1, page_size: 8 })

const typeMeta = {
  runbook: { text: 'Runbook', color: '#6366f1' },
  sop: { text: 'SOP', color: '#38bdf8' },
  postmortem: { text: '复盘', color: '#f59e0b' },
  standard: { text: '规范', color: '#22c55e' },
  faq: { text: 'FAQ', color: '#8b5cf6' },
}

const allTags = ['数据库', '性能', 'Runbook', '容器', '自愈', '存储', 'SOP', '告警', '网络', '变更', 'Kubernetes', '安全']

async function load() {
  loading.value = true
  try {
    const res = await searchKnowledge({
      keyword: query.keyword || undefined,
      tag: query.tag || undefined,
      type: query.type || undefined,
      page: query.page,
      page_size: query.page_size,
    })
    docs.value = res?.list || []
    total.value = res?.total || 0
  } finally {
    loading.value = false
  }
}

function onSearch() {
  query.page = 1
  load()
}

function reset() {
  query.keyword = ''
  query.tag = ''
  query.type = ''
  onSearch()
}

function openDetail(doc) {
  detail.doc = doc
  detail.visible = true
}

const publishedCount = computed(() => docs.value.filter((d) => d.status === 'published').length)
const totalChunks = computed(() => docs.value.reduce((s, d) => s + (d.chunks || 0), 0))
const totalHits = computed(() => docs.value.reduce((s, d) => s + (d.hit_count || 0), 0))

onMounted(load)
</script>

<template>
  <div class="space-y-4">
    <section class="grid-cards">
      <div class="card card-pad">
        <p class="card-sub">文档总数</p>
        <p class="mt-1 text-xl font-semibold text-ink">{{ total }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">含 Runbook / SOP / 复盘 / 规范</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">本页已发布</p>
        <p class="mt-1 text-xl font-semibold text-ok">{{ publishedCount }} / {{ docs.length }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">status=published 才能被 Agent 检索</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">切片总数</p>
        <p class="mt-1 text-xl font-semibold text-brand-300">{{ formatNumber(totalChunks) }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">阶段 2 向量化后按 chunk 召回</p>
      </div>
      <div class="card card-pad">
        <p class="card-sub">累计命中</p>
        <p class="mt-1 text-xl font-semibold text-violet2">{{ formatNumber(totalHits) }}</p>
        <p class="mt-1 text-[11px] text-ink-dim">被 Agent / 人工检索命中次数</p>
      </div>
    </section>

    <!-- 检索区：模拟"语义检索"入口 -->
    <section class="card card-pad">
      <div class="flex flex-wrap items-center gap-2">
        <el-input
          v-model="query.keyword"
          placeholder="用自然语言检索，例如：mysql 连接超时 怎么排查"
          clearable
          class="!w-[380px] max-w-full"
          @keyup.enter="onSearch"
        >
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <el-select v-model="query.tag" placeholder="标签" clearable class="!w-[150px]">
          <el-option v-for="t in allTags" :key="t" :label="t" :value="t" />
        </el-select>
        <el-select v-model="query.type" placeholder="文档类型" clearable class="!w-[150px]">
          <el-option v-for="(v, k) in typeMeta" :key="k" :label="v.text" :value="k" />
        </el-select>
        <el-button type="primary" @click="onSearch">检索</el-button>
        <el-button @click="reset">重置</el-button>
        <span class="ml-auto chip border-warn/40 text-warn">阶段 2 接入向量检索</span>
      </div>
      <p class="mt-2 text-[11px] text-ink-dim">
        当前按关键词/标签匹配（Mock）。阶段 2 计划：POST /knowledge/search 支持向量召回 +
        重排序，返回 chunk 级片段与出处。
      </p>
    </section>

    <!-- 文档列表 -->
    <section v-loading="loading" class="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <article
        v-for="d in docs"
        :key="d.id"
        class="card card-pad flex flex-col transition-colors hover:border-brand-500/60"
      >
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0">
            <div class="flex items-center gap-2">
              <span
                class="chip shrink-0"
                :style="{
                  color: typeMeta[d.type]?.color,
                  borderColor: (typeMeta[d.type]?.color || '#333') + '55',
                }"
              >
                {{ typeMeta[d.type]?.text || d.type }}
              </span>
              <span v-if="d.status === 'draft'" class="chip border-warn/40 text-warn">草稿</span>
            </div>
            <h3 class="mt-2 truncate text-sm font-semibold text-ink">{{ d.title }}</h3>
            <p class="mt-1 line-clamp-2 text-[11px] leading-5 text-ink-dim">{{ d.summary }}</p>
          </div>
          <span class="chip shrink-0 font-mono text-[10px]">{{ d.id }}</span>
        </div>

        <div class="mt-3 flex flex-wrap gap-1">
          <span v-for="t in d.tags" :key="t" class="chip">{{ t }}</span>
        </div>

        <div class="mt-3 grid grid-cols-3 gap-2 text-center">
          <div class="rounded-lg bg-navy-900 py-1.5">
            <p class="text-[10px] text-ink-dim">切片</p>
            <p class="text-[11px] text-ink-muted">{{ d.chunks }}</p>
          </div>
          <div class="rounded-lg bg-navy-900 py-1.5">
            <p class="text-[10px] text-ink-dim">命中</p>
            <p class="text-[11px] text-ink-muted">{{ d.hit_count }}</p>
          </div>
          <div class="rounded-lg bg-navy-900 py-1.5">
            <p class="text-[10px] text-ink-dim">更新</p>
            <p class="text-[11px] text-ink-muted">{{ formatRelative(d.updated) }}</p>
          </div>
        </div>

        <div class="mt-3 flex items-center gap-2 border-t border-navy-600 pt-3">
          <span class="text-[10px] text-ink-dim">维护者 {{ d.owner }}</span>
          <div class="ml-auto flex gap-2">
            <el-button size="small" plain @click="openDetail(d)">详情</el-button>
            <el-button size="small" type="primary" plain @click="openDetail(d)">查看切片</el-button>
          </div>
        </div>
      </article>

      <div v-if="!loading && !docs.length" class="card lg:col-span-2">
        <EmptyState
          icon="Notebook"
          title="没有检索到匹配的文档"
          desc="换个关键词，或清空标签筛选。阶段 2 会支持自然语言语义检索，命中率会明显提升。"
          action-text="重置筛选"
          @action="reset"
        />
      </div>
    </section>

    <div class="flex items-center justify-between">
      <p class="text-[11px] text-ink-dim">
        共 {{ total }} 篇 · 接口 POST /knowledge/search 规划中（阶段 2）
      </p>
      <el-pagination
        v-model:current-page="query.page"
        v-model:page-size="query.page_size"
        :total="total"
        :page-sizes="[8, 16, 32]"
        layout="sizes, prev, pager, next"
        background
        @current-change="load"
        @size-change="onSearch"
      />
    </div>

    <!-- 文档详情 -->
    <el-drawer v-model="detail.visible" :size="560" :title="detail.doc?.title">
      <div v-if="detail.doc" class="scroll-area h-full px-5 py-4">
        <div class="card card-pad">
          <p class="section-label">摘要</p>
          <p class="mt-1.5 text-xs leading-6 text-ink-muted">{{ detail.doc.summary }}</p>

          <div class="mt-4 flex flex-wrap gap-1">
            <span v-for="t in detail.doc.tags" :key="t" class="chip">{{ t }}</span>
          </div>
        </div>

        <div class="card mt-4 card-pad">
          <p class="section-label">元数据</p>
          <el-descriptions :column="2" border size="small" class="mt-2">
            <el-descriptions-item label="文档 ID">
              <span class="font-mono">{{ detail.doc.id }}</span>
            </el-descriptions-item>
            <el-descriptions-item label="类型">
              {{ typeMeta[detail.doc.type]?.text || detail.doc.type }}
            </el-descriptions-item>
            <el-descriptions-item label="来源">{{ detail.doc.source }}</el-descriptions-item>
            <el-descriptions-item label="维护者">{{ detail.doc.owner }}</el-descriptions-item>
            <el-descriptions-item label="切片数">{{ detail.doc.chunks }}</el-descriptions-item>
            <el-descriptions-item label="命中次数">{{ detail.doc.hit_count }}</el-descriptions-item>
            <el-descriptions-item label="最近更新">
              {{ formatRelative(detail.doc.updated) }}
            </el-descriptions-item>
            <el-descriptions-item label="状态">
              {{ detail.doc.status === 'published' ? '已发布' : '草稿' }}
            </el-descriptions-item>
          </el-descriptions>
        </div>

        <div class="card mt-4 card-pad">
          <p class="section-label">切片正文</p>
          <div class="mt-2 rounded-lg border border-dashed border-navy-500 bg-navy-900 p-4 text-center">
            <el-icon :size="20" class="text-ink-dim"><Files /></el-icon>
            <p class="mt-2 text-xs text-ink-muted">正文切片尚未提供</p>
            <p class="mt-1 text-[11px] leading-5 text-ink-dim">
              该文档已切分为 {{ detail.doc.chunks }} 个 chunk，但正文检索接口
              <code class="text-brand-300">POST /knowledge/chunks</code> 属于阶段 2 规划项，
              后端还没有对应实现，因此这里不展示伪造的正文内容。
            </p>
          </div>
        </div>

        <div class="card mt-4 card-pad">
          <p class="section-label">相关动作</p>
          <div class="mt-2 flex flex-wrap gap-2">
            <el-button size="small" plain @click="ElMessage.info('跳转到引用该文档的 run（阶段 2）')">
              查看引用该文档的执行
            </el-button>
            <el-button size="small" plain @click="ElMessage.info('Agent 会基于该 Runbook 生成步骤（阶段 2）')">
              交给 Agent 执行
            </el-button>
          </div>
        </div>
      </div>
    </el-drawer>
  </div>
</template>