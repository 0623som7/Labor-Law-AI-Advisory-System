// 审查页的"要不要给开始按钮"。
//
// 单独抽成纯函数是因为这条规则最容易写错：只要这份合同已经有审查结果，
// 就不该再让用户点一次（后端会复用旧结果，用户等 2 秒拿到的还是同一份）。
// 纯函数能在 node --test 里直接断言，不用挂载组件。

import type { Review } from '@/api/types'

/** GET /reviews?file_id=… 的返回是一个 0/1 长度的列表，取第一条。 */
export function pickExistingReview(items: readonly Review[] | null | undefined): Review | null {
  if (!items || items.length === 0) return null
  return items[0] ?? null
}

export interface StartButtonState {
  /** 进页面时从后端恢复到的审查记录（null = 还没审过）。 */
  restored: Review | null
  running: boolean
  /** 42201：没有文字层，审查无从下手。 */
  scanned: boolean
  loadError: string
}

export function shouldOfferStart(state: StartButtonState): boolean {
  if (state.running) return false
  if (state.scanned) return false
  if (state.loadError !== '') return false
  return state.restored === null
}
