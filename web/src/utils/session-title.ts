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

/** 文件名保留多少个字，超出截断。 */
const FILE_MAX = 12
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

/** 从一段"原话标题"里认出条款号与规则号；认不出就返回空串。 */
export function parseAskTitle(raw: string): { section: string; ruleId: string } {
  const section = /合同「([^」]*)」/.exec(raw)?.[1]?.trim() ?? ''
  const ruleId = /结论\s+([A-Z][A-Z0-9-]{2,})/.exec(raw)?.[1]?.trim() ?? ''
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
  const file = stripExtension(input.fileName ?? '')
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
  return clip(parts.join(' · '), TITLE_MAX)
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
