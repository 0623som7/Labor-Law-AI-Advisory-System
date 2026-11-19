<script setup lang="ts">
// 两页共用的页头：左边标题/副标题，右边插槽放面包屑等导航。
// 吸顶与分隔线沿用两页原来的做法（box-shadow），避免滚动时高度跳 1px。
defineProps<{
  title: string
  subtitle?: string
}>()
</script>

<template>
  <header class="page-header">
    <div class="head-main">
      <span class="head-title">{{ title }}</span>
      <span v-if="subtitle" class="head-sub" :title="subtitle">{{ subtitle }}</span>
    </div>
    <slot name="right" />
  </header>
</template>

<style scoped>
.page-header {
  position: sticky;
  top: 0;
  z-index: 10;
  flex: none;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  height: 56px;
  padding: 0 24px;
  background: var(--ll-surface);
  box-shadow: 0 1px 0 var(--ll-border);
}

.head-main {
  display: flex;
  align-items: baseline;
  gap: 12px;
  min-width: 0;
}

.head-title {
  /* 页头标题 */
  font-size: var(--ll-fs-title);
  font-weight: 600;
  color: var(--ll-text-strong);
  white-space: nowrap;
}

.head-sub {
  /* 副标题：合同文件名与页数，说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-weak);
  max-width: 40vw;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
