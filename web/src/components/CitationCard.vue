<script setup lang="ts">
// 法条卡片。内容全部来自 citation 事件，一个字都不在前端拼：
// 法条原文只能来自 statutes 表，前端自己补"第 X 条"就是把法律文本交给了模型。

import { computed } from 'vue'

import type { Citation } from '@/utils/sse'

const props = defineProps<{ citation: Citation }>()

const title = computed(() => {
  // 库里 law 是"中华人民共和国劳动合同法"，条号是"第三十一条"。
  const law = props.citation.law.replace(/^中华人民共和国/, '')
  return `${law} ${props.citation.article}`
})
</script>

<template>
  <article class="citation-card">
    <header class="citation-head">
      <span class="citation-badge">法条</span>
      <span class="citation-title">{{ title }}</span>
    </header>
    <p class="citation-text ll-pre-wrap">{{ citation.text }}</p>
    <a
      v-if="citation.url"
      class="citation-link"
      :href="citation.url"
      target="_blank"
      rel="noopener noreferrer"
    >
      查看原文
    </a>
  </article>
</template>

<style scoped>
.citation-card {
  padding: 10px 12px;
  background: var(--ll-surface);
  border: 1px solid var(--ll-border);
  border-left: 3px solid var(--ll-primary);
  border-radius: 8px;
}

.citation-head {
  display: flex;
  gap: 8px;
  align-items: center;
  margin-bottom: 6px;
}

.citation-badge {
  flex: none;
  padding: 1px 6px;
  font-size: 11px;
  color: var(--ll-primary);
  background: var(--ll-primary-weak);
  border-radius: 4px;
}

.citation-title {
  font-size: 13px;
  font-weight: 600;
}

.citation-text {
  margin: 0;
  font-size: 13px;
  color: #3b4048;
}

.citation-link {
  display: inline-block;
  margin-top: 6px;
  font-size: 12px;
  color: var(--ll-primary);
  text-decoration: none;
}

.citation-link:hover {
  text-decoration: underline;
}
</style>
