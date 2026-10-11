<script setup>
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { storeToRefs } from 'pinia'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import { searchAgentSessions } from '@/api/agent'
import navGroups from '@/config/nav'

/**
 * 左侧固定侧边栏（宽 245px，可整体折叠为 68px 图标栏）。
 *
 * 折叠行为：
 *   - 顶部分组标题可单独展开/收起；
 *   - 顶部按钮 "Collapse All" 一次性收成图标栏并关闭所有分组；
 *   - 视口 <=1024px 时由 store 强制折叠（硬性要求）；
 *   - 折叠态下点击分组图标 = 展开侧边栏并打开该分组。
 *
 * 底部状态徽标：阶段 1 这里写死过 "6 Agent · 12 Job"，而 Go 侧**没有** Agent 实体模型，
 * 那些数字是无从核对的。阶段 2 改成显示真实存在的 Agent 会话条数（契约 §1 的 total），
 * 取不到就只显示 System Normal —— 宁可不显示，也不编数字。
 */
const app = useAppStore()
const auth = useAuthStore()
const route = useRoute()
const { collapsed } = storeToRefs(app)

const statusText = computed(() => 'System Normal')
const sessionTotal = ref(null)

onMounted(async () => {
  try {
    const res = await searchAgentSessions({ page: 1, page_size: 1 })
    sessionTotal.value = Number(res?.total || 0)
  } catch {
    sessionTotal.value = null // 取不到就不显示数字
  }
})

function onGroupIconClick(key) {
  app.userCollapsed = false
  app.groupOpen[key] = true
}

function isActiveGroup(group) {
  return group.items.some((it) => it.to === route.path)
}
</script>

<template>
  <aside
    class="fixed inset-y-0 left-0 z-40 flex flex-col border-r border-navy-600 bg-navy-850 transition-[width] duration-200"
    :style="{ width: collapsed ? 'var(--sidebar-w-collapsed)' : 'var(--sidebar-w)' }"
  >
    <!-- 品牌区 -->
    <div class="flex h-16 shrink-0 items-center gap-3 border-b border-navy-600 px-3.5">
      <div
        class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-gradient-to-br from-brand-500 to-violet2 text-white shadow-glow"
      >
        <el-icon :size="18"><Cpu /></el-icon>
      </div>
      <div v-if="!collapsed" class="min-w-0 flex-1">
        <p class="truncate text-sm font-semibold leading-5 text-ink">ITOps Agent</p>
        <p class="truncate text-[11px] leading-4 text-ink-dim">Multi-Agent Automation Platform</p>
      </div>
    </div>

    <!-- 折叠控制 -->
    <div class="flex shrink-0 items-center gap-1 border-b border-navy-600 px-2.5 py-2">
      <el-tooltip :content="collapsed ? '展开侧边栏' : '折叠侧边栏'" placement="right">
        <button
          class="flex h-7 w-7 items-center justify-center rounded-md text-ink-dim transition-colors hover:bg-navy-700 hover:text-ink"
          @click="app.toggleSidebar()"
        >
          <el-icon :size="15"><component :is="collapsed ? 'Expand' : 'Fold'" /></el-icon>
        </button>
      </el-tooltip>
      <template v-if="!collapsed">
        <el-tooltip content="收起全部菜单分组" placement="bottom">
          <button
            class="flex items-center gap-1.5 rounded-md px-2 py-1 text-[11px] text-ink-dim transition-colors hover:bg-navy-700 hover:text-ink"
            @click="app.collapseAll()"
          >
            <el-icon :size="12"><Sort /></el-icon>
            Collapse All
          </button>
        </el-tooltip>
        <el-tooltip content="展开全部菜单分组" placement="bottom">
          <button
            class="ml-auto flex h-7 w-7 items-center justify-center rounded-md text-ink-dim transition-colors hover:bg-navy-700 hover:text-ink"
            @click="app.expandAll()"
          >
            <el-icon :size="13"><Sort /></el-icon>
          </button>
        </el-tooltip>
      </template>
    </div>

    <!-- 菜单 -->
    <nav class="scroll-area flex-1 px-2.5 py-3">
      <!-- 折叠态：只显示分组图标 -->
      <template v-if="collapsed">
        <div class="flex flex-col items-center gap-1.5">
          <el-tooltip
            v-for="group in navGroups"
            :key="group.key"
            :content="group.label"
            placement="right"
          >
            <button
              class="flex h-10 w-10 items-center justify-center rounded-lg transition-colors"
              :class="
                isActiveGroup(group)
                  ? 'bg-brand-500/15 text-brand-300'
                  : 'text-ink-dim hover:bg-navy-700 hover:text-ink'
              "
              @click="onGroupIconClick(group.key)"
            >
              <el-icon :size="17"><component :is="group.icon" /></el-icon>
            </button>
          </el-tooltip>
        </div>
      </template>

      <!-- 展开态：分组 + 菜单项 -->
      <template v-else>
        <div v-for="group in navGroups" :key="group.key" class="mb-1">
          <button
            class="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left transition-colors hover:bg-navy-800"
            @click="app.toggleGroup(group.key)"
          >
            <el-icon :size="12" class="text-ink-dim"><component :is="group.icon" /></el-icon>
            <span class="section-label flex-1">{{ group.label }}</span>
            <el-icon
              :size="12"
              class="text-ink-dim transition-transform"
              :class="app.groupOpen[group.key] ? 'rotate-180' : ''"
            >
              <ArrowDown />
            </el-icon>
          </button>

          <div v-show="app.groupOpen[group.key]" class="mt-0.5 space-y-0.5">
            <router-link
              v-for="item in group.items"
              :key="item.to"
              :to="item.to"
              class="nav-item"
              :title="item.label"
            >
              <el-icon :size="15" class="shrink-0"><component :is="item.icon" /></el-icon>
              <span class="truncate">{{ item.label }}</span>
            </router-link>
          </div>
        </div>
      </template>
    </nav>

    <!-- 底部用户信息条 + 系统状态徽标 -->
    <div class="shrink-0 border-t border-navy-600 p-2.5">
      <div
        class="flex items-center gap-2.5 rounded-lg bg-navy-800 px-2.5 py-2"
        :class="collapsed ? 'justify-center' : ''"
      >
        <div
          class="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-brand-600 to-violet2 text-[11px] font-semibold text-white"
        >
          {{ (auth.displayName || 'A').slice(0, 1).toUpperCase() }}
        </div>
        <div v-if="!collapsed" class="min-w-0 flex-1">
          <p class="truncate text-xs font-medium leading-4 text-ink">{{ auth.displayName }}</p>
          <p class="truncate text-[10px] leading-4 text-ink-dim">
            {{ auth.isAdmin ? 'Administrator' : 'Operator' }} · JWT 12h
          </p>
        </div>
      </div>

      <div
        v-if="!collapsed"
        class="mt-2 flex items-center gap-1.5 rounded-lg border border-ok/25 bg-ok/10 px-2.5 py-1.5"
      >
        <span class="relative flex h-2 w-2 shrink-0">
          <span class="absolute inline-flex h-full w-full rounded-full bg-ok opacity-70 animate-pulse-ring" />
          <span class="relative inline-flex h-2 w-2 rounded-full bg-ok" />
        </span>
        <span class="truncate text-[10px] leading-4 text-ok/90">
          {{ statusText }}<template v-if="sessionTotal !== null"> · {{ sessionTotal }} Agent 会话</template>
        </span>
      </div>
      <div v-else class="mt-2 flex justify-center">
        <el-tooltip
          :content="sessionTotal !== null ? `${statusText} · ${sessionTotal} Agent 会话` : statusText"
          placement="right"
        >
          <span class="inline-flex h-2 w-2 rounded-full bg-ok" />
        </el-tooltip>
      </div>
    </div>
  </aside>
</template>