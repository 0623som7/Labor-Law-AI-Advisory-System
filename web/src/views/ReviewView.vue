<script setup lang="ts">
// 合同审查页：左边是 PDF 原页面 + 风险高亮框，右边是风险清单。
//
// 三件事的顺序不能换：先确认文件存在（40401 直接回上传页），再取解析结果
// （42201 说明没文字层，这时连「开始审查」都不该给），最后才渲染 PDF。
// 反过来的话，用户会先看到一片空白画布，再被弹回上传页。

import { computed, nextTick, onBeforeUnmount, ref, shallowRef, watch } from 'vue'
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
import { countBySeverity, filterFindings, type SeverityFilter } from '@/utils/findings-filter'
import InlineChat from '@/components/InlineChat.vue'
import PageHeader from '@/components/PageHeader.vue'
import { buildAskContext } from '@/utils/ask-this'
import { reviewProgressLine } from '@/utils/review-progress'
import { shouldAutostart, shouldOfferStart } from '@/utils/review-restore'
import { breadcrumbFor } from '@/utils/breadcrumb'
import {
  clampSideWidth,
  KEY_STEP,
  MAX_SIDE,
  MIN_SIDE,
  nextWidthByKey,
  readFlag,
  readSideWidth,
  writeFlag,
  writeSideWidth,
} from '@/utils/splitter'
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

/**
 * 轮询节奏：前几次密、之后退到 2 秒。
 *
 * 审查本身实测几十毫秒到一两秒，固定 2 秒跳一次等于"结果早好了，观众还在看骨架屏"——
 * 自动开审之后这段空等直接决定"上传完多久看到风险卡片"。任务长起来之后再退到 2 秒，
 * 免得空转几百次。
 */
const POLL_BACKOFF = [250, 400, 600, 900, 1200, 1600]

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
/** 本轮开始的本机时间。阶段文案按"已等毫秒"算，所以记起点而不是累加秒数。 */
const startedAt = ref(0)
const elapsedMs = ref(0)
const reviewError = ref('')
/** 失败的具体原因：有就展示在提示下面一行小字，没有就不占位。 */
const reviewReason = ref('')
/** 左侧与右侧共享的高亮条目，只此一个来源。 */
const activeId = ref<string | null>(null)

let pollTimer: number | null = null
let tickTimer: number | null = null
let disposed = false
let reviewId: string | null = null

// 审查进行中的那一行文案：按已等毫秒换档，结束/失败立刻不可见。
const progressLine = computed(() => reviewProgressLine({ elapsedMs: elapsedMs.value, running: running.value }))

const summary = computed(() => summarizeFindings(findings.value))

const crumbs = breadcrumbFor('review')

// 页头副标题：合同文件名 + 页数；文件名过长时由 PageHeader 自己省略
const headerSubtitle = computed(() => {
  if (!file.value) return ''
  return `${file.value.filename} · ${file.value.page_count} 页`
})

// ---- 右栏折叠 ------------------------------------------------------------
// 投屏时想给合同原文更多横向空间，但完全藏掉清单会看不见"有几条高风险"，
// 所以折叠成 40px 的图标条：数字还在，正文让开。折叠不改 panelWidth，
// 展开后回到用户上次拖的宽度。
const PANEL_COLLAPSED_KEY = 'laborlaw.review.panelCollapsed'
const panelCollapsed = ref(readFlag(PANEL_COLLAPSED_KEY, false))

function togglePanel(): void {
  panelCollapsed.value = !panelCollapsed.value
  writeFlag(PANEL_COLLAPSED_KEY, panelCollapsed.value)
}

// ---- 两栏分隔条 ----------------------------------------------------------
// 面板宽度存 localStorage：答辩前会反复调这个宽度（投屏投影比例跟本地不一样），
// 每次刷新都回到 420 很烦。默认值 420 是改动前的硬编码宽度，必须保持一致。
const PANEL_KEY = 'laborlaw.review.panelWidth'
const PANEL_DEFAULT = 460
const panelWidth = ref(readSideWidth(PANEL_KEY, PANEL_DEFAULT, MIN_SIDE, MAX_SIDE))
const bodyEl = ref<HTMLElement | null>(null)
const maxPanelWidth = computed(() =>
  bodyEl.value ? Math.min(MAX_SIDE, Math.round(bodyEl.value.clientWidth * 0.6)) : MAX_SIDE,
)

const dragging = ref(false)
let stopDrag: (() => void) | null = null

function onSplitterDown(e: PointerEvent): void {
  if (!bodyEl.value) return
  e.preventDefault()
  const target = e.currentTarget as HTMLElement
  target.setPointerCapture?.(e.pointerId)
  const rect = bodyEl.value.getBoundingClientRect()
  // 拖拽期间禁掉选中：否则鼠标划过条文会选中文字，看起来像页面卡住了。
  // 类挂在根节点而不是 document.body：样式是 scoped 的，挂 body 上没人给它写规则。
  dragging.value = true

  const onMove = (ev: PointerEvent): void => {
    panelWidth.value = clampSideWidth(rect.right - ev.clientX, rect.width, {
      min: MIN_SIDE,
      max: MAX_SIDE,
      minOpposite: 320,
    })
  }
  const onUp = (): void => {
    dragging.value = false
    writeSideWidth(PANEL_KEY, panelWidth.value)
    stopDrag?.()
    stopDrag = null
  }
  target.addEventListener('pointermove', onMove)
  target.addEventListener('pointerup', onUp)
  target.addEventListener('pointercancel', onUp)
  stopDrag = () => {
    target.removeEventListener('pointermove', onMove)
    target.removeEventListener('pointerup', onUp)
    target.removeEventListener('pointercancel', onUp)
  }
}

function onSplitterKey(e: KeyboardEvent): void {
  const next = nextWidthByKey(panelWidth.value, e.key, {
    step: KEY_STEP,
    min: MIN_SIDE,
    max: maxPanelWidth.value,
  })
  if (next === null) return
  panelWidth.value = next
  writeSideWidth(PANEL_KEY, next)
  e.preventDefault()
}

function resetPanelWidth(): void {
  panelWidth.value = PANEL_DEFAULT
  writeSideWidth(PANEL_KEY, PANEL_DEFAULT)
}
const sorted = computed(() => sortBySeverity(findings.value))

// ---- 筛选与搜索 ----------------------------------------------------------
const severityFilter = ref<SeverityFilter>('all')
const keyword = ref('')

/**
 * chip 上的数字取 sorted（全部结果），不取 visible。
 *
 * 跟着筛选变的话，点「高风险」之后另外三个 chip 会全变成 0——那是筛选结果，
 * 不是"这份合同有几条"，用户会以为数据丢了。
 */
const counts = computed(() => countBySeverity(sorted.value))

const visible = computed(() =>
  filterFindings(sorted.value, { severity: severityFilter.value, query: keyword.value }),
)

/** 有没有筛选在生效。图上被筛掉的框靠它决定要不要淡下去。 */
const filtering = computed(() => severityFilter.value !== 'all' || keyword.value.trim() !== '')

const visibleIds = computed(() => new Set(visible.value.map((f) => f.id)))

const chips = computed<{ value: SeverityFilter; label: string; n: number }[]>(() => [
  { value: 'all', label: '全部', n: counts.value.all },
  { value: 'high', label: '高风险', n: counts.value.high },
  { value: 'medium', label: '中风险', n: counts.value.medium },
  { value: 'low', label: '低风险', n: counts.value.low },
])

function clearFilters(): void {
  severityFilter.value = 'all'
  keyword.value = ''
}

/**
 * 筛选一变，原来高亮的那条可能已经不在列表里。
 *
 * 不置空的话左边原文还亮着一个框、右边找不到对应卡片，看起来像页面坏了；
 * 置 null 会让高亮整体消失，与"现在没有选中项"这个事实一致。
 */
watch([severityFilter, keyword], () => {
  if (activeId.value !== null && !visibleIds.value.has(activeId.value)) activeId.value = null
})

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

/**
 * 选中一条就常驻高亮，点下一条自然切换。
 *
 * 早先是"亮 1.5 秒后自动灭"，结果用户还没看清就闪没了；现在默认浅、
 * 选中深且不自动灭，取消选中靠点另一条或离开页面。
 */
/**
 * 「就问这条」：把这条结论原样带进对话页的输入框。
 *
 * 不自动发送——预填只是省掉用户复述"哪一条、什么毛病"的力气，
 * 发不发、要不要补一句自己的情况，由他自己决定。
 */
/** 抽屉开关与「这次问什么」。seed 每次点都换新值，子组件据此判断要不要发。 */
const chatOpen = ref(false)
const chatSeed = ref('')
/** 第几次点「就问这条」。连点同一张卡时 seed 文本不变，靠序号触发重发。 */
const chatSeedSeq = ref(0)
/** 这次追问针对哪一条，交给抽屉拼进「在对话页继续」的 query（空串表示无条款号）。 */
const chatSection = ref('')

/** 最近一次点「就问这条」的条目 id，用于关闭抽屉后把焦点还回去。 */
const askedFromId = ref('')

/**
 * 关闭抽屉：面板是 v-show 常驻的，这里只切可见性并把焦点还给触发它的按钮。
 * 生成中的流不掐断——Esc 的意思是"我先看合同"，不是"别答了"。
 */
function closeChat(): void {
  chatOpen.value = false
  const id = askedFromId.value
  if (id === '') return
  void nextTick(() => {
    const btn = document.querySelector<HTMLElement>(`.cards [data-finding="${id}"] .ask-this`)
    btn?.focus()
  })
}

function askThis(f: Finding): void {
  // 点按钮是 @click.stop，不会走 focusFinding；这一条既然是被追问的对象，
  // 就把它设成当前条目——左边原文的高亮与右边卡的选中态才跟得上。
  activeId.value = f.id
  chatSeed.value = buildAskContext(f)
  chatSection.value = (f.clause_ref?.section ?? '').trim()
  chatSeedSeq.value += 1
  chatOpen.value = true
  // 记住是谁打开的：Esc 关掉抽屉之后焦点要回到这个按钮上，
  // 否则键盘用户被扔回文档开头，得重新 Tab 一遍。
  askedFromId.value = f.id
  // 抽屉占掉下方 40vh，卡片列表被挤矮——这张卡的「复制改法 / 就问这条」很可能
  // 正好落在抽屉后面。等抽屉进 DOM 后再滚一次，让它整排留在抽屉上方。
  void nextTick(() => revealCardIntoView(f.id))
}

/**
 * 「复制改法」：把改好的那一段放进剪贴板。
 *
 * 审查结果最常见的用法是把建议条款贴回 Word 里改，让人用鼠标拖选一段中文
 * 再 Ctrl+C 是白费劲。复制成功给 1.5 秒反馈，之后复原——按钮文字不该
 * 一直停在「已复制」，那会让人以为再点一次是「取消复制」。
 */
const copiedId = ref('')

async function copySuggestion(f: Finding): Promise<void> {
  const text = f.suggestion.revised?.trim() ?? ''
  if (text === '') return
  try {
    await navigator.clipboard.writeText(text)
    copiedId.value = f.id
    window.setTimeout(() => {
      if (copiedId.value === f.id) copiedId.value = ''
    }, 1500)
  } catch {
    // 非安全上下文（http 局域网演示）没有 clipboard API：把改法摊出来让人自己复制，
    // 比弹一句「复制失败」更接近他要做的事。
    window.prompt('复制这段改法：', text)
  }
}

function focusFinding(f: Finding): void {
  activeId.value = f.id
  void nextTick(() => {
    document
      .querySelector(`[data-page="${findingPage(f)}"]`)
      ?.scrollIntoView({ behavior: 'smooth', block: 'center' })
    document
      .querySelector(`[data-finding="${f.id}"]`)
      ?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
  })
}

/** 把键盘焦点落到某张卡上，同时选中它（左右两栏共用 activeId）。 */
function focusCard(f: Finding): void {
  document.querySelector<HTMLElement>(`.cards [data-finding="${f.id}"]`)?.focus()
  focusFinding(f)
}

/** ↑/↓ 在**当前可见**列表里换卡：边界停住，不绕回另一端（绕回会让人失去位置感）。 */
function moveCardFocus(delta: 1 | -1, index: number): void {
  const next = visible.value[index + delta]
  if (!next) return
  focusCard(next)
}

/**
 * 卡片上的键：Enter / 空格选中，↑/↓ 换卡。
 *
 * 不用模板里的 .prevent 修饰符：卡片里还嵌着「复制改法」「就问这条」和两个
 * <details>，键盘事件会冒泡上来，.prevent 会把这些控件自己的空格/回车吃掉。
 * 所以只处理"焦点正好在卡片本身"的情况，其余交给子控件。
 */
function onCardKey(evt: KeyboardEvent, f: Finding, index: number): void {
  if (evt.target !== evt.currentTarget) return
  if (evt.key === 'Enter' || evt.key === ' ') {
    evt.preventDefault()
    focusFinding(f)
    return
  }
  if (evt.key === 'ArrowDown' || evt.key === 'ArrowUp') {
    evt.preventDefault()
    moveCardFocus(evt.key === 'ArrowDown' ? 1 : -1, index)
  }
}

/**
 * 图上点一个框。被筛掉的框（mark--muted）照常可点。
 *
 * 选了「点它就把挡路的筛选撤掉」而不是「只 focusFinding」：用户在原文里看见一处
 * 问题，想立刻在右侧看到它，这时让他先回上面找 chip 是反人性。撤的方式是先复位
 * 严重度，关键词仍然挡着就一并清空——只复位一半会出现"左边亮了、右边没有卡"。
 * 高亮本身仍走 focusFinding，与点卡片完全同一条路径。
 */
function onMarkClick(f: Finding): void {
  if (filtering.value && !visibleIds.value.has(f.id)) {
    severityFilter.value = 'all'
    if (filterFindings([f], { severity: 'all', query: keyword.value }).length === 0) {
      keyword.value = ''
    }
  }
  focusFinding(f)
}

/** 卡片底边与卡片容器底边之间留的空，比「就问这条」那一排按钮略高一点。 */
const REVEAL_GAP = 12

/**
 * 把某张卡滚进右侧可视区，并且保证它的操作按钮不被抽屉压住。
 *
 * 抽屉是 .panel 这个 flex 列的最后一个子项，卡片容器（.cards）的底边就是抽屉的顶边——
 * 所以这里只用 .cards 的实时 rect，不写死 40vh 的像素值，也不去读抽屉内部结构。
 *
 * 为什么按"底边对齐"算而不是 scrollIntoView({block:'nearest'})：卡片展开法条后会
 * 比可视区还高，nearest 在那种情况下只保证"露出来一点"，操作按钮仍然落在抽屉后面。
 * 用户下一步要点按钮，所以必须保证底边可见。
 */
function revealCardIntoView(id: string): void {
  const card = document.querySelector<HTMLElement>(`.cards [data-finding="${id}"]`)
  if (!card) return
  const scroller = card.closest<HTMLElement>('.cards')
  if (!scroller) return
  const box = card.getBoundingClientRect()
  const view = scroller.getBoundingClientRect()
  // 底边越界：往下滚，让按钮整排落在抽屉上方。
  const down = box.bottom - (view.bottom - REVEAL_GAP)
  if (down > 0) {
    scroller.scrollBy({ top: down, behavior: 'smooth' })
    return
  }
  // 顶边越界（卡片比可视区矮、被滚到闷在顶上）：滚回顶部。
  const up = box.top - view.top
  if (up < 0) scroller.scrollBy({ top: up, behavior: 'smooth' })
}

/**
 * 展开/收起法条或修改建议后卡片高度变了，操作按钮可能整体沉到抽屉后面。
 *
 * 只处理"正在被追问的那一张"：别的卡展开时去动当前卡，会把用户正在看的位置抢走。
 */
function onFoldToggle(f: Finding): void {
  if (activeId.value !== f.id) return
  void nextTick(() => revealCardIntoView(f.id))
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
  startedAt.value = Date.now()
  elapsedMs.value = 0
  // 200ms 而不是 1000ms：第一档只有 1500ms，秒级 tick 会让它晚半拍才换文案。
  tickTimer = window.setInterval(() => {
    elapsedMs.value = Date.now() - startedAt.value
  }, 200)
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
    await wait(POLL_BACKOFF[Math.min(i, POLL_BACKOFF.length - 1)] ?? POLL_INTERVAL)
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
      // 这份合同已经在跑了（另一台设备、或者自动开审撞上手动点）。这不是错误：
      // 不闪红条，接上那条任务继续等，用户看到的就是"已在跑"。
      await adoptRunningReview()
      return
    }
    if (code === 40401) {
      ElMessage.error('合同不存在或无权访问')
      await router.replace({ name: 'upload' })
    } else {
      reviewError.value = message(err, '提交审查失败，请重试')
    }
    running.value = false
    stopTicking()
  }
}

/** 40901 之后：把界面接到那份正在跑的审查上，而不是报错。 */
async function adoptRunningReview(): Promise<void> {
  try {
    const existing = await findReviewByFile(fileId.value)
    if (existing && (existing.status === 'pending' || existing.status === 'running') && existing.task_id) {
      reviewId = existing.id
      running.value = true
      startTicking()
      await pollTask(existing.task_id)
      return
    }
  } catch {
    // 查不到就当没接上，下面按"没结果"处理，不把内部错误抛给用户。
  }
  // 接不上任务：不报错，回到"还没结果"的状态，用户可以再点一次。
  running.value = false
  stopTicking()
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
      startTicking()
      await pollTask(existing.task_id)
      return
    }
    review.value = existing
    if (existing.status === 'failed' || existing.status === 'cancelled') {
      reviewError.value = '上一次审查没有完成，可以重新开始'
      reviewReason.value = (existing.error ?? '').trim()
      // 失败的那次不算"有结果"，按钮要还回去。
      review.value = null
    }
  } catch (err) {
    reviewError.value = message(err, '读取已审查结果失败')
  }
}

/**
 * 上传页带 `?autostart=1` 跳过来时自动开跑一次。
 *
 * 先抹掉标记再发请求：不抹的话用户刷新页面会二次触发，把刚跑完的结果又当成"要开审"。
 * 已有结果时 shouldAutostart 返回 false，不重复开审（后端会复用旧结果，用户白等两秒）。
 */
async function maybeAutostart(): Promise<void> {
  const marked = route.query.autostart !== undefined
  const start = shouldAutostart(route.query, review.value !== null)
  // 标记一律抹掉（哪怕这次不开审）：留着它，用户刷新页面会二次触发。
  if (marked) await router.replace({ query: {} })
  if (!start) return
  await startReview()
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
    // PDF 渲染和"开审/接上已审"互不依赖：串着等的话，四页 PDF 画完（dev 下两秒多）
    // 才开始提交审查，观众看到风险卡片的时刻被白白推迟。高亮框是绝对定位的 div，
    // viewports 晚到只是框先没有坐标、拿到后自动补上，不影响画布。
    const rendering = renderPdf()
    await restoreExisting()
    await maybeAutostart()
    await rendering
  } else {
    loading.value = false
  }
}

function retry(): void {
  void load()

// 同一路由换 fileId 时组件被复用（App.vue 是裸 <RouterView />，没有 :key），
// 不重新 load 就会把上一份合同的卡片和 PDF 留在屏幕上——浏览器前进后退最容易撞上。
watch(fileId, () => {
  chatOpen.value = false
  void load()
})
}

onBeforeUnmount(() => {
  disposed = true
  if (pollTimer !== null) window.clearTimeout(pollTimer)
  stopTicking()
  dragging.value = false
  stopDrag?.()
  stopDrag = null
})

void load()
</script>

<template>
  <div class="review-page" :class="{ 'is-resizing': dragging }">
    <PageHeader title="劳动法 AI 顾问" :subtitle="headerSubtitle">
      <template #right>
        <nav class="links" aria-label="面包屑">
          <ol class="crumbs">
            <li v-for="c in crumbs" :key="c.label" class="crumb">
              <router-link v-if="c.to" :to="{ name: c.to }">{{ c.label }}</router-link>
              <span v-else class="current" aria-current="page">{{ c.label }}</span>
            </li>
          </ol>
        </nav>
      </template>
    </PageHeader>

    <div v-if="loading" class="loading">正在加载合同…</div>

    <div v-else-if="loadError" class="blocked">
      <p class="blocked-title">{{ loadError }}</p>
      <el-button type="primary" @click="retry">重试</el-button>
    </div>

    <div v-else-if="scanned" class="blocked">
      <p class="blocked-title">这份文件没有文字层，可能是扫描件，暂不支持审查</p>
      <p class="blocked-hint">扫描件需要先做 OCR 才能定位条款，当前版本还没有这一步。</p>
      <el-button type="primary" @click="router.push({ name: 'upload' })">重新上传</el-button>
    </div>

    <main v-else ref="bodyEl" class="body">
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
                { 'mark--active': activeId === f.id, 'mark--muted': filtering && !visibleIds.has(f.id) },
              ]"
              :style="markStyle(f)"
              :title="f.title"
              @click="onMarkClick(f)"
            />
          </div>
          <p v-if="pdfError" class="pdf-error">{{ pdfError }}</p>
        </div>
      </section>

      <div
        v-if="!panelCollapsed"
        class="splitter"
        role="separator"
        aria-orientation="vertical"
        :aria-valuenow="panelWidth"
        :aria-valuemin="MIN_SIDE"
        :aria-valuemax="maxPanelWidth"
        aria-label="调整合同原文与风险面板的宽度"
        title="拖动调宽，双击复位"
        tabindex="0"
        @pointerdown="onSplitterDown"
        @keydown="onSplitterKey"
        @dblclick="resetPanelWidth"
      />

      <aside id="review-panel" class="panel" :class="{ 'panel--collapsed': panelCollapsed }">
        <!-- 折叠态：只剩一列图标条，数字还在，点任意位置展开。 -->
        <div v-if="panelCollapsed" class="panel-rail" @click="togglePanel">
          <button
            type="button"
            class="rail-btn"
            :aria-expanded="false"
            aria-controls="review-panel"
            title="展开风险清单"
          >
            <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">
              <path d="M10 3 5 8l5 5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
            </svg>
          </button>
          <span class="rail-count rail-count--high" :title="`高风险 ${summary.high} 条`">{{ summary.high }}</span>
          <span class="rail-count rail-count--medium" :title="`中风险 ${summary.medium} 条`">{{ summary.medium }}</span>
          <span class="rail-count rail-count--low" :title="`低风险 ${summary.low} 条`">{{ summary.low }}</span>
        </div>

        <div v-else class="panel-head">
          <div class="summary">
            <span class="sum sum--high">高风险 {{ summary.high }}</span>
            <span class="sum sum--medium">中风险 {{ summary.medium }}</span>
            <span class="sum sum--low">低风险 {{ summary.low }}</span>
          </div>
          <div class="panel-head-actions">
            <el-button v-if="showStart" type="primary" :loading="running" @click="startReview">
              开始审查
            </el-button>
            <button
              type="button"
              class="panel-fold"
              :aria-expanded="true"
              aria-controls="review-panel"
              title="收起风险清单"
              @click="togglePanel"
            >
              <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">
                <path d="M6 3l5 5-5 5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
              </svg>
            </button>
          </div>
        </div>

        <!-- 折叠态只留图标条：结果区整块不渲染，否则 40px 宽里塞着卡片会溢出到屏幕外 -->
        <template v-if="!panelCollapsed">
        <div v-if="running" class="running">
          <p
            v-if="progressLine.visible"
            class="running-line"
            role="status"
            aria-live="polite"
            aria-atomic="false"
          >{{ progressLine.text }}</p>
          <el-skeleton :rows="6" animated />
        </div>

        <div v-else-if="reviewError" class="review-error" role="alert">
          <p class="review-error-text">{{ reviewError }}</p>
          <span v-if="reviewReason" class="review-reason">{{ reviewReason }}</span>
          <!-- 失败之后用户只有两条路：换一份合同，或者原地再试一次。都给成按钮，别让人回去翻页。 -->
          <div class="review-error-actions">
            <el-button type="primary" @click="router.push({ name: 'upload' })">重新上传</el-button>
            <el-button @click="startReview">重新审查</el-button>
          </div>
        </div>

        <p v-else-if="!sorted.length" class="empty">
          还没有审查结果。点「开始审查」，21 条规则通常几秒内跑完；合同要素要靠模型补齐时会久一些。
        </p>

        <template v-else>
          <div class="filters" role="group" aria-label="筛选风险项">
            <div class="chips">
              <button
                v-for="c in chips"
                :key="c.value"
                type="button"
                class="chip"
                :class="{ 'chip--on': severityFilter === c.value }"
                :aria-pressed="severityFilter === c.value"
                @click="severityFilter = c.value"
              >{{ c.label }} {{ c.n }}</button>
            </div>
            <input
              v-model="keyword"
              class="filter-search"
              type="search"
              placeholder="搜索风险或条款"
              aria-label="搜索风险或条款"
            />
          </div>

          <p v-if="!visible.length" class="empty empty--filter">
            没有匹配的风险项
            <button type="button" class="clear-filter" @click="clearFilters">清除筛选</button>
          </p>

          <ul v-else class="cards">
            <li
              v-for="(f, i) in visible"
              :key="f.id"
              class="card"
              :class="{ 'card--active': activeId === f.id }"
              :data-finding="f.id"
              tabindex="0"
              role="button"
              :aria-current="activeId === f.id"
              @click="focusFinding(f)"
              @keydown="onCardKey($event, f, i)"
            >
              <div class="card-head">
                <span class="tag" :class="`tag--${f.severity}`">{{ SEVERITY_LABEL[f.severity] }}</span>
                <span class="card-title">{{ f.title }}</span>
                <span v-if="f.clause_ref.section" class="card-section">{{ f.clause_ref.section }}</span>
              </div>

              <p class="card-evidence">{{ f.evidence }}</p>

              <details v-if="f.legal_basis.length" class="fold" @toggle="onFoldToggle(f)">
                <summary>{{ legalBasisTitle(f) }}</summary>
                <div v-for="(b, bi) in f.legal_basis" :key="bi" class="basis">
                  <p class="basis-head">{{ b.law }} {{ b.article }}</p>
                  <p class="basis-text">{{ b.text }}</p>
                </div>
              </details>

              <details class="fold" @toggle="onFoldToggle(f)">
                <summary>修改建议</summary>
                <p v-if="f.suggestion.original" class="sug-original">{{ f.suggestion.original }}</p>
                <p v-if="f.suggestion.revised" class="sug-revised">{{ f.suggestion.revised }}</p>
                <p v-if="f.suggestion.reason" class="sug-reason">{{ f.suggestion.reason }}</p>
              </details>

              <div class="card-actions">
                <button
                  v-if="f.suggestion.revised"
                  type="button"
                  class="copy-sug"
                  @click.stop="copySuggestion(f)"
                >
                  {{ copiedId === f.id ? '已复制' : '复制改法' }}
                </button>
                <button type="button" class="ask-this" @click.stop="askThis(f)">就问这条</button>
              </div>
            </li>
          </ul>
        </template>
        </template>

        <!-- 抽屉放在 panel 里而不是盖在它上面：同一个 flex 列里上下分，
             卡片列表（flex:1）自动让位，被压住的内容也就不会滚不出来。

             用 v-show 不用 v-if：关闭只是 display:none，组件不销毁，state 与
             sessionId 都留着——关掉再打开接着问，一份合同的多次追问落在同一个
             会话里。v-if 会在每次关闭时把会话打回初始态，下一次追问又新建一条
             会话（侧栏多一条、多一次完整模型调用、彼此没有上下文）。 -->
        <InlineChat
          :key="fileId"
          v-show="chatOpen"
          :file-id="fileId"
          :seed="chatSeed"
          :seed-seq="chatSeedSeq"
          :section="chatSection"
          :resizable="!panelCollapsed"
          @close="closeChat"
        />
      </aside>
    </main>
  </div>
</template>

<style scoped>
.review-page.is-resizing {
  cursor: col-resize;
  user-select: none;
}

.review-page {
  /* 页面在 height:100vh 的 .app-shell 里，上方还有一条 24px 的提示条：
     min-height:100vh 会让它比容器高 24px，永远多出一条不该有的纵向滚动条。 */
  min-height: 0;
  display: flex;
  flex-direction: column;
  background: var(--ll-page-alt);
}

.links {
  margin-left: auto;
  /* 面包屑：正文导航 */
  font-size: var(--ll-fs-body);
  line-height: var(--ll-lh-body);
}

.crumbs {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.crumb {
  display: flex;
  align-items: center;
  gap: 8px;
}

.crumb + .crumb::before {
  content: '/';
  color: var(--ll-border);
}

.crumbs a {
  color: var(--ll-accent);
  text-decoration: none;
}

.crumbs a:hover {
  text-decoration: underline;
}

.crumbs .current {
  color: var(--ll-text-weak);
}

.loading,
.blocked {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 12px;
  /* 页面级左右内边距统一 32px */
  padding: 32px;
}

.blocked-title {
  margin: 0;
  /* 阻断标题：卡片标题 */
  font-size: var(--ll-fs-title);
  color: var(--ll-text-mid);
}

.blocked-hint {
  margin: 0;
  /* 阻断说明：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-faint);
}

.body {
  flex: 1;
  display: flex;
  min-height: 0;
  position: relative;
}

/* 分隔条是 body 的普通 flex 子项（6px），不叠在画布上：
   叠上去会挡住 PDF 右边缘，而 40 页合同正好在那一侧有条文。 */
.splitter {
  flex: none;
  width: 6px;
  cursor: col-resize;
  background: transparent;
  border-left: 1px solid var(--ll-border-muted);
}

.splitter:hover,
.splitter:focus-visible {
  background: var(--ll-primary-wash);
  outline: none;
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
  padding: 12px 24px;
  /* 查看器页头：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-subtle);
}

.viewer-title {
  color: var(--ll-text-mid);
  font-weight: 500;
}

.pages {
  flex: 1;
  overflow: auto;
  /* 页面级左右内边距统一 32px */
  padding: 0 32px 32px;
}

.page {
  position: relative;
  margin: 0 auto 20px;
  background: var(--ll-paper);
  box-shadow: var(--ll-paper-shadow);
}

.page-canvas {
  display: block;
}

.mark {
  position: absolute;
  padding: 0;
  border-radius: var(--ll-radius-sm);
  border: 2px solid;
  background: transparent;
  cursor: pointer;
  /* 浅在底色上，不浅在边框上：框要一直看得见，条文也要一直读得清 */
  transition: background-color 0.15s ease, border-color 0.15s ease, opacity 0.15s ease;
}

/* 被当前筛选挡在外面的框：只压透明度，不碰 --ll-mark-high/medium/low——
   那三个色值是逐像素验收过的，改一个数值整张纸上的配色就得重验。 */
.mark--muted {
  opacity: 0.25;
}

.mark--high {
  border-color: rgba(217, 48, 38, 0.55);
  background: rgba(217, 48, 38, 0.07);
}

.mark--medium {
  border-color: rgba(217, 119, 6, 0.55);
  background: rgba(217, 119, 6, 0.07);
}

.mark--low {
  border-color: rgba(202, 138, 4, 0.55);
  background: rgba(202, 138, 4, 0.07);
}

/* 选中态常驻：不自动灭，点下一条自然切换。描边沿用风险配色。
   这里和下面两条都用 --ll-mark-* 或 rgba 字面量：风险框压在白色 PDF 纸面上，
   深色模式把纸面留白、标记也必须保持原色，不能跟着主题变量一起变浅。 */
.mark--active {
  z-index: 2;
  border-color: rgba(217, 48, 38, 1);
  background: rgba(217, 48, 38, 0.16);
  outline: 2px solid rgba(217, 48, 38, 0.85);
  outline-offset: 1px;
  animation: pulse 1.2s ease-out 2;
}

/* 描边跟随风险等级：中/低风险点中后仍是自己的橙/黄，不然颜色自相矛盾。 */
.mark--active.mark--medium {
  border-color: var(--ll-mark-medium);
  outline-color: rgba(217, 119, 6, 0.85);
}

.mark--active.mark--low {
  border-color: var(--ll-mark-low);
  outline-color: rgba(202, 138, 4, 0.85);
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
  /* PDF 错误：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-danger);
}

.panel {
  /* 宽度由分隔条控制（默认 420，与改动前一致）；flex: none 不能省，
     否则它会变成可伸缩项，拖拽立刻失效。 */
  width: v-bind('panelWidth + "px"');
  flex: none;
  display: flex;
  flex-direction: column;
  background: var(--ll-surface);
  border-left: 1px solid var(--ll-border-muted);
}

.panel-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 16px 16px;
  border-bottom: 1px solid var(--ll-hairline-2);
}

.summary {
  display: flex;
  gap: 10px;
  /* 风险汇总徽标：标签 */
  font-size: var(--ll-fs-tag);
}

.sum--high {
  color: var(--ll-danger);
}

.sum--medium {
  color: var(--ll-warn);
}

.sum--low {
  color: var(--ll-text-faint);
}

.running {
  padding: 16px;
}

.running-line {
  margin: 0 0 10px;
  /* 正在生成：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-faint);
}

.review-error {
  margin: 16px;
  /* 审查失败说明：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-danger);
}

.review-error-text {
  margin: 0;
}

.review-error-actions {
  display: flex;
  gap: 8px;
  margin-top: 12px;
}

/* 具体原因比标题轻一档：用户先看到"没完成"，想深究再读这一行。 */
.review-reason {
  display: block;
  margin-top: 4px;
  /* 失败原因小字：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-weak);
}

.empty {
  margin: 0;
  padding: 32px 16px;
  /* 空态说明 */
  font-size: var(--ll-fs-body);
  line-height: 1.7;
  color: var(--ll-text-faint);
}

/* ---- 筛选条 --------------------------------------------------------------
   夹在面板头与卡片之间：先定"看哪一档"，再看列表。四个 chip 的数字来自
   countBySeverity（全部结果），所以切档之后数字不动，只有下面的卡在变。 */
.filters {
  flex: none;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 12px 16px 10px;
  border-bottom: 1px solid var(--ll-hairline-2);
}

.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.chip {
  padding: 2px 10px;
  border: 1px solid var(--ll-border-muted);
  border-radius: var(--ll-radius-pill);
  background: var(--ll-surface);
  color: var(--ll-text-mid);
  /* 筛选 chip：标签 */
  font-size: var(--ll-fs-tag);
  cursor: pointer;
}

.chip:hover {
  border-color: var(--ll-primary);
  color: var(--ll-primary);
}

.chip--on {
  border-color: var(--ll-primary);
  background: var(--ll-primary-weak);
  color: var(--ll-primary);
}

.chip:focus-visible,
.clear-filter:focus-visible {
  outline: 2px solid var(--ll-primary);
  outline-offset: 2px;
}

.filter-search {
  width: 100%;
  padding: 4px 8px;
  border: 1px solid var(--ll-border-muted);
  border-radius: var(--ll-radius-sm);
  background: var(--ll-surface);
  color: var(--ll-text-body);
  /* 搜索框：说明文字 */
  font-size: var(--ll-fs-meta);
  font-family: inherit;
}

.filter-search:focus-visible {
  outline: 2px solid var(--ll-primary);
  outline-offset: 1px;
}

/* 筛空了：一行说完，别撑成一大块空白——下面还有筛选条，用户要能立刻改条件。 */
.empty--filter {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 16px;
  /* 无匹配：说明文字 */
  font-size: var(--ll-fs-meta);
}

.clear-filter {
  padding: 0;
  border: 0;
  background: none;
  color: var(--ll-accent);
  /* 文字按钮：说明文字 */
  font-size: var(--ll-fs-meta);
  text-decoration: underline;
  cursor: pointer;
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
  border: 1px solid var(--ll-border-muted);
  border-radius: var(--ll-radius-md);
  cursor: pointer;
  transition: border-color 0.15s ease;
}

/* 卡片是 tabindex=0 的 role=button：键盘 Tab 进来必须看得见停在哪。
   用 outline 而不是 box-shadow——深色底上阴影几乎不可见，而 .card--active
   已经占用了 box-shadow，两件事也不该抢同一个属性。 */
.card:focus-visible {
  outline: 2px solid var(--ll-primary);
  outline-offset: 2px;
}

.card--active {
  border-color: var(--ll-accent);
  box-shadow: var(--ll-shadow-focus);
}

.card-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
}

.tag {
  flex: none;
  /* 风险等级徽标：标签 */
  font-size: var(--ll-fs-tag);
  padding: 0 4px;
  border-radius: var(--ll-radius-sm);
  color: var(--ll-on-primary);
}

.tag--high {
  background: var(--ll-danger);
}

.tag--medium {
  background: var(--ll-warn);
}

.tag--low {
  background: var(--ll-warn-mid);
}

.card-title {
  /* 风险卡标题：卡片标题 */
  font-size: var(--ll-fs-title);
  font-weight: 500;
  color: var(--ll-text-strong);
}

.card-section {
  margin-left: auto;
  /* 条款位置：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-faint);
}

.card-evidence {
  margin: 8px 0 0;
  /* 风险卡正文（证据句） */
  font-size: var(--ll-fs-body);
  line-height: 1.7;
  color: var(--ll-text-mid);
  white-space: pre-wrap;
  word-break: break-word;
}

.ask-this {
  margin-top: 8px;
  padding: 4px 12px;
  border: 1px solid var(--ll-border-strong);
  border-radius: var(--ll-radius-sm);
  background: var(--ll-surface);
  color: var(--ll-btn-text);
  /* 就问这条：按钮小字 */
  font-size: var(--ll-fs-tag);
  cursor: pointer;
}

.ask-this:hover {
  border-color: var(--ll-primary);
  color: var(--ll-primary);
}

.card-actions {
  display: flex;
  gap: 8px;
  align-items: center;
}

.copy-sug {
  margin-top: 8px;
  padding: 4px 12px;
  border: 1px solid var(--ll-border-strong);
  border-radius: var(--ll-radius-sm);
  background: var(--ll-surface);
  color: var(--ll-btn-text);
  /* 复制改法：按钮小字 */
  font-size: var(--ll-fs-tag);
  cursor: pointer;
}

.copy-sug:hover {
  border-color: var(--ll-primary);
  color: var(--ll-primary);
}

.fold {
  margin-top: 8px;
  /* 折叠三角：说明文字 */
  font-size: var(--ll-fs-meta);
}

.fold summary {
  cursor: pointer;
  color: var(--ll-accent);
  list-style: none;
}

.fold summary::-webkit-details-marker {
  display: none;
}

.basis {
  margin-top: 8px;
  padding: 8px 12px;
  background: var(--ll-surface-muted);
  border-radius: var(--ll-radius-sm);
}

.basis-head {
  margin: 0;
  font-weight: 500;
  color: var(--ll-text-strong);
}

.basis-text {
  margin: 4px 0 0;
  line-height: 1.7;
  color: var(--ll-text-soft);
  white-space: pre-wrap;
}

.sug-original {
  margin: 8px 0 0;
  padding: 8px 8px;
  background: var(--ll-surface-sunken);
  color: var(--ll-text-subtle);
  text-decoration: line-through;
  border-radius: var(--ll-radius-sm);
  white-space: pre-wrap;
}

.sug-revised {
  margin: 6px 0 0;
  padding: 8px 8px;
  background: var(--ll-ok-wash);
  color: var(--ll-ok-deep);
  border-radius: var(--ll-radius-sm);
  white-space: pre-wrap;
}

.sug-reason {
  margin: 6px 0 0;
  color: var(--ll-text-subtle);
  line-height: 1.7;
}
/* 窄屏：分隔条消失、面板回到改动前的 420，等于退回原来的固定两栏。 */
@media (max-width: 1100px) {
  .splitter {
    display: none;
  }

  .panel {
    width: 420px;
  }
}


/* ---- 右栏折叠 ------------------------------------------------------------ */
/* 收成图标条而不是 display:none：数字还在，用户知道有几条风险；也不改
   panelWidth，展开就回到上次拖的宽度。 */
.panel.panel--collapsed {
  width: 40px;
  padding: 0;
}

.panel-head-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

.panel-fold,
.rail-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  padding: 0;
  color: var(--ll-text-weak);
  background: none;
  border: 1px solid var(--ll-border);
  border-radius: var(--ll-radius-sm);
  cursor: pointer;
}

.panel-fold:hover,
.rail-btn:hover {
  color: var(--ll-primary);
  border-color: var(--ll-primary);
}

.panel-rail {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 12px 0;
  cursor: pointer;
}

.rail-count {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  /* 图标条计数：标签 */
  font-size: var(--ll-fs-tag);
  font-weight: 600;
  color: var(--ll-on-primary);
  border-radius: 50%;
}

.rail-count--high {
  background: var(--ll-danger);
}

.rail-count--medium {
  background: var(--ll-warn);
}

.rail-count--low {
  background: var(--ll-warn-mid);
}
</style>
