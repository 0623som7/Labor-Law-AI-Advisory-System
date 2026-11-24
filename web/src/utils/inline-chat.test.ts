import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'

import {
  appendCitation,
  appendDelta,
  beginTurn,
  finishTurn,
  focusField,
  initialInlineState,
  inputLock,
  lastUserTextOf,
  setSession,
  shouldRestoreFocus,
  takePending,
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

test('lastUserTextOf 取最后一条用户提问', () => {
  const first = beginTurn(initialInlineState(), '第一问')
  const second = beginTurn(first.state, '第二问')
  assert.equal(lastUserTextOf(second.state), '第二问')
})

test('lastUserTextOf 只有 assistant 时返回空串', () => {
  const { state, index } = beginTurn(initialInlineState(), '问')
  // 把用户那条改掉：只留 assistant 的情形（理论上不该出现，但不能抛）
  const onlyAssistant = {
    ...state,
    turns: state.turns.map((t, i) => (i === 0 ? { ...t, role: 'assistant' as const } : t)),
  }
  assert.equal(onlyAssistant.turns[index].role, 'assistant')
  assert.equal(lastUserTextOf(onlyAssistant), '')
})

test('lastUserTextOf 空 state 返回空串', () => {
  assert.equal(lastUserTextOf(initialInlineState()), '')
})

test('takePending 正在回答时不出队', () => {
  assert.equal(takePending('问题A', true), '')
})

test('takePending 空闲时出队', () => {
  assert.equal(takePending('问题A', false), '问题A')
})

test('takePending 队列为空返回空串', () => {
  assert.equal(takePending('', false), '')
})

test('inputLock 生成期间是 readonly 而不是 disabled', () => {
  const busy = inputLock(true)
  assert.equal(busy.readonly, true)
  assert.equal(busy.disabled, false)
  assert.equal(busy.ariaBusy, 'true')

  const idle = inputLock(false)
  assert.equal(idle.readonly, false)
  assert.equal(idle.disabled, false)
  assert.equal(idle.ariaBusy, 'false')
})

test('InlineChat 的输入框按 lock 绑定 readonly / aria-busy，抽屉根元素接容器级 Esc', () => {
  // 不挂组件的渲染断言：直接读 SFC 模板，锁的形态写在模板上才算数，
  // 只测纯函数而模板仍然 :disabled="busy" 时这条会红。
  const sfc = readFileSync(new URL('../components/InlineChat.vue', import.meta.url), 'utf8')
  const ta = sfc.slice(sfc.indexOf('<textarea'), sfc.indexOf('</textarea>'))
  assert.match(ta, /:readonly="lock\.readonly"/)
  assert.match(ta, /:aria-busy="lock\.ariaBusy"/)
  assert.doesNotMatch(ta, /:disabled="busy"/)

  const root = sfc.slice(sfc.indexOf('<section'), sfc.indexOf('</section>'))
  assert.match(root, /@keydown="onPanelKey"/)
})

test('流结束时 only 在焦点掉到 body 时才归还焦点，且 takePending 复位不抛', () => {
  const started = beginTurn(initialInlineState(), '问一句')
  assert.equal(started.state.sending, true)

  // 生成中：不出队、不抢焦点
  assert.equal(takePending('排队的一问', true), '')
  assert.equal(shouldRestoreFocus(false, true, true), false)

  const done = finishTurn(started.state, '')
  assert.equal(done.sending, false)
  assert.equal(takePending('排队的一问', done.sending), '排队的一问')

  let focused = 0
  const el = { focus: () => { focused += 1 } }
  assert.equal(focusField(el, shouldRestoreFocus(true, false, true)), true)
  assert.equal(focused, 1)

  // 焦点在别处（用户自己点的）不抢
  assert.equal(focusField(el, shouldRestoreFocus(true, false, false)), false)
  assert.equal(focused, 1)
  // 没有元素时不炸
  assert.equal(focusField(null, true), false)
})
