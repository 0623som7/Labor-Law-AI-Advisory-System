import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  appendCitation,
  appendDelta,
  beginTurn,
  finishTurn,
  initialInlineState,
  setSession,
} from './inline-chat.ts'
import type { Citation } from '@/utils/sse'

function citation(statuteId: string): Citation {
  return {
    statute_id: statuteId,
    law: '中华人民共和国劳动合同法',
    article: '第四十七条',
    text: '经济补偿按劳动者在本单位工作的年限…',
  }
}

test('初始状态是空的、不在发送中', () => {
  const s = initialInlineState()
  assert.equal(s.sessionId, '')
  assert.deepEqual(s.turns, [])
  assert.equal(s.sending, false)
  assert.equal(s.error, '')
})

test('beginTurn 追加用户提问与空的 assistant 占位，index 指向占位', () => {
  const { state, index } = beginTurn(initialInlineState(), '这条合法吗？')
  assert.equal(state.turns.length, 2)
  assert.equal(state.turns[0].role, 'user')
  assert.equal(state.turns[0].content, '这条合法吗？')
  assert.equal(state.turns[1].role, 'assistant')
  assert.equal(state.turns[1].content, '')
  assert.equal(index, 1)
  assert.equal(state.sending, true)
})

test('beginTurn 不改入参', () => {
  const before = initialInlineState()
  beginTurn(before, 'x')
  assert.deepEqual(before.turns, [])
  assert.equal(before.sending, false)
})

test('appendDelta 累加到指定轮次', () => {
  const { state, index } = beginTurn(initialInlineState(), 'q')
  const s1 = appendDelta(state, index, '第一段')
  const s2 = appendDelta(s1, index, '第二段')
  assert.equal(s2.turns[index].content, '第一段第二段')
  // 入参不被改
  assert.equal(state.turns[index].content, '')
})

test('appendDelta 越界索引原样返回，不抛不写越', () => {
  const { state, index } = beginTurn(initialInlineState(), 'q')
  assert.equal(appendDelta(state, 99, 'x'), state)
  assert.equal(appendDelta(state, -1, 'x'), state)
  assert.equal(state.turns[index].content, '')
})

test('appendCitation 同一条按 statute_id 去重', () => {
  const { state, index } = beginTurn(initialInlineState(), 'q')
  const s1 = appendCitation(state, index, citation('LCL-47'))
  const s2 = appendCitation(s1, index, citation('LCL-47'))
  assert.equal(s2.turns[index].citations.length, 1)
  // 去重命中时直接返回同一个对象，调用方不必重渲染
  assert.equal(s2, s1)
})

test('appendCitation 不同条号各留一张，且不改入参', () => {
  const { state, index } = beginTurn(initialInlineState(), 'q')
  const s1 = appendCitation(state, index, citation('LCL-47'))
  const s2 = appendCitation(s1, index, citation('LCL-87'))
  assert.deepEqual(
    s2.turns[index].citations.map((c) => c.statute_id),
    ['LCL-47', 'LCL-87'],
  )
  assert.equal(state.turns[index].citations.length, 0)
})

test('appendCitation 越界索引原样返回', () => {
  const { state } = beginTurn(initialInlineState(), 'q')
  assert.equal(appendCitation(state, 42, citation('LCL-47')), state)
})

test('finishTurn 收尾：sending 落回 false，error 原样带上', () => {
  const { state } = beginTurn(initialInlineState(), 'q')
  const ok = finishTurn(state, '')
  assert.equal(ok.sending, false)
  assert.equal(ok.error, '')

  const bad = finishTurn(state, '检索服务暂时不可用，请稍后重试')
  assert.equal(bad.sending, false)
  assert.equal(bad.error, '检索服务暂时不可用，请稍后重试')
})

test('setSession 只认第一次，已有值时不覆盖', () => {
  const s0 = initialInlineState()
  const s1 = setSession(s0, 'sess-1')
  assert.equal(s1.sessionId, 'sess-1')
  // 后续帧（或另一条流）不能把会话换掉，否则下一轮发到别的会话去
  assert.equal(setSession(s1, 'sess-2'), s1)
  // 空值也不写
  assert.equal(setSession(s0, ''), s0)
})

test('setSession 不改入参', () => {
  const s0 = initialInlineState()
  setSession(s0, 'sess-1')
  assert.equal(s0.sessionId, '')
})
