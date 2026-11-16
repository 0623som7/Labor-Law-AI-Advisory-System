import { test } from 'node:test'
import assert from 'node:assert/strict'

import { pickExistingReview, shouldAutostart, shouldOfferStart } from '../utils/review-restore.ts'
import type { Review } from '../api/types.ts'

const review: Review = {
  id: 'R-1',
  file_id: 'F-1',
  status: 'succeeded',
  risk_level: 'high',
  score: 65,
  clause_pass_rate: 1,
  counts: { high: 5, medium: 1, low: 0 },
  task_id: 'T-1',
  created_at: '2026-11-08T10:00:00Z',
  updated_at: '2026-11-08T10:00:20Z',
}

test('pickExistingReview 空列表返回 null，有记录返回第一条', () => {
  assert.equal(pickExistingReview([]), null)
  assert.equal(pickExistingReview(null), null)
  assert.equal(pickExistingReview(undefined), null)
  assert.equal(pickExistingReview([review])?.id, 'R-1')
})

test('已有审查结果时不再渲染「开始审查」按钮', () => {
  assert.equal(shouldOfferStart({ restored: review, running: false, scanned: false, loadError: '' }), false)
})

test('还没审过时给按钮', () => {
  assert.equal(shouldOfferStart({ restored: null, running: false, scanned: false, loadError: '' }), true)
})

test('运行中、扫描件、加载失败都不给按钮', () => {
  assert.equal(shouldOfferStart({ restored: null, running: true, scanned: false, loadError: '' }), false)
  assert.equal(shouldOfferStart({ restored: null, running: false, scanned: true, loadError: '' }), false)
  assert.equal(shouldOfferStart({ restored: null, running: false, scanned: false, loadError: '解析失败' }), false)
})

test('带 autostart 标记且没有结果时自动开审', () => {
  assert.equal(shouldAutostart({ autostart: '1' }, false), true)
})

test('带 autostart 标记但已有结果时不开审（后端会复用旧结果，白等两秒）', () => {
  assert.equal(shouldAutostart({ autostart: '1' }, true), false)
})

test('没有 query、空 query、标记值不对都不开审', () => {
  assert.equal(shouldAutostart(null, false), false)
  assert.equal(shouldAutostart({}, false), false)
  assert.equal(shouldAutostart({ autostart: '0' }, false), false)
  assert.equal(shouldAutostart({ autostart: '' }, false), false)
})

test('query 传字符串（整条查询串）也能认出来', () => {
  assert.equal(shouldAutostart('?autostart=1', false), true)
  assert.equal(shouldAutostart('autostart=1', false), true)
  assert.equal(shouldAutostart('?file=abc', false), false)
})

test('query 值是数组时任一为 1 即可', () => {
  assert.equal(shouldAutostart({ autostart: ['1', '0'] }, false), true)
  assert.equal(shouldAutostart({ autostart: ['0'] }, false), false)
})
