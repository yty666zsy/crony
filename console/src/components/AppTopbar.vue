<script setup>
import { computed, ref } from 'vue'
import { useRoute } from 'vue-router'
import { useAppStore } from '@/stores/app'
import { formatRelative } from '@/utils/format'
import { ALERT_LEVEL } from '@/utils/dict'

/**
 * 顶栏：当前页标题 + 副标题（面包屑风格）+ 右侧 "Visit Website" / 主题切换 / 通知。
 *
 * 通知面板在阶段 1 用本地 Mock 列表（阶段 2 接 告警中心 的真实接口），
 * 目的是让"通知图标 + 未读徽标"这个信息架构先立起来。
 */
const app = useAppStore()
const route = useRoute()

const siteUrl = import.meta.env.VITE_SITE_URL || 'https://github.com/tmnhs/crony'

const title = computed(() => route.meta?.title || 'Console')
const subtitle = computed(() => route.meta?.subtitle || '')
const group = computed(() => route.meta?.group || '')

const notifications = ref([
  {
    id: 'AL-500',
    level: 'critical',
    title: 'MySQL 慢查询巡检连续 2 次连接超时',
    node: 'node-prod-02',
    time: Math.floor(Date.now() / 1000) - 42 * 60,
  },
  {
    id: 'AL-499',
    level: 'warning',
    title: 'node-db-01 心跳丢失超过 20 分钟',
    node: 'node-db-01',
    time: Math.floor(Date.now() / 1000) - 23 * 60,
  },
  {
    id: 'AL-501',
    level: 'critical',
    title: 'node-edge-01 /data 磁盘使用率 91%',
    node: 'node-edge-01',
    time: Math.floor(Date.now() / 1000) - 2 * 3600,
  },
])

const unread = computed(() => notifications.value.length)
</script>

<template>
  <header
    class="sticky top-0 z-30 flex h-16 shrink-0 items-center gap-3 border-b border-navy-600 bg-navy-900/85 px-5 backdrop-blur"
  >
    <!-- 窄屏展开按钮 -->
    <button
      v-if="app.collapsed"
      class="flex h-8 w-8 items-center justify-center rounded-md text-ink-muted transition-colors hover:bg-navy-700 hover:text-ink"
      title="展开侧边栏"
      @click="app.toggleSidebar()"
    >
      <el-icon :size="16"><Expand /></el-icon>
    </button>

    <!-- 标题 + 面包屑副标题 -->
    <div class="min-w-0 flex-1">
      <div class="flex items-center gap-2">
        <h1 class="truncate text-[15px] font-semibold leading-5 text-ink">{{ title }}</h1>
        <span v-if="route.meta?.planned" class="chip border-warn/40 text-warn">
          {{ route.meta.planned }}
        </span>
      </div>
      <p class="truncate text-[11px] leading-4 text-ink-dim">
        <span v-if="group" class="text-ink-dim">{{ group }}</span>
        <span v-if="group && subtitle" class="mx-1.5 text-navy-500">/</span>
        <span>{{ subtitle }}</span>
      </p>
    </div>

    <!-- 右侧操作区 -->
    <div class="flex shrink-0 items-center gap-1.5">
      <a
        :href="siteUrl"
        target="_blank"
        rel="noopener noreferrer"
        class="hide-sm hidden items-center gap-1.5 rounded-lg border border-navy-600 px-3 py-1.5 text-xs text-ink-muted transition-colors hover:border-brand-500 hover:text-ink md:inline-flex"
      >
        <el-icon :size="13"><Link /></el-icon>
        Visit Website
      </a>

      <el-tooltip :content="app.theme === 'dark' ? '切换到浅色主题' : '切换到深色主题'" placement="bottom">
        <button
          class="flex h-8 w-8 items-center justify-center rounded-lg border border-navy-600 text-ink-muted transition-colors hover:border-brand-500 hover:text-ink"
          @click="app.toggleTheme()"
        >
          <el-icon :size="14"><component :is="app.theme === 'dark' ? 'Sunny' : 'Moon'" /></el-icon>
        </button>
      </el-tooltip>

      <el-popover placement="bottom-end" :width="340" trigger="click" popper-class="crony-popover">
        <template #reference>
          <button
            class="relative flex h-8 w-8 items-center justify-center rounded-lg border border-navy-600 text-ink-muted transition-colors hover:border-brand-500 hover:text-ink"
          >
            <el-icon :size="14"><Bell /></el-icon>
            <span
              v-if="unread"
              class="absolute -right-1 -top-1 flex h-4 min-w-4 items-center justify-center rounded-full bg-danger px-1 text-[10px] font-semibold text-white"
            >
              {{ unread }}
            </span>
          </button>
        </template>

        <div>
          <div class="flex items-center justify-between border-b border-navy-600 px-1 pb-2">
            <span class="text-xs font-semibold text-ink">通知（Mock）</span>
            <router-link to="/dashboard" class="text-[11px] text-brand-300 hover:underline">
              查看全部
            </router-link>
          </div>
          <ul class="divide-y divide-navy-600">
            <li v-for="n in notifications" :key="n.id" class="flex gap-2.5 py-2.5">
              <span
                class="mt-1.5 inline-block h-1.5 w-1.5 shrink-0 rounded-full"
                :style="{ backgroundColor: ALERT_LEVEL[n.level]?.color }"
              />
              <div class="min-w-0">
                <p class="text-xs leading-5 text-ink">{{ n.title }}</p>
                <p class="text-[10px] text-ink-dim">
                  {{ n.node }} · {{ formatRelative(n.time) }}
                </p>
              </div>
            </li>
          </ul>
          <p class="border-t border-navy-600 px-1 pt-2 text-[10px] text-ink-dim">
            告警中心接口（阶段 2）落地后替换为真实数据
          </p>
        </div>
      </el-popover>
    </div>
  </header>
</template>