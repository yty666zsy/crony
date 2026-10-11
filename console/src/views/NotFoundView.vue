<script setup>
import { useRouter } from 'vue-router'
import navGroups from '@/config/nav'

// 404：把可用入口直接列出来，而不是给一个只能后退的死胡同。
const router = useRouter()
const links = navGroups.flatMap((g) => g.items).slice(0, 9)
</script>

<template>
  <div class="flex min-h-[70vh] items-center justify-center px-4">
    <div class="w-full max-w-2xl text-center">
      <div
        class="mx-auto flex h-20 w-20 items-center justify-center rounded-2xl bg-brand-500/15 text-brand-300"
      >
        <el-icon :size="36"><Compass /></el-icon>
      </div>

      <h1 class="mt-5 text-2xl font-semibold text-ink">页面不存在</h1>
      <p class="mt-2 text-xs leading-6 text-ink-dim">
        当前地址 <code class="text-brand-300">{{ $route.fullPath }}</code> 没有对应的控制台页面。<br />
        路由采用 hash 模式（<code class="text-brand-300">#/dashboard</code>），
        如果你是从收藏夹进入，请检查是否漏掉了 <code class="text-brand-300">#</code>。
      </p>

      <div class="mt-6 flex flex-wrap justify-center gap-2">
        <el-button type="primary" @click="router.push('/dashboard')">
          <el-icon class="mr-1"><HomeFilled /></el-icon>回到总览
        </el-button>
        <el-button plain @click="router.back()">返回上一页</el-button>
      </div>

      <div class="card mt-8 card-pad text-left">
        <p class="card-title">可用的页面</p>
        <div class="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-3">
          <router-link
            v-for="l in links"
            :key="l.to"
            :to="l.to"
            class="flex items-center gap-2 rounded-lg bg-navy-900 px-3 py-2 text-[11px] text-ink-muted transition-colors hover:bg-navy-700 hover:text-ink"
          >
            <el-icon :size="13"><component :is="l.icon" /></el-icon>
            <span class="truncate">{{ l.label }}</span>
          </router-link>
        </div>
      </div>
    </div>
  </div>
</template>