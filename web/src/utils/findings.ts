// 审查结果的纯函数：汇总、按页过滤、严重度文案。
//
// 抽出来只为一件事：这些判断能在 Node 里直接测，不用挂载组件。
// 组件里只留渲染与交互。

import type { Finding, Severity } from '@/api/types'
// 相对路径带扩展名：单测用 node --test 直接跑 .ts，`@/` 别名只有 TS 与 Vite 认得。
import { findingPage } from './pdf/coords.ts'

export const SEVERITY_ORDER: Severity[] = ['high', 'medium', 'low']

export const SEVERITY_LABEL: Record<Severity, string> = {
  high: '高',
  medium: '中',
  low: '低',
}

export interface FindingsSummary {
  high: number
  medium: number
  low: number
  total: number
}

/** 顶部汇总。三类都要显示，0 也显示——"低风险 0"是结论，不是没数据。 */
export function summarizeFindings(findings: readonly Finding[]): FindingsSummary {
  const summary: FindingsSummary = { high: 0, medium: 0, low: 0, total: findings.length }
  for (const f of findings) {
    if (f.severity === 'high' || f.severity === 'medium' || f.severity === 'low') {
      summary[f.severity] += 1
    }
  }
  return summary
}

/**
 * 落在第 pageIndex 页（0 起）的风险条目。
 *
 * 页号换算只走 findingPage：那里已经把缺失、负数、非整数收口成 0，
 * 组件里再写一遍 `?? 0` 就会出现两套口径。
 */
export function findingsOnPage(findings: readonly Finding[], pageIndex: number): Finding[] {
  return findings.filter((f) => findingPage(f) === pageIndex)
}

/** 严重度排序：高在前，同级保持后端返回的顺序（后端已按严重度倒序）。 */
export function sortBySeverity(findings: readonly Finding[]): Finding[] {
  const rank = (s: Severity): number => {
    const i = SEVERITY_ORDER.indexOf(s)
    return i === -1 ? SEVERITY_ORDER.length : i
  }
  return [...findings].sort((a, b) => rank(a.severity) - rank(b.severity))
}

/** 法条依据的折叠标题：多条时列条号，一条时只列那一条。 */
export function legalBasisTitle(f: Finding): string {
  const articles = (f.legal_basis ?? []).map((b) => b.article).filter((a) => a)
  return articles.length > 0 ? `法律依据（${articles.join('、')}）` : '法律依据'
}
