<script setup>
import { computed } from 'vue'
import {
  RUN_STATUS,
  NODE_STATUS,
  AGENT_STATUS,
  JOB_STATUS,
  AUDIT_RESULT,
  AGENT_SESSION_STATUS,
} from '@/utils/dict'

/**
 * 统一状态徽标。
 * kind 决定用哪个枚举字典（对应 Go 里不同模型的 status 字段）。
 */
const props = defineProps({
  kind: { type: String, default: 'run' }, // run | node | agent | job | audit | session
  value: { type: [Number, String], required: true },
  /** 显示为圆点 + 文字（用于列表），或纯标签 */
  dot: { type: Boolean, default: true },
  size: { type: String, default: 'default' }, // default | small
})

const DICTS = {
  run: RUN_STATUS,
  node: NODE_STATUS,
  agent: AGENT_STATUS, // 阶段 1 遗留（无接口对应），仅为旧组件兜底
  job: JOB_STATUS,
  audit: AUDIT_RESULT,
  // 阶段 2：Agent 会话状态（契约 §1）
  session: AGENT_SESSION_STATUS,
}

const meta = computed(() => {
  const dict = DICTS[props.kind] || RUN_STATUS
  return dict[props.value] || { text: String(props.value ?? '—'), color: '#7c8aa8', tag: 'info' }
})
</script>

<template>
  <span
    class="inline-flex items-center gap-1.5 rounded-md px-2 py-0.5 font-medium"
    :class="size === 'small' ? 'text-[11px]' : 'text-xs'"
    :style="{ backgroundColor: meta.color + '1f', color: meta.color }"
  >
    <span
      v-if="dot"
      class="inline-block h-1.5 w-1.5 shrink-0 rounded-full"
      :style="{ backgroundColor: meta.color }"
    />
    {{ meta.text }}
  </span>
</template>