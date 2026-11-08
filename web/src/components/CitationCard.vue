<script setup lang="ts">
// 法条卡片。内容全部来自 citation 事件，一个字都不在前端拼：
// 法条原文只能来自 statutes 表，前端自己补"第 X 条"就是把法律文本交给了模型。
//
// 默认收起条文、点开才显示全文：
//   - 等待回答的那 30 秒里三张卡片同时在屏幕上，全展开会把输入框顶出视口；
//   - 评委要问"这条原文怎么写的"，点一下就展开，比读一屏小字快。
// 收起状态下 .citation-text 仍然在 DOM 里（只是 display:none），
// 这样 e2e 与"文本与法条库逐字一致"的检查不受影响。

import { computed, ref } from 'vue'

import type { Citation } from '@/utils/sse'

const props = defineProps<{ citation: Citation }>()

const expanded = ref(false)

const title = computed(() => {
  // 库里 law 是"中华人民共和国劳动合同法"，条号是"第三十一条"。
  const law = props.citation.law.replace(/^中华人民共和国/, '')
  return `${law} ${props.citation.article}`
})

/** 条文第一句，收起时给个预览，让人知道点开能看到什么。 */
const preview = computed(() => {
  const text = props.citation.text.replace(/\s+/g, ' ').trim()
  return text.length > 34 ? `${text.slice(0, 34)}…` : text
})

function toggle(): void {
  expanded.value = !expanded.value
}

function onKey(evt: KeyboardEvent): void {
  if (evt.key === 'Enter' || evt.key === ' ') {
    evt.preventDefault()
    toggle()
  }
}
</script>

<template>
  <article class="citation-card" :class="{ expanded }">
    <button
      type="button"
      class="citation-head"
      :aria-expanded="expanded"
      :title="expanded ? '收起条文' : '展开条文原文'"
      @click="toggle"
      @keydown="onKey"
    >
      <span class="citation-badge">法条</span>
      <span class="citation-title">{{ title }}</span>
      <span v-if="!expanded" class="citation-preview">{{ preview }}</span>
      <span class="citation-toggle">{{ expanded ? '收起' : '展开' }}</span>
    </button>

    <div class="citation-body" :class="{ open: expanded }" :hidden="!expanded">
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
    </div>
  </article>
</template>

<style scoped>
.citation-card {
  padding: 8px 12px;
  background: var(--ll-surface);
  border: 1px solid var(--ll-border);
  border-left: 3px solid var(--ll-primary);
  border-radius: 8px;
}

.citation-head {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: baseline;
  width: 100%;
  padding: 0;
  font: inherit;
  color: inherit;
  text-align: left;
  cursor: pointer;
  background: transparent;
  border: 0;
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
  flex: none;
  font-size: 13px;
  font-weight: 600;
}

.citation-preview {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  font-size: 12px;
  color: var(--ll-text-weak);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.citation-toggle {
  flex: none;
  font-size: 12px;
  color: var(--ll-primary);
}

.citation-body {
  margin-top: 6px;
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
