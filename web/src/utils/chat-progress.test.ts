// 用一串假的 SSE 事件把等待态走一遍：这正是任务里要求的"组件测试"——
// 项目没有 @vue/test-utils，但组件显示什么完全由 statusLine 决定，
// 所以把事件序列喂给状态机就能断言"第一个 delta 之后状态行消失"。
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { applyProgress, startProgress, statusLine, type ProgressEvent } from './chat-progress.ts'

const T0 = 1_700_000_000_000

function feed(events: ProgressEvent[], at: number[]) {
  let state = startProgress(T0)
  events.forEach((event, i) => {
    state = applyProgress(state, event, T0 + at[i])
  })
  return state
}

test('真实事件序列：检索完成 → 3 条 citation → 状态行按事件变文案', () => {
  const state = feed(
    [
      { type: 'meta' },
      { type: 'tool_call', status: 'running', detail: '检索相关法条' },
      { type: 'tool_call', status: 'ok', detail: '命中 3 条法条' },
      { type: 'citation' },
      { type: 'citation' },
      { type: 'citation' },
    ],
    [30, 30, 40, 45, 46, 47],
  )

  assert.equal(state.citations, 3)
  assert.equal(state.retrieval, 'ok')
  // 0.3 秒：tool_call 已 ok，不能还写"正在检索"。
  assert.deepEqual(statusLine(state, T0 + 300), {
    visible: true,
    text: '已找到 3 条相关法条，正在分析',
    showSeconds: false,
  })
  // 10 秒：进入梳理阶段。
  assert.equal(statusLine(state, T0 + 10_000).text, '正在梳理法律关系与适用条款…')
  // 26 秒：开始报秒数，且数字是真实的等待时长。
  const late = statusLine(state, T0 + 26_400)
  assert.equal(late.text, '正在组织回答，请稍候（已用时 26s）')
  assert.equal(late.showSeconds, true)
})

test('第一个 delta 到达后状态行整行消失，不再回来', () => {
  const state = feed(
    [
      { type: 'tool_call', status: 'ok', detail: '命中 3 条法条' },
      { type: 'citation' },
      { type: 'delta' },
      { type: 'citation' },
      { type: 'delta' },
    ],
    [30, 40, 31_000, 31_100, 31_200],
  )

  assert.equal(state.phase, 'streaming')
  assert.equal(state.firstDeltaAt, T0 + 31_000)
  // 正文已经在写了，哪怕又来了 citation，状态行也不能复活。
  assert.equal(statusLine(state, T0 + 31_500).visible, false)
  assert.equal(statusLine(state, T0 + 40_000).visible, false)
})

test('空 citation 时状态行不编造"已找到 0 条"', () => {
  const state = feed(
    [
      { type: 'tool_call', status: 'ok', detail: '命中 0 条法条' },
    ],
    [40],
  )
  assert.equal(statusLine(state, T0 + 3000).text, '未检索到直接相关法条，正在分析')
})

test('done 之后状态行关闭（even if never saw delta）', () => {
  const state = feed([{ type: 'tool_call', status: 'ok', detail: '命中 3 条法条' }, { type: 'done' }], [30, 5000])
  assert.equal(state.phase, 'done')
  assert.equal(statusLine(state, T0 + 6000).visible, false)
})

test('中断时状态行关闭，气泡上的「已中断」由组件自己渲染', () => {
  const state = feed([{ type: 'stopped' }], [1200])
  assert.equal(state.phase, 'stopped')
  assert.equal(statusLine(state, T0 + 1300).visible, false)
})

test('ABORT 之前收到过 delta 就不会退回等待态', () => {
  const state = feed([{ type: 'delta' }, { type: 'stopped' }], [1000, 2000])
  assert.equal(state.phase, 'stopped')
  assert.equal(statusLine(state, T0 + 2100).visible, false)
})

test('只有 running 时仍算"正在检索"，过了 2 秒窗口才让位', () => {
  // 网络抖动时 citation 可能比 tool_call 晚到：running ≠ 检索完成。
  const state = feed([{ type: 'tool_call', status: 'running', detail: '检索相关法条' }], [30])

  assert.equal(state.citations, 0)
  assert.equal(state.retrieval, 'running')
  assert.equal(statusLine(state, T0 + 500).text, '正在检索相关法条…')
  // 2 秒窗口过了、citation 还是 0，才可以说"未检索到"。
  assert.equal(statusLine(state, T0 + 3000).text, '未检索到直接相关法条，正在分析')
})
