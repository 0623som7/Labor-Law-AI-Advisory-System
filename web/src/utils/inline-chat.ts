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
