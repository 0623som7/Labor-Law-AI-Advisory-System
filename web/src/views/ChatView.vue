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

import { computed, nextTick, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'

import { ApiError } from '@/api/client'
import { streamChat } from '@/api/chat'
import * as sessionApi from '@/api/session'
import type { Citation, ToolStatus } from '@/api/types'
import type { Session } from '@/api/types'
import CitationCard from '@/components/CitationCard.vue'
import ToolStatusBar from '@/components/ToolStatusBar.vue'
import { useAuthStore } from '@/stores/auth'

/** 页面上的一条消息。历史消息与流式消息共用这个形状，差别只在有没有引用。 */
interface UiMessage {
  key: string
  role: 'user' | 'assistant'
  content: string
  /** 只有本轮实时生成的消息会有；历史消息恒为空数组 */
  citations: Citation[]
  /** 占位气泡状态：还没收到第一个 delta */
  thinking: boolean
  toolStatus: ToolStatus | null
  toolDetail: string
  /** 首个 delta 到达后状态条淡出 */
  barFaded: boolean
  /** 后端 error 帧或 HTTP 失败 */
  error: string
  interrupted: boolean
  fromHistory: boolean
}

const router = useRouter()
const auth = useAuthStore()

const sessions = ref<Session[]>([])
const currentSessionId = ref('')
const messages = ref<UiMessage[]>([])
const input = ref('')
const sending = ref(false)
const collapsed = ref(false)
const listLoading = ref(false)
const historyLoading = ref(false)
const banner = ref('')

const scroller = ref<HTMLElement | null>(null)
let controller: AbortController | null = null
let keySeed = 0

const currentTitle = computed(() => {
  if (currentSessionId.value === '') return '新对话'
  const hit = sessions.value.find((s) => s.id === currentSessionId.value)
  return hit?.title || '对话'
})

const canSend = computed(() => !sending.value && input.value.trim() !== '')

function nextKey(prefix: string): string {
  keySeed += 1
  return `${prefix}-${keySeed}`
}

function newMessage(role: 'user' | 'assistant', content: string, fromHistory = false): UiMessage {
  return {
    key: nextKey(role),
    role,
    content,
    citations: [],
    thinking: false,
    toolStatus: null,
    toolDetail: '',
    barFaded: false,
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

async function openSession(session: Session): Promise<void> {
  if (sending.value) return
  currentSessionId.value = session.id
  messages.value = []
  historyLoading.value = true
  banner.value = ''
  try {
    const items = await sessionApi.listMessages(session.id)
    // 历史消息不带 citations：库里没有存引用，硬补就是编造。
    messages.value = items.map((m) => newMessage(m.role, m.content, true))
    await scrollToBottom()
  } catch (error) {
    banner.value = error instanceof Error ? error.message : '历史消息加载失败'
  } finally {
    historyLoading.value = false
  }
}

function startNewSession(): void {
  if (sending.value) return
  currentSessionId.value = ''
  messages.value = []
  banner.value = ''
  input.value = ''
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
  const text = input.value.trim()
  if (text === '' || sending.value) return

  banner.value = ''
  input.value = ''
  const startedNew = currentSessionId.value === ''

  messages.value.push(newMessage('user', text))
  const assistant = reactive(newMessage('assistant', '')) as UiMessage
  assistant.thinking = true
  messages.value.push(assistant)
  await scrollToBottom()

  sending.value = true
  controller = new AbortController()

  try {
    const result = await streamChat(
      {
        input: text,
        ...(currentSessionId.value === '' ? {} : { session_id: currentSessionId.value }),
      },
      {
        onMeta: (event) => {
          // 新会话的 ID 只从这里拿：服务端在建会话时生成，
          // 没有它下一轮就会又新建一个会话。
          currentSessionId.value = event.session_id
          if (startedNew) void refreshSessions()
        },
        onDelta: (event) => {
          assistant.thinking = false
          assistant.barFaded = true
          assistant.content += event.text
          void scrollToBottom()
        },
        onCitation: (event) => addCitation(assistant, event.citation),
        onToolCall: (event) => {
          assistant.toolStatus = event.tool.status
          assistant.toolDetail = event.tool.detail
        },
        onDone: () => {
          assistant.thinking = false
        },
      },
      controller.signal,
    )

    assistant.thinking = false
    if (result.aborted) {
      assistant.interrupted = true
      if (assistant.content === '') assistant.content = '（已中断）'
    } else if (result.done) {
      // 会话标题与更新时间在服务端落库后才变，这里顺带刷一次左侧列表。
      void refreshSessions()
    }
  } catch (error) {
    assistant.thinking = false
    if (error instanceof ApiError && error.code === 40101) {
      // 拦截器已经把令牌清掉并跳登录页，这里不再往气泡上写错误。
      return
    }
    assistant.error = error instanceof Error ? error.message : '发送失败'
    if (assistant.content === '') assistant.content = '（没有收到回答）'
    assistant.barFaded = true
  } finally {
    assistant.thinking = false
    sending.value = false
    controller = null
    await scrollToBottom()
  }
}

function stop(): void {
  controller?.abort()
}

function onComposerKeydown(evt: Event | KeyboardEvent): void {
  // ElInput 把原生 keydown 透出来，类型上是 Event；不判一下拿不到 ctrlKey。
  if (!(evt instanceof KeyboardEvent)) return
  // Ctrl+Enter 与 Cmd+Enter 都发：Mac 上没有 Ctrl 的习惯。
  if (evt.key === 'Enter' && (evt.ctrlKey || evt.metaKey)) {
    evt.preventDefault()
    void send()
  }
}

async function signOut(): Promise<void> {
  await auth.logout()
  await router.replace({ name: 'login' })
}

onMounted(async () => {
  await auth.loadMe()
  await refreshSessions()
})
</script>

<template>
  <div class="chat-page">
    <aside class="sidebar" :class="{ collapsed }">
      <div class="sidebar-head">
        <el-button type="primary" class="new-btn" :disabled="sending" @click="startNewSession">
          ＋ 新对话
        </el-button>
        <button
          type="button"
          class="collapse-btn"
          :title="collapsed ? '展开会话列表' : '收起会话列表'"
          @click="collapsed = !collapsed"
        >
          {{ collapsed ? '»' : '«' }}
        </button>
      </div>

      <div v-if="!collapsed" class="sidebar-body">
        <p v-if="listLoading && sessions.length === 0" class="sidebar-hint">加载中…</p>
        <p v-else-if="sessions.length === 0" class="sidebar-hint">还没有会话，发第一句就会出现在这里。</p>
        <ul v-else class="session-list">
          <li
            v-for="s in sessions"
            :key="s.id"
            class="session-item"
            :class="{ active: s.id === currentSessionId }"
          >
            <button type="button" class="session-title" :disabled="sending" @click="openSession(s)">
              {{ s.title }}
            </button>
            <button
              type="button"
              class="session-del"
              title="删除会话"
              :disabled="sending"
              @click="removeSession(s)"
            >
              ×
            </button>
          </li>
        </ul>
      </div>

      <div v-if="!collapsed" class="sidebar-foot">
        <span class="me">{{ auth.user?.nickname || auth.user?.phone_masked || '已登录' }}</span>
        <button type="button" class="logout" @click="signOut">退出</button>
      </div>
    </aside>

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
          <div class="bubble" :class="m.role === 'user' ? 'bubble-user' : 'bubble-assistant'">
            <template v-if="m.thinking">
              <span class="thinking">正在思考…</span>
            </template>
            <p v-else class="content ll-pre-wrap">{{ m.content }}</p>

            <p v-if="m.interrupted" class="meta-line">已中断</p>
            <p v-if="m.error" class="error-line">{{ m.error }}</p>

            <div
              v-if="!m.fromHistory && (m.thinking || m.toolStatus !== null)"
              class="tool-row"
              :class="{ faded: m.barFaded }"
            >
              <ToolStatusBar :status="m.toolStatus" :detail="m.toolDetail" :faded="m.barFaded" />
            </div>

            <div v-if="m.citations.length > 0" class="citations">
              <CitationCard v-for="c in m.citations" :key="c.statute_id" :citation="c" />
            </div>
          </div>
        </div>
      </div>

      <footer class="composer">
        <el-input
          v-model="input"
          type="textarea"
          :rows="3"
          resize="none"
          maxlength="4000"
          show-word-limit
          placeholder="描述你的情况，Ctrl+Enter 发送"
          @keydown="onComposerKeydown"
        />
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
  width: 280px;
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
  padding: 6px;
  overflow: hidden;
  font-size: 13px;
  color: var(--ll-text);
  text-align: left;
  text-overflow: ellipsis;
  white-space: nowrap;
  cursor: pointer;
  background: transparent;
  border: 0;
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

.content {
  margin: 0;
  font-size: 14px;
}

.thinking {
  font-size: 13px;
  color: var(--ll-text-weak);
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

.tool-row {
  max-height: 40px;
  margin-top: 8px;
  overflow: hidden;
  /* 首个 delta 到达后淡出并收起高度：直接 v-if 抽掉会有一次跳动，
     而"正在检索法条…"这句话应该在正文出现时安静退出，不是突然消失。 */
  transition:
    opacity 0.3s ease,
    max-height 0.3s ease,
    margin-top 0.3s ease;
}

.tool-row.faded {
  max-height: 0;
  margin-top: 0;
  opacity: 0;
}

.citations {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 10px;
}

.composer {
  flex: none;
  padding: 10px 16px 14px;
  background: var(--ll-surface);
  border-top: 1px solid var(--ll-border);
}

.composer-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 8px;
}
</style>
