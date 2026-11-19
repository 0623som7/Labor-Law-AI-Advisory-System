// 顶栏面包屑。抽成纯函数是因为两个页面写的是同一串路径，但截断位置不同：
// 纯模板里写死两次，改一次漏一次，测试也测不到。

export type CrumbTarget = 'chat' | 'upload'

export interface Crumb {
  label: string
  /** 省略表示"当前页"，渲染成纯文本，不可点。 */
  to?: CrumbTarget
}

/**
 * page 决定最后一段是什么：
 * - upload：对话咨询 / 合同审查（当前）
 * - review：对话咨询 / 合同审查 / 审查结果（当前）
 */
export function breadcrumbFor(page: 'upload' | 'review'): Crumb[] {
  if (page === 'upload') {
    return [{ label: '对话咨询', to: 'chat' }, { label: '合同审查' }]
  }
  return [
    { label: '对话咨询', to: 'chat' },
    { label: '合同审查', to: 'upload' },
    { label: '审查结果' },
  ]
}
