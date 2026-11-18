import assert from 'node:assert/strict'
import { test } from 'node:test'
import { shouldSendOnKey } from './send-key.ts'

function key(over: Partial<Parameters<typeof shouldSendOnKey>[0]> = {}) {
  return { key: 'Enter', shiftKey: false, ctrlKey: false, metaKey: false, ...over }
}

test('Enter 发送', () => {
  assert.equal(shouldSendOnKey(key()), true)
})

test('Shift+Enter 换行，不发送', () => {
  assert.equal(shouldSendOnKey(key({ shiftKey: true })), false)
})

test('Ctrl+Enter 也发送（不再让它换行）', () => {
  assert.equal(shouldSendOnKey(key({ ctrlKey: true })), true)
})

test('Meta+Enter 也发送', () => {
  assert.equal(shouldSendOnKey(key({ metaKey: true })), true)
})

test('输入法组字期间按 Enter 只上屏，不发送', () => {
  assert.equal(shouldSendOnKey(key({ isComposing: true })), false)
})

test('isComposing 为 undefined 时按没在组字处理', () => {
  assert.equal(shouldSendOnKey(key({ isComposing: undefined })), true)
})

test('非 Enter 键不发送', () => {
  assert.equal(shouldSendOnKey(key({ key: 'a' })), false)
})

test('空字符串 key 不发送', () => {
  assert.equal(shouldSendOnKey(key({ key: '' })), false)
})
