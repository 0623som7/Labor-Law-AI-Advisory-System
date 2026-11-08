import { test } from 'node:test'
import assert from 'node:assert/strict'

import { historyBubbles } from './chat-history.ts'
import type { Citation } from './sse.ts'

const lcl38: Citation = {
  statute_id: 'LCL-38',
  law: '中华人民共和国劳动合同法',
  article: '第三十八条',
  text: '用人单位有下列情形之一的，劳动者可以解除劳动合同。',
  url: 'https://flk.npc.gov.cn/',
}

const lcl46: Citation = {
  statute_id: 'LCL-46',
  law: '中华人民共和国劳动合同法',
  article: '第四十六条',
  text: '下列情形之一的，用人单位应当向劳动者支付经济补偿。',
}

test('历史 assistant 消息带 citations 时映射出可渲染的卡片数据', () => {
  const bubbles = historyBubbles([
    { role: 'user', content: '公司不给我交社保，我能直接走吗' },
    { role: 'assistant', content: '可以解除并要求经济补偿。', citations: [lcl38, lcl46] },
  ])

  assert.equal(bubbles.length, 2)
  assert.deepEqual(bubbles[0].citations, [])
  assert.equal(bubbles[1].citations.length, 2)
  assert.equal(bubbles[1].citations[0].statute_id, 'LCL-38')
  assert.equal(bubbles[1].citations[1].article, '第四十六条')
  // 卡片上的"查看原文"依赖 url；没有 url 的那条必须保持缺失，不能补假链接。
  assert.equal(bubbles[1].citations[0].url, 'https://flk.npc.gov.cn/')
  assert.equal(bubbles[1].citations[1].url, undefined)
})

test('没有 citations 的历史消息映射成空数组而不是补法条', () => {
  const bubbles = historyBubbles([{ role: 'assistant', content: '老数据没有引用。' }])
  assert.deepEqual(bubbles[0].citations, [])
})

test('历史气泡标记 fromHistory，避免被当成正在生成的那一条', () => {
  const bubbles = historyBubbles([{ role: 'user', content: '那我能要补偿吗' }])
  assert.equal(bubbles[0].fromHistory, true)
})
