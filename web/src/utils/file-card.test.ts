import { test } from 'node:test'
import assert from 'node:assert/strict'

import { fileCardLabel } from './file-card.ts'

test('文件名与页数拼成一行', () => {
  assert.equal(fileCardLabel({ filename: '劳动合同.pdf', page_count: 4 }), '劳动合同.pdf · 4 页')
})

test('页数为 0 表示还没解析完，不能显示成「0 页」', () => {
  // 缺陷形态：上传成功立刻发消息时 page_count 还是 0，显示「0 页」会让用户以为文件坏了。
  assert.equal(fileCardLabel({ filename: '劳动合同.pdf', page_count: 0 }), '劳动合同.pdf · 页数待解析')
})

test('文件名首尾空格不影响展示，空文件名给兜底文案', () => {
  assert.equal(fileCardLabel({ filename: '  劳动合同.pdf  ', page_count: 12 }), '劳动合同.pdf · 12 页')
  assert.equal(fileCardLabel({ filename: '   ', page_count: 3 }), '未命名文件 · 3 页')
})
