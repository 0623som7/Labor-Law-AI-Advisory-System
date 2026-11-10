// 与后端 REST 接口一一对应的类型。
//
// 字段名逐字照抄 contract/openapi.yaml 的 components.schemas：
// 后端有专门的 DTO 层把 port 结构体转成 snake_case，这里再手写一遍是为了
// 让 TS 的严格模式能在编译期抓出"后端改了字段名前端还在用旧名字"。
//
// SSE 的事件类型不在这里重复定义，直接从 utils/sse 再导出，
// 两处各写一份必然会漂移。

export type { Citation, SSEEvent, ToolStatus } from '@/utils/sse'

// 再导出的同时也要在本文件里可见：ChatMessage.citations 用到它。
import type { Citation } from '@/utils/sse'

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
  /** assistant 消息引用的法条；user 消息没有该字段。刷新历史后据此重建卡片。 */
  citations?: Citation[]
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

// ---- 文件与审查（第二轮） ----

/** components.schemas.FileRecord */
export interface FileRecord {
  id: string
  filename: string
  ext: string
  size_bytes: number
  page_count: number
  ocr_used: boolean
  confidence: number
  created_at: string
}

/** components.schemas.TextBlock。bbox 是 pt、原点左下、y 向上，换算只走 utils/pdf/coords。 */
export interface TextBlock {
  id: string
  page: number
  bbox: [number, number, number, number]
  text: string
  char_start: number
  char_end: number
}

/** components.schemas.ParsedPage */
export interface ParsedPage {
  index: number
  /** pt，A4 = 595.32 × 841.92 */
  width: number
  height: number
  blocks: TextBlock[]
}

/** components.schemas.ParsedDoc（GET /files/{id}/pages 的 data，整份一次返回） */
export interface ParsedDoc {
  file_id: string
  page_count: number
  ocr_used: boolean
  confidence: number
  pages: ParsedPage[]
}

/** components.schemas.ReviewAccepted（POST /contracts/review 的 202 载荷） */
export interface ReviewAccepted {
  task_id: string
  review_id: string
  status?: TaskStatus
}

export type TaskStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled'

/** components.schemas.Task */
export interface Task {
  id: string
  type: string
  status: TaskStatus
  progress: number
  stage: string
  error: string
  attempt: number
  created_at: string
  updated_at: string
}

export interface ReviewCounts {
  high: number
  medium: number
  low: number
}

export type Severity = 'high' | 'medium' | 'low'

/** components.schemas.Review */
export interface Review {
  id: string
  file_id: string
  /** 上传时的原文件名，只用于展示。后端补不到时是空串。 */
  filename?: string
  status: TaskStatus
  risk_level: Severity
  score: number
  clause_pass_rate: number
  counts: ReviewCounts
  task_id: string
  created_at: string
  updated_at: string
}

/** components.schemas.ClauseRef。page 是 0 起。 */
export interface ClauseRef {
  section: string
  page: number
  bbox: [number, number, number, number]
}

/** components.schemas.LegalBasis */
export interface LegalBasis {
  law: string
  article: string
  text: string
}

/** components.schemas.Suggestion */
export interface ReviewSuggestion {
  type: 'rewrite' | 'add' | 'delete'
  original: string
  revised: string
  reason: string
}

/** components.schemas.Finding */
export interface Finding {
  id: string
  review_id: string
  rule_id: string
  severity: Severity
  title: string
  clause_ref: ClauseRef
  evidence: string
  legal_basis: LegalBasis[]
  suggestion: ReviewSuggestion
  /** 当前后端恒为 null，界面不渲染它 */
  economic_impact: number | null
}
