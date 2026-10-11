<script setup>
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import navGroups from '@/config/nav'

/**
 * 规划页占位视图。
 *
 * 除 9 个必需页面外，侧边栏还保留了观测站基线里的基础设施菜单项
 * （数据库连接 / SSH 密钥 / 远程桌面 / 容器与虚拟化），
 * 它们在后端尚无任何数据模型与路由；本视图统一承载，说明"为什么是空的、
 * 谁负责、什么时候交付"，避免出现白屏或占位符空白页。
 */
const route = useRoute()

const meta = computed(() => route.meta || {})
const bullets = computed(() => meta.value.bullets || [])

const relatedPages = computed(() =>
  navGroups
    .flatMap((g) => g.items)
    .filter((it) => it.to !== route.path)
    .slice(0, 6),
)
</script>

<template>
  <div class="space-y-4">
    <section class="card card-pad">
      <div class="flex flex-wrap items-start gap-4">
        <div
          class="flex h-14 w-14 shrink-0 items-center justify-center rounded-2xl bg-brand-500/15 text-brand-300"
        >
          <el-icon :size="26"><component :is="meta.icon || 'Box'" /></el-icon>
        </div>
        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-2">
            <h2 class="text-base font-semibold text-ink">{{ meta.title }}</h2>
            <span class="chip border-warn/40 text-warn">{{ meta.planned || '规划中' }}</span>
          </div>
          <p class="mt-1 text-xs leading-6 text-ink-dim">{{ meta.desc }}</p>
        </div>
      </div>
    </section>

    <section class="grid gap-4 lg:grid-cols-3">
      <div class="card card-pad lg:col-span-2">
        <p class="card-title">计划交付内容</p>
        <ul class="mt-3 space-y-2.5">
          <li v-for="(b, i) in bullets" :key="i" class="flex gap-3">
            <span
              class="mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-md bg-navy-800 text-[10px] font-semibold text-brand-300"
            >
              {{ i + 1 }}
            </span>
            <span class="text-[12px] leading-6 text-ink-muted">{{ b }}</span>
          </li>
          <li v-if="!bullets.length" class="text-[12px] text-ink-dim">
            该页面的交付细节尚未确定，等后端数据模型冻结后再补充。
          </li>
        </ul>
      </div>

      <div class="space-y-4">
        <div class="card card-pad">
          <p class="card-title">后端状态</p>
          <div class="mt-3 space-y-2">
            <div class="flex items-center gap-2 rounded-lg bg-navy-900 px-3 py-2">
              <span class="inline-block h-2 w-2 rounded-full bg-danger" />
              <span class="text-[11px] text-ink-muted">数据模型：未定义</span>
            </div>
            <div class="flex items-center gap-2 rounded-lg bg-navy-900 px-3 py-2">
              <span class="inline-block h-2 w-2 rounded-full bg-danger" />
              <span class="text-[11px] text-ink-muted">HTTP 路由：未注册</span>
            </div>
            <div class="flex items-center gap-2 rounded-lg bg-navy-900 px-3 py-2">
              <span class="inline-block h-2 w-2 rounded-full bg-warn" />
              <span class="text-[11px] text-ink-muted">前端：信息架构已就位</span>
            </div>
          </div>
          <p class="mt-3 text-[11px] leading-5 text-ink-dim">
            本页不展示任何伪造数据。控制台在 Mock 模式下也只对该菜单项返回"未实现"的中性状态，
            以免把规划能力误当成已上线功能。
          </p>
        </div>

        <div class="card card-pad">
          <p class="card-title">先看已有能力</p>
          <div class="mt-2.5 flex flex-wrap gap-2">
            <router-link v-for="p in relatedPages" :key="p.to" :to="p.to">
              <span class="chip cursor-pointer hover:border-brand-500 hover:text-ink">
                <el-icon :size="11" class="mr-1"><component :is="p.icon" /></el-icon>{{ p.label }}
              </span>
            </router-link>
          </div>
        </div>
      </div>
    </section>

    <section class="card card-pad">
      <p class="card-title">为什么先建菜单而不先建功能</p>
      <p class="mt-2 text-[11px] leading-6 text-ink-dim">
        侧边栏结构本身就是产品规划的一部分：先把"运维对象"的边界定下来（服务器、数据库连接、密钥、
        终端、远程桌面、容器），后续每个模块的实现都要落进这个信息架构里，避免后端按模块开发、
        前端再拼成两套心智模型。这些菜单项当前都是可点击的占位页，不会出现空白页或 404。
      </p>
    </section>
  </div>
</template>