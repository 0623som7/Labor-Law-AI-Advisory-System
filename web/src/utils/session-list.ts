import type { Session } from '@/api/types'

/**
 * 把一条会话插到侧栏最前面（已存在则原样返回）。
 *
 * 深链接打开一个不在首屏 50 条里的老会话时，正文能渲染但侧栏没有它，
 * 用户会以为"打开的是个不存在的东西"。插进列表头既补上这一条，
 * 也顺手做了去重——接口重放、连点两次都不会出现两份同名条目。
 */
export function prependSession(list: Session[], session: Session): Session[] {
  if (list.some((s) => s.id === session.id)) return list
  return [session, ...list]
}
