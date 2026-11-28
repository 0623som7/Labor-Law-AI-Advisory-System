/**
 * 侧栏会话标题。
 *
 * 后端建会话时把用户原话截 20 字当标题（`internal/api/chat.go` 的 `chatSessionTitleRunes`）。
 * 从审查页追问时原话是 `合同「第七条」 · 合同中有 2 处互不相同的试用期约定…`，
 * 截完只剩前半句——同一个条款问三次，三条会话长得一模一样，侧栏分不出是哪一次。
 *
 * 这里不改后端也不改存储，只在渲染层把标题重新拼一次：文件名（有就带）+ 条款号 +
 * 规则号，都比那句"证据首句"更适合当标题。
 */

/** 文件名保留多少个字，超出截断。18 字能容下 `contract-demo.pdf` 这类常见名。 */
const FILE_MAX = 18
/** 条款号保留多少个字。 */
const SECTION_MAX = 10
/** 整条标题的上限。侧栏一栏只有两百多像素，再长也是省略号。 */
const TITLE_MAX = 30

const ELLIPSIS = '…'

function clip(text: string, max: number): string {
  const chars = [...text]
  return chars.length > max ? chars.slice(0, max).join('') + ELLIPSIS : text
}

/** 去掉扩展名：「劳动合同.pdf」→「劳动合同」。只认最后一段且不长于 5 个字符的后缀。 */
export function stripExtension(name: string): string {
  const trimmed = name.trim()
  const dot = trimmed.lastIndexOf('.')
  if (dot <= 0) return trimmed
  const ext = trimmed.slice(dot + 1)
  if (ext === '' || ext.length > 5 || /[\\/]/.test(ext)) return trimmed
  return trimmed.slice(0, dot)
}

/**
 * 规则号的形状：`R-PROBATION-ONCE`。
 *
 * 不允许 `R-` 后面直接跟 `-`，否则 `合同「第七条」 · -` 这种残片也会被当成规则号。
 */
const RULE_ID = '[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)+'

/**
 * 从一段"原话标题"里认出条款号与规则号；认不出就返回空串。
 *
 * 两种形态都要认，因为它们同时存在于库里的历史会话中：
 * - 老形态（`buildAskContext` 时代）：`… · 结论 R-PROBATION-ONCE`
 * - 新形态（后端 `structuredTitle` 时代）：`合同「第七条」 · R-PROBATION-ONCE`
 * 只认「结论」前缀时，新形态会解析出空规则号，侧栏退回只剩「第七条」——
 * 等于后端的 T4 修复在页面上没发生。
 */
export function parseAskTitle(raw: string): { section: string; ruleId: string } {
  const section = /合同「([^」]*)」/.exec(raw)?.[1]?.trim() ?? ''
  const ruleId =
    new RegExp(`结论\\s+(${RULE_ID})`).exec(raw)?.[1]?.trim() ??
    new RegExp(`·\\s*(${RULE_ID})\\s*$`).exec(raw)?.[1]?.trim() ??
    ''
  return { section, ruleId }
}

/**
 * 拼一条标题。
 *
 * 优先级：文件名 + 条款 → 条款 + 规则号 → 条款 → fallback。
 * 文件名与条款号能让人分辨"哪份合同的哪一条"，规则号是给答辩/排查用的，放最后。
 */
export function sessionTitle(input: {
  fileName?: string
  section?: string
  ruleId?: string
  fallback?: string
}): string {
  // 文件名分支保留扩展名：侧栏这行字要回答"这条对话在说哪个文件"，
  // 剥掉 .pdf 之后 contract-demo / sample-v5 / 劳动合同-李小雨 这些名字
  // 光看前面分不出是文档还是表格。标题分支（只有条款号那种）仍走 stripExtension
  // 之外的逻辑，一个字没动。
  const file = (input.fileName ?? '').trim()
  const section = (input.section ?? '').trim()
  const ruleId = (input.ruleId ?? '').trim()
  const fallback = (input.fallback ?? '').trim()

  const parts: string[] = []
  if (file !== '') {
    // 有文件名：文件名 + 条款。规则号不进标题，免得一条挤成三个 · 分段的串。
    parts.push(clip(file, FILE_MAX))
    if (section !== '') parts.push(clip(section, SECTION_MAX))
  } else if (section !== '') {
    // 没文件名：条款 + 规则号。规则号是"哪条规则报的"，排查时要看。
    parts.push(clip(section, SECTION_MAX))
    if (ruleId !== '') parts.push(ruleId)
  } else if (ruleId !== '') {
    parts.push(ruleId)
  }

  if (parts.length === 0) return clip(fallback, TITLE_MAX)
  // TITLE_MAX - 1：clip 截断时会把省略号追加在后面，用 TITLE_MAX 会得到
  // TITLE_MAX + 1 个字符。保留扩展名后拼起来更容易触到这个边界，
  // 顺手把"整条不超过 30 字"这条约束修成真的。
  return clip(parts.join(' · '), TITLE_MAX - 1)
}

/**
 * 侧栏副标题：`文件名 · 时间`，没有文件名时只显示时间。
 *
 * 文件名在这里只做一次 clip，超宽的单行省略交给 CSS：侧栏一栏两百多像素，
 * 用 JS 按像素算宽会在字体没加载完时算错，换字号也不会跟着变。
 */
export function sessionSubtitle(fileName: string | undefined, time: string): string {
  const file = (fileName ?? '').trim()
  if (file === '') return time
  return `${clip(file, FILE_MAX)} · ${time}`
}

/**
 * 同名去重：第二次出现起追加 ` #2`、` #3`……
 *
 * 只在渲染层做，按传入顺序（列表是时间倒序）编号——最新的那条不带后缀，
 * 越往下越旧，所以 #2 比 #1 旧，符合"往下翻是历史"的直觉。
 */
export function dedupeTitles(titles: string[]): string[] {
  const seen = new Map<string, number>()
  return titles.map((t) => {
    const n = (seen.get(t) ?? 0) + 1
    seen.set(t, n)
    return n === 1 ? t : `${t} #${n}`
  })
}
