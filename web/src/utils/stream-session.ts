// 一次流式回答的生命周期。
//
// 单独成模块而不是内联在 ChatView 里，是为了能在 node --test 里验证：组件挂载/
// 卸载需要 DOM，而这里要守住的规则只有一条——会话作废（用户点了停止，或者页面
// 已经卸载）之后，还在路上的事件绝不能再改 messages。
//
// 缺陷形态：没有 onBeforeUnmount，用户发出问题就切走页面，SSE 的 onDelta 仍然
// 持着组件里的 assistant 对象继续 push 文本；退出登录时同理，流还在跑，令牌已经
// 清掉，下一帧还可能触发 40101 再跳一次登录页。

export interface StreamSession {
  /** 是否已经作废。作废后事件处理器必须直接 return。 */
  closed: () => boolean
  /** 绑定底层的 AbortController；若此前已作废，立刻 abort 它。 */
  bind: (controller: AbortController) => void
  /** 作废：置标记并取消 fetch。幂等，重复调用无副作用。 */
  close: () => void
}

export function createStreamSession(): StreamSession {
  let controller: AbortController | null = null
  let closed = false

  return {
    closed: () => closed,
    bind(next) {
      // 先作废、后绑定的顺序是真实会发生的（卸载发生在 await 之前）：
      // 这时不能把 controller 存下来当作还能用，必须当场取消。
      if (closed) {
        next.abort()
        return
      }
      controller = next
    },
    close() {
      closed = true
      controller?.abort()
    },
  }
}
