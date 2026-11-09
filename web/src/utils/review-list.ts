// 审查历史列表的纯函数：状态文案、风险文案、"还有没有下一页"。
//
// 抽出来只为一件事：这些判断能在 Node 里直接测，不用挂载组件。
// 项目没装 @vue/test-utils，为三个字符串映射引一套组件测试基建不划算。

import type { Severity, TaskStatus } from '@/api/types'

const STATUS_LABEL: Record<TaskStatus, string> = {
  pending: '排队中',
  running: '审查中',
  succeeded: '已完成',
  failed: '失败',
  cancelled: '已取消',
}

/**
 * 状态的中文文案。未知值原样返回而不是抛错：
 * 后端将来加了新状态时，列表页显示一个英文单词，好过整页白屏。
 */
export function statusLabel(status: string): string {
  return STATUS_LABEL[status as TaskStatus] ?? status
}

const RISK_LABEL: Record<Severity, string> = {
  high: '高风险',
  medium: '中风险',
  low: '低风险',
}

/** 风险等级文案。后端对"没有风险"给空串，这里单独成一个词。 */
export function riskLabel(risk: string): string {
  if (risk === '') {
    return '未评级'
  }
  return RISK_LABEL[risk as Severity] ?? risk
}

/**
 * 还有没有下一页。
 *
 * 后端 GET /reviews 只回数组、不回 total，所以只能按"这一页有没有装满"推断：
 * 装满就还可能有下一页，没装满就到底了。limit 非法（<=0）时一律认为没有下一页，
 * 否则会无限翻页。
 */
export function hasMore(count: number, limit: number): boolean {
  if (limit <= 0) {
    return false
  }
  return count >= limit
}

/**
 * 这一行能不能点进去。
 *
 * 失败和取消的记录没有结果页可看，点进去只会看到一个错误页——
 * 与其让用户点两次才明白，不如直接不给点。
 */
export function isClickable(status: string): boolean {
  return status !== 'failed' && status !== 'cancelled'
}
