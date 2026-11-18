/**
 * 两个输入框共用的发送判定。
 *
 * 对话页原来是 Ctrl/Cmd+Enter 发送、单独 Enter 换行；审查页抽屉反过来是
 * Enter 发送。同一个产品两种规矩，用户按错过一次就不敢按了。这里只留一份规则：
 * Enter 发送、Shift+Enter 换行，Ctrl/Cmd 按不按都一样。
 */
export function shouldSendOnKey(ev: {
  key: string
  shiftKey: boolean
  ctrlKey: boolean
  metaKey: boolean
  isComposing?: boolean
}): boolean {
  if (ev.key !== 'Enter') return false
  // 中文输入法选词时按 Enter 是确认候选词，不是发送。
  // ev.isComposing 在部分浏览器是 undefined，按没在组字处理。
  if (ev.isComposing === true) return false
  if (ev.shiftKey) return false
  // ctrlKey / metaKey 故意不参与判断：按住 Ctrl 打 Enter 也应当发送。
  return true
}
