// SSE 帧解析。纯函数 + 一个只持有缓冲区的解析器，不碰 DOM、不碰网络，
// 所以可以直接用 `node --test src/utils/sse.test.ts` 跑。
//
// 为什么不用 EventSource：它不支持 POST，也不支持带 Authorization 头。
// 为什么要自己切帧：一次 read() 可能拿到半帧、也可能拿到三帧；
// 中文是 3 字节，被 chunk 边界切断是常态，所以 TextDecoder 必须带 stream: true。
//
// data 的形状以 contract/openapi.yaml 的 x-sse-events 为准：
//   meta      {seq, message_id, session_id, model}
//   delta     {seq, text}
//   citation  {seq, citation:{statute_id, law, article, text, url}}
//   tool_call {seq, tool:{tool, status, detail}}
//   done      {seq, message_id, finish_reason, usage:{...}}
//   error     {seq, code, message}
//
// 注意：事件类型在 `event:` 行，data 的 JSON 里没有 type 字段。

export interface Citation {
  statute_id: string
  law: string
  article: string
  text: string
  url?: string
}

export interface SSERawFrame {
  id: string
  event: string
  data: string
}

export interface MetaEvent {
  type: 'meta'
  seq: number
  message_id: string
  session_id: string
  model: string
}

export interface DeltaEvent {
  type: 'delta'
  seq: number
  text: string
}

export interface CitationEvent {
  type: 'citation'
  seq: number
  citation: Citation
}

export type ToolStatus = 'running' | 'ok' | 'failed'

export interface ToolCallEvent {
  type: 'tool_call'
  seq: number
  tool: { tool: string; status: ToolStatus; detail: string }
}

export interface Usage {
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
}

export interface DoneEvent {
  type: 'done'
  seq: number
  message_id: string
  finish_reason: string
  usage: Usage
}

export interface ErrorEvent {
  type: 'error'
  seq: number
  code: number
  message: string
}

export type SSEEvent = MetaEvent | DeltaEvent | CitationEvent | ToolCallEvent | DoneEvent | ErrorEvent

/** 契约里定义的事件闭集。未知事件名一律 warn 后跳过，不能抛异常也不能白屏。 */
const KNOWN_EVENTS = ['meta', 'delta', 'citation', 'tool_call', 'done', 'error'] as const

export function isKnownEvent(name: string): boolean {
  return (KNOWN_EVENTS as readonly string[]).includes(name)
}

/** 把 \r\n 与单独的 \r 统一成 \n。服务端可能用 CRLF，浏览器不保证。 */
export function normalizeNewlines(text: string): string {
  return text.replace(/\r\n?/g, '\n')
}

/**
 * 解析一帧（已经按空行切开、不含结尾空行）。
 *
 * 多行 data 用 \n 拼接：本后端不发多行，但 SSE 规范允许，前端不该在这上面出错。
 * 只有注释行（以 ':' 开头）的帧返回 null —— 那是心跳，不是事件。
 */
export function parseFrame(raw: string): SSERawFrame | null {
  if (raw.trim() === '') return null

  let id = ''
  let event = 'message'
  const data: string[] = []

  for (const line of normalizeNewlines(raw).split('\n')) {
    if (line === '') continue
    if (line.startsWith(':')) continue

    const colon = line.indexOf(':')
    const field = colon < 0 ? line : line.slice(0, colon)
    let value = colon < 0 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)

    switch (field) {
      case 'id':
        id = value
        break
      case 'event':
        event = value === '' ? 'message' : value
        break
      case 'data':
        data.push(value)
        break
      default:
        // retry 等字段本轮不用；未知字段忽略了也不会错。
        break
    }
  }

  if (data.length === 0) return null
  return { id, event, data: data.join('\n') }
}

function asRecord(value: unknown): Record<string, unknown> | null {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return null
  return value as Record<string, unknown>
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

function asNumber(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

/**
 * 把原始帧解成带类型的消息。
 *
 * 返回 null 表示这一帧不能用：未知事件名、JSON 坏了、必需字段不是对象。
 * 这里不抛异常——一帧坏数据不该让整条流停下来，调用方 warn 后继续读下一帧。
 */
export function decodeEvent(frame: SSERawFrame): SSEEvent | null {
  if (frame.data === '') return null

  let parsed: unknown
  try {
    parsed = JSON.parse(frame.data)
  } catch {
    return null
  }
  const payload = asRecord(parsed)
  if (!payload) return null

  const seq = asNumber(payload.seq)

  switch (frame.event) {
    case 'meta':
      return {
        type: 'meta',
        seq,
        message_id: asString(payload.message_id),
        session_id: asString(payload.session_id),
        model: asString(payload.model),
      }

    case 'delta':
      return { type: 'delta', seq, text: asString(payload.text) }

    case 'citation': {
      const citation = asRecord(payload.citation)
      if (!citation) return null
      const url = asString(citation.url)
      return {
        type: 'citation',
        seq,
        citation: {
          statute_id: asString(citation.statute_id),
          law: asString(citation.law),
          article: asString(citation.article),
          text: asString(citation.text),
          ...(url === '' ? {} : { url }),
        },
      }
    }

    case 'tool_call': {
      const tool = asRecord(payload.tool)
      if (!tool) return null
      const raw = asString(tool.status)
      const status: ToolStatus = raw === 'ok' || raw === 'failed' || raw === 'running' ? raw : 'running'
      return {
        type: 'tool_call',
        seq,
        tool: { tool: asString(tool.tool), status, detail: asString(tool.detail) },
      }
    }

    case 'done': {
      const usage = asRecord(payload.usage)
      return {
        type: 'done',
        seq,
        message_id: asString(payload.message_id),
        finish_reason: asString(payload.finish_reason),
        usage: {
          prompt_tokens: asNumber(usage?.prompt_tokens),
          completion_tokens: asNumber(usage?.completion_tokens),
          total_tokens: asNumber(usage?.total_tokens),
        },
      }
    }

    case 'error':
      return {
        type: 'error',
        seq,
        code: asNumber(payload.code) || 50001,
        message: asString(payload.message),
      }

    default:
      return null
  }
}

/**
 * 有状态的切帧器：喂进任意大小的 chunk，吐出完整的帧。
 *
 * 状态只有两块：解码器（处理跨 chunk 的半个汉字）和缓冲区（处理跨 chunk 的半帧）。
 */
export class SSEParser {
  private buffer = ''
  private readonly decoder = new TextDecoder('utf-8')

  /** 喂一个字节 chunk，返回本次能凑出的完整帧（可能 0 个，也可能多个）。 */
  push(chunk: Uint8Array): SSERawFrame[] {
    this.buffer += this.decoder.decode(chunk, { stream: true })
    return this.drain(false)
  }

  /** 流结束时调用：把残留在缓冲区里的最后一帧（服务端没补空行）也吐出来。 */
  end(): SSERawFrame[] {
    this.buffer += this.decoder.decode()
    return this.drain(true)
  }

  private drain(final: boolean): SSERawFrame[] {
    this.buffer = normalizeNewlines(this.buffer)

    const frames: SSERawFrame[] = []
    for (;;) {
      const at = this.buffer.indexOf('\n\n')
      if (at < 0) break
      const raw = this.buffer.slice(0, at)
      this.buffer = this.buffer.slice(at + 2)
      const frame = parseFrame(raw)
      if (frame) frames.push(frame)
    }

    if (final) {
      const rest = this.buffer
      this.buffer = ''
      const frame = parseFrame(rest)
      if (frame) frames.push(frame)
    }

    return frames
  }
}
