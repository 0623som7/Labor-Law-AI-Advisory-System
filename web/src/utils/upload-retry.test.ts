import { test } from 'node:test'
import assert from 'node:assert/strict'

import { canRetryUpload } from './upload-retry.ts'

test('canRetryUpload 网络/超时可以重试', () => {
  assert.equal(canRetryUpload(-1), true)
})

test('canRetryUpload 5xx 与 5xxxx 服务端故障可以重试', () => {
  for (const code of [500, 502, 503, 504, 50001, 50201]) {
    assert.equal(canRetryUpload(code), true, `code=${code}`)
  }
})

test('canRetryUpload 确定性失败不给重试', () => {
  // 41301 体积超限、41501 不是 PDF、42201 无文字层：再点一百次也是同一个结果。
  for (const code of [40001, 40101, 40401, 41301, 41501, 42201, 42901, 400, 413, 422]) {
    assert.equal(canRetryUpload(code), false, `code=${code}`)
  }
})

test('canRetryUpload 0 与未知码不给重试', () => {
  assert.equal(canRetryUpload(0), false)
  assert.equal(canRetryUpload(10001), false)
})
