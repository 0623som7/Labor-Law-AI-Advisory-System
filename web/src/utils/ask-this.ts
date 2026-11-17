import type { Finding } from '@/api/types'

/** 预填句子的总长度上限。
 *
 *  对话输入上限是 4000 字，这里只留 200：预填的目的是替用户把"哪一条、
 *  什么毛病"说清楚，剩下的由他自己补。填满一屏反而要删。 */
const MAX_LEN = 200

/** 句子末尾固定带这一问。
 *
 *  审查页看到风险后最常见的下一句就是"那我能怎么办"，写死它可以让用户
 *  直接回车，也可以接着往下改。 */
const TAIL = '公司不同意改，我该怎么办？'

/** 把一条审查结论变成一句可以直接发给顾问的追问。
 *
 *  只用条款位置、标题与证据三样：这三样是规则引擎自己的输出，不含模型生成内容，
 *  所以预填的句子不会替用户"编"出一个他没问过的事实。 */
export function buildAskQuestion(f: Finding): string {
  const section = (f.clause_ref?.section ?? '').trim()
  const head = section ? `合同「${section}」这一条被判为「${f.title}」` : `这份合同被判为「${f.title}」`
  const evidence = (f.evidence ?? '').trim()

  // 证据为空时不拼"：＋空"，否则会出现一句以冒号结尾的残句。
  const body = evidence ? `${head}：${evidence}` : head
  // 截断只切证据那一段：末尾那一问必须留着，否则预填出来的是一个问不出口的残句。
  const room = MAX_LEN - TAIL.length - 1 - 1
  const clipped = body.length > room ? `${body.slice(0, Math.max(room, 0))}…` : body
  return `${clipped}\n${TAIL}`
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
