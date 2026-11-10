// 对话流式接口。
//
// 为什么不用 EventSource，也不用 axios：
//   - EventSource 只能 GET、不能带 Authorization 头，而这个接口是 POST + Bearer；
//   - axios 的 responseType 没有"边收边读"，它会等整个响应体收完。
// 所以这里手写 fetch + getReader，帧解析交给 utils/sse 的 SSEParser（有单测）。
//
// 三条必须守住的规矩：
//   1. 收到 done 或 error 立刻 reader.cancel()，别让连接多挂一秒；
//   2. 一个坏帧只 warn，不中断整条流（否则一次 JSON 不完整就白屏）；
//   3. 没等到 done 就结束 = 失败（"连接中断，请重试"），不能当成功渲染半句话。

import { ApiError, ensureFreshToken, ensureFreshTokenFailure, getTokens, MSG_RELOGIN, notifyUnauthorized } from './client'
import type { ChatRequest, Envelope } from './types'
import { isKnownEvent, decodeEvent, SSEParser } from '@/utils/sse'
import type { CitationEvent, DeltaEvent, DoneEvent, MetaEvent, ToolCallEvent } from '@/utils/sse'

export interface ChatStreamHandlers {
  onMeta?: (event: MetaEvent) => void
  onDelta?: (event: DeltaEvent) => void
  onCitation?: (event: CitationEvent) => void
  onToolCall?: (event: ToolCallEvent) => void
  onDone?: (event: DoneEvent) => void
}

export interface ChatStreamResult {
  /** 用户按了"停止" */
  aborted: boolean
  /** 正常收到 done */
  done: boolean
}

/** HTTP 层就失败时，把包络里的业务码翻出来（此时还没有任何 SSE 帧）。 */
async function readErrorEnvelope(response: Response): Promise<ApiError> {
  let code = response.status
  let message = `请求失败（HTTP ${response.status}）`
  let retryable = false

  try {
    const body = (await response.json()) as Envelope<unknown>
    if (typeof body.code === 'number') code = body.code
    if (typeof body.message === 'string' && body.message !== '') message = body.message
    retryable = body.retryable ?? false
  } catch {
    // 响应体不是 JSON（例如网关的 HTML 错误页），保持上面的默认文案。
  }

  // 只在后端明确回 40101 时才清令牌：没有包络的 401 来自网关/反代，
  // 把它当成登录失效会在一次证书或上游抖动里把所有人踢下线。
  if (code === 40101) notifyUnauthorized()
  return new ApiError(code, message, undefined, retryable)
}

/**
 * 发一条消息并逐帧消费响应。
 *
 * 返回 {aborted,done} 而不是 void：调用方能区分"用户主动停"与"真的答完"，
 * 前者要把气泡标成已中断，后者才去刷新会话列表。
 * 失败一律抛 ApiError（HTTP 失败、error 事件、连接提前断开），
 * 视图层 catch 后用 error.message 直接显示——后端文案就是最终文案。
 */
export async function streamChat(
  payload: ChatRequest,
  handlers: ChatStreamHandlers,
  signal?: AbortSignal,
): Promise<ChatStreamResult> {
  // fetch 不经过 axios 拦截器，续期得自己来一次；否则两小时一到，
  // 用户打完一屏字发出去才被 40101 打回登录页。
  const usable = await ensureFreshToken()
  if (!usable) {
    if (signal?.aborted) return { aborted: true, done: false }
    const cause = ensureFreshTokenFailure()
    const err = cause instanceof ApiError ? cause : new ApiError(40101, MSG_RELOGIN)
    // 40101 必须在这里也通知一次：ensureFresh 返回 false 的两条路径里，
    // 只有"真的去换了但没换成"会调 onUnauthorized，剩下的不会。
    if (err.code === 40101) notifyUnauthorized()
    throw err
  }

  const tokens = getTokens()
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Accept: 'text/event-stream',
  }
  if (tokens) headers.Authorization = `Bearer ${tokens.access_token}`

  let response: Response
  try {
    response = await fetch('/api/v1/chat/completions', {
      method: 'POST',
      headers,
      body: JSON.stringify(payload),
      signal,
    })
  } catch (error) {
    if (signal?.aborted) return { aborted: true, done: false }
    // fetch 的失败信息是浏览器原话（断网时是 "Failed to fetch"），对用户没有意义。
    // 能走到这里只有网络层失败——HTTP 状态码不会抛，它在下面按包络解析。
    if (error instanceof TypeError) {
      throw new ApiError(-1, '网络连接失败，请检查网络后重试')
    }
    throw new ApiError(-1, error instanceof Error ? error.message : '网络异常，请检查后端是否在运行')
  }

  if (!response.ok) {
    const failure = await readErrorEnvelope(response)
    // 用户已经点了停止，就不必再把"请求失败"甩到界面上。
    if (signal?.aborted) return { aborted: true, done: false }
    throw failure
  }
  if (!response.body) {
    throw new ApiError(-1, '当前浏览器不支持流式响应')
  }

  const reader = response.body.getReader()
  const parser = new SSEParser()
  let gotDone = false
  let streamError: ApiError | null = null
  let reachedEof = false

  try {
    readLoop: for (;;) {
      const { value, done } = await reader.read()
      const frames = done ? parser.end() : parser.push(value as Uint8Array)

      for (const frame of frames) {
        const event = decodeEvent(frame)
        if (!event) {
          // 未知事件名按契约跳过；能识别的名字但载荷坏了也跳过，但都要留痕。
          if (isKnownEvent(frame.event)) {
            console.warn('[sse] 无法解析的事件载荷', frame.event, frame.data)
          } else {
            console.warn('[sse] 跳过未知事件', frame.event)
          }
          continue
        }

        switch (event.type) {
          case 'meta':
            handlers.onMeta?.(event)
            break
          case 'delta':
            handlers.onDelta?.(event)
            break
          case 'citation':
            handlers.onCitation?.(event)
            break
          case 'tool_call':
            handlers.onToolCall?.(event)
            break
          case 'done':
            gotDone = true
            handlers.onDone?.(event)
            break readLoop
          case 'error':
            if (event.code === 40101) notifyUnauthorized()
            streamError = new ApiError(event.code, event.message || '模型调用失败')
            break readLoop
        }
      }

      if (done) {
        reachedEof = true
        break
      }
    }
  } catch (error) {
    if (signal?.aborted) return { aborted: true, done: false }
    throw new ApiError(-1, error instanceof Error ? error.message : '读取响应失败')
  } finally {
    // 无论是收到 done/error 提前跳出还是正常 EOF，都主动放掉连接。
    if (!reachedEof) {
      await reader.cancel().catch(() => undefined)
    }
  }

  if (streamError) throw streamError
  if (gotDone) return { aborted: false, done: true }
  if (signal?.aborted) return { aborted: true, done: false }

  // 后端异常退出时连接会被直接关闭，这里就是用户看到的最后一句。
  throw new ApiError(-1, '连接中断，请重试')
}
