import type { Finding } from '@/api/types'

/** 上下文一行的长度上限。
 *
 *  这一行会拼在用户问题前面发给模型，也会沉淀进历史气泡，所以必须短：
 *  超过一屏的"前言"比没有前言更难读，用户还得滚动才看得到自己问的那句。 */
const MAX_LEN = 120

/** 把一条审查结论变成一行"正在追问什么"的上下文。
 *
 *  只陈述、不提问——问什么由用户决定。这三样（条款位置、证据首句、规则 ID）
 *  全部来自规则引擎，不含模型生成内容，所以不会替用户编出一个他没问过的事实。
 *
 *  为什么必须有这一行：后端 chatRequest 只有 input/session_id/file_id，
 *  FileID 不过编排层（internal/api/chat.go 的注释写明 Runner 不感知附件），
 *  模型看不到合同正文。不把条款号和证据塞进 input，它就只能泛泛而谈。 */
export function buildAskContext(f: Finding): string {
  const section = (f.clause_ref?.section ?? '').trim() || '该条款'
  // 只取证据的第一句：证据常有两三句，全带上会把这一行撑到两倍长。
  const reason = (f.evidence ?? '').trim().split(/[。；]/)[0].trim()

  const parts = [`合同「${section}」`]
  if (reason !== '') parts.push(reason)
  if (f.rule_id !== '') parts.push(`结论 ${f.rule_id}`)

  const line = parts.join(' · ')
  return line.length > MAX_LEN ? `${line.slice(0, MAX_LEN)}…` : line
}

/** 路由里带了 ask 参数、且不是空串时才预填。 */
export function shouldPrefill(query: Record<string, unknown>): boolean {
  const ask = query?.ask
  return typeof ask === 'string' && ask.trim() !== ''
}

/** 来源条里条款号最多显示多少字。
 *
 *  `clause_ref.section` 是规则引擎从合同正文里截出来的，正常是「第四条」这种；
 *  真遇到奇怪的解析结果，宁可截断也不让一条来源把输入框顶下去。 */
const SECTION_MAX = 20

/** 对话页顶部来源条文案；没有来源时返回空串，调用方据此决定是否渲染。 */
export function contextLabel(query: Record<string, unknown>): string {
  if (query?.from !== 'review') return ''
  const section = typeof query.section === 'string' ? query.section.trim() : ''
  if (section === '') return '正在追问：这份合同的审查结论'
  const shown = section.length > SECTION_MAX ? `${section.slice(0, SECTION_MAX)}…` : section
  return `正在追问：合同「${shown}」`
}

/** 「查看原文」能不能给：必须有 file，否则点到 /review/ 空参数。 */
export function contextFileId(query: Record<string, unknown>): string {
  const file = query?.file
  return typeof file === 'string' ? file.trim() : ''
}
