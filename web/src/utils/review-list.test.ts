// 审查历史列表的三个纯函数。
//
// 渲染本身（两行记录渲染两行、空态）靠手动验收 + 类型检查兜住：
// 项目没有 @vue/test-utils，为它引一套组件测试基建属于"为测试而加依赖"。

import { test } from 'node:test'
import assert from 'node:assert/strict'

import { hasMore, isClickable, riskLabel, statusLabel } from './review-list.ts'

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

test('isClickable 只放过有结果的记录', () => {
  assert.equal(isClickable('succeeded'), true)
  assert.equal(isClickable('running'), true)
  assert.equal(isClickable('pending'), true)
  assert.equal(isClickable('failed'), false)
  assert.equal(isClickable('cancelled'), false)
})
