// 一次流式回答的进度状态机（纯函数）。
//
// 为什么要单独抽出来：等待态必须由真实的 SSE 事件驱动，而 Vue 组件没有
// 组件测试库可依赖（项目只有 node --test）。把"事件 → 状态"写成纯函数，
// 就能用一串假的事件序列把「收到第一个 delta 后状态行消失」钉死。

import type { ToolStatus } from '@/utils/sse'
// 运行时 import 必须写相对路径带扩展名：node --test 不认 `@` 别名。
import { waitingStage, type WaitingStage } from './waiting.ts'

export type ChatPhase = 'waiting' | 'streaming' | 'done' | 'stopped' | 'error'

export interface ChatProgress {
  phase: ChatPhase
  /** 这一轮开始的本机时间（Date.now()）。 */
  startedAt: number
  /** 已收到的 citation 条数（后端逐条推，这里只数，不去重） */
  citations: number
  /** 检索工具的真实状态。null = 还没收到 tool_call */
  retrieval: ToolStatus | null
  /** 首个 delta 到达的时间。null 表示正文还没开始 */
  firstDeltaAt: number | null
}

export type ProgressEvent =
  | { type: 'meta' }
  | { type: 'tool_call'; status: ToolStatus; detail: string }
  | { type: 'citation' }
  | { type: 'delta' }
  | { type: 'done' }
  | { type: 'stopped' }
  | { type: 'error'; message: string }

export function startProgress(now: number): ChatProgress {
  return { phase: 'waiting', startedAt: now, citations: 0, retrieval: null, firstDeltaAt: null }
}

/** applyProgress 是状态机的唯一入口：组件只负责把事件喂进来。 */
export function applyProgress(progress: ChatProgress, event: ProgressEvent, now: number): ChatProgress {
  switch (event.type) {
    case 'meta':
      // meta 只是确认服务端接上了，界面不必变——状态行本来就在。
      return progress
    case 'tool_call':
      return { ...progress, retrieval: event.status }
    case 'citation':
      return { ...progress, citations: progress.citations + 1 }
    case 'delta':
      // 第一个字节到了：等待态结束，状态行整行移除（不留"已完成"的尾巴）。
      return progress.firstDeltaAt !== null
        ? progress
        : { ...progress, phase: 'streaming', firstDeltaAt: now }
    case 'done':
      return { ...progress, phase: 'done' }
    case 'stopped':
      return { ...progress, phase: 'stopped' }
    case 'error':
      return { ...progress, phase: 'error' }
    default:
      return progress
  }
}

export interface StatusLine extends WaitingStage {
  /** false 时整行不渲染（正文开始了、或者已经结束/失败）。 */
  visible: boolean
}

/**
 * statusLine 算出此刻该显示的状态行。
 *
 * 失败与中断不在这里显示：气泡下方已经有专门的错误行与「已中断」标记，
 * 再在状态行重复一遍就是两句话说同一件事。
 */
export function statusLine(progress: ChatProgress, now: number): StatusLine {
  if (progress.phase !== 'waiting') {
    return { visible: false, text: '', showSeconds: false }
  }
  const stage = waitingStage(now - progress.startedAt, progress.citations, {
    // 只有 ok / failed 才算"检索结束"。running 是刚开始，citation 可能还没到，
    // 这时候把窗口让出去会先写「未检索到直接相关法条」，半秒后卡片又冒出来。
    retrievalDone: progress.retrieval === 'ok' || progress.retrieval === 'failed',
  })
  return { visible: true, ...stage }
}
