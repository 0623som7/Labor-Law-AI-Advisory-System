import { test } from 'node:test'
import assert from 'node:assert/strict'

import { findingsOnPage, legalBasisTitle, sortBySeverity, summarizeFindings } from './findings.ts'
import type { Finding } from '../api/types.ts'

function finding(id: string, severity: Finding['severity'], page: number | undefined): Finding {
  return {
    id,
    review_id: 'r1',
    rule_id: 'R-X',
    severity,
    title: 't',
    clause_ref: { section: '第二条', page: page as number, bbox: [0, 0, 10, 10] },
    evidence: 'e',
    legal_basis: [],
    suggestion: { type: 'rewrite', original: 'o', revised: 'r', reason: 'why' },
    economic_impact: null,
  }
}

test('summarizeFindings 三类都计数，0 也保留', () => {
  const s = summarizeFindings([finding('1', 'high', 0), finding('2', 'high', 0), finding('3', 'medium', 1)])
  assert.deepEqual(s, { high: 2, medium: 1, low: 0, total: 3 })
})

test('summarizeFindings 空数组返回全 0', () => {
  assert.deepEqual(summarizeFindings([]), { high: 0, medium: 0, low: 0, total: 0 })
})

test('findingsOnPage 按 0 起页号过滤，缺字段归第 0 页', () => {
  const list = [finding('a', 'high', 0), finding('b', 'medium', 1), finding('c', 'low', undefined)]
  assert.deepEqual(
    findingsOnPage(list, 0).map((f) => f.id),
    ['a', 'c'],
  )
  assert.deepEqual(
    findingsOnPage(list, 1).map((f) => f.id),
    ['b'],
  )
  assert.deepEqual(findingsOnPage(list, 2), [])
})

test('sortBySeverity 高在前且不改原数组', () => {
  const list = [finding('low', 'low', 0), finding('high', 'high', 0), finding('mid', 'medium', 0)]
  assert.deepEqual(
    sortBySeverity(list).map((f) => f.id),
    ['high', 'mid', 'low'],
  )
  assert.deepEqual(
    list.map((f) => f.id),
    ['low', 'high', 'mid'],
  )
})

test('legalBasisTitle 列出条号，没有依据时退回通用标题', () => {
  const f = finding('x', 'high', 0)
  assert.equal(legalBasisTitle(f), '法律依据')
  f.legal_basis = [{ law: '中华人民共和国劳动合同法', article: '第十九条', text: '…' }]
  assert.equal(legalBasisTitle(f), '法律依据（第十九条）')
  f.legal_basis = [
    { law: '中华人民共和国劳动合同法', article: '第十七条', text: '…' },
    { law: '中华人民共和国劳动法', article: '第七十二条', text: '…' },
  ]
  assert.equal(legalBasisTitle(f), '法律依据（第十七条、第七十二条）')
})
