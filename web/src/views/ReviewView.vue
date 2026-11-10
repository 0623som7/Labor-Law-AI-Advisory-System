<script setup lang="ts">
// 合同审查页：左边是 PDF 原页面 + 风险高亮框，右边是风险清单。
//
// 三件事的顺序不能换：先确认文件存在（40401 直接回上传页），再取解析结果
// （42201 说明没文字层，这时连「开始审查」都不该给），最后才渲染 PDF。
// 反过来的话，用户会先看到一片空白画布，再被弹回上传页。

import { computed, nextTick, onBeforeUnmount, ref, shallowRef } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import * as pdfjsLib from 'pdfjs-dist'
// worker 必须打包进产物：走 CDN 的话内网演示直接白屏，而且版本容易对不上。
import workerUrl from 'pdfjs-dist/build/pdf.worker.min.mjs?url'

import { ApiError } from '@/api/client'
import {
  findReviewByFile,
  getFile,
  getFileBytes,
  getPages,
  getReview,
  getTask,
  listFindings,
  startReview as requestReview,
  type ReviewFile,
} from '@/api/files'
import type { Finding, ParsedDoc, Review } from '@/api/types'
import { SEVERITY_LABEL, findingsOnPage, legalBasisTitle, sortBySeverity, summarizeFindings } from '@/utils/findings'
import { shouldOfferStart } from '@/utils/review-restore'
import {
  findingPage,
  inflateRect,
  pdfBBoxToViewport,
  rectToCss,
  type ViewportLike,
} from '@/utils/pdf/coords'

pdfjsLib.GlobalWorkerOptions.workerSrc = workerUrl

/** 渲染缩放。A4 在 1.25 倍下是 744×1052px，四页 canvas 合计不到 8MB。 */
const SCALE = 1.25
const POLL_INTERVAL = 2000
// 320s > WORKER_TASK_TIMEOUT(300s)：前端不能比后端先放弃，
// 否则慢任务表现为"失败"，刷新后又出现。
// 不从后端读这个值——前端拿不到，硬编码加注释比多一次请求好维护。
const POLL_MAX = 160
const ACTIVE_MS = 1500

const route = useRoute()
const router = useRouter()
const fileId = computed(() => String(route.params.fileId ?? ''))

const file = ref<ReviewFile | null>(null)
const doc = ref<ParsedDoc | null>(null)
const pageCount = ref(0)
const loading = ref(true)
const loadError = ref('')
const pdfError = ref('')
/** 42201：文件在，但没有文字层。 */
const scanned = ref(false)

const viewports = shallowRef<ViewportLike[]>([])
const canvases: Array<HTMLCanvasElement | null> = []

const review = ref<Review | null>(null)
const findings = ref<Finding[]>([])
const running = ref(false)
const elapsed = ref(0)
const stage = ref('')
const reviewError = ref('')
/** 左侧与右侧共享的高亮条目，只此一个来源。 */
const activeId = ref<string | null>(null)

let pollTimer: number | null = null
let tickTimer: number | null = null
let activeTimer: number | null = null
let disposed = false
let reviewId: string | null = null

const summary = computed(() => summarizeFindings(findings.value))
const sorted = computed(() => sortBySeverity(findings.value))
// 有结果就不再给「开始审查」：后端会复用旧结果，用户点了只是白等两秒。
const showStart = computed(() =>
  shouldOfferStart({
    restored: review.value,
    running: running.value,
    scanned: scanned.value,
    loadError: loadError.value,
  }),
)

function message(err: unknown, fallback: string): string {
  return err instanceof ApiError ? err.message : fallback
}

function setCanvas(index: number, el: unknown): void {
  canvases[index] = el instanceof HTMLCanvasElement ? el : null
}

function marksOn(pageIndex: number): Finding[] {
  return findingsOnPage(findings.value, pageIndex)
}

function markStyle(f: Finding): Record<string, string> {
  const viewport = viewports.value[findingPage(f)]
  if (!viewport) return {}
  return rectToCss(inflateRect(pdfBBoxToViewport(f.clause_ref.bbox, viewport, SCALE), viewport))
}

function pageStyle(pageIndex: number): Record<string, string> {
  const viewport = viewports.value[pageIndex]
  if (!viewport) return {}
  return { width: `${viewport.width}px`, height: `${viewport.height}px` }
}

/** 高亮只维持一会儿：一直亮着会盖住原文，而用户点它的目的就是读原文。 */
function focusFinding(f: Finding): void {
  activeId.value = f.id
  if (activeTimer !== null) window.clearTimeout(activeTimer)
  activeTimer = window.setTimeout(() => {
    activeId.value = null
    activeTimer = null
  }, ACTIVE_MS)
  void nextTick(() => {
    document
      .querySelector(`[data-page="${findingPage(f)}"]`)
      ?.scrollIntoView({ behavior: 'smooth', block: 'center' })
    document
      .querySelector(`[data-finding="${f.id}"]`)
      ?.scrollIntoView({ behavior: 'smooth', block: 'nearest' })
  })
}

async function renderPdf(): Promise<void> {
  pdfError.value = ''
  try {
    // 必须自己带 token 取字节：PDF.js 拿到 URL 后用它自己的 XHR，不带 Authorization。
    const data = await getFileBytes(fileId.value)
    const pdf = await pdfjsLib.getDocument({ data }).promise
    // 先让页数和画布进 DOM 再取 canvas：模板里的 .page 是 v-for 出来的，
    // 这之前（loading 还盖着）canvases 全是 undefined，渲染会被静默跳过。
    if (pdf.numPages !== pageCount.value) {
      pageCount.value = pdf.numPages
      await nextTick()
    }
    const vps: ViewportLike[] = []
    for (let i = 1; i <= pdf.numPages; i += 1) {
      // getPage 是 1 起，pages[].index 是 0 起，这里两边都按自己的口径用。
      const page = await pdf.getPage(i)
      const viewport = page.getViewport({ scale: SCALE })
      vps.push(viewport)
      const canvas = canvases[i - 1]
      if (!canvas) continue
      canvas.width = Math.floor(viewport.width)
      canvas.height = Math.floor(viewport.height)
      canvas.style.width = `${viewport.width}px`
      canvas.style.height = `${viewport.height}px`
      const ctx = canvas.getContext('2d')
      if (!ctx) continue
      await page.render({ canvasContext: ctx, viewport }).promise
    }
    if (!disposed) viewports.value = vps
  } catch (err) {
    // 走到这里通常是 worker 或字节流没加载起来（如 .mjs 被发成 octet-stream 时
    // module worker 构造失败），而不是画布画不出来，所以文案说"加载"。
    pdfError.value = message(err, 'PDF 加载失败，请刷新页面重试')
  }
}

function startTicking(): void {
  stopTicking()
  elapsed.value = 0
  tickTimer = window.setInterval(() => {
    elapsed.value += 1
  }, 1000)
}

function stopTicking(): void {
  if (tickTimer !== null) {
    window.clearInterval(tickTimer)
    tickTimer = null
  }
}

/**
 * 把一份审查结果画到界面上。恢复已有结果和跑完新审查都走这里，
 * 免得两处各写一份"拉 findings + 排序"，迟早只改一处。
 */
async function showResults(id: string): Promise<void> {
  review.value = await getReview(id)
  findings.value = sortBySeverity(await listFindings(id))
}

async function pollTask(taskId: string): Promise<void> {
  for (let i = 0; i < POLL_MAX; i += 1) {
    if (disposed) return
    const task = await getTask(taskId)
    if (disposed) return
    stage.value = task.stage || ''
    if (task.status === 'succeeded') {
      if (reviewId) await showResults(reviewId)
      running.value = false
      stopTicking()
      return
    }
    if (task.status === 'failed' || task.status === 'cancelled') {
      reviewError.value = task.error || '审查没有完成，请重试'
      running.value = false
      stopTicking()
      return
    }
    await wait(POLL_INTERVAL)
  }
  if (disposed) return
  reviewError.value = '审查用时过长，已停止等待。稍后刷新页面可继续查看结果。'
  running.value = false
  stopTicking()
}

/** 2 秒一跳，定时器句柄留着一个，卸载时要能清掉。 */
function wait(ms: number): Promise<void> {
  return new Promise((resolve) => {
    pollTimer = window.setTimeout(() => {
      pollTimer = null
      resolve()
    }, ms)
  })
}

async function startReview(): Promise<void> {
  if (running.value) return
  running.value = true
  reviewError.value = ''
  findings.value = []
  stage.value = ''
  startTicking()
  try {
    const started = await requestReview(fileId.value)
    reviewId = started.reviewId
    if (started.kind === 'accepted') {
      await pollTask(started.taskId)
    } else {
      // 后端把上次的结果直接给了回来（同一份文件重复审查），不再排任务。
      await showResults(reviewId)
      running.value = false
      stopTicking()
    }
  } catch (err) {
    const code = err instanceof ApiError ? err.code : 0
    if (code === 40901) {
      reviewError.value = '这份合同正在审查中，请稍候'
    } else if (code === 40401) {
      ElMessage.error('合同不存在或无权访问')
      await router.replace({ name: 'upload' })
    } else {
      reviewError.value = message(err, '提交审查失败，请重试')
    }
    running.value = false
    stopTicking()
  }
}

/**
 * 进页面时把这份合同已有的审查结果拉回来。
 *
 * 之前 reviewId 只存在 localStorage 里，换设备、清缓存、或者审查是别人用 API
 * 触发的，进页面就等于没审过。后端一直有"按文件查最近一次审查"的能力
 * （ReviewStore.GetByFile），这轮把它接到 HTTP 上（GET /reviews?file_id=…）。
 */
async function restoreExisting(): Promise<void> {
  try {
    const existing = await findReviewByFile(fileId.value)
    if (!existing) return
    reviewId = existing.id
    if (existing.status === 'succeeded') {
      await showResults(existing.id)
      return
    }
    // 还在跑：接着等它的任务，别让用户看到"没结果"。
    if ((existing.status === 'pending' || existing.status === 'running') && existing.task_id) {
      running.value = true
      stage.value = '继续等待上次的审查结果'
      startTicking()
      await pollTask(existing.task_id)
      return
    }
    review.value = existing
    if (existing.status === 'failed' || existing.status === 'cancelled') {
      reviewError.value = '上一次审查没有完成，可以重新开始'
      // 失败的那次不算"有结果"，按钮要还回去。
      review.value = null
    }
  } catch (err) {
    reviewError.value = message(err, '读取已审查结果失败')
  }
}

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  scanned.value = false
  try {
    file.value = await getFile(fileId.value)
  } catch (err) {
    if (err instanceof ApiError && err.code === 40401) {
      ElMessage.error('合同不存在或无权访问')
      await router.replace({ name: 'upload' })
      return
    }
    loadError.value = message(err, '合同信息加载失败')
    loading.value = false
    return
  }

  try {
    doc.value = await getPages(fileId.value)
  } catch (err) {
    if (err instanceof ApiError && err.code === 42201) {
      scanned.value = true
    } else if (err instanceof ApiError && err.code === 40401) {
      ElMessage.error('合同不存在或无权访问')
      await router.replace({ name: 'upload' })
      return
    } else {
      loadError.value = message(err, '解析结果加载失败，请重试')
    }
  }

  if (!loadError.value) {
    // 页数用后端给的先占位，这样 loading 撤掉时 .page 与 canvas 已经在 DOM 里，
    // renderPdf 才能拿到真实元素；PDF 实际页数不同时它自己会再纠正一次。
    pageCount.value = doc.value?.page_count ?? 0
    loading.value = false
    await nextTick()
    await renderPdf()
    await restoreExisting()
  } else {
    loading.value = false
  }
}

function retry(): void {
  void load()
}

onBeforeUnmount(() => {
  disposed = true
  if (pollTimer !== null) window.clearTimeout(pollTimer)
  if (activeTimer !== null) window.clearTimeout(activeTimer)
  stopTicking()
})

void load()
</script>

<template>
  <div class="review-page">
    <header class="topbar">
      <div class="brand">劳动法 AI 顾问</div>
      <div class="file-meta" v-if="file">
        <span class="file-name">{{ file.filename }}</span>
        <span class="file-pages">{{ file.page_count }} 页</span>
      </div>
      <nav class="links">
        <router-link :to="{ name: 'chat' }">对话咨询</router-link>
        <router-link :to="{ name: 'upload' }">合同审查</router-link>
      </nav>
    </header>

    <div v-if="loading" class="loading">正在加载合同…</div>

    <div v-else-if="loadError" class="blocked">
      <p class="blocked-title">{{ loadError }}</p>
      <el-button type="primary" @click="retry">重试</el-button>
    </div>

    <div v-else-if="scanned" class="blocked">
      <p class="blocked-title">这份文件没有文字层，可能是扫描件，暂不支持审查</p>
      <p class="blocked-hint">扫描件需要先做 OCR 才能定位条款，当前版本还没有这一步。</p>
      <el-button @click="router.push({ name: 'upload' })">换一份文件</el-button>
    </div>

    <main v-else class="body">
      <section class="viewer">
        <div class="viewer-head">
          <span class="viewer-title">合同原文</span>
          <span v-if="findings.length" class="viewer-hint">点方框或右侧条目互相定位</span>
        </div>
        <div class="pages">
          <div
            v-for="pageIndex in pageCount"
            :key="pageIndex"
            class="page"
            :data-page="pageIndex - 1"
            :style="pageStyle(pageIndex - 1)"
          >
            <canvas :ref="(el) => setCanvas(pageIndex - 1, el)" class="page-canvas" />
            <button
              v-for="f in marksOn(pageIndex - 1)"
              :key="f.id"
              type="button"
              class="mark"
              :class="[
                `mark--${f.severity}`,
                { 'mark--active': activeId === f.id, 'mark--dim': activeId !== null && activeId !== f.id },
              ]"
              :style="markStyle(f)"
              :title="f.title"
              @click="focusFinding(f)"
            />
          </div>
          <p v-if="pdfError" class="pdf-error">{{ pdfError }}</p>
        </div>
      </section>

      <aside class="panel">
        <div class="panel-head">
          <div class="summary">
            <span class="sum sum--high">高风险 {{ summary.high }}</span>
            <span class="sum sum--medium">中风险 {{ summary.medium }}</span>
            <span class="sum sum--low">低风险 {{ summary.low }}</span>
          </div>
          <el-button v-if="showStart" type="primary" :loading="running" @click="startReview">
            开始审查
          </el-button>
        </div>

        <div v-if="running" class="running">
          <el-skeleton :rows="6" animated />
          <p class="running-line">
            已用时 {{ elapsed }} 秒<span v-if="stage"> · {{ stage }}</span>
          </p>
        </div>

        <p v-else-if="reviewError" class="review-error">{{ reviewError }}</p>

        <p v-else-if="!sorted.length" class="empty">
          还没有审查结果。点「开始审查」，8 条规则会在十几秒内跑完。
        </p>

        <ul v-else class="cards">
          <li
            v-for="f in sorted"
            :key="f.id"
            class="card"
            :class="{ 'card--active': activeId === f.id }"
            :data-finding="f.id"
            @click="focusFinding(f)"
          >
            <div class="card-head">
              <span class="tag" :class="`tag--${f.severity}`">{{ SEVERITY_LABEL[f.severity] }}</span>
              <span class="card-title">{{ f.title }}</span>
              <span v-if="f.clause_ref.section" class="card-section">{{ f.clause_ref.section }}</span>
            </div>

            <p class="card-evidence">{{ f.evidence }}</p>

            <details v-if="f.legal_basis.length" class="fold">
              <summary>{{ legalBasisTitle(f) }}</summary>
              <div v-for="(b, i) in f.legal_basis" :key="i" class="basis">
                <p class="basis-head">{{ b.law }} {{ b.article }}</p>
                <p class="basis-text">{{ b.text }}</p>
              </div>
            </details>

            <details class="fold">
              <summary>修改建议</summary>
              <p v-if="f.suggestion.original" class="sug-original">{{ f.suggestion.original }}</p>
              <p v-if="f.suggestion.revised" class="sug-revised">{{ f.suggestion.revised }}</p>
              <p v-if="f.suggestion.reason" class="sug-reason">{{ f.suggestion.reason }}</p>
            </details>
          </li>
        </ul>
      </aside>
    </main>
  </div>
</template>

<style scoped>
.review-page {
  /* 页面在 height:100vh 的 .app-shell 里，上方还有一条 24px 的提示条：
     min-height:100vh 会让它比容器高 24px，永远多出一条不该有的纵向滚动条。 */
  min-height: 0;
  display: flex;
  flex-direction: column;
  background: #f5f7fa;
}

.topbar {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 0 24px;
  height: 56px;
  background: #fff;
  border-bottom: 1px solid #e5e7eb;
}

.brand {
  font-weight: 600;
  color: #1f2937;
}

.file-meta {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-width: 0;
}

.file-name {
  font-size: 14px;
  color: #111827;
  max-width: 40vw;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.file-pages {
  font-size: 12px;
  color: #9ca3af;
}

.links {
  margin-left: auto;
  display: flex;
  gap: 16px;
  font-size: 14px;
}

.links a {
  color: #2563eb;
  text-decoration: none;
}

.loading,
.blocked {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 12px;
  padding: 64px 24px;
}

.blocked-title {
  margin: 0;
  font-size: 16px;
  color: #374151;
}

.blocked-hint {
  margin: 0;
  font-size: 13px;
  color: #9ca3af;
}

.body {
  flex: 1;
  display: flex;
  min-height: 0;
}

.viewer {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.viewer-head {
  display: flex;
  align-items: baseline;
  gap: 12px;
  padding: 12px 20px;
  font-size: 13px;
  color: #6b7280;
}

.viewer-title {
  color: #374151;
  font-weight: 500;
}

.pages {
  flex: 1;
  overflow: auto;
  padding: 0 20px 32px;
}

.page {
  position: relative;
  margin: 0 auto 20px;
  background: #fff;
  box-shadow: 0 1px 6px rgba(15, 23, 42, 0.12);
}

.page-canvas {
  display: block;
}

.mark {
  position: absolute;
  padding: 0;
  border-radius: 2px;
  border: 2px solid;
  background: transparent;
  cursor: pointer;
  transition: opacity 0.15s ease;
}

.mark--high {
  border-color: #d93026;
  background: rgba(217, 48, 38, 0.14);
}

.mark--medium {
  border-color: #d97706;
  background: rgba(217, 119, 6, 0.14);
}

.mark--low {
  border-color: #ca8a04;
  background: rgba(202, 138, 4, 0.14);
}

.mark--dim {
  opacity: 0.35;
}

.mark--active {
  animation: pulse 1.2s ease-out 2;
  z-index: 2;
}

@keyframes pulse {
  0% {
    box-shadow: 0 0 0 0 rgba(217, 48, 38, 0.45);
  }
  100% {
    box-shadow: 0 0 0 10px rgba(217, 48, 38, 0);
  }
}

.pdf-error {
  text-align: center;
  font-size: 13px;
  color: #d93026;
}

.panel {
  width: 420px;
  flex: none;
  display: flex;
  flex-direction: column;
  background: #fff;
  border-left: 1px solid #e5e7eb;
}

.panel-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 14px 16px;
  border-bottom: 1px solid #f1f2f4;
}

.summary {
  display: flex;
  gap: 10px;
  font-size: 13px;
}

.sum--high {
  color: #d93026;
}

.sum--medium {
  color: #d97706;
}

.sum--low {
  color: #9ca3af;
}

.running {
  padding: 16px;
}

.running-line {
  margin: 12px 0 0;
  font-size: 13px;
  color: #6b7280;
}

.review-error {
  margin: 16px;
  font-size: 13px;
  color: #d93026;
}

.empty {
  margin: 16px;
  font-size: 13px;
  line-height: 1.7;
  color: #9ca3af;
}

.cards {
  flex: 1;
  overflow: auto;
  margin: 0;
  padding: 12px 16px 24px;
  list-style: none;
}

.card {
  padding: 12px;
  margin-bottom: 10px;
  border: 1px solid #e5e7eb;
  border-radius: 8px;
  cursor: pointer;
  transition: border-color 0.15s ease;
}

.card--active {
  border-color: #2563eb;
  box-shadow: 0 0 0 2px rgba(37, 99, 235, 0.12);
}

.card-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
}

.tag {
  flex: none;
  font-size: 12px;
  padding: 0 5px;
  border-radius: 3px;
  color: #fff;
}

.tag--high {
  background: #d93026;
}

.tag--medium {
  background: #d97706;
}

.tag--low {
  background: #ca8a04;
}

.card-title {
  font-size: 14px;
  font-weight: 500;
  color: #111827;
}

.card-section {
  margin-left: auto;
  font-size: 12px;
  color: #9ca3af;
}

.card-evidence {
  margin: 8px 0 0;
  font-size: 13px;
  line-height: 1.7;
  color: #374151;
  white-space: pre-wrap;
  word-break: break-word;
}

.fold {
  margin-top: 8px;
  font-size: 13px;
}

.fold summary {
  cursor: pointer;
  color: #2563eb;
  list-style: none;
}

.fold summary::-webkit-details-marker {
  display: none;
}

.basis {
  margin-top: 8px;
  padding: 8px 10px;
  background: #f9fafb;
  border-radius: 6px;
}

.basis-head {
  margin: 0;
  font-weight: 500;
  color: #111827;
}

.basis-text {
  margin: 4px 0 0;
  line-height: 1.7;
  color: #4b5563;
  white-space: pre-wrap;
}

.sug-original {
  margin: 8px 0 0;
  padding: 6px 8px;
  background: #f3f4f6;
  color: #6b7280;
  text-decoration: line-through;
  border-radius: 6px;
  white-space: pre-wrap;
}

.sug-revised {
  margin: 6px 0 0;
  padding: 6px 8px;
  background: #ecfdf5;
  color: #065f46;
  border-radius: 6px;
  white-space: pre-wrap;
}

.sug-reason {
  margin: 6px 0 0;
  color: #6b7280;
  line-height: 1.7;
}
</style>
