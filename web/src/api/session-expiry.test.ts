// 会话失效信号的行为约定：一次失效只广播一次，登录后必须能重置。
//
// 这几个断言守着"并发 40101 只弹一个框"：一次过期可能有七八个请求同时失败，
// 每条都弹一个模态框，用户点掉七个还剩一个。

import assert from 'node:assert/strict'
import test from 'node:test'

import { notifySessionExpired, onSessionExpired, resetSessionExpired } from './session-expiry.ts'

test('第一次通知触发一次监听', () => {
  let calls = 0
  onSessionExpired(() => {
    calls += 1
  })
  resetSessionExpired()

  notifySessionExpired()

  assert.equal(calls, 1)
})

test('连续通知五次只触发一次', () => {
  let calls = 0
  onSessionExpired(() => {
    calls += 1
  })
  resetSessionExpired()

  for (let i = 0; i < 5; i += 1) notifySessionExpired()

  assert.equal(calls, 1)
})

test('重置之后能再触发第二次', () => {
  let calls = 0
  onSessionExpired(() => {
    calls += 1
  })
  resetSessionExpired()

  notifySessionExpired()
  resetSessionExpired()
  notifySessionExpired()

  assert.equal(calls, 2)
})

test('监听自己抛异常时不把状态卡死，重置之后仍能再次通知', () => {
  // main.ts 的监听会操作 router 与 Element Plus；万一其中一步抛了，
  // 标志位不能停在 prompted=true，否则用户这辈子都看不到第二次提示。
  let calls = 0
  onSessionExpired(() => {
    calls += 1
  })
  resetSessionExpired()

  assert.doesNotThrow(() => notifySessionExpired())
  resetSessionExpired()
  notifySessionExpired()

  assert.equal(calls, 2)
})
