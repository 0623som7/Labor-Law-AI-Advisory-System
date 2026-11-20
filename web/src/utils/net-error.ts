/**
 * 断网判断：只回答"这个错误是不是网络断了"，不判断业务错误码。
 *
 * 浏览器在这一层的报错文案没有规范，Chromium 是 "Failed to fetch"，
 * Firefox 是 "Network Error"，Safari 是 "Load failed"，Node 端（e2e）是
 * "fetch failed"，所以按文案而不是错误类型判。navigator.onLine 为 false 时
 * 不管错误长什么样都算断网——那是浏览器自己给的确定信号。
 */
const OFFLINE_MESSAGES = ['Failed to fetch', 'Network Error', 'Load failed', 'fetch failed']

/** err 传 null 表示"还没有发出请求，只想问现在是否在线"。 */
export function isOfflineError(err: unknown): boolean {
  if (typeof navigator !== 'undefined' && navigator.onLine === false) return true
  const message =
    err instanceof Error ? err.message : typeof err === 'string' ? err : ''
  const lower = message.toLowerCase()
  return OFFLINE_MESSAGES.some((mark) => lower.includes(mark.toLowerCase()))
}

/**
 * 网络层失败：断网、连接被重置、代理挂掉。
 *
 * fetch 抛的 TypeError 在 api/chat.ts 里已经被换成 ApiError(-1, "网络连接失败…")，
 * 所以到了视图层光看文案已经认不出原始错误，得按 code 认；但 -1 同时也给
 * "浏览器不支持流式响应"用，所以还要文案里带"网络"才算。
 */
export function isNetworkFailure(err: unknown): boolean {
  if (isOfflineError(err)) return true
  if (typeof err !== 'object' || err === null) return false
  const code = (err as { code?: unknown }).code
  const message = (err as { message?: unknown }).message
  return code === -1 && typeof message === 'string' && message.includes('网络')
}
