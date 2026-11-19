<script setup lang="ts">
// 对话页。
//
// 两条数据来源要分清，混在一起出问题：
//   - 左侧会话列表、历史消息来自 REST（刷新后必须还在）；
//   - 正在生成的那条消息只有本地状态（citation 事件里的法条库没存，
//     所以历史消息永远没有引用卡片，这是数据决定的，不是前端偷懒）。
//
// 流的三种收尾也必须分清：done（正常）、用户 abort（标记已中断）、
// 没等到 done 就断（抛 ApiError"连接中断，请重试"）。
// 三者在界面上长得不一样，否则用户分不清"答完了"和"断线了"。

import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { ApiError } from '@/api/client'
import { streamChat } from '@/api/chat'
import { uploadFile } from '@/api/files'
import * as sessionApi from '@/api/session'
import type { Attachment, ChatMessage, Citation } from '@/api/types'
import {
  applyProgress,
  startProgress,
  statusLine,
  type ChatProgress,
  type ProgressEvent,
} from '@/utils/chat-progress'
import { historyBubbles } from '@/utils/chat-history'
import { fileCardLabel } from '@/utils/file-card'
import { prependSession } from '@/utils/session-list'
import { contextFileId, contextLabel, shouldPrefill } from '@/utils/ask-this'
import { formatRelative } from '@/utils/format-relative'
import type { Session } from '@/api/types'
import CitationCard from '@/components/CitationCard.vue'
import { useAuthStore } from '@/stores/auth'
import { createStreamSession, type StreamSession } from '@/utils/stream-session'
import { shouldSendOnKey } from '@/utils/send-key'
import {
  clampSideWidth,
  KEY_STEP,
  nextWidthByKey,
  readSideWidth,
  writeSideWidth,
} from '@/utils/splitter'
import { dedupeTitles, parseAskTitle, sessionTitle } from '@/utils/session-title'

/** 页面上的一条消息。历史消息与流式消息共用这个形状，差别只在有没有引用。 */
interface UiMessage {
  key: string
  role: 'user' | 'assistant'
  content: string
  /** 只有本轮实时生成的消息会有；历史消息恒为空数组 */
  citations: Citation[]
  /** user 消息附带的合同文件；assistant 消息恒为空数组。刷新后来自 messages.attachments。 */
  attachments: Attachment[]
  /**
   * 本轮流式回答的进度（纯函数状态机，见 utils/chat-progress.ts）。
   * 历史消息为 null，因为历史消息本来就没有"等待"这件事。
   */
  progress: ChatProgress | null
  /** 后端 error 帧或 HTTP 失败 */
  error: string
  interrupted: boolean
  fromHistory: boolean
}

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

const sessions = ref<Session[]>([])
const currentSessionId = ref('')
const messages = ref<UiMessage[]>([])
const input = ref('')
const sending = ref(false)
const collapsed = ref(false)

// ---- 侧栏宽度 ------------------------------------------------------------
// 会话标题长短差很多（「第七条」vs 一句完整提问），280 宽经常要截断，
// 但太宽会把回答挤窄，所以 220–420 之间交给用户拖。
// 注意方向与审查页相反：侧栏在左，ArrowRight 是变宽。
const SIDE_KEY = 'laborlaw.chat.sideWidth'
const SIDE_DEFAULT = 280
const SIDE_MIN = 220
const SIDE_MAX = 420
const chatPageEl = ref<HTMLElement | null>(null)
const sideWidth = ref(readSideWidth(SIDE_KEY, SIDE_DEFAULT, SIDE_MIN, SIDE_MAX))
const sideDragging = ref(false)

/** 容器宽度要减去分隔条自己占的 6px，否则夹出来的宽度会把主区挤出横向滚动条。 */
function availableWidth(): number {
  const w = chatPageEl.value?.clientWidth ?? 0
  return w > 0 ? w - 6 : 0
}

function applySideWidth(desired: number): void {
  sideWidth.value = clampSideWidth(desired, availableWidth(), { min: SIDE_MIN, max: SIDE_MAX })
}

let stopSideDrag: (() => void) | null = null

function onSideDown(e: PointerEvent): void {
  e.preventDefault()
  const target = e.currentTarget as HTMLElement
  target.setPointerCapture?.(e.pointerId)
  const rect = chatPageEl.value?.getBoundingClientRect()
  if (!rect) return
  const startWidth = sideWidth.value
  const startX = e.clientX
  document.body.style.userSelect = 'none'
  sideDragging.value = true

  const onMove = (ev: PointerEvent): void => {
    applySideWidth(startWidth + (ev.clientX - startX))
  }
  const onUp = (): void => {
    sideDragging.value = false
    document.body.style.userSelect = ''
    writeSideWidth(SIDE_KEY, sideWidth.value)
    stopSideDrag?.()
    stopSideDrag = null
  }
  target.addEventListener('pointermove', onMove)
  target.addEventListener('pointerup', onUp)
  target.addEventListener('pointercancel', onUp)
  stopSideDrag = () => {
    target.removeEventListener('pointermove', onMove)
    target.removeEventListener('pointerup', onUp)
    target.removeEventListener('pointercancel', onUp)
  }
}

function onSideKey(e: KeyboardEvent): void {
  const next = nextWidthByKey(sideWidth.value, e.key, { step: KEY_STEP, min: SIDE_MIN, max: SIDE_MAX })
  if (next === null) return
  applySideWidth(next)
  writeSideWidth(SIDE_KEY, sideWidth.value)
  e.preventDefault()
}

function resetSideWidth(): void {
  applySideWidth(SIDE_DEFAULT)
  writeSideWidth(SIDE_KEY, sideWidth.value)
}
// 初值 true：首屏本来就在"还没拉到列表"的状态，先显示「加载中…」再落空态，
// 不要先闪一下「还没有会话」。后续 refreshSessions() 不清这个标记（列表已有内容时不显示提示）。
const listLoading = ref(true)
const historyLoading = ref(false)
const banner = ref('')
/** 左栏搜索词。纯本地过滤，不发请求——搜会话不该产生流量，也不该因为网络抖动变空白。 */
const search = ref('')

/**
 * 侧栏标题：把后端截出来的那句话重新拼一次，同名再按顺序编号。
 *
 * 后端标题是"用户原话截 20 字"，从审查页追问时原话是 `合同「第七条」 · …`，
 * 同一个条款问几次就几条一模一样。这里认回条款号与规则号，再交给 dedupeTitles
 * 加 #2/#3，用户才能从侧栏分出是哪一次。
 */
const sessionTitles = computed<string[]>(() => {
  const raw = sessions.value.map((s) => {
    const { section, ruleId } = parseAskTitle(s.title)
    // 有 file_name 就用它当标题前缀：同一份合同的同一条款追问多次，
    // 靠文件名 + #2/#3 才分得清；历史会话没有这列，退回条款 + 规则号。
    return sessionTitle({ fileName: s.file_name, section, ruleId, fallback: s.title })
  })
  return dedupeTitles(raw)
})

/** 标题跟着会话顺序走，模板里按下标取，避免再建一张 Map。 */
const titleOf = (index: number): string => sessionTitles.value[index] ?? ''

/** 列表行：标题在整张表上算好（去重序号不随搜索结果变），再按搜索词过滤。 */
const visibleRows = computed<{ session: Session; title: string }[]>(() => {
  const rows = sessions.value.map((s, i) => ({ session: s, title: titleOf(i) }))
  const q = search.value.trim().toLowerCase()
  if (q === '') return rows
  return rows.filter((r) => r.title.toLowerCase().includes(q))
})

/** 有会话但被搜索滤空：文案要和"一条都没有"区分开。 */
const searchEmpty = computed(() => search.value.trim() !== '' && visibleRows.value.length === 0)

const scroller = ref<HTMLElement | null>(null)
// 当前这一轮流式回答。卸载、点停止、退出登录都要把它作废：
// 否则组件已经没了，SSE 的 onDelta 还在往 assistant 气泡里写。
let session: StreamSession | null = null
// 组件是否还在。卸载之后不再碰 DOM/滚动，也不用再去改 messages。
let alive = true
let keySeed = 0
/**
 * 状态行的"当前时间"。等待态文案靠它重算（2s / 8s / 20s 三档切换、
 * 20 秒后秒数跳动）。它只表示"现在几点"，不代表任何进度，所以不是假进度条。
 */
const nowTick = ref(Date.now())
let ticker: ReturnType<typeof setInterval> | null = null

function startTicking(): void {
  stopTicking()
  // 250ms 一跳：够让 1999/2000ms 这种边界在肉眼上看不出来偏差，又不费 CPU。
  ticker = setInterval(() => {
    nowTick.value = Date.now()
  }, 250)
}

function stopTicking(): void {
  if (ticker !== null) {
    clearInterval(ticker)
    ticker = null
  }
}

/** 本轮流式回答当前该显示的状态行；历史消息恒为不可见。 */
function statusOf(m: UiMessage): { visible: boolean; text: string } {
  if (m.progress === null) return { visible: false, text: '' }
  const line = statusLine(m.progress, nowTick.value)
  return { visible: line.visible, text: line.text }
}

const currentTitle = computed(() => {
  if (currentSessionId.value === '') return '新对话'
  const hit = sessions.value.find((s) => s.id === currentSessionId.value)
  return hit?.title || '对话'
})

const canSend = computed(
  () => !sending.value && (input.value.trim() !== '' || pendingFile.value !== null),
)

// ---- 合同附件 ----
//
// 附件只在"这一轮"有意义：它是给模型的问题配的一份材料，不是会话级设置。
// 所以发送成功就清空，不做"记住上次选的合同"这种贴心——那会让第二轮悄悄
// 带着上一份合同去问，用户完全看不出来。
const pendingFile = ref<Attachment | null>(null)
const uploading = ref(false)
// 从审查页带过来的上下文：只用来在输入框上方显示「我在追问哪一条」，
// 不跟着消息走、不落库——它是这次追问的入口凭据，不是会话的一部分。
const askCtx = ref<{ label: string; fileId: string } | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)

/** 只挂了合同没打字时的默认问法。空 input 后端会回 40001，不能发出去。 */
const DEFAULT_ATTACH_INPUT = '帮我看看这份合同有没有风险'

function pickFile(): void {
  if (sending.value || uploading.value) return
  fileInput.value?.click()
}

async function onFilePicked(evt: Event): Promise<void> {
  const el = evt.target as HTMLInputElement
  const file = el.files?.[0]
  // 立刻清空选择：不清的话第二次选同一个文件不会触发 change，用户会以为按钮坏了。
  el.value = ''
  if (!file) return
  if (file.type !== 'application/pdf' && !file.name.toLowerCase().endsWith('.pdf')) {
    ElMessage.error('目前只支持 PDF 合同')
    return
  }
  uploading.value = true
  try {
    const rec = await uploadFile(file)
    pendingFile.value = { file_id: rec.id, filename: rec.filename, page_count: rec.page_count }
  } catch (error) {
    // 上传失败必须出声：静默失败时用户会以为附件挂上了，发出去却什么都没有。
    ElMessage.error(error instanceof Error ? error.message : '合同上传失败')
  } finally {
    uploading.value = false
  }
}

function dropPendingFile(): void {
  pendingFile.value = null
}

/** 点卡片进审查页，并带 autostart=1 让它自动开审（与上传页同一个入口语义）。 */
function openReview(fileId: string): void {
  void router.push({ name: 'review', params: { fileId }, query: { autostart: '1' } })
}

function nextKey(prefix: string): string {
  keySeed += 1
  return `${prefix}-${keySeed}`
}

/**
 * toUiMessages 把历史消息映射成气泡。
 *
 * 映射本身在 utils/chat-history.ts 里（纯函数，可单测）；这里只负责补上
 * 界面专属字段。引用来自库（messages.citations），缺失时是空数组——
 * 不补默认法条，那是编造。
 */
function toUiMessages(items: readonly ChatMessage[]): UiMessage[] {
  return historyBubbles(items).map((b) => {
    const msg = newMessage(b.role, b.content, true)
    msg.citations = b.citations
    msg.attachments = b.attachments
    return msg
  })
}

function newMessage(role: 'user' | 'assistant', content: string, fromHistory = false): UiMessage {
  return {
    key: nextKey(role),
    role,
    content,
    citations: [],
    attachments: [],
    progress: null,
    error: '',
    interrupted: false,
    fromHistory,
  }
}

async function scrollToBottom(): Promise<void> {
  await nextTick()
  const el = scroller.value
  if (el) el.scrollTop = el.scrollHeight
}

async function refreshSessions(): Promise<void> {
  listLoading.value = true
  try {
    sessions.value = await sessionApi.listSessions()
  } catch (error) {
    banner.value = error instanceof Error ? error.message : '会话列表加载失败'
  } finally {
    listLoading.value = false
  }
}

/** 只负责把一条会话的正文拉进来；地址栏的事由调用方决定。 */
async function loadSession(session: Session): Promise<void> {
  currentSessionId.value = session.id
  messages.value = []
  historyLoading.value = true
  banner.value = ''
  try {
    const items = await sessionApi.listMessages(session.id)
    messages.value = toUiMessages(items)
    await scrollToBottom()
  } catch (error) {
    banner.value = error instanceof Error ? error.message : '历史消息加载失败'
  } finally {
    historyLoading.value = false
  }
}

async function openSession(session: Session): Promise<void> {
  if (sending.value) return
  // 先改地址再拉正文：地址是这条会话的唯一真相，F5/后退都靠它重建。
  // 这里用 push 而不是 replace：切会话算一次真实导航，后退要能退回上一条会话。
  // 点当前这条不会重复入栈——下面的相等判断直接跳过。
  if (route.params.sessionId !== session.id) {
    void router.push({ name: 'chat', params: { sessionId: session.id } })
  }
  await loadSession(session)
}

/** 生成中被打断的那次导航：地址已经变了、正文还没跟上，等流结束补一次。 */
let pendingSession: string | null = null

/**
 * 把「地址里的会话」落到界面上：重建正文。
 *
 * watch 与 onMounted 共用这一份实现，避免"恢复"和"跟随"两处各写一半而走岔。
 * 生成中一律推迟：onMounted 是异步的，它的 applyRoute 可能在用户已经发问之后才跑到，
 * 那时清空 messages 会把刚推上去的用户气泡和正在长的回答一起抹掉。
 */
async function applyRoute(want: string): Promise<void> {
  if (sending.value) {
    pendingSession = want
    return
  }
  if (want === '') {
    currentSessionId.value = ''
    messages.value = []
    return
  }
  const hit = sessions.value.find((s) => s.id === want)
  if (hit) {
    await loadSession(hit)
    return
  }
  // 侧栏只装了首屏 50 条：更早的会话、别的设备新建的会话都不在 sessions 里。
  // 找不到不代表链接是脏的，先按 id 问一次后端，只有它明确说「没有这条」才回落。
  try {
    const fresh = await sessionApi.getSession(want)
    sessions.value = prependSession(sessions.value, fresh)
    await loadSession(fresh)
  } catch (error) {
    if (error instanceof ApiError && error.code === 40401) {
      // 会话被删、或链接本来就是脏的：回落并告知一声，不要静默停在空对话上。
      // 只在 want !== '' 时到这里，进 /chat 空态不弹。
      ElMessage.info('这条会话不存在或已被删除')
      void router.replace({ name: 'chat' })
      return
    }
    // 网络抖动、后端 5xx：留着地址不动，用户刷新一次就能重试。
    banner.value = '这条会话没能打开，稍后再试'
  }
}

function startNewSession(): void {
  if (sending.value) return
  currentSessionId.value = ''
  messages.value = []
  banner.value = ''
  input.value = ''
  if (route.params.sessionId !== undefined) void router.replace({ name: 'chat' })
}

/** 侧栏当前停靠的行：roving tabindex 的"当前"就是它。 */
const listActive = ref(0)

// 列表长度会变（搜索过滤、删会话、新建会话），把停靠点夹回合法范围，
// 否则 Tab 进来落到一个不存在的行上，等于列表失去焦点入口。
watch(
  () => visibleRows.value.length,
  (n) => {
    if (n === 0) {
      listActive.value = 0
      return
    }
    if (listActive.value > n - 1) listActive.value = n - 1
  },
)

/** 把焦点挪到第 idx 行（越界夹住）。 */
function focusSessionRow(idx: number): void {
  const rows = visibleRows.value
  if (rows.length === 0) return
  const next = Math.min(Math.max(idx, 0), rows.length - 1)
  listActive.value = next
  const id = rows[next].session.id
  const el = document.querySelector<HTMLElement>(
    `.session-item[data-session-id="${id}"] .session-title`,
  )
  el?.focus()
}

/** 侧栏键盘：↑/↓ 换行、Home/End 到头尾、Delete 删当前行。 */
function onListKeydown(evt: KeyboardEvent, idx: number, session: Session): void {
  if (evt.key === 'ArrowDown') {
    evt.preventDefault()
    focusSessionRow(idx + 1)
    return
  }
  if (evt.key === 'ArrowUp') {
    evt.preventDefault()
    focusSessionRow(idx - 1)
    return
  }
  if (evt.key === 'Home') {
    evt.preventDefault()
    focusSessionRow(0)
    return
  }
  if (evt.key === 'End') {
    evt.preventDefault()
    focusSessionRow(visibleRows.value.length - 1)
    return
  }
  if (evt.key === 'Delete') {
    evt.preventDefault()
    void removeSession(session)
  }
}

async function removeSession(session: Session): Promise<void> {
  try {
    await ElMessageBox.confirm(`删除后「${session.title}」里的对话将无法恢复。`, '删除会话', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }

  try {
    await sessionApi.deleteSession(session.id)
    sessions.value = sessions.value.filter((s) => s.id !== session.id)
    if (currentSessionId.value === session.id) startNewSession()
    ElMessage.success('已删除')
  } catch (error) {
    ElMessage.error(error instanceof Error ? error.message : '删除失败')
  }
}

function addCitation(target: UiMessage, citation: Citation): void {
  // 同一条法条可能被检索多次命中，按 statute_id 去重即可；
  // 不去重的话一次咨询能挂出五张一模一样的第三十八条。
  if (citation.statute_id === '') return
  if (target.citations.some((c) => c.statute_id === citation.statute_id)) return
  target.citations.push(citation)
}

async function send(): Promise<void> {
  const attached = pendingFile.value
  const typed = input.value.trim()
  // 只挂了合同没打字：补一句默认问法，而不是把空 input 发给后端换一个 40001。
  const text = typed !== '' ? typed : attached ? DEFAULT_ATTACH_INPUT : ''
  if (text === '' || sending.value) return

  banner.value = ''
  input.value = ''
  pendingFile.value = null
  const startedNew = currentSessionId.value === ''

  const bubble = newMessage('user', text)
  // 卡片跟着这条消息走：刷新后从 messages.attachments 重建，所以本地也要填。
  if (attached) bubble.attachments = [attached]
  messages.value.push(bubble)
  const assistant = reactive(newMessage('assistant', '')) as UiMessage
  // 等待态从"点击发送"就开始计时，而不是从 meta 到齐开始：
  // 用户感受到的等待就是从按下发送那一刻起的。
  assistant.progress = startProgress(Date.now())
  messages.value.push(assistant)
  nowTick.value = Date.now()
  startTicking()
  await scrollToBottom()

  sending.value = true
  const current = createStreamSession()
  const controller = new AbortController()
  current.bind(controller)
  session = current

  /** 把真实事件喂给状态机；状态行的所有文案都从这里来。 */
  const push = (event: ProgressEvent): void => {
    if (assistant.progress === null) return
    assistant.progress = applyProgress(assistant.progress, event, Date.now())
  }

  try {
    const result = await streamChat(
      {
        input: text,
        ...(currentSessionId.value === '' ? {} : { session_id: currentSessionId.value }),
        // 附件只影响落库与展示，编排行为不变（B 侧不感知这个字段）。
        ...(attached ? { file_id: attached.file_id } : {}),
      },
      {
        // 每个处理器第一行都判一次：页面卸载或用户点了停止之后，
        // 还在路上的帧一律丢弃，不能再去改 messages / 会话列表。
        onMeta: (event) => {
          if (current.closed()) return
          // 新会话的 ID 只从这里拿：服务端在建会话时生成，
          // 没有它下一轮就会又新建一个会话。
          currentSessionId.value = event.session_id
          // 新会话一有 ID 就写进地址栏：不然 F5 之后这条会话就找不回来了。
          void router.replace({ name: 'chat', params: { sessionId: event.session_id } })
          push({ type: 'meta' })
          if (startedNew) void refreshSessions()
        },
        onDelta: (event) => {
          if (current.closed()) return
          // 先喂事件再写正文：状态行消失要和第一个字同时发生。
          push({ type: 'delta' })
          assistant.content += event.text
          void scrollToBottom()
        },
        onCitation: (event) => {
          if (current.closed()) return
          addCitation(assistant, event.citation)
          push({ type: 'citation' })
        },
        onToolCall: (event) => {
          if (current.closed()) return
          push({ type: 'tool_call', status: event.tool.status, detail: event.tool.detail })
        },
        onDone: () => {
          if (current.closed()) return
          push({ type: 'done' })
        },
      },
      controller.signal,
    )

    if (result.aborted) {
      push({ type: 'stopped' })
      assistant.interrupted = true
      if (assistant.content === '') assistant.content = '（已中断）'
    } else if (result.done) {
      // 会话标题与更新时间在服务端落库后才变，这里顺带刷一次左侧列表。
      void refreshSessions()
    }
  } catch (error) {
    // 已停止或已卸载：不要把错误再糊到气泡上（停止是用户主动的，不是失败）。
    if (current.closed() || !alive) return
    if (error instanceof ApiError && error.code === 40101) {
      // 令牌已失效，main.ts 的全局弹框会提示重新登录；
      // 这里只管收尾，不往气泡上写第二份错误文案。
      return
    }
    const message = error instanceof Error ? error.message : '发送失败'
    push({ type: 'error', message })
    assistant.error = message
    if (assistant.content === '') assistant.content = '（没有收到回答）'
  } finally {
    stopTicking()
    if (session === current) session = null
    // "发送中"是界面状态，跟这轮流有没有被掐断无关：点了停止之后必须复位，
    // 否则输入框永远发不出下一条（e2e 第 4 步就是抓这个）。
    sending.value = false
    if (!alive) return
    // 生成期间后退留下的导航：现在流停了，按地址把正文补上。
    // 不补的话地址显示 A、内容还是 B，且没人会再纠正——用户 F5 才发现"消息丢了"。
    if (pendingSession !== null) {
      const want = pendingSession
      pendingSession = null
      await applyRoute(want)
      return
    }
    await scrollToBottom()
  }
}

function stop(): void {
  session?.close()
}

/** 复制按钮的"已复制"只亮给刚点的那一条，1.5 秒后自己灭。 */
const copiedKey = ref('')
/** 正在流式生成的那条（最后一条 assistant）：它的复制按钮要禁用。 */
const streamingMessage = computed(() => (sending.value ? messages.value[messages.value.length - 1] ?? null : null))

async function copyAnswer(m: UiMessage): Promise<void> {
  if (m.content === '') return
  try {
    await navigator.clipboard.writeText(m.content)
  } catch {
    // 无 user activation、非 https、权限被拒都会走到这里。
    // 弹一个能手动全选复制的框，比"复制失败"四个字有用。
    window.prompt('复制这条回答：', m.content)
    return
  }
  copiedKey.value = m.key
  window.setTimeout(() => {
    if (copiedKey.value === m.key) copiedKey.value = ''
  }, 1500)
}

function onComposerKeydown(evt: Event | KeyboardEvent): void {
  // ElInput 把原生 keydown 透出来，类型上是 Event；不判一下拿不到 ctrlKey。
  if (!(evt instanceof KeyboardEvent)) return
  // 规则在 utils/send-key.ts 里，与审查页抽屉共用一份：Enter 发、Shift+Enter 换行。
  if (!shouldSendOnKey(evt)) return
  // preventDefault 只拦下"换行"，发送与否交给 send() 里的 sending 判断：
  // 生成中按 Enter 不该插一个空行，也不该打断正在跑的那一轮。
  evt.preventDefault()
  void send()
}

/**
 * 粘贴超长文本时提示一次。
 *
 * 必须看 clipboardData 的原始长度：maxlength 先把内容截到 4000，
 * 等 v-model 更新完再看 input.value.length 永远是 4000，判不出"被截了"。
 */
const INPUT_LIMIT = 4000
let limitHinted = false

function onPasteLimit(evt: ClipboardEvent): void {
  const pasted = evt.clipboardData?.getData('text') ?? ''
  if (pasted.length <= INPUT_LIMIT || limitHinted) return
  limitHinted = true
  ElMessage.warning(`单条最多 ${INPUT_LIMIT} 字，超出的部分已截断`)
  window.setTimeout(() => {
    limitHinted = false
  }, 3000)
}

async function signOut(): Promise<void> {
  // 先掐断在飞的流：令牌一清，下一帧就可能 40101 把用户又弹回登录页。
  session?.close()
  await auth.logout()
  await router.replace({ name: 'login' })
}

onMounted(async () => {
  // 预填只在进入时做一次：先把 query 抹掉，刷新就不会反复塞同一句话。
  // 不自动发送——审查页那边只负责把"哪一条、什么毛病"带过来，
  // 发不发、要不要补自己的情况由用户决定。
  // 来源条与预填同源，一起读：query 抹掉之后就再也取不到了。
  const ctxLabel = contextLabel(route.query)
  const ctxFile = contextFileId(route.query)
  if (ctxLabel !== '') {
    askCtx.value = { label: ctxLabel, fileId: ctxFile }
  }
  if (shouldPrefill(route.query)) {
    input.value = String(route.query.ask)
    void router.replace({ query: {} })
  }
  await auth.loadMe()
  await refreshSessions()

  // 地址里带着会话 ID 就把它恢复出来（F5、深链、后退都走这条路）。
  // 查不到就抹掉那段脏 ID：留在地址栏上会让人以为"这条会话还在"。
  await applyRoute(String(route.params.sessionId ?? ''))
})

// 地址变了才拉正文：后退/前进、点会话、点新对话都只改地址，
// 正文由这里统一跟着走，避免两处各写一半。
watch(
  () => route.params.sessionId,
  async (id) => {
    const want = String(id ?? '')
    // 自己刚写的地址不要再触发一次加载（openSession 已经拉过了）。
    if (want === currentSessionId.value) return
    // 生成中的打断由 applyRoute 记进 pendingSession，收尾时补切。
    await applyRoute(want)
  },
)

onBeforeUnmount(() => {
  // 用户切走页面/退出时，流必须跟着停：组件都没了，onDelta 还在改 messages，
  // 除了漏内存没有别的结果。
  alive = false
  session?.close()
  session = null
  document.body.style.userSelect = ''
  stopSideDrag?.()
  stopSideDrag = null
})

// 挂载后再夹一次：localStorage 里存的是上次那台机器的值，当前窗口可能更窄。
onMounted(() => {
  void nextTick(() => applySideWidth(sideWidth.value))
})
</script>

<template>
  <div ref="chatPageEl" class="chat-page" :class="{ 'is-resizing': sideDragging }">
    <aside class="sidebar" :class="{ collapsed }">
      <div class="sidebar-head">
        <el-button type="primary" class="new-btn" :disabled="sending" @click="startNewSession">
          ＋ 新对话
        </el-button>
        <!--
          收起/展开是纯视觉开关，不进 Tab 顺序：键盘用户从搜索框走到会话行只要一次，
          而它夹在中间会把"到主输入框几次 Tab"顶上去（实测 8 次，目标是 6 次以内）。
          收起之后主区顶部的 » 按钮仍在 Tab 顺序里，键盘照样能展开回来。
        -->
        <button
          type="button"
          class="collapse-btn"
          tabindex="-1"
          :title="collapsed ? '展开会话列表' : '收起会话列表'"
          @click="collapsed = !collapsed"
        >
          {{ collapsed ? '»' : '«' }}
        </button>
      </div>

      <div v-if="!collapsed" class="sidebar-body">
        <input
          v-if="sessions.length > 0"
          v-model="search"
          class="session-search"
          type="search"
          placeholder="搜索会话"
          aria-label="搜索会话"
        />
        <p v-if="listLoading && sessions.length === 0" class="sidebar-hint">加载中…</p>
        <p v-else-if="sessions.length === 0" class="sidebar-hint">还没有会话，发第一句就会出现在这里。</p>
        <p v-else-if="searchEmpty" class="sidebar-hint">没有匹配的会话</p>
        <!--
          侧栏用 roving tabindex：整张列表只留一个 Tab 停靠点。
          之前每行两个可聚焦按钮（标题 + 删除），50 条会话意味着从搜索框走到
          主输入框要按 107 次 Tab——纯键盘用户基本上用不了。
          现在 ↑/↓ 在行间移动、Enter 打开、Delete 删除，Tab 一次就离开列表。
        -->
        <ul v-else class="session-list">
          <li
            v-for="(row, idx) in visibleRows"
            :key="row.session.id"
            class="session-item"
            :class="{ active: row.session.id === currentSessionId }"
            :data-session-id="row.session.id"
            @keydown="onListKeydown($event, idx, row.session)"
          >
            <button
              type="button"
              class="session-title"
              :tabindex="idx === listActive ? 0 : -1"
              :disabled="sending"
              @focus="listActive = idx"
              @click="openSession(row.session)"
            >
              <span class="session-name">{{ row.title }}</span>
              <span class="session-time">{{ formatRelative(row.session.updated_at) }}</span>
            </button>
            <button
              type="button"
              class="session-del"
              title="删除会话"
              tabindex="-1"
              :disabled="sending"
              @click="removeSession(row.session)"
            >
              ×
            </button>
          </li>
        </ul>
      </div>

      <router-link v-if="!collapsed" class="review-link" :to="{ name: 'upload' }">
        合同审查
      </router-link>

      <div v-if="!collapsed" class="sidebar-foot">
        <span class="me">{{ auth.user?.nickname || auth.user?.phone_masked || '已登录' }}</span>
        <button type="button" class="logout" :disabled="sending" @click="signOut">退出</button>
      </div>
    </aside>

    <div
      v-if="!collapsed"
      class="splitter"
      role="separator"
      aria-orientation="vertical"
      :aria-valuenow="Math.round(sideWidth)"
      :aria-valuemin="SIDE_MIN"
      :aria-valuemax="SIDE_MAX"
      aria-label="调整会话列表与对话区的宽度"
      title="拖动调宽，双击复位"
      tabindex="0"
      @pointerdown="onSideDown"
      @keydown="onSideKey"
      @dblclick="resetSideWidth"
    />

    <section class="main">
      <header class="main-head">
        <button
          v-if="collapsed"
          type="button"
          class="collapse-btn"
          title="展开会话列表"
          @click="collapsed = false"
        >
          »
        </button>
        <h2 class="main-title">{{ currentTitle }}</h2>
      </header>

      <p v-if="banner" class="banner">{{ banner }}</p>

      <div ref="scroller" class="messages">
        <div v-if="historyLoading" class="empty">历史消息加载中…</div>

        <div v-else-if="messages.length === 0" class="empty">
          <h3>把问题说得越具体，答案越准</h3>
          <p>例如：</p>
          <ul>
            <li>公司不给我交社保，我能直接走吗？</li>
            <li>试用期说好三个月，结果拖到半年，合法吗？</li>
            <li>离职时公司扣我 2000 块押金，能要回来吗？</li>
          </ul>
        </div>

        <div
          v-for="m in messages"
          :key="m.key"
          class="row"
          :class="m.role === 'user' ? 'row-user' : 'row-assistant'"
        >
          <div
            class="bubble"
            :class="[
              m.role === 'user' ? 'bubble-user' : 'bubble-assistant',
              // 只挂了合同没打字时正文是空的：不要一个装着空气泡的深色块。
              m.content === '' && m.attachments.length > 0 ? 'bubble-plain' : '',
            ]"
          >
            <div
              v-for="a in m.attachments"
              :key="a.file_id"
              class="file-card"
              role="button"
              tabindex="0"
              :title="`查看 ${a.filename} 的审查结果`"
              @click="openReview(a.file_id)"
              @keydown.enter.prevent="openReview(a.file_id)"
            >
              <svg class="file-card-icon" viewBox="0 0 24 24" aria-hidden="true">
                <path
                  d="M6 2h7l5 5v13a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2z"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.6"
                  stroke-linejoin="round"
                />
                <path d="M13 2v5h5" fill="none" stroke="currentColor" stroke-width="1.6" />
                <text x="12" y="17" text-anchor="middle" font-size="6" fill="currentColor">PDF</text>
              </svg>
              <span class="file-card-label">{{ fileCardLabel(a) }}</span>
              <button
                type="button"
                class="file-card-more"
                @click.stop="openReview(a.file_id)"
              >
                查看风险
              </button>
            </div>

            <div v-if="statusOf(m).visible" class="status-row">
              <span class="status-dot" aria-hidden="true" />
              <span class="status-text">{{ statusOf(m).text }}</span>
            </div>
            <!--
              流式期间挂 aria-live="off"，答完切回 polite：
              逐字播报会把屏幕阅读器刷成噪音，而"一次性念完整段"才是用户要的。
            -->
            <p
              v-if="m.content !== ''"
              class="content ll-pre-wrap"
              :aria-live="sending && m === streamingMessage ? 'off' : 'polite'"
            >{{ m.content }}</p>

            <p v-if="m.interrupted" class="meta-line">已中断</p>
            <p v-if="m.error" class="error-line" role="alert">{{ m.error }}</p>

            <div v-if="m.citations.length > 0" class="citations">
              <CitationCard v-for="c in m.citations" :key="c.statute_id" :citation="c" />
            </div>

            <div v-if="m.role === 'assistant' && m.content !== ''" class="bubble-actions">
              <button
                type="button"
                class="copy-btn"
                :disabled="sending && m === streamingMessage"
                @click="copyAnswer(m)"
              >
                {{ copiedKey === m.key ? '已复制' : '复制' }}
              </button>
            </div>
          </div>
        </div>
      </div>

      <footer class="composer">
        <div v-if="askCtx" class="ask-ctx">
          <span class="ask-ctx-text">{{ askCtx.label }}</span>
          <router-link
            v-if="askCtx.fileId"
            class="ask-ctx-link"
            :to="{ name: 'review', params: { fileId: askCtx.fileId } }"
          >查看原文</router-link>
          <button
            type="button"
            class="ask-ctx-close"
            title="不用这条上下文"
            @click="askCtx = null"
          >
            ×
          </button>
        </div>
        <div v-if="pendingFile || uploading" class="pending-file">
          <span class="pending-name">
            {{ uploading ? '正在上传合同…' : fileCardLabel(pendingFile!) }}
          </span>
          <button
            v-if="!uploading"
            type="button"
            class="pending-remove"
            title="不附这份合同"
            @click="dropPendingFile"
          >
            ×
          </button>
        </div>
        <el-input
          v-model="input"
          type="textarea"
          :rows="3"
          resize="none"
          maxlength="4000"
          show-word-limit
          placeholder="描述你的情况，Enter 发送，Shift+Enter 换行"
          @keydown="onComposerKeydown"
          @paste="onPasteLimit"
        />
        <div class="composer-tools">
          <button
            type="button"
            class="attach-btn"
            :disabled="sending || uploading"
            @click="pickFile"
          >
            附合同
          </button>
          <input
            ref="fileInput"
            class="attach-input"
            type="file"
            accept="application/pdf"
            @change="onFilePicked"
          />
        </div>
        <div class="composer-actions">
          <el-button v-if="sending" @click="stop">停止</el-button>
          <el-button v-else type="primary" :disabled="!canSend" @click="send">发送</el-button>
        </div>
      </footer>
    </section>
  </div>
</template>

<style scoped>
.chat-page {
  flex: 1;
  display: flex;
  min-width: 0;
  min-height: 0;
}

.sidebar {
  display: flex;
  flex-direction: column;
  flex: none;
  width: v-bind('sideWidth + "px"');
  background: var(--ll-surface);
  border-right: 1px solid var(--ll-border);
  transition: width 0.2s ease;
}

.sidebar.collapsed {
  width: 56px;
}

.sidebar-head {
  display: flex;
  flex: none;
  gap: 6px;
  align-items: center;
  padding: 10px;
}

.new-btn {
  flex: 1;
}

.collapse-btn {
  flex: none;
  width: 26px;
  height: 26px;
  font-size: 12px;
  color: var(--ll-text-weak);
  cursor: pointer;
  background: transparent;
  border: 1px solid var(--ll-border);
  border-radius: 6px;
}

.sidebar-body {
  flex: 1;
  min-height: 0;
  padding: 0 8px;
  overflow-y: auto;
}

.sidebar-hint {
  padding: 8px;
  margin: 0;
  font-size: 12px;
  color: var(--ll-text-weak);
}

.session-list {
  padding: 0;
  margin: 0;
  list-style: none;
}

/* 搜索框：左栏只有两百多像素，不加边框阴影，一条细线隔开就够。 */
.session-search {
  width: 100%;
  padding: 5px 8px;
  margin: 0 0 6px;
  font-size: 12px;
  color: var(--ll-text);
  background: var(--ll-bg);
  border: 1px solid var(--ll-border, #e5e7eb);
  border-radius: 6px;
  outline: none;
}

.session-search:focus {
  border-color: var(--ll-primary);
}

.session-item {
  display: flex;
  gap: 4px;
  align-items: center;
  padding: 2px 4px;
  border-radius: 6px;
}

.session-item:hover {
  background: var(--ll-bg);
}

.session-item.active {
  background: var(--ll-primary-weak);
}

.session-title {
  flex: 1;
  min-width: 0;
  padding: 6px;
  overflow: hidden;
  font-size: 13px;
  color: var(--ll-text);
  text-align: left;
  cursor: pointer;
  background: transparent;
  border: 0;
  /* 标题一行、时间一行：只有标题时看不出会话是昨天还是上周的，顺序像是随机的。 */
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.session-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.session-time {
  font-size: 12px;
  color: #909399;
}

.session-del {
  flex: none;
  width: 20px;
  height: 20px;
  font-size: 14px;
  line-height: 1;
  color: var(--ll-text-weak);
  cursor: pointer;
  background: transparent;
  border: 0;
  border-radius: 4px;
  opacity: 0;
}

.session-item:hover .session-del {
  opacity: 1;
}

.session-del:hover {
  color: #d93026;
  background: #fdecea;
}

.review-link {
  flex: none;
  padding: 8px 12px;
  font-size: 13px;
  color: var(--ll-accent, #2563eb);
  text-decoration: none;
  border-top: 1px solid var(--ll-border);
}

.review-link:hover {
  background: rgba(37, 99, 235, 0.06);
}

.sidebar-foot {
  display: flex;
  flex: none;
  align-items: center;
  justify-content: space-between;
  padding: 10px 12px;
  font-size: 12px;
  color: var(--ll-text-weak);
  border-top: 1px solid var(--ll-border);
}

.me {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.logout {
  font-size: 12px;
  color: var(--ll-text-weak);
  cursor: pointer;
  background: transparent;
  border: 0;
}

.logout:hover {
  color: var(--ll-primary);
}

.main {
  display: flex;
  flex: 1;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
}

.main-head {
  display: flex;
  flex: none;
  gap: 8px;
  align-items: center;
  padding: 10px 16px;
  background: var(--ll-surface);
  border-bottom: 1px solid var(--ll-border);
}

.main-title {
  margin: 0;
  overflow: hidden;
  font-size: 14px;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.banner {
  flex: none;
  padding: 8px 16px;
  margin: 0;
  font-size: 12px;
  color: #d93026;
  background: #fdecea;
}

.messages {
  flex: 1;
  min-height: 0;
  padding: 16px;
  overflow-y: auto;
}

.empty {
  max-width: 520px;
  margin: 40px auto;
  color: var(--ll-text-weak);
}

.empty h3 {
  margin: 0 0 8px;
  font-size: 15px;
  color: var(--ll-text);
}

.empty ul {
  padding-left: 18px;
  margin: 4px 0 0;
}

.empty li {
  margin-bottom: 4px;
}

.row {
  display: flex;
  margin-bottom: 14px;
}

.row-user {
  justify-content: flex-end;
}

.row-assistant {
  justify-content: flex-start;
}

.bubble {
  max-width: min(720px, 82%);
  padding: 10px 12px;
  border-radius: var(--ll-radius);
}

.bubble-user {
  color: #fff;
  background: var(--ll-user-bubble);
}

.bubble-assistant {
  background: var(--ll-surface);
  border: 1px solid var(--ll-border);
}

/*
 * 状态行。
 *
 * 后端 0.1 秒把 meta / tool_call / citation 推完，然后模型推理 30 多秒不吐字。
 * 状态行就是给这 30 秒一个"它在动"的可见证据：
 *   - 文案由真实耗时与真实 citation 条数算出（见 utils/waiting.ts）；
 *   - 左边的小圆点只是呼吸动画，不带任何进度含义；
 *   - 第一个 delta 到达时整行消失，不留"已完成"的尾巴。
 * 次要文字色，不抢正文。
 */
.status-row {
  display: flex;
  gap: 8px;
  align-items: center;
  font-size: 13px;
  color: var(--ll-text-weak);
}

.status-dot {
  flex: none;
  width: 7px;
  height: 7px;
  background: var(--ll-primary);
  border-radius: 50%;
  animation: breathe 1.4s ease-in-out infinite;
}

@keyframes breathe {
  0%,
  100% {
    opacity: 0.25;
    transform: scale(0.85);
  }

  50% {
    opacity: 1;
    transform: scale(1.1);
  }
}

/* 关掉动效的用户：静态点，别抖。 */
@media (prefers-reduced-motion: reduce) {
  .status-dot {
    opacity: 0.75;
    animation: none;
    transform: none;
  }
}

.content {
  margin: 0;
  font-size: 14px;
}

/* 状态行与正文之间留一点气口，正文第一行不要贴着点点。 */
.status-row + .content {
  margin-top: 6px;
}

.meta-line {
  margin: 6px 0 0;
  font-size: 11px;
  color: var(--ll-text-weak);
}

.error-line {
  margin: 6px 0 0;
  font-size: 12px;
  color: #d93026;
}

.citations {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 10px;
}

/* 复制入口平时是淡的、悬停才变实：投影演示时鼠标不在气泡上，观众也要看得出这里能复制；
   常亮到底又会把引用卡片挤下去，所以取中间态（0.55）。 */
.bubble-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 6px;
  /* 常驻可见：投影演示时讲解者的鼠标不在气泡上，观众也要看得出这里能复制 */
  opacity: 0.55;
  transition: opacity 0.15s;
}

.row:hover .bubble-actions,
.bubble-actions:focus-within {
  opacity: 1;
}

/* 触屏没有 hover，常亮，否则手机上永远点不到。 */
@media (hover: none) {
  .bubble-actions {
    opacity: 1;
  }
}

.copy-btn {
  font-size: 12px;
  color: #6b7280;
  background: none;
  border: 1px solid #e5e7eb;
  border-radius: 6px;
  padding: 2px 8px;
  cursor: pointer;
}

.copy-btn:hover:not(:disabled) {
  color: var(--ll-primary, #1d4ed8);
  border-color: var(--ll-primary, #1d4ed8);
}

.copy-btn:disabled {
  color: #c0c4cc;
  border-color: #ebeef5;
  cursor: not-allowed;
}

.composer {
  /*
    flex 列 + 显式 order：DOM 里把「附合同」放到输入框之后，是为了让键盘先落到
    主输入框（Tab 到 textarea 的次数是可用性指标），视觉上它仍然在输入框上方。
  */
  display: flex;
  flex-direction: column;
  flex: none;
  padding: 10px 16px 14px;
  background: var(--ll-surface);
  border-top: 1px solid var(--ll-border);
}

.composer-actions {
  order: 5;
  display: flex;
  justify-content: flex-end;
  margin-top: 8px;
}

.composer-tools {
  order: 3;
  display: flex;
  gap: 8px;
  align-items: center;
  margin-bottom: 8px;
}

.attach-btn {
  padding: 4px 10px;
  font-size: 12px;
  color: var(--ll-text-weak);
  cursor: pointer;
  background: transparent;
  border: 1px solid var(--ll-border);
  border-radius: 6px;
}

.attach-btn:hover:not(:disabled) {
  color: var(--ll-primary);
  border-color: var(--ll-primary);
}

.attach-btn:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}

/* 真正的 file input 藏起来，按钮只是它的把手：原生控件在各浏览器长得都不一样。 */
.attach-input {
  display: none;
}

/* 待发送芯片：让用户在按发送之前就知道"这一轮带了一份合同"。 */
.ask-ctx {
  order: 1;
  display: flex;
  gap: 8px;
  align-items: center;
  margin-bottom: 6px;
}

.ask-ctx-text {
  font-size: 12px;
  padding: 4px 10px;
  border-radius: 6px;
  background: #f9fafb;
  border: 1px solid #e5e7eb;
  color: #4b5563;
}

.ask-ctx-link {
  font-size: 12px;
  color: #2563eb;
  text-decoration: none;
}

.ask-ctx-link:hover {
  text-decoration: underline;
}

.ask-ctx-close {
  border: none;
  background: transparent;
  color: #9ca3af;
  font-size: 14px;
  line-height: 1;
  cursor: pointer;
  padding: 2px 4px;
}

.ask-ctx-close:hover {
  color: #4b5563;
}

.pending-file {
  order: 2;
  display: flex;
  gap: 8px;
  align-items: center;
  padding: 4px 8px;
  margin-bottom: 8px;
  font-size: 12px;
  color: var(--ll-text);
  background: rgba(37, 99, 235, 0.08);
  border-radius: 6px;
}

.pending-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.pending-remove {
  flex: none;
  padding: 0 4px;
  font-size: 14px;
  line-height: 1;
  color: var(--ll-text-weak);
  cursor: pointer;
  background: transparent;
  border: 0;
}

.pending-remove:hover {
  color: #d93026;
}

/*
 * 消息里的合同卡片。
 *
 * 气泡本身是深蓝底，卡片用白底压上去：深底上的白色卡片刻意做成"文件"的样子，
 * 与引用卡片（CitationCard）视觉语言一致，但不复用它的组件——
 * 那张卡是法条、可点开原文，这张是文件、点进审查页，交互目标不同。
 */
.file-card {
  display: flex;
  gap: 8px;
  align-items: center;
  padding: 8px 10px;
  margin-bottom: 8px;
  color: var(--ll-text);
  cursor: pointer;
  background: #fff;
  border: 1px solid var(--ll-border);
  border-radius: 8px;
}

.file-card:hover {
  border-color: var(--ll-primary);
}

.file-card-icon {
  flex: none;
  width: 26px;
  height: 26px;
  color: #d93026;
}

.file-card-label {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  font-size: 13px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.file-card-more {
  flex: none;
  padding: 2px 8px;
  font-size: 12px;
  color: var(--ll-primary);
  cursor: pointer;
  background: transparent;
  border: 1px solid var(--ll-border);
  border-radius: 6px;
}

.file-card-more:hover {
  border-color: var(--ll-primary);
}

/* 只有卡片、没有正文的 user 气泡：去掉深色底与内边距，别画一个空盒子。 */
.bubble-plain {
  padding: 0;
  background: transparent;
}

.splitter {
  flex: none;
  width: 6px;
  cursor: col-resize;
  background: transparent;
}

.splitter:hover,
.splitter:focus-visible {
  background: var(--ll-primary-weak, #dbeafe);
  outline: none;
}

.chat-page.is-resizing {
  cursor: col-resize;
  user-select: none;
}

/* 窄屏不做拖拽：两栏本来就快没地方了，锁回默认宽度。 */
@media (max-width: 900px) {
  .splitter {
    display: none;
  }
}
</style>
