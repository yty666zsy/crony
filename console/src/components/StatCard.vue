<script setup>
import { computed } from 'vue'
import Sparkline from './Sparkline.vue'

/**
 * KPI 卡片：数值 + 标签 + 迷你趋势图 或 进度条。
 * Dashboard 第一行 4 张卡都用它；三栏区的 "Knowledge Base" 卡也复用了简化版。
 */
const props = defineProps({
  label: { type: String, required: true },
  value: { type: [String, Number], required: true },
  unit: { type: String, default: '' },
  hint: { type: String, default: '' },
  icon: { type: String, default: '' },
  color: { type: String, default: '#6366f1' },
  trend: { type: Array, default: null },
  /** 0~100：给定时显示进度条，替代趋势图 */
  progress: { type: Number, default: null },
  delta: { type: String, default: '' },
  deltaType: { type: String, default: 'flat' }, // up | down | flat
  /** 数据来源脚注（例：近 24 小时 · GET /job/run/overview），用于区分真实/规划接口 */
  sub: { type: String, default: '' },
  planned: { type: String, default: '' },
})

const progressColor = computed(() => props.color)
const deltaClass = computed(() => {
  if (props.deltaType === 'up') return 'text-ok'
  if (props.deltaType === 'down') return 'text-danger'
  return 'text-ink-dim'
})
</script>

<template>
  <div class="card card-pad flex flex-col gap-3">
    <div class="flex items-start justify-between gap-2">
      <div class="min-w-0">
        <div class="flex items-center gap-2">
          <span class="card-sub truncate">{{ label }}</span>
          <span v-if="planned" class="chip border-warn/40 text-warn">{{ planned }}</span>
        </div>
        <div class="mt-2 flex items-baseline gap-1.5">
          <span class="text-2xl font-semibold tracking-tight text-ink">{{ value }}</span>
          <span v-if="unit" class="text-xs text-ink-dim">{{ unit }}</span>
        </div>
      </div>
      <div
        v-if="icon"
        class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg"
        :style="{ backgroundColor: color + '22', color }"
      >
        <el-icon :size="17"><component :is="icon" /></el-icon>
      </div>
    </div>

    <!-- 进度条模式 -->
    <div v-if="progress !== null" class="space-y-1.5">
      <div class="h-1.5 w-full overflow-hidden rounded-full bg-navy-600">
        <div
          class="h-full rounded-full transition-all"
          :style="{ width: Math.min(100, Math.max(0, progress)) + '%', backgroundColor: progressColor }"
        />
      </div>
      <div class="flex items-center justify-between">
        <span class="text-[11px] text-ink-dim">{{ hint }}</span>
        <span class="text-[11px]" :class="deltaClass">{{ delta }}</span>
      </div>
    </div>

    <!-- 趋势图模式 -->
    <div v-else class="flex items-end justify-between gap-2">
      <span class="text-[11px] leading-4 text-ink-dim">{{ hint }}</span>
      <div v-if="trend && trend.length" class="shrink-0">
        <Sparkline :values="trend" :color="color" :width="110" :height="34" />
      </div>
      <span v-else-if="delta" class="shrink-0 text-[11px]" :class="deltaClass">{{ delta }}</span>
    </div>

    <div v-if="sub" class="text-[10px] leading-3 text-ink-dim opacity-70">{{ sub }}</div>
  </div>
</template>