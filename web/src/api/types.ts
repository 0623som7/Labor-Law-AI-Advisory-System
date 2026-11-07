// 与后端 REST 接口一一对应的类型。
//
// 字段名逐字照抄 contract/openapi.yaml 的 components.schemas：
// 后端有专门的 DTO 层把 port 结构体转成 snake_case，这里再手写一遍是为了
// 让 TS 的严格模式能在编译期抓出"后端改了字段名前端还在用旧名字"。
//
// SSE 的事件类型不在这里重复定义，直接从 utils/sse 再导出，
// 两处各写一份必然会漂移。

export type { Citation, SSEEvent, ToolStatus } from '@/utils/sse'

/** 所有接口的统一外层包络。code === 0 才算成功。 */
export interface Envelope<T> {
  code: number
  message: string
  retryable?: boolean
  data?: T
  fields?: Record<string, string>
  trace_id?: string
}

/** components.schemas.User */
export interface User {
  id: string
  phone_masked: string
  nickname: string
  created_at: string
}

/** components.schemas.AuthResult。注册与登录都返回它。 */
export interface AuthResult {
  user: User
  access_token: string
  refresh_token: string
  /** access token 剩余秒数 */
  expires_in: number
}

export type Scene = 'consult' | 'qa'

/** components.schemas.Session */
export interface Session {
  id: string
  title: string
  scene: Scene
  created_at: string
  updated_at: string
}

export type MessageRole = 'user' | 'assistant'

/** components.schemas.ChatMessage */
export interface ChatMessage {
  id: string
  session_id: string
  role: MessageRole
  content: string
  created_at: string
}

/** components.schemas.ChatRequest */
export interface ChatRequest {
  input: string
  session_id?: string
}

/** 本地存的令牌。expires_at 是绝对毫秒时间戳，方便算是否过期。 */
export interface TokenPair {
  access_token: string
  refresh_token: string
  expires_at: number
}
