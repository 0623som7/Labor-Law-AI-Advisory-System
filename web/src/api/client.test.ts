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

test('续期遇到网络错误：这次失败，但不把用户踢下线', async () => {
  // 缺陷形态：catch 里无条件 onUnauthorized()，地铁里信号断一下、网关回 502，
  // 用户手里的令牌明明还有二十分钟可用，却被清掉跳登录页。
  const { state, refresher } = harness(pair('access-1', 'refresh-1', NOW + 1_000))
  state.failWith = new Error('Network Error')

  assert.equal(await refresher.ensureFresh(), false)
  assert.equal(state.refreshCalls, 1)
  assert.equal(state.unauthorizedCalls, 0)
  assert.notEqual(state.tokens, null)
  assert.equal(state.tokens?.refresh_token, 'refresh-1')
})

test('续期遇到 5xx / 限流：同样只让本次失败', async () => {
  for (const code of [50001, 50201, 42901]) {
    const { state, refresher } = harness(pair('access-1', 'refresh-1', NOW + 1_000))
    state.failWith = new ApiError(code, '服务暂时不可用')

    assert.equal(await refresher.ensureFresh(), false)
    assert.equal(state.unauthorizedCalls, 0, `code=${code} 不该清令牌`)
    assert.notEqual(state.tokens, null, `code=${code} 不该清令牌`)
  }
})

test('别的标签页已经换过令牌：40101 之后对账，不把用户踢下线', async () => {
  // 两个标签页同时过期，都用同一个 refresh_token 去换；后端轮换后到者必然 40101。
  // 此时 localStorage 里是赢家的新令牌，清掉它等于"一个人刷新页面把另一个也甩出去"。
  let tokens: TokenPair | null = pair('access-1', 'refresh-1', NOW + 1_000)
  let refreshCalls = 0
  let unauthorizedCalls = 0

  const refresher = createTokenRefresher({
    now: () => NOW,
    readTokens: () => tokens,
    saveTokens: (result) => {
      tokens = pair(result.access_token, result.refresh_token, NOW + result.expires_in * 1000)
    },
    onUnauthorized: () => {
      unauthorizedCalls += 1
      tokens = null
    },
    refreshTokens: async () => {
      refreshCalls += 1
      // 另一个标签页抢先换成功，把新令牌写进了 localStorage。
      tokens = pair('other-access', 'other-refresh', NOW + 2 * 60 * MINUTE)
      throw new ApiError(40101, '登录状态已失效，请重新登录')
    },
  })

  assert.equal(await refresher.ensureFresh(), true)
  assert.equal(refreshCalls, 1)
  assert.equal(unauthorizedCalls, 0)
  assert.equal(tokens?.access_token, 'other-access')
  assert.equal(refresher.failure(), null)
})

test('对账发现别人的令牌也快过期：用它再换一次，成功了就不踢人', async () => {
  let tokens: TokenPair | null = pair('access-1', 'refresh-1', NOW + 1_000)
  let refreshCalls = 0
  let unauthorizedCalls = 0

  const refresher = createTokenRefresher({
    now: () => NOW,
    readTokens: () => tokens,
    saveTokens: (result) => {
      tokens = pair(result.access_token, result.refresh_token, NOW + result.expires_in * 1000)
    },
    onUnauthorized: () => {
      unauthorizedCalls += 1
      tokens = null
    },
    refreshTokens: async (refreshToken) => {
      refreshCalls += 1
      if (refreshCalls === 1) {
        // 别人的令牌拿着也快了，本标签页醒来时已经进了续期窗口。
        tokens = pair('other-access', 'other-refresh', NOW + 1_000)
        throw new ApiError(40101, '登录状态已失效，请重新登录')
      }
      return authResult('fresh-access', `fresh-${refreshToken}`)
    },
  })

  assert.equal(await refresher.ensureFresh(), true)
  assert.equal(refreshCalls, 2)
  assert.equal(unauthorizedCalls, 0)
  assert.equal(tokens?.access_token, 'fresh-access')
  assert.equal(tokens?.refresh_token, 'fresh-other-refresh')
})

test('完全没有令牌时 ensureFresh 通知一次失效，不静默返回 false', async () => {
  // 缺陷形态：isStale() 在 readTokens() 为 null 时返回 false，于是走到
  // resolve(readTokens() !== null) 直接给个 false —— 调用方以为只是"没登录"，
  // 外层永远收不到失效信号，页面停在原地一动不动。
  const { state, refresher } = harness(null)

  assert.equal(refresher.isStale(), false)
  assert.equal(await refresher.ensureFresh(), false)
  assert.equal(state.unauthorizedCalls, 1)
  assert.equal(state.refreshCalls, 0)
})

test('令牌还没到期时 ensureFresh 返回 true 且不通知失效', async () => {
  // 与上一条成对：补漏不能把正常路径也改成误报，否则每次请求都会弹一次登录框。
  const { state, refresher } = harness(pair('access-1', 'refresh-1', NOW + 30 * MINUTE))

  assert.equal(refresher.isStale(), false)
  assert.equal(await refresher.ensureFresh(), true)
  assert.equal(state.unauthorizedCalls, 0)
  assert.equal(state.refreshCalls, 0)
})
