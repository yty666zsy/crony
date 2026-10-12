<script setup>
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import { storeToRefs } from 'pinia'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import AppSidebar from './AppSidebar.vue'
import AppTopbar from './AppTopbar.vue'
import AiSessionWidget from './AgentSessionWidget.vue'
import LoginView from '@/views/LoginView.vue'
import { USE_MOCK } from '@/api/client'

/**
 * 控制台主布局。
 *
 * 两种形态：
 *   - 默认：左侧固定侧边栏 + 顶栏 + 内容区 + 右下角 Agent 会话速查入口；
 *   - meta.bare = true（/big-screen）：全屏无外壳，页面自己管布局。
 *
 * 响应式：内容区左边距跟随侧边栏宽度（245px / 68px），由 store 的折叠态驱动。
 */
const app = useAppStore()
const auth = useAuthStore()
const route = useRoute()
const { collapsed } = storeToRefs(app)

const bare = computed(() => Boolean(route.meta?.bare))
const useMock = String(import.meta.env.VITE_USE_MOCK ?? 'true')
const apiBase = import.meta.env.VITE_API_BASE || 'http://127.0.0.1:8089'

/** 真实模式且没有令牌 → 先登录，避免每个接口各弹一次"未登录"（Mock 模式永不进入登录页）。 */
const needLogin = computed(() => !USE_MOCK && !auth.token)

const mainStyle = computed(() => ({
  marginLeft: collapsed.value ? 'var(--sidebar-w-collapsed)' : 'var(--sidebar-w)',
}))

function onResize() {
  app.syncViewport()
}

/** 令牌失效（业务码 1000）：清会话 → needLogin 变真 → 回到登录页。 */
function onUnauthorized() {
  auth.logout()
}

onMounted(() => {
  app.syncViewport()
  app.initTheme()
  window.addEventListener('resize', onResize, { passive: true })
  window.addEventListener('crony:unauthorized', onUnauthorized)
  // 有令牌（或 Mock 模式）才回查用户；无令牌时交给登录页，不发无谓请求。
  if (USE_MOCK || auth.token) auth.loadProfile()
})

onBeforeUnmount(() => {
  window.removeEventListener('resize', onResize)
  window.removeEventListener('crony:unauthorized', onUnauthorized)
})
</script>

<template>
  <LoginView v-if="needLogin" />
  <div v-else class="min-h-screen bg-navy-900">
    <template v-if="!bare">
      <AppSidebar />

      <div class="flex min-h-screen flex-col transition-[margin] duration-200" :style="mainStyle">
        <AppTopbar />

        <main class="flex-1 px-5 py-5">
          <router-view v-slot="{ Component }">
            <transition
              enter-active-class="transition duration-200 ease-out"
              enter-from-class="translate-y-1 opacity-0"
              enter-to-class="translate-y-0 opacity-100"
              mode="out-in"
            >
              <component :is="Component" />
            </transition>
          </router-view>
        </main>

        <footer class="shrink-0 px-5 pb-6 pt-1">
          <p class="text-[11px] text-ink-dim">
            crony 控制台 · 分布式定时任务与 AI Agent 运维 ·
            <span :class="useMock === 'true' ? 'text-warn' : 'text-ok'">
              VITE_USE_MOCK={{ useMock }}
            </span>
            <span class="mx-1.5 text-navy-500">|</span>
            API {{ apiBase }}
          </p>
        </footer>
      </div>

      <AiSessionWidget />
    </template>

    <router-view v-else />
  </div>
</template>