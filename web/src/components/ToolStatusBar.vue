<script setup lang="ts">
// 工具状态条。
//
// 首字可能要等 25 秒，中间这段空白必须有事发生，否则用户以为按钮没生效。
// 只做三态展示，不做定时器：状态全部由 tool_call 事件驱动，
// 前端猜"现在应该在检索"迟早会和后端对不上。

import { computed } from 'vue'

import type { ToolStatus } from '@/utils/sse'

const props = defineProps<{
  status: ToolStatus | null
  detail: string
  /** 首个 delta 到达后淡出：正文开始出现时状态条就该让位。 */
  faded: boolean
}>()

const label = computed(() => {
  switch (props.status) {
    case 'running':
      return '正在检索法条…'
    case 'ok':
      return props.detail !== '' ? props.detail : '检索完成'
    case 'failed':
      return '法条检索失败，回答可能缺少依据'
    default:
      return '正在思考…'
  }
})
</script>

<template>
  <div class="tool-bar" :class="{ faded, failed: status === 'failed' }">
    <span class="tool-dot" :class="status ?? 'running'" />
    <span class="tool-label">{{ label }}</span>
  </div>
</template>

<style scoped>
.tool-bar {
  display: inline-flex;
  gap: 8px;
  align-items: center;
  padding: 4px 10px;
  font-size: 12px;
  color: var(--ll-text-weak);
  background: var(--ll-bg);
  border-radius: 999px;
  transition: opacity 0.3s ease;
}

.tool-bar.faded {
  opacity: 0;
}

.tool-bar.failed {
  color: #b26a00;
  background: #fff7e6;
}

.tool-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--ll-primary);
}

.tool-dot.running {
  animation: pulse 1s ease-in-out infinite;
}

.tool-dot.ok {
  background: #1f9c4d;
}

.tool-dot.failed {
  background: #d98026;
}

@keyframes pulse {
  0%,
  100% {
    opacity: 0.3;
  }

  50% {
    opacity: 1;
  }
}
</style>
