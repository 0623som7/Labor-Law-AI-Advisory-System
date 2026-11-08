import type { Citation } from '@/utils/sse'

/** 历史消息里我们关心的字段。 */
export interface HistoryMessage {
  role: 'user' | 'assistant'
  content: string
  /** assistant 消息引用的法条；user 消息与老数据都没有这个字段。 */
  citations?: Citation[]
}

/** 由历史消息还原出来的气泡数据。 */
export interface HistoryBubble {
  role: 'user' | 'assistant'
  content: string
  citations: Citation[]
  fromHistory: true
}

/**
 * historyBubbles 把接口返回的历史消息映射成气泡。
 *
 * 单独抽成纯函数有两个原因：
 *   1. 引用卡片能不能在刷新后重建，全取决于这一步有没有把 citations 带过来，
 *      而它又不能靠"看一眼页面"验证；
 *   2. 项目只有 node:test（没有 vitest / @vue/test-utils，也不打算为此加依赖），
 *      挂在 SFC 里的逻辑无法直接测，抽出来才测得动。
 *
 * citations 缺失时给空数组而不是补默认法条：库里没有引用就是没有，
 * 补一条出来等于编造法条。
 */
export function historyBubbles(items: readonly HistoryMessage[]): HistoryBubble[] {
  return items.map((m) => ({
    role: m.role,
    content: m.content,
    citations: m.citations ? [...m.citations] : [],
    fromHistory: true as const,
  }))
}
