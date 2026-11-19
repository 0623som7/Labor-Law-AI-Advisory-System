import assert from 'node:assert/strict'
import test from 'node:test'

import { breadcrumbFor } from './breadcrumb.ts'

test('breadcrumbFor：上传页两段，最后一段是当前页', () => {
  const crumbs = breadcrumbFor('upload')
  assert.equal(crumbs.length, 2)
  assert.deepEqual(crumbs.map((c) => c.label), ['对话咨询', '合同审查'])
  assert.equal(crumbs[0].to, 'chat')
  assert.equal(crumbs[1].to, undefined) // 当前页不可点
})

test('breadcrumbFor：审查页三段，只有最后一段不可点', () => {
  const crumbs = breadcrumbFor('review')
  assert.equal(crumbs.length, 3)
  assert.deepEqual(crumbs.map((c) => c.label), ['对话咨询', '合同审查', '审查结果'])
  assert.equal(crumbs[0].to, 'chat')
  assert.equal(crumbs[1].to, 'upload')
  assert.equal(crumbs[2].to, undefined)
})
