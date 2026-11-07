// REST 客户端。
//
// 三件事都在这里做掉，视图层就不再重复：
//   1. 解包络——后端永远回 {code,message,data}，code !== 0 一律抛 ApiError；
//   2. 带令牌——请求拦截器统一加 Authorization，页面里不出现 token；
//   3. 会话失效——收到 40101（或 HTTP 401）就清令牌并通知外层跳登录页。
//
// 流式接口不走这里（axios 处理不了边收边解），见 api/chat.ts。

import axios, { type AxiosRequestConfig } from 'axios'

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
 * 未登录/令牌过期的回调。
 *
 * 用回调而不是在这里 import router：api 层被 store 层引用，router 又引用 store，
 * 直接 import 会绕成环。由 main.ts 在启动时注册一次。
 */
let unauthorizedHandler: (() => void) | null = null

export function onUnauthorized(handler: () => void): void {
  unauthorizedHandler = handler
}

export function notifyUnauthorized(): void {
  clearTokens()
  unauthorizedHandler?.()
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

const http = axios.create({
  // 后端路由前缀是 /api/v1；开发期由 Vite 代理到 127.0.0.1:8080，部署期由 nginx 反代。
  baseURL: '/api/v1',
  timeout: 30000,
  headers: { 'Content-Type': 'application/json' },
})

http.interceptors.request.use((config) => {
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
    if (error.response?.status === 401) {
      notifyUnauthorized()
    }
    if (error.code === 'ECONNABORTED') {
      return new ApiError(-1, '请求超时，请重试')
    }
    return new ApiError(error.response?.status ?? -1, error.message || '网络异常，请检查后端是否在运行')
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
