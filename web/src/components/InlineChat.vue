<script setup lang="ts">
// 审查页右下角的抽屉对话。
//
// 为什么是抽屉而不是分屏：分屏要把 ChatView 的流式状态机（会话列表、进度行、
// 停止与重试）抽成共享组件，会新开一堆状态分裂点；抽屉只新增一个组件，
// 审查页现有布局一行不动，关掉就跟没开过一样。
//
// 三条边界：
//   1. 只有第一问带 file_id——后端 resolveAttachment 会校验归属，后续轮次
//      会话已经记住了上下文，再带只会多查一次库；
//   2. 回答用 {{ }} 渲染，绝不 v-html：模型输出是不可信文本；
//   3. 卸载时 abort()，否则离开页面后 SSE 连接还挂着。

import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'

import { ApiError } from '@/api/client'
import { streamChat } from '@/api/chat'
import type { ChatStreamResult } from '@/api/chat'
import CitationCard from '@/components/CitationCard.vue'
import {
  appendCitation,
  appendDelta,
  beginTurn,
  finishTurn,
  initialInlineState,
  lastUserTextOf,
  setSession,
  takePending,
} from '@/utils/inline-chat'
import type { InlineState } from '@/utils/inline-chat'
import { shouldSendOnKey } from '@/utils/send-key'
import {
  clampDrawerHeight,
  defaultDrawerHeight,
  KEY_STEP,
  nextHeightByKey,
  readSideWidth,
  writeSideWidth,
} from '@/utils/splitter'

/** resizable 由父级控制：右栏折叠成图标条时抽屉没有高度可言，拖拽条要一起消失。 */
const props = defineProps<{
  fileId: string
  seed: string
  seedSeq: number
  section: string
  resizable?: boolean
}>()
const emit = defineEmits<{ close: [] }>()

const state = ref<InlineState>(initialInlineState())
const input = ref('')
const scroller = ref<HTMLElement | null>(null)

/** 当前追问的上下文（一行文字，来自审查结论，不含问句）。 */
const context = ref(props.seed)
/** 上一次真正发给模型的上下文；没变就不再重复拼前缀。 */
const sentContext = ref('')

const inputEl = ref<HTMLTextAreaElement | null>(null)

// ---- 抽屉高度 ------------------------------------------------------------
// 抽屉贴底，拖动条在顶上：往上拖变高。高度存 localStorage。
const HEIGHT_KEY = 'laborlaw.inlineChat.height'
const chatRoot = ref<HTMLElement | null>(null)
const chatHeight = ref(defaultDrawerHeight(typeof window === 'undefined' ? 900 : window.innerHeight))
const resizing = ref(false)
const narrow = ref(false)

function measure() {
  const panel = chatRoot.value?.parentElement?.clientHeight ?? 0
  const viewport = typeof window === 'undefined' ? 0 : window.innerHeight
  const upper = clampDrawerHeight(Number.MAX_SAFE_INTEGER, panel, viewport)
  const lower = clampDrawerHeight(0, panel, viewport)
  return { panel, viewport, upper, lower }
}

function applyHeight(next: number): void {
  const { upper, lower } = measure()
  chatHeight.value = Math.max(lower, Math.min(upper, Math.round(next)))
}

let stopDrag: (() => void) | null = null

function onResizeDown(e: PointerEvent): void {
  if (props.resizable === false) return
  e.preventDefault()
  const target = e.currentTarget as HTMLElement
  target.setPointerCapture?.(e.pointerId)
  const startY = e.clientY
  const startHeight = chatHeight.value
  const body = document.body
  const prevSelect = body.style.userSelect
  body.style.userSelect = 'none'
  resizing.value = true

  const onMove = (ev: PointerEvent): void => {
    // 往上拖（clientY 变小）高度变大
    applyHeight(startHeight + (startY - ev.clientY))
  }
  const onUp = (): void => {
    resizing.value = false
    body.style.userSelect = prevSelect
    writeSideWidth(HEIGHT_KEY, chatHeight.value)
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

function onResizeKey(e: KeyboardEvent): void {
  const { upper, lower } = measure()
  const next = nextHeightByKey(chatHeight.value, e.key, { step: KEY_STEP, min: lower, max: upper })
  if (next === null) return
  applyHeight(next)
  writeSideWidth(HEIGHT_KEY, chatHeight.value)
  e.preventDefault()
}

function resetHeight(): void {
  applyHeight(defaultDrawerHeight(typeof window === 'undefined' ? 900 : window.innerHeight))
  writeSideWidth(HEIGHT_KEY, chatHeight.value)
}

/** 窄屏锁默认高度：手机上拖抽屉和滚页面会打架。 */
const renderedHeight = computed(() =>
  narrow.value ? defaultDrawerHeight(typeof window === 'undefined' ? 900 : window.innerHeight) : chatHeight.value,
)
const resizerVisible = computed(() => props.resizable !== false && !narrow.value)

async function focusInput(): Promise<void> {
  await nextTick()
  inputEl.value?.focus()
}

/** 当前正在被写入的那条 assistant 气泡。 */
const activeIndex = ref(-1)

/** 这一轮之后还能不能带 file_id：只有第一问带。 */
const attachedOnce = ref(false)

let controller: AbortController | null = null

// 排队位：只存最后一问。答案还在流时点另一张卡，不丢这一问，也不并发第二条
// 模型调用——等这一轮结束再发。连点三次只答最后一句。
const pending = ref('')

const busy = computed(() => state.value.sending)

async function scrollToBottom(): Promise<void> {
  const el = scroller.value
  if (!el) return
  // 等一帧再滚：delta 写进 DOM 之后高度才变，否则永远差最后一行。
  await Promise.resolve()
  el.scrollTop = el.scrollHeight
}

async function send(raw: string): Promise<void> {
  const text = raw.trim()
  if (text === '') return
  // busy 分支必须留在最前：watch 回调里再进 send 时若还能塞回队列，就会自激。
  if (busy.value) {
    pending.value = text
    return
  }

  // 上下文变了才拼：同一目标连问几句不必重复 120 字，
  // 但用户中途点了另一张卡时必须重新告诉模型"现在问的是哪一条"。
  const withContext = context.value !== '' && context.value !== sentContext.value
  const outgoing = withContext ? `${context.value}\n\n${text}` : text
  if (withContext) sentContext.value = context.value

  const started = beginTurn(state.value, outgoing)
  state.value = started.state
  activeIndex.value = started.index
  input.value = ''
  await scrollToBottom()

  const withFile = !attachedOnce.value
  attachedOnce.value = true

  const current = new AbortController()
  controller = current

  let result: ChatStreamResult | null = null
  try {
    result = await streamChat(
      {
        input: outgoing,
        ...(state.value.sessionId === '' ? {} : { session_id: state.value.sessionId }),
        ...(withFile && props.fileId !== '' ? { file_id: props.fileId } : {}),
      },
      {
        onMeta: (event) => {
          // 会话 ID 只从 meta 拿，而且只认第一次（见 inline-chat 的单测）。
          state.value = setSession(state.value, event.session_id)
        },
        onDelta: (event) => {
          state.value = appendDelta(state.value, activeIndex.value, event.text)
          void scrollToBottom()
        },
        onCitation: (event) => {
          state.value = appendCitation(state.value, activeIndex.value, event.citation)
          void scrollToBottom()
        },
        onDone: () => {
          state.value = finishTurn(state.value, '')
        },
      },
      current.signal,
    )
  } catch (err) {
    // 后端文案就是最终文案；只有网络层异常才用兜底那句。
    const message = err instanceof ApiError ? err.message : '发送失败，请重试'
    state.value = finishTurn(state.value, message)
  } finally {
    if (controller === current) controller = null
  }

  // 用户主动停：不算错误，但也不能假装答完了。
  if (result?.aborted === true) {
    state.value = finishTurn(state.value, '已停止')
  }
}

function onSendClick(): void {
  void send(input.value)
}

function onKeydown(evt: KeyboardEvent): void {
  // Esc 关面板：正在生成时也只关面板、不掐流。
  // 掐流会留下"已中断"的半句话，而用户按 Esc 的意思通常是"我先看看合同"。
  // 组件是 v-show 常驻的，关掉只是 display:none，state 与 session_id 都留着。
  if (evt.key === 'Escape') {
    evt.preventDefault()
    emit('close')
    return
  }
  // 规则在 utils/send-key.ts 里，与对话页共用一个函数：Enter 发、Shift+Enter 换行，
  // 输入法组字期间不拦（打拼音按回车选词不该把半句话发出去）。
  if (!shouldSendOnKey(evt)) return
  evt.preventDefault()
  void send(input.value)
}

// 点「就问这条」只更新上下文并聚焦输入框，绝不代发。
// 用户点的是"我想就这一条提问"，不是"替我提问"。曾经这里直接 send(props.seed)，
// 结果是每点一次就往历史里塞一句他没问过的话，还把他上一段问答顶出视口。
// 首次挂载（old === undefined，抽屉此时是 display:none）只设置上下文、不抢焦点。
watch(
  () => props.seedSeq,
  (_cur, old) => {
    context.value = props.seed
    if (old !== undefined) void focusInput()
  },
  { immediate: true },
)

// 一轮结束后把排队的那一句发出去。不 abort 当前请求：中断会留一句「已停止」，
// 看着像系统出错，改成等它答完再接力。
watch(busy, (sending) => {
  const next = takePending(pending.value, sending)
  if (next === '') return
  pending.value = ''
  void send(next)
})

const media = typeof window !== 'undefined' ? window.matchMedia('(max-width: 900px)') : null
function syncNarrow(): void {
  narrow.value = media?.matches ?? false
}

let panelObserver: ResizeObserver | null = null

onMounted(() => {
  syncNarrow()
  media?.addEventListener('change', syncNarrow)
  // 读出来再夹一次：换分辨率或换机器时，旧值可能已经超出当前面板能给的区间。
  const { viewport } = measure()
  applyHeight(readSideWidth(HEIGHT_KEY, defaultDrawerHeight(viewport), 120, 560))

  // 窗口变小（演示时临时拉小浏览器）要把抽屉重新夹进新的面板高度里，
  // 否则 560 的抽屉会把卡片区压成一条缝。
  const panel = chatRoot.value?.parentElement
  if (panel && typeof ResizeObserver !== 'undefined') {
    let scheduled = false
    panelObserver = new ResizeObserver(() => {
      // 拖拽中不跟手：pointermove 正在算高度，这里再夹一次会打架。
      if (scheduled || resizing.value) return
      // 合并连续回调：拉窗口时每帧都触发，逐次夹取会抖。
      scheduled = true
      requestAnimationFrame(() => {
        scheduled = false
        // 用当前值再夹一次，不读存储——存储里要留住用户拖到的那个高度。
        applyHeight(chatHeight.value)
      })
    })
    panelObserver.observe(panel)
  }
})

onBeforeUnmount(() => {
  panelObserver?.disconnect()
  panelObserver = null
  media?.removeEventListener('change', syncNarrow)
  document.body.style.userSelect = ''
  stopDrag?.()
  stopDrag = null
  // 离开页面必须断开：SSE 挂着会一直占着连接与后端的一条 goroutine。
  controller?.abort()
  controller = null
  // 排队位也要清：卸载后 watch 若再触发，会把早已离开的页面又发一问。
  pending.value = ''
})

const sessionLabel = computed(() => state.value.sessionId.slice(0, 8))

/** 带过去的那一句：有问过就带上，没问过就不带（空 ask 会被对话页忽略）。 */
const lastUserText = computed(() => lastUserTextOf(state.value))
</script>

<template>
  <section ref="chatRoot" class="inline-chat" :class="{ 'is-resizing': resizing }" aria-label="就这份合同提问">
    <div
      v-if="resizerVisible"
      class="ic-resizer"
      role="separator"
      aria-orientation="horizontal"
      aria-label="调整追问面板高度"
      :aria-valuenow="Math.round(renderedHeight)"
      :aria-valuemin="Math.round(measure().lower)"
      :aria-valuemax="Math.round(measure().upper)"
      title="拖动调高，双击复位"
      tabindex="0"
      @pointerdown="onResizeDown"
      @keydown="onResizeKey"
      @dblclick="resetHeight"
    />

    <header class="ic-head">
      <span class="ic-title">就这份合同提问</span>
      <span v-if="sessionLabel !== ''" class="ic-session" :title="state.sessionId">
        会话 {{ sessionLabel }}
      </span>
      <RouterLink
        class="ic-link"
        :to="{
          name: 'chat',
          query: {
            ...(lastUserText !== '' ? { ask: lastUserText } : {}),
            // 空串不传：ChatView 收到 section= 会拼出「合同「」」这种残句。
            ...(props.section !== '' ? { section: props.section } : {}),
            from: 'review',
            file: props.fileId,
          },
        }"
      >在对话页继续</RouterLink>
      <button type="button" class="ic-close" title="关闭" @click="emit('close')">×</button>
    </header>

    <p v-if="context !== ''" class="ic-context" :title="context">正在追问：{{ context }}</p>

    <div ref="scroller" class="ic-body">
      <p v-if="state.turns.length === 0" class="ic-empty">
        点风险卡片上的「就问这条」，或直接在这里问这份合同的问题。
      </p>

      <div v-for="(turn, i) in state.turns" :key="i" class="ic-turn" :class="`ic-${turn.role}`">
        <!--
          助手那一轮在生成期间 aria-live=off、答完切 polite：逐字播报只是噪音，
          用户要听的是"整段答案"，而不是每个 token 都念一遍。
        -->
        <p
          class="ic-text"
          :aria-live="turn.role === 'assistant'
            ? (busy && i === state.turns.length - 1 ? 'off' : 'polite')
            : undefined"
        >{{ turn.content }}</p>
        <div v-if="turn.citations.length" class="ic-citations">
          <CitationCard v-for="c in turn.citations" :key="c.statute_id" :citation="c" />
        </div>
      </div>

      <p v-if="state.error !== ''" class="ic-error" role="alert">{{ state.error }}</p>
    </div>

    <footer class="ic-composer">
      <textarea
        ref="inputEl"
        v-model="input"
        class="ic-input"
        rows="2"
        :placeholder="props.section !== '' ? `想问「${props.section}」什么？` : '就这份合同提问…'"
        title="Enter 发送，Shift+Enter 换行"
        :disabled="busy"
        @keydown="onKeydown"
      ></textarea>
      <button type="button" class="ic-send" :disabled="busy || input.trim() === ''" @click="onSendClick">
        {{ busy ? '正在回答…' : '发送' }}
      </button>
    </footer>
  </section>
</template>

<style scoped>
/* 抽屉是 .panel 的普通 flex 子项：卡片列表（.cards, flex:1, overflow:auto）
   自己让位，而不是被一层绝对定位盖住——盖住时往上滚也翻不出被压在下面的卡片。 */
.inline-chat {
  flex: none;
  width: 100%;
  height: v-bind('renderedHeight + "px"');
  display: flex;
  flex-direction: column;
  background: #fff;
}

/* 拖拽条 6px，border-top 挪到这里（留在 .inline-chat 上会叠成 2px）。 */
.ic-resizer {
  flex: none;
  height: 6px;
  cursor: ns-resize;
  border-top: 1px solid #e5e7eb;
}

.ic-resizer:hover,
.ic-resizer:focus-visible {
  background: var(--ll-primary-weak, #dbeafe);
  outline: none;
}

.inline-chat.is-resizing {
  user-select: none;
}

@media (max-width: 900px) {
  /* 窄屏由 JS 把高度锁成默认值，这里只保证手柄不出现（模板里也 v-if 掉了，双保险）。 */
  .ic-resizer {
    display: none;
  }
}

.ic-head {
  display: flex;
  gap: 8px;
  align-items: center;
  padding: 8px 12px;
  border-bottom: 1px solid #f0f1f3;
}

/* 上下文条比标题低一档：它是"现在在问哪一条"的提示，不该抢标题的位置。
   单行截断而不是折行——折成三行把抽屉里能看答案的高度吃掉一半。 */
.ic-context {
  margin: 0;
  padding: 6px 12px;
  border-bottom: 1px solid #f3f4f6;
  background: #f9fafb;
  color: #4b5563;
  font-size: 12px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.ic-title {
  font-size: 13px;
  font-weight: 600;
  color: #111827;
}

.ic-session {
  font-size: 11px;
  color: #9ca3af;
}

.ic-link {
  margin-left: auto;
  font-size: 12px;
  color: #2563eb;
  text-decoration: none;
}

.ic-link:hover {
  text-decoration: underline;
}

.ic-close {
  border: none;
  background: transparent;
  color: #9ca3af;
  font-size: 16px;
  line-height: 1;
  cursor: pointer;
  padding: 2px 4px;
}

.ic-close:hover {
  color: #4b5563;
}

.ic-body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 10px 12px;
}

.ic-empty {
  margin: 0;
  font-size: 12px;
  color: #9ca3af;
}

.ic-turn {
  margin-bottom: 10px;
}

.ic-text {
  margin: 0;
  font-size: 13px;
  line-height: 1.6;
  color: #1f2937;
  white-space: pre-wrap;
  word-break: break-word;
}

.ic-user .ic-text {
  padding: 6px 10px;
  border-radius: 8px;
  background: #f3f4f6;
}

.ic-citations {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-top: 6px;
}

.ic-error {
  margin: 6px 0 0;
  font-size: 12px;
  color: #b91c1c;
}

.ic-composer {
  display: flex;
  gap: 8px;
  align-items: flex-end;
  padding: 8px 12px 10px;
  border-top: 1px solid #f0f1f3;
}

.ic-input {
  flex: 1;
  resize: none;
  padding: 6px 8px;
  font-family: inherit;
  font-size: 13px;
  line-height: 1.5;
  border: 1px solid #d0d5dd;
  border-radius: 6px;
}

.ic-input:focus {
  outline: none;
  border-color: #2f6feb;
}

.ic-send {
  padding: 6px 12px;
  font-size: 13px;
  color: #fff;
  background: #2f6feb;
  border: none;
  border-radius: 6px;
  cursor: pointer;
}

.ic-send:disabled {
  background: #c7d2fe;
  cursor: not-allowed;
}
</style>
