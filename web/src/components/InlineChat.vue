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

import { computed, onBeforeUnmount, ref, watch } from 'vue'
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
} from '@/utils/inline-chat'
import type { InlineState } from '@/utils/inline-chat'

const props = defineProps<{ fileId: string; seed: string; seedSeq: number; section: string }>()
const emit = defineEmits<{ close: [] }>()

const state = ref<InlineState>(initialInlineState())
const input = ref('')
const scroller = ref<HTMLElement | null>(null)

/** 当前正在被写入的那条 assistant 气泡。 */
const activeIndex = ref(-1)

/** 这一轮之后还能不能带 file_id：只有第一问带。 */
const attachedOnce = ref(false)

let controller: AbortController | null = null

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
  if (text === '' || busy.value) return

  const started = beginTurn(state.value, text)
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
        input: text,
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
  // Enter 发送、Shift+Enter 换行：中文输入法组字期间不能拦，
  // 否则打拼音按回车选词会直接发出去半句话。
  if (evt.key !== 'Enter' || evt.shiftKey) return
  if (evt.isComposing) return
  evt.preventDefault()
  void send(input.value)
}

// 看的是「第几次点击」而不是 seed 文本：连点同一张卡时 seed 一个字都不变，
// 拿文本判重会把第二次点击吞掉。序号变一次就发一次。
watch(
  () => props.seedSeq,
  () => {
    if (props.seed === '') return
    void send(props.seed)
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  // 离开页面必须断开：SSE 挂着会一直占着连接与后端的一条 goroutine。
  controller?.abort()
  controller = null
})

const sessionLabel = computed(() => state.value.sessionId.slice(0, 8))

/** 带过去的那一句：有问过就带上，没问过就不带（空 ask 会被对话页忽略）。 */
const lastUserText = computed(() => lastUserTextOf(state.value))
</script>

<template>
  <section class="inline-chat" aria-label="就这份合同提问">
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

    <div ref="scroller" class="ic-body">
      <p v-if="state.turns.length === 0" class="ic-empty">
        点风险卡片上的「就问这条」，或直接在这里问这份合同的问题。
      </p>

      <div v-for="(turn, i) in state.turns" :key="i" class="ic-turn" :class="`ic-${turn.role}`">
        <p class="ic-text">{{ turn.content }}</p>
        <div v-if="turn.citations.length" class="ic-citations">
          <CitationCard v-for="c in turn.citations" :key="c.statute_id" :citation="c" />
        </div>
      </div>

      <p v-if="state.error !== ''" class="ic-error">{{ state.error }}</p>
    </div>

    <footer class="ic-composer">
      <textarea
        v-model="input"
        class="ic-input"
        rows="2"
        placeholder="追问这份合同的问题，Enter 发送，Shift+Enter 换行"
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
  height: min(40vh, 360px);
  border-top: 1px solid #e5e7eb;
  display: flex;
  flex-direction: column;
  background: #fff;
}

.ic-head {
  display: flex;
  gap: 8px;
  align-items: center;
  padding: 8px 12px;
  border-bottom: 1px solid #f0f1f3;
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
