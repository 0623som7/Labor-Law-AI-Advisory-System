import assert from 'node:assert/strict'
import { test } from 'node:test'

import { buildAskQuestion, contextFileId, contextLabel, shouldPrefill } from './ask-this.ts'
import type { Finding } from '@/api/types'

function finding(over: Partial<Finding> = {}): Finding {
  return {
    id: 'f1',
    review_id: 'r1',
    rule_id: 'R-PROBATION-MATCH',
    severity: 'high',
    title: '试用期期限超过法定上限',
    clause_ref: { section: '第二条 试用期', page: 0, bbox: [57, 550, 524, 561] },
    evidence: '约定试用期 8 个月，合同期限 2 年，法定上限 2 个月。',
    legal_basis: [],
    suggestion: { type: 'rewrite', original: '', revised: '', reason: '' },
    economic_impact: null,
    ...over,
  }
}

test('带出条款位置与标题', () => {
  const q = buildAskQuestion(finding())
  assert.ok(q.includes('第二条 试用期'), `应含条款位置：${q}`)
  assert.ok(q.includes('试用期期限超过法定上限'), `应含标题：${q}`)
})

test('证据为空时不出现空的冒号残句', () => {
  const q = buildAskQuestion(finding({ evidence: '' }))
  assert.ok(!q.includes('：'), `空证据不该留冒号：${q}`)
  assert.ok(q.includes('试用期期限超过法定上限'))
})

test('超长证据截断到 200 字以内并留省略号', () => {
  const q = buildAskQuestion(finding({ evidence: '很长的证据'.repeat(100) }))
  assert.ok(q.length <= 200, `总长应 ≤ 200，实际 ${q.length}`)
  assert.ok(q.includes('…'), `截断处应有省略号：${q}`)
  assert.ok(q.endsWith('公司不同意改，我该怎么办？'), `末尾那一问要留着：${q}`)
})

test('只有非空字符串 ask 才预填', () => {
  assert.equal(shouldPrefill({ ask: 'x' }), true)
  assert.equal(shouldPrefill({}), false)
  assert.equal(shouldPrefill({ ask: '' }), false)
  assert.equal(shouldPrefill({ ask: 123 }), false)
})

test('来源条带条款号', () => {
  const label = contextLabel({ from: 'review', section: '第二条' })
  assert.ok(label.includes('第二条'), `应含条款号：${label}`)
})

test('没有条款号时给整份合同的措辞，不留空引号', () => {
  const label = contextLabel({ from: 'review' })
  assert.notEqual(label, '')
  assert.ok(!label.includes('「」'), `不该出现空引号：${label}`)
})

test('不是从审查页来的就没有来源条', () => {
  assert.equal(contextLabel({}), '')
  assert.equal(contextLabel({ from: 'x' }), '')
})

test('只有 file 才给回原文的地址', () => {
  assert.equal(contextFileId({ file: 'abc' }), 'abc')
  assert.equal(contextFileId({}), '')
})
