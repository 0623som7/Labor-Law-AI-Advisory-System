// 风险清单的筛选与计数。
//
// 为什么单开一个文件而不是塞进 findings.ts：那边回答"这份合同有几条、在第几页"，
// 这里回答"我现在想看哪几条"。两件事混在一处，测试的意图就糊了。
//
// 纯函数、无 Vue 依赖：node --test 直接跑，组件里只剩绑定。

import type { Finding, Severity } from '@/api/types'

export type SeverityFilter = 'all' | Severity

export interface FindingsFilter {
  severity: SeverityFilter
  query: string
}

export interface SeverityCounts {
  all: number
  high: number
  medium: number
  low: number
}

/**
 * 命中范围：标题、证据句、条款位置、规则号。
 *
 * 包含而不是整词匹配：用户搜「试用期」要能命中「试用期超过六个月」这种句子；
 * 四段字段缺一段就当空串（后端契约里都有，但界面不该因为一条脏数据整列消失）。
 */
function haystack(f: Finding): string[] {
  return [f.title ?? '', f.evidence ?? '', f.clause_ref?.section ?? '', f.rule_id ?? '']
}

/**
 * 按严重度 + 关键词过滤，**保持入参顺序**。
 *
 * 顺序不能重排：入参已经是 sortBySeverity 排好的（高在前、同级按后端顺序），
 * 这里再排一次会让"同级里谁先谁后"变成另一套口径，卡片和图上框的编号对不上。
 */
export function filterFindings(list: readonly Finding[], opt: FindingsFilter): Finding[] {
  const q = opt.query.trim().toLowerCase()
  return list.filter((f) => {
    if (opt.severity !== 'all' && f.severity !== opt.severity) return false
    if (q === '') return true
    return haystack(f).some((field) => field.toLowerCase().includes(q))
  })
}

/** 各类条数。all 是总数，不是"筛选后的条数"该用的值。 */
export function countBySeverity(list: readonly Finding[]): SeverityCounts {
  const counts: SeverityCounts = { all: list.length, high: 0, medium: 0, low: 0 }
  for (const f of list) {
    if (f.severity === 'high' || f.severity === 'medium' || f.severity === 'low') {
      counts[f.severity] += 1
    }
  }
  return counts
}
