<script setup>
import { USE_MOCK } from '@/api/client'

/**
 * Mock 数据提示（统一口径）。
 *
 * 为什么要做成组件：阶段 1 每个页面各写一遍 <span class="chip border-warn/40 text-warn">，
 * 文案与位置都不统一，很容易漏标。契约要求"UI 必须让人看出这是 Mock 数据"，
 * 收敛到一处最不容易漏；VITE_USE_MOCK=false 时它会显示"后端真实数据"，
 * 避免把"真的来自后端"和"演示数据"混为一谈。
 */
const props = defineProps({
  text: { type: String, default: 'Mock 数据（VITE_USE_MOCK=true）' },
})

const REAL_TEXT = '后端真实数据（VITE_USE_MOCK=false）'
</script>

<template>
  <span v-if="USE_MOCK" class="chip border-warn/40 text-warn" :title="REAL_TEXT">
    {{ props.text }}
  </span>
  <span v-else class="chip border-ok/40 text-ok" title="VITE_USE_MOCK=false">{{ REAL_TEXT }}</span>
</template>