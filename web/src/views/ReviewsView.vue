<script setup lang="ts">
// 审查历史：把"审过但关掉了页面"的记录捞回来。
//
// 之前 fileId 只活在 URL 里，换设备、清缓存、第二天再来就再也找不到那份结果。
// 后端 GET /reviews 一直能列，只是前端从来没人调。
//
// 分页按"这一页装满没有"推断下一页：后端只回数组，没有 total。

import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'

import { ApiError } from '@/api/client'
import { listReviews } from '@/api/files'
import type { Review } from '@/api/types'
import { formatRelative } from '@/utils/format-relative'
import { hasMore, isClickable, riskLabel, statusLabel } from '@/utils/review-list'

const LIMIT = 20

const router = useRouter()

const rows = ref<Review[]>([])
const loading = ref(true)
const loadingMore = ref(false)
const errorText = ref('')
/** 上一页是否装满，决定要不要显示「加载更多」。 */
const more = ref(false)

function message(err: unknown): string {
  return err instanceof ApiError ? err.message : '加载失败，请重试'
}

async function load(reset: boolean): Promise<void> {
  const offset = reset ? 0 : rows.value.length
  if (reset) {
    loading.value = true
    errorText.value = ''
  } else {
    loadingMore.value = true
  }
  try {
    const page = await listReviews(LIMIT, offset)
    rows.value = reset ? page : [...rows.value, ...page]
    more.value = hasMore(page.length, LIMIT)
  } catch (err) {
    errorText.value = message(err)
  } finally {
    loading.value = false
    loadingMore.value = false
  }
}

function retry(): void {
  void load(true)
}

function open(row: Review): void {
  // 失败/取消行也照常进页面：那一页会自动给出「开始审查」按钮，
  // 后端对失败记录会新建一份任务，所以这里就是重试入口。
  void router.push({ name: 'review', params: { fileId: row.file_id } })
}

onMounted(() => {
  void load(true)
})
</script>

<template>
  <div class="reviews-page">
    <header class="topbar">
      <div class="brand">劳动法 AI 顾问</div>
      <nav class="links">
        <router-link :to="{ name: 'chat' }">对话咨询</router-link>
        <router-link :to="{ name: 'upload' }">合同审查</router-link>
        <span class="current">审查历史</span>
      </nav>
    </header>

    <main class="body">
      <h1 class="title">审查历史</h1>
      <p class="subtitle">每一份审过的合同都在这里，点一行回到当时的风险标注。</p>

      <p v-if="loading" class="state">加载中…</p>

      <div v-else-if="errorText" class="state">
        <p class="error">{{ errorText }}</p>
        <el-button @click="retry">重试</el-button>
      </div>

      <div v-else-if="rows.length === 0" class="state empty">
        <p>还没有审查记录</p>
        <el-button type="primary" @click="router.push({ name: 'upload' })">去上传合同</el-button>
      </div>

      <template v-else>
        <ul class="rows">
          <li
            v-for="row in rows"
            :key="row.id"
            class="row"
            :class="{ clickable: isClickable(row.status) }"
            @click="open(row)"
          >
            <div class="row-head">
              <span
                class="row-status"
                :class="`status-${row.status}`"
                :title="
                  row.status === 'failed'
                    ? '上一次审查没有完成，点击可重新审查'
                    : row.status === 'cancelled'
                      ? '上一次审查已取消，点击可重新发起'
                      : ''
                "
                >{{ statusLabel(row.status) }}</span
              >
              <span class="row-risk" :class="`risk-${row.risk_level || 'none'}`">{{ riskLabel(row.risk_level) }}</span>
              <span class="row-time">{{ formatRelative(row.created_at) }}</span>
            </div>
            <div class="row-metrics">
              <span>得分 <em>{{ row.score }}</em></span>
              <span>高风险 <em class="high">{{ row.counts?.high ?? 0 }}</em></span>
              <span>中风险 <em class="medium">{{ row.counts?.medium ?? 0 }}</em></span>
              <span>低风险 <em class="low">{{ row.counts?.low ?? 0 }}</em></span>
            </div>
          </li>
        </ul>

        <div v-if="more" class="more">
          <el-button :loading="loadingMore" :disabled="loadingMore" @click="load(false)">
            加载更多
          </el-button>
        </div>
      </template>
    </main>
  </div>
</template>

<style scoped>
.reviews-page {
  /* 同 .review-page：外层 .app-shell 已是 100vh，这里再要 100vh 会多出一条纵向滚动条。 */
  min-height: 0;
  background: #f5f7fa;
}

.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 24px;
  height: 56px;
  background: #fff;
  border-bottom: 1px solid #e5e7eb;
}

.brand {
  font-weight: 600;
  color: #1f2937;
}

.links {
  display: flex;
  gap: 16px;
  font-size: 14px;
}

.links a {
  color: #2563eb;
  text-decoration: none;
}

.current {
  color: #6b7280;
}

.body {
  max-width: 720px;
  margin: 48px auto;
  padding: 0 24px 64px;
}

.title {
  margin: 0 0 8px;
  font-size: 22px;
  color: #1f2937;
}

.subtitle {
  margin: 0 0 24px;
  font-size: 14px;
  line-height: 1.7;
  color: #6b7280;
}

.state {
  padding: 32px 0;
  font-size: 14px;
  color: #6b7280;
  text-align: center;
}

.empty {
  display: flex;
  flex-direction: column;
  gap: 12px;
  align-items: center;
  padding: 48px 0;
  background: #fff;
  border-radius: 10px;
}

.error {
  margin: 0 0 12px;
  color: #b91c1c;
}

.rows {
  display: flex;
  flex-direction: column;
  gap: 12px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.row {
  padding: 14px 16px;
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 10px;
}

.row.clickable {
  cursor: pointer;
}

.row.clickable:hover {
  border-color: #93c5fd;
  box-shadow: 0 1px 6px rgba(37, 99, 235, 0.12);
}

.row-head {
  display: flex;
  gap: 10px;
  align-items: center;
  font-size: 13px;
}

.row-status {
  padding: 1px 8px;
  font-weight: 500;
  color: #1f2937;
  background: #f3f4f6;
  border-radius: 999px;
}

.status-succeeded {
  color: #047857;
  background: #ecfdf5;
}

.status-running {
  color: #1d4ed8;
  background: #eff6ff;
}

.status-failed {
  color: #b91c1c;
  background: #fef2f2;
}

.row-risk {
  padding: 1px 8px;
  border-radius: 999px;
}

.risk-high {
  color: #b91c1c;
  background: #fef2f2;
}

.risk-medium {
  color: #b45309;
  background: #fffbeb;
}

.risk-low {
  color: #047857;
  background: #ecfdf5;
}

.risk-none {
  color: #6b7280;
  background: #f3f4f6;
}

.row-time {
  margin-left: auto;
  color: #9ca3af;
}

.row-metrics {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  margin-top: 8px;
  font-size: 13px;
  color: #6b7280;
}

.row-metrics em {
  font-style: normal;
  font-weight: 600;
  color: #1f2937;
}

.row-metrics em.high {
  color: #b91c1c;
}

.row-metrics em.medium {
  color: #b45309;
}

.row-metrics em.low {
  color: #047857;
}

.more {
  margin-top: 16px;
  text-align: center;
}
</style>
