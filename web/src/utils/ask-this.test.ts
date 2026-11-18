import assert from 'node:assert/strict'
import { test } from 'node:test'

import { buildAskContext, contextFileId, contextLabel, shouldPrefill } from './ask-this.ts'
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

test('上下文只陈述不提问', () => {
  const line = buildAskContext(finding())
  for (const bad of ['？', '?', '怎么办', '吗']) {
    assert.ok(!line.includes(bad), `上下文里不该出现「${bad}」：${line}`)
  }
})

test('带出条款位置与证据首句', () => {
  const line = buildAskContext(finding())
  assert.ok(line.includes('合同「第二条 试用期」'), `应含条款位置：${line}`)
  assert.ok(line.includes('约定试用期 8 个月'), `应含证据首句：${line}`)
  // 证据常有两三句，只取第一句，否则这一行会被撑成两倍长。
  assert.ok(!line.includes('法定上限 2 个月。'), `第二句不该带进来：${line}`)
})

test('证据为空时不留连续分隔符', () => {
  const line = buildAskContext(
    finding({
      evidence: '',
      rule_id: 'R-PROBATION-SALARY',
      clause_ref: { section: '第四条', page: 2, bbox: [57, 515, 496, 526] },
    }),
  )
  assert.equal(line, '合同「第四条」 · 结论 R-PROBATION-SALARY')
  assert.ok(!line.includes('· ·'), `不该出现连续分隔符：${line}`)
})

test('没有条款号时用「该条款」兜底', () => {
  const line = buildAskContext(finding({ clause_ref: { section: '  ', page: 0, bbox: [0, 0, 0, 0] } }))
  assert.ok(line.startsWith('合同「该条款」'), `应兜底为「该条款」：${line}`)
})

test('超长时截断到 120 字并留省略号（总长 121）', () => {
  const line = buildAskContext(finding({ evidence: '很长的证据'.repeat(100) }))
  assert.equal(line.length, 121)
  assert.ok(line.endsWith('…'), `截断处应以省略号结尾：${line}`)
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
