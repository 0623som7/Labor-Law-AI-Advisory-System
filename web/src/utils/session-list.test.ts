import { test } from 'node:test'
import assert from 'node:assert/strict'

import { prependSession } from './session-list.ts'
import type { Session } from '@/api/types'

function sess(id: string, title = id): Session {
  return { id, title, scene: 'consult' } as Session
}

test('prependSession 把会话插到最前面', () => {
  const list = [sess('a'), sess('b')]
  const next = prependSession(list, sess('c'))
  assert.deepEqual(
    next.map((s) => s.id),
    ['c', 'a', 'b'],
  )
  // 不改原数组：调用方可能还有别处引用着它。
  assert.deepEqual(
    list.map((s) => s.id),
    ['a', 'b'],
  )
})

test('prependSession 遇到已存在的 id 原样返回', () => {
  const list = [sess('a'), sess('b')]
  const next = prependSession(list, sess('b'))
  assert.equal(next, list)
  assert.deepEqual(
    next.map((s) => s.id),
    ['a', 'b'],
  )
})

test('prependSession 处理空列表', () => {
  const next = prependSession([], sess('a'))
  assert.deepEqual(
    next.map((s) => s.id),
    ['a'],
  )
})
