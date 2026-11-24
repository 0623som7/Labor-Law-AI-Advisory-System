// 审查页内联对话的状态归约。
//
// 为什么不复用 ChatView 的状态机：那套是「会话列表 + 消息流 + 进度行 + 重试」
// 四件事绑在一起的，抽出来要新开一堆分裂点；抽屉只要「一问一答 + 引用」。
// 所以这里只留最小的五个操作，全部是纯函数——可以 node --test 直接跑，
// 不需要挂载组件、不需要假 SSE。
//
// 三条硬规矩（都有单测）：
//   1. 索引越界原样返回：帧是网络来的，乱序/重复都可能，宁可丢一帧也不能抛；
//   2. 引用按 statute_id 去重：重试与重放会让同一张卡片到两次；
//   3. session_id 只认第一次：meta 之后如果还改，下一轮就发到另一个会话去了。

import type { Citation } from '@/utils/sse'

export interface InlineTurn {
  role: 'user' | 'assistant'
  content: string
  citations: Citation[]
}

export interface InlineState {
  sessionId: string
  turns: InlineTurn[]
  sending: boolean
  error: string
}

export function initialInlineState(): InlineState {
  return { sessionId: '', turns: [], sending: false, error: '' }
}

/** 追加一条用户提问 + 一条空的 assistant 占位，返回新 state（不可变）。 */
export function beginTurn(s: InlineState, text: string): { state: InlineState; index: number } {
  const turns = [
    ...s.turns,
    { role: 'user' as const, content: text, citations: [] },
    { role: 'assistant' as const, content: '', citations: [] },
  ]
  // index 指向 assistant 那一条：后续 delta / citation 都往它身上写。
  return {
    state: { ...s, turns, sending: true, error: '' },
    index: turns.length - 1,
  }
}

export function appendDelta(s: InlineState, index: number, text: string): InlineState {
  const turn = s.turns[index]
  if (!turn) return s
  const turns = s.turns.slice()
  turns[index] = { ...turn, content: turn.content + text }
  return { ...s, turns }
}

export function appendCitation(s: InlineState, index: number, citation: Citation): InlineState {
  const turn = s.turns[index]
  if (!turn) return s
  if (turn.citations.some((c) => c.statute_id === citation.statute_id)) return s
  const turns = s.turns.slice()
  turns[index] = { ...turn, citations: [...turn.citations, citation] }
  return { ...s, turns }
}

export function finishTurn(s: InlineState, error: string): InlineState {
  return { ...s, sending: false, error }
}

export function setSession(s: InlineState, sessionId: string): InlineState {
  if (s.sessionId !== '' || sessionId === '') return s
  return { ...s, sessionId }
}

/**
 * 最后一条用户提问。
 *
 * 「在对话页继续」要把人带过去时不能空手走：抽屉里问过的话在下一层
 * 得能接着问，否则等于让他把问题重打一遍。
 */
export function lastUserTextOf(s: InlineState): string {
  for (let i = s.turns.length - 1; i >= 0; i -= 1) {
    const turn = s.turns[i]
    if (turn.role === 'user') return turn.content
  }
  return ''
}

/**
 * 排队位能不能出队。
 *
 * 回答还在流时点了另一张卡：那一问不能丢，也不能并发第二条模型调用，
 * 只能先存着（只存最后一个，连点三次也只答最后一句）。流结束后再问这里
 * 「现在能发吗、发哪一句」。拆成纯函数是为了让"什么情况下出队"能被单测，
 * 而不是埋在 watch 回调里只能靠手点。
 */
export function takePending(pending: string, sending: boolean): string {
  if (sending || pending === '') return ''
  return pending
}

/**
 * 生成期间输入框的锁形态。
 *
 * 为什么不是 disabled：disabled 的元素不可聚焦，浏览器会把焦点抛回 body，
 * 于是 Esc 的 keydown 发不到抽屉里，用户只能去点右上角的 ×（D5b 的现场）。
 * readonly 保留焦点与键盘事件，字符进不去、Esc 还能到。
 */
export interface InputLock {
  readonly: boolean
  disabled: boolean
  ariaBusy: 'true' | 'false'
}

export function inputLock(busy: boolean): InputLock {
  return { readonly: busy, disabled: false, ariaBusy: busy ? 'true' : 'false' }
}

/**
 * 流结束后要不要把焦点还给输入框。
 *
 * 只在「刚刚还在生成、现在结束、焦点已经掉到 body」时抢回来；用户在别处
 * 打字（比如搜合同）时不抢。抽成纯函数是为了能单测这条判定。
 */
export function shouldRestoreFocus(prevSending: boolean, sending: boolean, focusOnBody: boolean): boolean {
  return prevSending && !sending && focusOnBody
}

/** 焦点归还：拿到元素才动，返回是否真的聚焦了。 */
export function focusField(el: { focus: () => void } | null | undefined, when: boolean): boolean {
  if (!when || !el) return false
  el.focus()
  return true
}
