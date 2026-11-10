// 会话失效的唯一出口。
//
// 名字里带 -expiry 是为了和 api/session.ts（会话 CRUD：listSessions 等）区分开，
// 两者不是一回事：那个是"对话会话"的增删查，这个是"登录会话过期"的信号。
//
// 为什么不在这里直接跳转：跳转会丢掉当前页面和输入框里的草稿，而用户可能
// 正在读一段很长的回答。所以这里只发一个"失效了"的信号，由 main.ts 决定
// 怎么提示（模态框）、什么时候跳（用户点确认之后）。
//
// 幂等：一次失效期间可能有七八个并发请求同时 40101，不能让每一条都弹一个框。

type Listener = () => void

let listener: Listener | null = null
let prompted = false

export function onSessionExpired(fn: Listener): void {
  listener = fn
}

/** 抛出"会话失效"。同一次失效只生效一次，直到 resetSessionExpired() 被调用。 */
export function notifySessionExpired(): void {
  if (prompted) return
  prompted = true
  listener?.()
}

/** 登录成功、或在登录页上收到失效信号时必须调用，否则下次失效弹不出来。 */
export function resetSessionExpired(): void {
  prompted = false
}
