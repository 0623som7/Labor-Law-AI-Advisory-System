// 4 档的分界线必须逐个卡一遍：边界写 <  还是 <= ，肉眼看不出来，
// 但 1500ms 那一刻该换档的事，用户是能感觉到的。
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { reviewProgressLine, reviewStage, REVIEW_STAGE_MS } from './review-progress.ts'

test('第一档：0 到 1499ms 说在解析文字层', () => {
  assert.equal(reviewStage(0), '正在解析文字层…')
  assert.equal(reviewStage(1), '正在解析文字层…')
  assert.equal(reviewStage(REVIEW_STAGE_MS.parse - 1), '正在解析文字层…')
})

test('第二档：1500 到 3999ms 说要比对 8 类风险', () => {
  assert.equal(reviewStage(REVIEW_STAGE_MS.parse), '正在比对试用期、社保、押金、最低工资等 8 类风险…')
  assert.equal(reviewStage(REVIEW_STAGE_MS.rules - 1), '正在比对试用期、社保、押金、最低工资等 8 类风险…')
})

test('第三档：4000 到 8999ms 说正在逐条定位原文位置', () => {
  assert.equal(reviewStage(REVIEW_STAGE_MS.rules), '正在逐条定位原文位置…')
  assert.equal(reviewStage(REVIEW_STAGE_MS.locate - 1), '正在逐条定位原文位置…')
})

test('第四档：9000ms 起承认这一步慢，但仍在处理', () => {
  assert.equal(reviewStage(REVIEW_STAGE_MS.locate), '这一步比平时慢，仍在处理，请不要关闭页面…')
  assert.equal(reviewStage(60_000), '这一步比平时慢，仍在处理，请不要关闭页面…')
})

test('负数与 NaN 按第一档说，不吐空白也不吐 NaN', () => {
  assert.equal(reviewStage(-1), '正在解析文字层…')
  assert.equal(reviewStage(Number.NaN), '正在解析文字层…')
})

test('审查一结束整行就不可见，不留残影', () => {
  assert.deepEqual(reviewProgressLine({ elapsedMs: 0, running: false }), { visible: false, text: '' })
  assert.deepEqual(reviewProgressLine({ elapsedMs: 30_000, running: false }), { visible: false, text: '' })
  assert.deepEqual(reviewProgressLine({ elapsedMs: 2000, running: true }), {
    visible: true,
    text: '正在比对试用期、社保、押金、最低工资等 8 类风险…',
  })
})
