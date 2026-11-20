import assert from 'node:assert/strict'
import { test } from 'node:test'

import { isNetworkFailure, isOfflineError } from './net-error.ts'

/** Node 的 navigator 是个 getter，断网用例临时换掉它再换回来。 */
function withOnline(online: boolean, fn: () => void): void {
  const original = Object.getOwnPropertyDescriptor(globalThis, 'navigator')
  Object.defineProperty(globalThis, 'navigator', {
    value: { onLine: online },
    configurable: true,
    writable: true,
  })
  try {
    fn()
  } finally {
    if (original) Object.defineProperty(globalThis, 'navigator', original)
    else delete (globalThis as { navigator?: unknown }).navigator
  }
}

test('navigator.onLine 为 false 时一律算断网', () => {
  withOnline(false, () => {
    assert.equal(isOfflineError(null), true)
    assert.equal(isOfflineError(new Error('随便什么错误')), true)
  })
})

test('Chromium 的 Failed to fetch 算断网', () => {
  withOnline(true, () => {
    assert.equal(isOfflineError(new TypeError('Failed to fetch')), true)
  })
})

test('Firefox 的 Network Error 与 Safari 的 Load failed 算断网', () => {
  withOnline(true, () => {
    assert.equal(isOfflineError(new Error('Network Error')), true)
    assert.equal(isOfflineError(new TypeError('Load failed')), true)
    assert.equal(isOfflineError(new Error('fetch failed')), true)
  })
})

test('业务错误与服务端报错不算断网', () => {
  withOnline(true, () => {
    assert.equal(isOfflineError(new Error('40001 input 不能为空')), false)
    assert.equal(isOfflineError(new Error('接口返回 50001')), false)
    assert.equal(isOfflineError(undefined), false)
    assert.equal(isOfflineError('登录状态已失效'), false)
  })
})

test('api 层把 fetch 失败包成 code -1 之后仍认得出是网络问题', () => {
  withOnline(true, () => {
    assert.equal(isNetworkFailure({ code: -1, message: '网络连接失败，请检查网络后重试' }), true)
    assert.equal(isNetworkFailure({ code: -1, message: '当前浏览器不支持流式响应' }), false)
    assert.equal(isNetworkFailure({ code: 50001, message: '网络连接失败' }), false)
    assert.equal(isNetworkFailure({ code: 40001, message: 'input 不能为空' }), false)
  })
})

test('isNetworkFailure 覆盖原始断网错误', () => {
  withOnline(true, () => {
    assert.equal(isNetworkFailure(new TypeError('Failed to fetch')), true)
    assert.equal(isNetworkFailure(new Error('40001 input 不能为空')), false)
  })
})
