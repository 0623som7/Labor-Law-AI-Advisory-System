import { test } from 'node:test'
import assert from 'node:assert/strict'

import { formatRelative } from './format-relative.ts'

// 固定基准时刻，避免测试跟真实时钟赛跑。
const NOW = Date.parse('2026-11-08T12:00:00Z')
const at = (ms: number): string => new Date(NOW - ms).toISOString()

test('59 秒内显示刚刚', () => {
  assert.equal(formatRelative(at(59_000), NOW), '刚刚')
  assert.equal(formatRelative(at(0), NOW), '刚刚')
})

test('整 60 秒进入分钟档', () => {
  assert.equal(formatRelative(at(60_000), NOW), '1 分钟前')
})

test('59 分钟仍是分钟档', () => {
  assert.equal(formatRelative(at(59 * 60_000), NOW), '59 分钟前')
})

test('23 小时仍是小时档', () => {
  assert.equal(formatRelative(at(23 * 3_600_000), NOW), '23 小时前')
})

test('6 天仍是天档', () => {
  assert.equal(formatRelative(at(6 * 86_400_000), NOW), '6 天前')
})

test('30 天以上显示日期', () => {
  const iso = at(30 * 86_400_000)
  const d = new Date(Date.parse(iso))
  const pad = (n: number): string => String(n).padStart(2, '0')
  const want = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
  assert.equal(formatRelative(iso, NOW), want)
  assert.match(formatRelative(iso, NOW), /^\d{4}-\d{2}-\d{2}$/)
})

test('服务端时间比本地快时按刚刚处理，不出现负数', () => {
  assert.equal(formatRelative(at(-5_000), NOW), '刚刚')
})

test('非法时间串返回空串而不是 Invalid Date', () => {
  assert.equal(formatRelative('', NOW), '')
  assert.equal(formatRelative('not-a-date', NOW), '')
})
