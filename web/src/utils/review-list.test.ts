// 审查历史列表的三个纯函数。
//
// 渲染本身（两行记录渲染两行、空态）靠手动验收 + 类型检查兜住：
// 项目没有 @vue/test-utils，为它引一套组件测试基建属于"为测试而加依赖"。

import { test } from 'node:test'
import assert from 'node:assert/strict'

import { filterReviews, hasMore, hasPending, isClickable, riskLabel, statusLabel } from './review-list.ts'

test('statusLabel 覆盖五个后端状态', () => {
  assert.equal(statusLabel('pending'), '排队中')
  assert.equal(statusLabel('running'), '审查中')
  assert.equal(statusLabel('succeeded'), '已完成')
  assert.equal(statusLabel('failed'), '失败')
  assert.equal(statusLabel('cancelled'), '已取消')
})

test('statusLabel 遇到未知状态原样返回，不抛错', () => {
  assert.equal(statusLabel('weird'), 'weird')
  assert.equal(statusLabel(''), '')
})

test('riskLabel 覆盖三档，空串算未评级', () => {
  assert.equal(riskLabel('high'), '高风险')
  assert.equal(riskLabel('medium'), '中风险')
  assert.equal(riskLabel('low'), '低风险')
  assert.equal(riskLabel(''), '未评级')
})

test('riskLabel 遇到未知等级原样返回', () => {
  assert.equal(riskLabel('critical'), 'critical')
})

test('hasMore 只在装满一页时给 true', () => {
  assert.equal(hasMore(20, 20), true)
  assert.equal(hasMore(0, 20), false)
  assert.equal(hasMore(19, 20), false)
})

test('hasMore 对非法 limit 一律 false，避免无限翻页', () => {
  assert.equal(hasMore(20, 0), false)
  assert.equal(hasMore(20, -5), false)
})

test('失败的记录仍然可点：进页面会拿到「开始审查」按钮', () => {
  // 重启中断的审查会被启动对账标成 failed，审查历史是这些合同的唯一入口
  // （上传页不列历史文件），所以这里必须是 true。
  assert.equal(isClickable('failed'), true)
})

test('取消的记录同样可点', () => {
  assert.equal(isClickable('cancelled'), true)
})

test('成功与进行中的记录可点', () => {
  assert.equal(isClickable('succeeded'), true)
  assert.equal(isClickable('running'), true)
  assert.equal(isClickable('pending'), true)
})

test('hasPending 只把排队中与审查中算作未完成', () => {
  assert.equal(hasPending([{ status: 'pending' }]), true)
  assert.equal(hasPending([{ status: 'running' }]), true)
  assert.equal(hasPending([{ status: 'succeeded' }, { status: 'running' }]), true)
})

test('hasPending 对终态与空数组都是 false', () => {
  assert.equal(hasPending([{ status: 'succeeded' }]), false)
  assert.equal(hasPending([{ status: 'failed' }]), false)
  assert.equal(hasPending([{ status: 'cancelled' }]), false)
  assert.equal(hasPending([]), false)
})

const ROWS = [
  { file_id: '01M7AAAAAAAAAAAAAAAAAAAAAA', filename: '劳动合同A.pdf' },
  { file_id: '01M7BBBBBBBBBBBBBBBBBBBB9Q', filename: 'Offer-Letter.pdf' },
  { file_id: '01M7CCCCCCCCCCCCCCCCCCCC77', filename: '' },
]

test('filterReviews 关键字为空时返回同一个数组引用', () => {
  // 引用相等而不是内容相等：列表每次按键都重建的话，滚动位置和 DOM 都会被无谓地丢掉。
  assert.equal(filterReviews(ROWS, ''), ROWS)
  assert.equal(filterReviews(ROWS, '   '), ROWS)
})

test('filterReviews 命中文件名片段', () => {
  assert.deepEqual(
    filterReviews(ROWS, '合同A').map((r) => r.file_id),
    ['01M7AAAAAAAAAAAAAAAAAAAAAA'],
  )
})

test('filterReviews 命中 ULID 尾号', () => {
  assert.deepEqual(
    filterReviews(ROWS, '9q').map((r) => r.file_id),
    ['01M7BBBBBBBBBBBBBBBBBBBB9Q'],
  )
})

test('filterReviews 大小写不敏感', () => {
  assert.equal(filterReviews(ROWS, 'offer-letter').length, 1)
  assert.equal(filterReviews(ROWS, 'OFFER-LETTER').length, 1)
})

test('filterReviews 无匹配时返回空数组而不是 undefined', () => {
  const out = filterReviews(ROWS, '不存在的名字')
  assert.ok(Array.isArray(out))
  assert.equal(out.length, 0)
})

test('filterReviews 文件名缺失的记录不会崩，只按 ULID 尾号命中', () => {
  const rows = [{ file_id: '01M7DDDDDDDDDDDDDDDDDDDDXY' }]
  assert.equal(filterReviews(rows, 'xy').length, 1)
  assert.equal(filterReviews(rows, '劳动合同').length, 0)
})
