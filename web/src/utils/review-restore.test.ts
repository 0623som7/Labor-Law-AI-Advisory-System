import { test } from 'node:test'
import assert from 'node:assert/strict'

import { pickExistingReview, shouldOfferStart } from '../utils/review-restore.ts'
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
