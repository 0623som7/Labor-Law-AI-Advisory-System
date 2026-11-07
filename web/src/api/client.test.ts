// 续期单飞逻辑的测试。
//
// 这里只测 createTokenRefresher：它把"读令牌/写令牌/通知失效/真的去换"全抽成了
// 参数，所以不需要 DOM、不需要 axios、不需要后端，node --test 直接跑。
// tokenRefresher 这个模块级实例只是把 localStorage 和 axios 填进去，
// 行为与本文件被测的东西完全一致。

import assert from 'node:assert/strict'
import test from 'node:test'

import { ApiError, createTokenRefresher, REFRESH_LEAD_MS } from './client.ts'
import type { AuthResult, TokenPair } from './types.ts'

const NOW = 1_700_000_000_000
const MINUTE = 60_000

function pair(access: string, refresh: string, expiresAt: number): TokenPair {
  return { access_token: access, refresh_token: refresh, expires_at: expiresAt }
}

function authResult(access: string, refresh: string): AuthResult {
  return {
    user: {
      id: 'U1',
      phone_masked: '138****0000',
      nickname: '测试',
      created_at: '2026-11-07T00:00:00Z',
    },
    access_token: access,
    refresh_token: refresh,
    expires_in: 7200,
  }
}

/** 一个可观测的替身：记录读了几次、换了几次、存了什么、有没有被判定失效。 */
function harness(initial: TokenPair | null) {
  const state = {
    tokens: initial,
    refreshCalls: 0,
    saved: [] as AuthResult[],
    unauthorizedCalls: 0,
    failWith: null as Error | null,
  }

  const refresher = createTokenRefresher({
    now: () => NOW,
    readTokens: () => state.tokens,
    saveTokens: (result) => {
      state.saved.push(result)
      state.tokens = pair(
        result.access_token,
        result.refresh_token,
        NOW + result.expires_in * 1000,
      )
    },
    onUnauthorized: () => {
      state.unauthorizedCalls += 1
      state.tokens = null
    },
    refreshTokens: async (refreshToken) => {
      state.refreshCalls += 1
      if (state.failWith) throw state.failWith
      return authResult(`new-access-${state.refreshCalls}`, `new-refresh-${refreshToken}`)
    },
  })

  return { state, refresher }
}

test('未过期不发 refresh，请求照常带旧令牌', async () => {
  const { state, refresher } = harness(pair('access-1', 'refresh-1', NOW + 30 * MINUTE))

  assert.equal(refresher.isStale(), false)
  assert.equal(await refresher.ensureFresh(), true)
  assert.equal(state.refreshCalls, 0)
  assert.equal(state.saved.length, 0)
  assert.equal(state.unauthorizedCalls, 0)
  assert.equal(state.tokens?.access_token, 'access-1')
})

test('进入续期窗口就换一次，并用响应里的新令牌整套覆盖', async () => {
  // 剩 30 秒 < 60 秒提前量 → 必须续期，否则到点上就是 40101。
  const { state, refresher } = harness(pair('access-1', 'refresh-1', NOW + 30_000))

  assert.equal(refresher.isStale(), true)
  assert.equal(await refresher.ensureFresh(), true)
  assert.equal(state.refreshCalls, 1)
  assert.equal(state.saved.length, 1)
  // 后端会作废旧 refresh_token，所以本地必须换成响应里的那一个。
  assert.equal(state.tokens?.refresh_token, 'new-refresh-refresh-1')
  assert.equal(state.tokens?.access_token, 'new-access-1')
  assert.equal(state.unauthorizedCalls, 0)
})

test('临界点：剩余时间正好等于提前量时不换，少一毫秒就换', async () => {
  // 契约写的是 expires_at - 60_000 < Date.now()，等于不算过期，所以这两个断言都不能松。
  const exact = harness(pair('access-1', 'refresh-1', NOW + REFRESH_LEAD_MS))
  assert.equal(exact.refresher.isStale(), false)
  assert.equal(await exact.refresher.ensureFresh(), true)
  assert.equal(exact.state.refreshCalls, 0)

  const justUnder = harness(pair('access-1', 'refresh-1', NOW + REFRESH_LEAD_MS - 1))
  assert.equal(await justUnder.refresher.ensureFresh(), true)
  assert.equal(justUnder.state.refreshCalls, 1)
})

test('两个请求同时发现过期，只打一次 /auth/refresh', async () => {
  const { state, refresher } = harness(pair('access-1', 'refresh-1', NOW + 1_000))

  const [first, second] = await Promise.all([refresher.ensureFresh(), refresher.ensureFresh()])

  assert.equal(first, true)
  assert.equal(second, true)
  assert.equal(state.refreshCalls, 1)
  assert.equal(state.saved.length, 1)
})

test('续期失败清掉令牌并通知外层，调用方拿到 false', async () => {
  const { state, refresher } = harness(pair('access-1', 'refresh-1', NOW + 1_000))
  state.failWith = new ApiError(40101, '登录状态已失效，请重新登录')

  assert.equal(await refresher.ensureFresh(), false)
  assert.equal(state.refreshCalls, 1)
  assert.equal(state.unauthorizedCalls, 1)
  assert.equal(state.tokens, null)
})

test('没有令牌时直接返回 false，不去打刷新接口', async () => {
  const { state, refresher } = harness(null)

  assert.equal(await refresher.ensureFresh(), false)
  assert.equal(state.refreshCalls, 0)
})

test('失败后不会卡在单飞状态里：令牌被清掉，后续调用立刻返回 false', async () => {
  const { state, refresher } = harness(pair('access-1', 'refresh-1', NOW + 1_000))
  state.failWith = new ApiError(40101, '登录状态已失效，请重新登录')
  await refresher.ensureFresh()

  state.failWith = null
  assert.equal(await refresher.ensureFresh(), false)
  assert.equal(state.refreshCalls, 1)
})
