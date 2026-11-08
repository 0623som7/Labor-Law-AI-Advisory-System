// 状态行文案的边界全部钉死：0–2s / 2–8s / 8–20s / 20s 起报秒数。
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { waitingStage } from './waiting.ts'

test('waitingStage 四档文案的边界', () => {
  assert.deepEqual(waitingStage(0, 3), { text: '正在检索相关法条…', showSeconds: false })
  assert.deepEqual(waitingStage(1999, 3), { text: '正在检索相关法条…', showSeconds: false })
  assert.deepEqual(waitingStage(2000, 3), { text: '已找到 3 条相关法条，正在分析', showSeconds: false })
  assert.deepEqual(waitingStage(7999, 3), { text: '已找到 3 条相关法条，正在分析', showSeconds: false })
  assert.deepEqual(waitingStage(8000, 3), { text: '正在梳理法律关系与适用条款…', showSeconds: false })
  assert.deepEqual(waitingStage(19999, 3), { text: '正在梳理法律关系与适用条款…', showSeconds: false })
  assert.deepEqual(waitingStage(20000, 3), {
    text: '正在组织回答，请稍候（已用时 20s）',
    showSeconds: true,
  })
})

test('waitingStage 20 秒之后秒数按整秒走', () => {
  assert.equal(waitingStage(20000, 3).text, '正在组织回答，请稍候（已用时 20s）')
  assert.equal(waitingStage(20999, 3).text, '正在组织回答，请稍候（已用时 20s）')
  assert.equal(waitingStage(21000, 3).text, '正在组织回答，请稍候（已用时 21s）')
  assert.equal(waitingStage(35400, 3).text, '正在组织回答，请稍候（已用时 35s）')
})

test('waitingStage citation 为 0 时不说"已找到 0 条"', () => {
  assert.equal(waitingStage(2000, 0).text, '未检索到直接相关法条，正在分析')
  assert.equal(waitingStage(7999, 0).text, '未检索到直接相关法条，正在分析')
  // 8 秒之后与 citation 无关，同一句。
  assert.equal(waitingStage(8000, 0).text, '正在梳理法律关系与适用条款…')
})

test('waitingStage 检索已完成时不再说"正在检索"', () => {
  // tool_call 的 ok 在 0.03 秒就到了，之后必须跟真实事件对齐。
  assert.equal(waitingStage(300, 3, { retrievalDone: true }).text, '已找到 3 条相关法条，正在分析')
  assert.equal(
    waitingStage(300, 0, { retrievalDone: true }).text,
    '未检索到直接相关法条，正在分析',
  )
  // 没收到 tool_call 时仍然是检索文案。
  assert.equal(waitingStage(300, 0, { retrievalDone: false }).text, '正在检索相关法条…')
})

test('waitingStage 对脏输入不抛异常', () => {
  assert.deepEqual(waitingStage(-100, 3), { text: '正在检索相关法条…', showSeconds: false })
  assert.equal(waitingStage(Number.NaN, 3).text, '正在检索相关法条…')
  assert.equal(waitingStage(3000, Number.NaN).text, '未检索到直接相关法条，正在分析')
  assert.equal(waitingStage(3000, -2).text, '未检索到直接相关法条，正在分析')
  assert.equal(waitingStage(3000, 2.7).text, '已找到 2 条相关法条，正在分析')
})
