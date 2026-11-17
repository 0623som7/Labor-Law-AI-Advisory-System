// 对话里合同文件卡片的文案。
//
// 抽成纯函数而不是写在 SFC 里：项目只有 node:test（没有 vitest / @vue/test-utils，
// 也不打算为此加依赖），挂在模板里的字符串拼不出来就测不到，而"页数为 0 显示成
// 0 页"这类错误恰恰是答辩时最容易被追问的那一格。

import type { Attachment } from '@/api/types'

/** 卡片上文件名与页数之间的分隔符。用中点而不是竖线：中文界面里竖线像表格。 */
const SEP = ' · '

/**
 * fileCardLabel 生成卡片副标题，形如「劳动合同.pdf · 4 页」。
 *
 * page_count 为 0 表示解析还没回填页数（上传成功即发消息就会遇到），
 * 这时写「页数待解析」而不是「0 页」：0 页是一份不存在的合同，
 * 用户会以为文件传坏了。
 */
export function fileCardLabel(file: Pick<Attachment, 'filename' | 'page_count'>): string {
  // 上传时的原文件名可能带首尾空格（拖拽/复制路径常见），直接拼会多出两个空格。
  const trimmed = file.filename.trim()
  const name = trimmed === '' ? '未命名文件' : trimmed
  const pages = file.page_count > 0 ? `${file.page_count} 页` : '页数待解析'
  return `${name}${SEP}${pages}`
}
