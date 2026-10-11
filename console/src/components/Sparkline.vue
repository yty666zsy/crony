<script setup>
import { computed } from 'vue'

/**
 * 迷你趋势图（纯内联 SVG，无第三方图表库）。
 * 之所以不引入 echarts：KPI 卡只需要一条折线，体积与收益不成比例（阶段 2 大屏再引入图表库）。
 */
const props = defineProps({
  values: { type: Array, default: () => [] },
  color: { type: String, default: '#6366f1' },
  width: { type: Number, default: 120 },
  height: { type: Number, default: 36 },
  filled: { type: Boolean, default: true },
  strokeWidth: { type: Number, default: 2 },
})

const points = computed(() => {
  const list = (props.values || []).map((v) => Number(v)).filter((v) => Number.isFinite(v))
  if (list.length === 0) return []
  if (list.length === 1) return [{ x: 0, y: props.height / 2 }, { x: props.width, y: props.height / 2 }]

  const max = Math.max(...list)
  const min = Math.min(...list)
  const span = max - min || 1
  const stepX = props.width / (list.length - 1)
  const pad = props.strokeWidth
  const usable = props.height - pad * 2

  return list.map((v, i) => ({
    x: Number((i * stepX).toFixed(2)),
    // SVG y 轴向下，所以用 (max - v) 翻转
    y: Number((pad + (1 - (v - min) / span) * usable).toFixed(2)),
  }))
})

const linePath = computed(() => points.value.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x},${p.y}`).join(' '))

const areaPath = computed(() => {
  if (points.value.length === 0) return ''
  const first = points.value[0]
  const last = points.value[points.value.length - 1]
  return `${linePath.value} L${last.x},${props.height} L${first.x},${props.height} Z`
})

const gradientId = computed(() => `spark-${Math.random().toString(36).slice(2, 9)}`)
</script>

<template>
  <svg
    :width="width"
    :height="height"
    :viewBox="`0 0 ${width} ${height}`"
    preserveAspectRatio="none"
    class="overflow-visible"
    role="img"
    aria-label="趋势迷你图"
  >
    <defs>
      <linearGradient :id="gradientId" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" :stop-color="color" stop-opacity="0.38" />
        <stop offset="100%" :stop-color="color" stop-opacity="0" />
      </linearGradient>
    </defs>
    <path v-if="filled && areaPath" :d="areaPath" :fill="`url(#${gradientId})`" />
    <path
      v-if="linePath"
      :d="linePath"
      fill="none"
      :stroke="color"
      :stroke-width="strokeWidth"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  </svg>
</template>