<script setup lang="ts">
// 审查历史列表。原来是整页（ReviewsView），现在抽成组件：
// 上传工作台的左栏、以及将来任何要"挑一份历史"的地方都复用它。
//
// 搜索是纯客户端过滤——在已加载的二十条里找，不是全库搜索。
// 后端 /reviews 没有关键字参数，为这个场景加一个参数不值得。
//
// 轮询只在有 pending/running 时开：否则二十九条已完成的历史也会每 2 秒打一次后端。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import { ApiError } from '@/api/client'
import { listReviews } from '@/api/files'
import type { Review } from '@/api/types'
import { formatRelative } from '@/utils/format-relative'
import {
  filterReviews,
  hasMore,
  hasPending,
  isClickable,
  riskLabel,
  statusLabel,
} from '@/utils/review-list'

const props = defineProps<{
  /** 当前正在看的那一份，用于左栏高亮。 */
  activeFileId?: string
}>()

const emit = defineEmits<{ open: [fileId: string] }>()

const LIMIT = 20
const POLL_MS = 2000
/** 90 次 = 3 分钟，比后端 WORKER_TASK_TIMEOUT（300s）略短；超时就停，别无限打后端。 */
const POLL_MAX = 90

const rows = ref<Review[]>([])
const loading = ref(true)
const loadingMore = ref(false)
const errorText = ref('')
const more = ref(false)
const keyword = ref('')
/**
 * 轮询到上限时如果还有没跑完的，标记一下。
 *
 * 后端 WORKER_TASK_TIMEOUT 是 300 秒，比这里的 3 分钟上限还长：真跑到 4 分钟的
 * 审查会遇到"我先停了"，那一行就永远停在「审查中」且不再自愈。停可以，
 * 但要让人知道是我停了、可以手动刷，而不是以为界面卡死。
 */
const pollExhausted = ref(false)

const shown = computed(() => filterReviews(rows.value, keyword.value))

let pollTimer: ReturnType<typeof setInterval> | null = null
let pollCount = 0
/** 上一次轮询还没回来就不再发：后端慢的时候不要让请求叠起来。 */
let refreshing = false

function stopPolling(): void {
  if (pollTimer !== null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function startPollingIfNeeded(): void {
  if (pollTimer !== null || !hasPending(rows.value)) return
  pollCount = 0
  // 重新开一轮就清掉上一次的"已暂停"：否则用户点了刷新、新的 running 又出现时，
  // 提示条还挂在那儿不走。
  pollExhausted.value = false
  pollTimer = setInterval(() => {
    pollCount += 1
    if (pollCount > POLL_MAX || !hasPending(rows.value)) {
      stopPolling()
      // 到上限时如果还有没跑完的，说清楚"我停了"，别让界面假装还在自动刷新。
      pollExhausted.value = hasPending(rows.value)
      return
    }
    void refresh()
  }, POLL_MS)
}

/**
 * 轮询用：重拉已经加载的那一段，就地换掉。
 *
 * 不能复用 load(false)——那是"加载更多"，offset 取的是 rows.length，
 * 拉回来的是下一页，第一页里那条 running 的状态永远不会变，轮询等于空转。
 * 也不能用 load(true)：它会亮起整屏「加载中…」，每 2 秒闪一次。
 *
 * 多拉一页没有意义：新记录只会出现在最前面，顺序不会变。
 */
async function refresh(): Promise<void> {
  if (refreshing) return
  refreshing = true
  const want = Math.max(rows.value.length, LIMIT)
  try {
    const page = await listReviews(want, 0)
    rows.value = page
    more.value = hasMore(page.length, want)
  } catch (err) {
    // 轮询失败保持现状：临时断网不该把已经显示的列表换成错误页。
    // 但也不能完全无声——否则"列表不动了"这件事没有任何线索可查。
    console.warn('[history] 轮询失败', err)
  } finally {
    refreshing = false
    if (hasPending(rows.value)) {
      // 还有 running，继续；没有了就停。
      startPollingIfNeeded()
    } else {
      stopPolling()
    }
  }
}

async function load(reset: boolean): Promise<void> {
  const offset = reset ? 0 : rows.value.length
  if (reset) {
    loading.value = true
    errorText.value = ''
    // 手动刷新就是"我重新看一次"：先把"已暂停"的提示撤掉。
    // 只在 startPollingIfNeeded 里清不够——那时如果已经全部到终态，
    // 早退分支会直接 return，提示条会一直挂着。
    pollExhausted.value = false
  } else {
    loadingMore.value = true
  }
  try {
    const page = await listReviews(LIMIT, offset)
    rows.value = reset ? page : [...rows.value, ...page]
    more.value = hasMore(page.length, LIMIT)
  } catch (err) {
    errorText.value = err instanceof ApiError ? err.message : '加载失败，请重试'
  } finally {
    loading.value = false
    loadingMore.value = false
    startPollingIfNeeded()
  }
}

function open(row: Review): void {
  if (!isClickable(row.status)) return
  emit('open', row.file_id)
}

onMounted(() => {
  void load(true)
})

onBeforeUnmount(stopPolling)
</script>

<template>
  <div class="history">
    <div class="history-head">
      <h2 class="history-title">审查历史</h2>
      <el-input
        v-model="keyword"
        size="small"
        clearable
        placeholder="搜索文件名"
        class="history-search"
      />
    </div>

    <p v-if="loading" class="state">加载中…</p>

    <div v-else-if="errorText" class="state">
      <p class="error">{{ errorText }}</p>
      <el-button size="small" @click="load(true)">重试</el-button>
    </div>

    <div v-else-if="rows.length === 0" class="state empty">
      <p>还没有审查记录</p>
      <p class="hint">在右侧上传一份劳动合同，结果会出现在这里。</p>
    </div>

    <div v-else-if="shown.length === 0" class="state empty">
      <p>没有匹配「{{ keyword }}」的记录</p>
    </div>

    <template v-else>
      <ul class="rows">
        <li
          v-for="row in shown"
          :key="row.id"
          class="row"
          :class="{ active: row.file_id === props.activeFileId, clickable: isClickable(row.status) }"
          :title="row.status === 'failed' ? '上一次审查没有完成，点击可重新审查' : ''"
          @click="open(row)"
        >
          <div class="row-name">{{ row.filename || '未命名合同' }}</div>
          <div class="row-head">
            <span class="row-status" :class="`status-${row.status}`">{{ statusLabel(row.status) }}</span>
            <span class="row-risk" :class="`risk-${row.risk_level || 'none'}`">{{ riskLabel(row.risk_level) }}</span>
            <span class="row-time">{{ formatRelative(row.created_at) }}</span>
          </div>
          <div class="row-metrics">
            <span>高 <em class="high">{{ row.counts?.high ?? 0 }}</em></span>
            <span>中 <em class="medium">{{ row.counts?.medium ?? 0 }}</em></span>
            <span>低 <em class="low">{{ row.counts?.low ?? 0 }}</em></span>
          </div>
        </li>
      </ul>

      <p v-if="pollExhausted" class="stalled">
        仍有审查在进行，自动刷新已暂停。
        <el-button link type="primary" size="small" @click="load(true)">刷新</el-button>
      </p>

      <div v-if="more && keyword === ''" class="more">
        <el-button size="small" :loading="loadingMore" :disabled="loadingMore" @click="load(false)">
          加载更多
        </el-button>
      </div>
    </template>
  </div>
</template>

<style scoped>
.history {
  display: flex;
  flex-direction: column;
  /* 必须：flex 子项默认 min-height:auto，列表一长就把左栏顶高、整页多一条滚动条 */
  min-height: 0;
  height: 100%;
}

.history-head {
  flex: none;
  padding: 16px 16px 12px;
  border-bottom: 1px solid #e5e7eb;
}

.history-title {
  margin: 0 0 10px;
  font-size: 15px;
  font-weight: 600;
  color: #1f2937;
}

.rows {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  margin: 0;
  padding: 0;
  list-style: none;
}

.row {
  padding: 12px 16px;
  border-bottom: 1px solid #f1f3f5;
}

.row.clickable {
  cursor: pointer;
}

.row.clickable:hover {
  background: #f8fafc;
}

.row.active {
  background: #eef4ff;
  box-shadow: inset 3px 0 0 #3b82f6;
}

.row-name {
  font-weight: 600;
  font-size: 13px;
  color: #1f2937;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.row-head {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 6px;
  font-size: 12px;
}

/* 340px 宽度下三行必须放得下：状态、风险、时间挤在一行里也靠 margin-left:auto 顶开 */
.row-status {
  flex: none;
  padding: 1px 8px;
  font-weight: 500;
  color: #1f2937;
  background: #f3f4f6;
  border-radius: 999px;
}

/* 配色与 ReviewsView 原页一致，不另定一套 */
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
  flex: none;
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
  white-space: nowrap;
}

.row-metrics {
  display: flex;
  gap: 8px;
  margin-top: 4px;
  font-size: 11px;
  color: #9ca3af;
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

.state {
  padding: 24px 16px;
  text-align: center;
  color: #6b7280;
  font-size: 13px;
}

.state .hint {
  margin-top: 6px;
  font-size: 12px;
  color: #9ca3af;
}

.error {
  color: #dc2626;
}

.more {
  flex: none;
  padding: 12px;
  text-align: center;
  border-top: 1px solid #f1f3f5;
}

.stalled {
  flex: none;
  margin: 0;
  padding: 8px 16px;
  font-size: 12px;
  color: #b45309;
  background: #fffbeb;
  border-top: 1px solid #fde68a;
}
</style>
