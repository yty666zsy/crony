<script setup>
/**
 * 统一空状态。
 * Dashboard 的 "Servers" 卡片用 onboarding 变体（观测站 Baseline 里该卡片就是空状态），
 * 其余列表用 default 变体。
 */
defineProps({
  icon: { type: String, default: 'Box' },
  title: { type: String, required: true },
  desc: { type: String, default: '' },
  actionText: { type: String, default: '' },
  actionTo: { type: String, default: '' },
  compact: { type: Boolean, default: false },
})

const emit = defineEmits(['action'])
</script>

<template>
  <div
    class="flex flex-col items-center justify-center text-center"
    :class="compact ? 'gap-2 py-6' : 'gap-3 py-10'"
  >
    <div
      class="flex items-center justify-center rounded-xl border border-dashed border-navy-500 bg-navy-800/60 text-ink-dim"
      :class="compact ? 'h-10 w-10' : 'h-14 w-14'"
    >
      <el-icon :size="compact ? 18 : 24"><component :is="icon" /></el-icon>
    </div>
    <div class="space-y-1">
      <p class="text-sm font-medium text-ink-muted">{{ title }}</p>
      <p v-if="desc" class="mx-auto max-w-sm text-xs leading-5 text-ink-dim">{{ desc }}</p>
    </div>
    <div v-if="actionText" class="pt-1">
      <router-link v-if="actionTo" :to="actionTo">
        <el-button type="primary" size="small" plain>{{ actionText }}</el-button>
      </router-link>
      <el-button v-else type="primary" size="small" plain @click="emit('action')">
        {{ actionText }}
      </el-button>
    </div>
  </div>
</template>