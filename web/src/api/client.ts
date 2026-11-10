// REST 客户端。
//
// 四件事都在这里做掉，视图层就不再重复：
//   1. 解包络——后端永远回 {code,message,data}，code !== 0 一律抛 ApiError；
//   2. 带令牌——请求拦截器统一加 Authorization，页面里不出现 token；
//   3. 提前续期——令牌快过期时先换新的，再发原请求（见下面的 createTokenRefresher）；
//   4. 会话失效——收到 40101（或 HTTP 401）就清令牌并通知外层跳登录页。
//
// 流式接口不走这里（axios 处理不了边收边解），见 api/chat.ts；它复用这里的
// ensureFreshToken()，所以"提前续期"只有一份实现。

import axios, { type AxiosRequestConfig } from 'axios'

import { notifySessionExpired } from './session-expiry.ts'
import type { AuthResult, Envelope, TokenPair } from './types'

const ACCESS_KEY = 'laborlaw.access_token'
const REFRESH_KEY = 'laborlaw.refresh_token'
const EXPIRES_KEY = 'laborlaw.expires_at'

/** 后端业务错误码到前端异常的载体。视图层靠 message 直接显示，不自己编文案。 */
export class ApiError extends Error {
  readonly code: number
  readonly fields?: Record<string, string>
  readonly retryable: boolean

  constructor(code: number, message: string, fields?: Record<string, string>, retryable = false) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.fields = fields
    this.retryable = retryable
  }
}

/**
 * 令牌彻底失效的唯一入口：清本地令牌 + 通知外层弹窗。
 *
 * 不在这里跳转：api 层拿不到 router（api → store → router 会成环），
 * 而且跳转会丢掉用户正在看的页面。
 */
export function notifyUnauthorized(): void {
  clearTokens()
  notifySessionExpired()
}

/** 读本地令牌。任一字段缺失都当未登录，避免拿到半个令牌去发请求。 */
export function getTokens(): TokenPair | null {
  const access = localStorage.getItem(ACCESS_KEY)
  const refresh = localStorage.getItem(REFRESH_KEY)
  if (!access || !refresh) return null
  const expiresAt = Number(localStorage.getItem(EXPIRES_KEY) ?? '0')
  return {
    access_token: access,
    refresh_token: refresh,
    expires_at: Number.isFinite(expiresAt) ? expiresAt : 0,
  }
}

/** 登录/注册成功后落盘。expires_in 是秒，转成绝对时间戳便于和 Date.now() 比较。 */
export function setTokens(result: AuthResult): void {
  localStorage.setItem(ACCESS_KEY, result.access_token)
  localStorage.setItem(REFRESH_KEY, result.refresh_token)
  localStorage.setItem(EXPIRES_KEY, String(Date.now() + result.expires_in * 1000))
}

export function clearTokens(): void {
  localStorage.removeItem(ACCESS_KEY)
  localStorage.removeItem(REFRESH_KEY)
  localStorage.removeItem(EXPIRES_KEY)
}

/** 续期失败时统一用后端 40101 的原文，别在前端另编一套说法。 */
export const MSG_RELOGIN = '登录状态已失效，请重新登录'

/** 令牌剩余时间不足这么长就先换新的：留一分钟给时钟漂移和网络往返。 */
export const REFRESH_LEAD_MS = 60_000

/** 续期需要的全部外部动作。抽成参数只为一件事：让单飞逻辑能在 Node 里被测试。 */
export interface TokenRefresherDeps {
  now: () => number
  readTokens: () => TokenPair | null
  saveTokens: (result: AuthResult) => void
  /** 彻底失效时清本地令牌并通知外层跳登录页。 */
  onUnauthorized: () => void
  refreshTokens: (refreshToken: string) => Promise<AuthResult>
}

export interface TokenRefresher {
  /**
   * 令牌可用返回 true（含"还不用换"和"换成功了"两种）；
   * 不可用返回 false（没登录、或续期失败，此时本地令牌已被清掉），调用方自己抛 ApiError。
   */
  ensureFresh: () => Promise<boolean>
  /** 只判断要不要换，不发请求。 */
  isStale: () => boolean
  /**
   * 上一次 ensureFresh 失败的原因（成功或没尝试过时为 null）。
   *
   * 调用方需要它来区分两种"令牌不能用"：后端明确回 40101（登录真的失效了）
   * 与网络抖动/网关 502（登录还好好的，只是这次没换成）。前者才该跳登录页。
   */
  failure: () => unknown
}

/**
 * 提前续期。
 *
 * 为什么不在 401 之后再刷：SSE 流一旦吐出半句话就没法重放，只能让用户重问；
 * 而 REST 请求重发一次也只是多一个往返。所以统一在发请求前把令牌换好。
 *
 * 单飞：多个请求同时发现过期时共用同一个 promise，只打一次 /auth/refresh。
 * 后端会轮换 refresh_token（旧的立刻失效），所以这里必须整套覆盖，不能只换 access。
 */
export function createTokenRefresher(deps: TokenRefresherDeps): TokenRefresher {
  // 唯一的单飞位：非 null 表示"正在换"，后来的调用直接 await 它。
  let refreshing: Promise<boolean> | null = null

  function isStale(): boolean {
    const tokens = deps.readTokens()
    if (!tokens) return false
    return tokens.expires_at - REFRESH_LEAD_MS < deps.now()
  }

  // 上一次失败的原因。只在"确实换了但没换成"时被写入，成功即清空。
  let lastFailure: unknown = null

  function markFailure(err: unknown): false {
    lastFailure = err
    // 只有后端明确说"这个刷新令牌不认了"才清令牌跳登录页。网络超时、502、
    // 限流都不代表登录失效——把它们也当成失效，就会在网抖一下时把用户踢出去，
    // 而他手上的令牌其实还有二十分钟。
    if (isAuthFailure(err)) deps.onUnauthorized()
    return false
  }

  async function run(): Promise<boolean> {
    const tokens = deps.readTokens()
    if (!tokens) return false
    try {
      const result = await deps.refreshTokens(tokens.refresh_token)
      deps.saveTokens(result)
      lastFailure = null
      return true
    } catch (err) {
      // 多标签页同时在跑：两边拿同一个 refresh_token 去换，后端轮换后到者必然
      // 收到 40101。这时 localStorage 里已经是赢家写进去的新令牌——先对账，
      // 别用旧令牌的失败去清掉别人刚换回来的登录态。
      const current = deps.readTokens()
      if (current && current.refresh_token !== tokens.refresh_token) {
        if (!isStale()) {
          lastFailure = null
          return true
        }
        // 别人换来的这个也快过期了（比如两个标签页隔了很久才先后醒过来），
        // 用它再换一次，仍然失败才认输。
        try {
          const retried = await deps.refreshTokens(current.refresh_token)
          deps.saveTokens(retried)
          lastFailure = null
          return true
        } catch (retryErr) {
          return markFailure(retryErr)
        }
      }
      return markFailure(err)
    }
  }

  return {
    ensureFresh(): Promise<boolean> {
      if (!isStale()) {
        // 没有令牌 = 会话已经失效（可能被另一个标签页清掉了，也可能后端刚回了 40101）。
        // 必须走一次 onUnauthorized，否则调用方只会拿到一个 false，然后静默失败。
        if (deps.readTokens() === null) {
          deps.onUnauthorized()
          return Promise.resolve(false)
        }
        return Promise.resolve(true)
      }
      if (!refreshing) {
        refreshing = run().finally(() => {
          refreshing = null
        })
      }
      return refreshing
    },
    isStale,
    failure: () => lastFailure,
  }
}

/**
 * 这个错误是不是"刷新令牌已经不被承认"。
 *
 * 只认后端业务码 40101。HTTP 401/403、502、ECONNABORTED 都不算：它们说明的是
 * "这次请求没走通"，不是"这个人不是登录状态"。
 */
function isAuthFailure(err: unknown): boolean {
  return err instanceof ApiError && err.code === 40101
}

const http = axios.create({
  // 后端路由前缀是 /api/v1；开发期由 Vite 代理到 127.0.0.1:8080，部署期由 nginx 反代。
  baseURL: '/api/v1',
  timeout: 30000,
  headers: { 'Content-Type': 'application/json' },
})

// 换令牌必须绕开下面的拦截器：拦截器会先判断"要不要续期"，而 /auth/refresh
// 自己就是续期，走过去等于自己等自己。
const rawHttp = axios.create({
  baseURL: '/api/v1',
  timeout: 30000,
  headers: { 'Content-Type': 'application/json' },
})

async function refreshTokensDirect(refreshToken: string): Promise<AuthResult> {
  // 必须把 axios 的 AxiosError 收敛成 ApiError：/auth/refresh 拒绝刷新令牌时回的是
  // HTTP 401 + 包络 code=40101，而 axios 会因为 401 状态码直接抛错，业务码就丢在
  // error.response.data 里。markFailure 只认 ApiError(40101)，AxiosError 掉进去会被
  // 当成"网络没通"，于是心跳静默续期失败后既不弹框也不清令牌——用户卡在原地。
  let response
  try {
    response = await rawHttp.post<Envelope<AuthResult>>('/auth/refresh', {
      refresh_token: refreshToken,
    })
  } catch (error) {
    throw toApiError(error)
  }
  const body = response.data
  if (body.code !== 0) {
    throw new ApiError(body.code, body.message || '刷新令牌失败', body.fields, body.retryable ?? false)
  }
  return body.data as AuthResult
}

const tokenRefresher = createTokenRefresher({
  now: () => Date.now(),
  readTokens: getTokens,
  saveTokens: setTokens,
  onUnauthorized: notifyUnauthorized,
  refreshTokens: refreshTokensDirect,
})

/** REST 与 SSE 共用的入口：发请求前确保手上的令牌还能用。 */
export function ensureFreshToken(): Promise<boolean> {
  return tokenRefresher.ensureFresh()
}

/** 上一次续期失败的原因；用于把"登录失效"与"网络没通"分开报给用户。 */
export function ensureFreshTokenFailure(): unknown {
  return tokenRefresher.failure()
}

http.interceptors.request.use(async (config) => {
  // 完全没登录时不在这里拦：/auth/login、/auth/register、/auth/refresh 都是匿名接口，
  // 拦了会把"密码错误"变成"请重新登录"。其它接口缺令牌由后端回 40101，走同一条失效链路。
  const before = getTokens()
  if (!before) return config

  if (!(await ensureFreshToken())) {
    // 拦截器里抛 ApiError，由下面的 request() 原样兜住，视图层拿到的文案与后端一致。
    // 续期失败的真实原因要原样抛出去：40101 是"请重新登录"，网络错误是
    // "连不上后端"，让用户看到的提示与实际原因对上。
    const cause = tokenRefresher.failure()
    if (cause instanceof ApiError) throw cause
    throw new ApiError(40101, MSG_RELOGIN)
  }
  const tokens = getTokens()
  if (tokens) {
    config.headers.set('Authorization', `Bearer ${tokens.access_token}`)
  }
  return config
})

/** 把任何失败（网络、HTTP 状态、业务码）收敛成 ApiError，视图层只需要 catch 一种。 */
function toApiError(error: unknown): ApiError {
  if (error instanceof ApiError) return error

  if (axios.isAxiosError(error)) {
    const body = error.response?.data as Envelope<unknown> | undefined
    if (body && typeof body.code === 'number') {
      // 业务码优先：40101 可能是 200 也可能是 401 回来的，以包络里的 code 为准。
      if (body.code === 40101) notifyUnauthorized()
      return new ApiError(body.code, body.message || '请求失败', body.fields, body.retryable ?? false)
    }
    // 不看 HTTP 状态码 401：后端所有失败都带包络，业务码才是唯一真相。没有包络
    // 的 401 来自反向代理/网关（证书过期、nginx 鉴权），把它当成"登录失效"会在
    // 一次网关抖动里踢掉所有人。
    if (error.code === 'ECONNABORTED') {
      return new ApiError(-1, '请求超时，请重试')
    }
    // 没有 response 就是请求根本没发出去（断网、DNS、被插件拦）。axios 在这里给的
    // 是 "Network Error" 这种英文串，直接抛给视图层等于给用户看控制台术语。
    if (!error.response) {
      return new ApiError(-1, '网络连接失败，请检查网络后重试')
    }
    return new ApiError(error.response.status, error.message || '请求失败，请重试')
  }

  return new ApiError(-1, error instanceof Error ? error.message : '未知错误')
}

async function request<T>(config: AxiosRequestConfig): Promise<T> {
  try {
    const response = await http.request<Envelope<T>>(config)
    const body = response.data
    if (body.code !== 0) {
      if (body.code === 40101) notifyUnauthorized()
      throw new ApiError(body.code, body.message || '请求失败', body.fields, body.retryable ?? false)
    }
    return body.data as T
  } catch (error) {
    throw toApiError(error)
  }
}

export function get<T>(url: string, params?: Record<string, unknown>): Promise<T> {
  return request<T>({ method: 'GET', url, params })
}

export function post<T>(url: string, data?: unknown): Promise<T> {
  return request<T>({ method: 'POST', url, data })
}

export function del<T>(url: string): Promise<T> {
  return request<T>({ method: 'DELETE', url })
}

/**
 * multipart 上传。
 *
 * 单独一条通道的必要性：实例默认头是 application/json，axios 的 transformRequest
 * 看到"JSON 头 + FormData"会把 FormData 序列化成 JSON（文件字段变成 {}），
 * 后端收到的是一个没有文件的请求。这里显式声明 multipart，浏览器再加 boundary。
 *
 * onProgress 拿不到 total 时回调 null：不显示百分比，而不是显示一个假的 100%。
 */
export async function postForm<T>(
  url: string,
  form: FormData,
  onProgress?: (percent: number | null) => void,
): Promise<T> {
  return request<T>({
    method: 'POST',
    url,
    data: form,
    headers: { 'Content-Type': 'multipart/form-data' },
    onUploadProgress: (event) => {
      if (!onProgress) return
      const total = event.total ?? 0
      onProgress(total > 0 ? Math.round((event.loaded / total) * 100) : null)
    },
  })
}

/**
 * 取二进制（PDF 原文件）。
 *
 * 这条路径不回 {code,message,data} 包络：成功就是裸字节。失败时后端仍会回包络，
 * 所以响应头是 JSON 时按 ApiError 解析，让 40101/40401 与其它接口表现一致。
 */
export async function getBinary(url: string): Promise<ArrayBuffer> {
  try {
    const response = await http.request<ArrayBuffer>({ method: 'GET', url, responseType: 'arraybuffer' })
    const contentType = String(response.headers['content-type'] ?? '')
    if (contentType.includes('application/json')) {
      const text = new TextDecoder('utf-8').decode(response.data)
      let body: Envelope<unknown> | null = null
      try {
        body = JSON.parse(text) as Envelope<unknown>
      } catch {
        body = null
      }
      if (body && typeof body.code === 'number') {
        if (body.code === 40101) notifyUnauthorized()
        throw new ApiError(body.code, body.message || '读取文件失败', body.fields, body.retryable ?? false)
      }
      throw new ApiError(-1, '读取文件失败：后端返回了非二进制内容')
    }
    if (response.data.byteLength === 0) {
      throw new ApiError(-1, '文件内容为空，无法解析')
    }
    return response.data
  } catch (error) {
    throw toApiError(error)
  }
}
