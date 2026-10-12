<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '@/stores/auth'
import { API_BASE } from '@/api/client'

/**
 * 真实模式（VITE_USE_MOCK=false）下的登录页。
 *
 * 为什么需要它：控制台早期的"无登录页"设计只在 Mock 模式下成立。
 * 一旦切到真实后端，没有令牌时所有需要登录的接口都会回业务码 1000（未登录），
 * 前端会为每个请求各弹一次提示 —— 实测单个页面可弹 4 条。这里补上最小登录闭环：
 *   POST /login → 存令牌 → 回查用户 → 进入控制台。
 *
 * Mock 模式下不会出现本页（AppLayout 用 USE_MOCK 短路）。
 */
const auth = useAuthStore()
const router = useRouter()

const form = ref({ username: 'admin', password: '' })
const loading = ref(false)

async function submit() {
  if (!form.value.username || !form.value.password) {
    ElMessage.warning('请输入用户名与口令')
    return
  }
  loading.value = true
  try {
    await auth.login({ username: form.value.username, password: form.value.password })
    ElMessage.success('登录成功')
    router.replace('/dashboard')
  } catch {
    /* 业务错误已由 api/client.js 的解包层统一提示 */
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="flex min-h-screen items-center justify-center bg-navy-900 px-4">
    <div class="w-full max-w-sm">
      <div class="mb-6 flex items-center gap-3">
        <div
          class="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl bg-gradient-to-br from-brand-500 to-violet2 text-white shadow-glow"
        >
          <el-icon :size="22"><Cpu /></el-icon>
        </div>
        <div class="min-w-0">
          <p class="truncate text-lg font-semibold leading-6 text-ink">crony</p>
          <p class="truncate text-[11px] leading-4 text-ink-dim">分布式定时任务 · AI Agent 运维</p>
        </div>
      </div>

      <div class="card card-pad">
        <h1 class="text-sm font-semibold text-ink">登录控制台</h1>
        <p class="mt-1 text-xs leading-5 text-ink-dim">
          当前为真实后端模式（<code class="font-mono">VITE_USE_MOCK=false</code>），
          请使用 crony-admin 的账号登录。
        </p>

        <el-form class="mt-4" @submit.prevent="submit">
          <el-input
            v-model="form.username"
            size="large"
            placeholder="用户名"
            autocomplete="username"
          >
            <template #prefix><el-icon><User /></el-icon></template>
          </el-input>

          <el-input
            v-model="form.password"
            class="mt-3"
            size="large"
            type="password"
            show-password
            placeholder="口令"
            autocomplete="current-password"
            @keyup.enter="submit"
          >
            <template #prefix><el-icon><Lock /></el-icon></template>
          </el-input>

          <el-button
            type="primary"
            size="large"
            class="mt-4 w-full"
            :loading="loading"
            @click="submit"
          >
            登录
          </el-button>
        </el-form>

        <p class="mt-3 text-[10px] leading-4 text-ink-dim">
          后端：<code class="font-mono">{{ API_BASE }}</code>
        </p>
      </div>
    </div>
  </div>
</template>
