// 会话 5 条。
//
// 注意返回值形状：后端 listSessions / listMessages 直接把数组放进 data
// （`OK(c, toSessionDTOs(items))`），不是 {items, total} 那种分页信封。
// 分页靠 query 参数 limit/offset，总数前端不显示，也就不需要 total。

import { del, get, post } from './client'
import type { ChatMessage, Scene, Session } from './types'

export function listSessions(limit = 50): Promise<Session[]> {
  return get<Session[]>('/sessions', { limit })
}

export function createSession(scene: Scene = 'consult', title = ''): Promise<Session> {
  return post<Session>('/sessions', { scene, title })
}

export function getSession(sessionId: string): Promise<Session> {
  return get<Session>(`/sessions/${encodeURIComponent(sessionId)}`)
}

export function deleteSession(sessionId: string): Promise<null> {
  return del<null>(`/sessions/${encodeURIComponent(sessionId)}`)
}

export function listMessages(sessionId: string, limit = 50): Promise<ChatMessage[]> {
  return get<ChatMessage[]>(`/sessions/${encodeURIComponent(sessionId)}/messages`, { limit })
}
