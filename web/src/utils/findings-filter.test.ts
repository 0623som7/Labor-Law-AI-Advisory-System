import { test } from 'node:test'
import assert from 'node:assert/strict'

import { countBySeverity, filterFindings } from './findings-filter.ts'
import type { Finding } from '../api/types.ts'

/** 只填被断言用到的字段，其余给中性值。 */
function finding(id: string, severity: Finding['severity'], over: Partial<Finding> = {}): Finding {
  return {
    id,
    review_id: 'r1',
    rule_id: 'R-X',
    severity,
    title: 't',
    clause_ref: { section: '第二条', page: 0, bbox: [0, 0, 10, 10] },
    evidence: 'e',
    legal_basis: [],
    suggestion: { type: 'rewrite', original: 'o', revised: 'r', reason: 'why' },
    economic_impact: null,
    ...over,
  }
}

const ALL = { severity: 'all' as const, query: '' }

test('filterFindings 空 query 全过，连只有空格也算空', () => {
  const list = [finding('a', 'high'), finding('b', 'low')]
  assert.deepEqual(filterFindings(list, ALL).map((f) => f.id), ['a', 'b'])
  assert.deepEqual(filterFindings(list, { severity: 'all', query: '   ' }).map((f) => f.id), ['a', 'b'])
})

test('filterFindings query 大小写不敏感', () => {
  const list = [finding('a', 'high', { title: 'SOCIAL Insurance 未缴' }), finding('b', 'low', { title: '押金' })]
  assert.deepEqual(filterFindings(list, { severity: 'all', query: 'social' }).map((f) => f.id), ['a'])
  assert.deepEqual(filterFindings(list, { severity: 'all', query: 'INSURANCE' }).map((f) => f.id), ['a'])
})

test('filterFindings 命中 title / evidence / clause_ref.section / rule_id 四段任一即可', () => {
  const list = [
    finding('title', 'high', { title: '试用期超限', evidence: 'x', clause_ref: { section: 's', page: 0, bbox: [0, 0, 1, 1] }, rule_id: 'R-1' }),
    finding('evidence', 'high', { title: 'x', evidence: '每月扣 2000 元押金' }),
    finding('section', 'high', { title: 'x', evidence: 'x', clause_ref: { section: '第九条', page: 0, bbox: [0, 0, 1, 1] } }),
    finding('rule', 'high', { title: 'x', evidence: 'x', rule_id: 'R-PROBATION-01' }),
    finding('none', 'high', { title: 'x', evidence: 'x', clause_ref: { section: 's', page: 0, bbox: [0, 0, 1, 1] }, rule_id: 'R-1' }),
  ]
  assert.deepEqual(
    filterFindings(list, { severity: 'all', query: '试用期' }).map((f) => f.id),
    ['title'],
  )
  assert.deepEqual(filterFindings(list, { severity: 'all', query: '押金' }).map((f) => f.id), ['evidence'])
  assert.deepEqual(filterFindings(list, { severity: 'all', query: '第九条' }).map((f) => f.id), ['section'])
  assert.deepEqual(filterFindings(list, { severity: 'all', query: 'probation' }).map((f) => f.id), ['rule'])
  assert.deepEqual(filterFindings(list, { severity: 'all', query: '查无此项' }).map((f) => f.id), [])
})

test('filterFindings severity 过滤，all 不过滤', () => {
  const list = [finding('h', 'high'), finding('m', 'medium'), finding('l', 'low')]
  assert.deepEqual(filterFindings(list, { severity: 'high', query: '' }).map((f) => f.id), ['h'])
  assert.deepEqual(filterFindings(list, { severity: 'medium', query: '' }).map((f) => f.id), ['m'])
  assert.deepEqual(filterFindings(list, { severity: 'low', query: '' }).map((f) => f.id), ['l'])
  assert.deepEqual(filterFindings(list, { severity: 'all', query: '' }).map((f) => f.id), ['h', 'm', 'l'])
})

test('filterFindings 严重度与关键词同时生效', () => {
  const list = [
    finding('h1', 'high', { title: '押金' }),
    finding('h2', 'high', { title: '社保' }),
    finding('l1', 'low', { title: '押金' }),
  ]
  assert.deepEqual(filterFindings(list, { severity: 'high', query: '押金' }).map((f) => f.id), ['h1'])
})

test('filterFindings 保持入参顺序，不按严重度重排', () => {
  const list = [finding('low', 'low', { title: '同一个词' }), finding('high', 'high', { title: '同一个词' })]
  assert.deepEqual(
    filterFindings(list, { severity: 'all', query: '同一个词' }).map((f) => f.id),
    ['low', 'high'],
  )
})

test('countBySeverity 分类计数，all 是总数', () => {
  const list = [finding('1', 'high'), finding('2', 'high'), finding('3', 'medium'), finding('4', 'low')]
  assert.deepEqual(countBySeverity(list), { all: 4, high: 2, medium: 1, low: 1 })
  assert.deepEqual(countBySeverity([]), { all: 0, high: 0, medium: 0, low: 0 })
})

test('filterFindings 无匹配时返回空数组，不是 null', () => {
  const out = filterFindings([finding('a', 'high')], { severity: 'low', query: '不存在' })
  assert.ok(Array.isArray(out))
  assert.deepEqual(out, [])
})
