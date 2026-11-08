import assert from 'node:assert/strict'
import test from 'node:test'

import { createStreamSession } from './stream-session.ts'

test('卸载后到达的 delta 不再改消息', () => {
  // 这是本轮要守的那条：用户发完问题就切走页面，SSE 的 delta 还在路上。
  const messages = [{ content: '' }]
  const session = createStreamSession()
  const controller = new AbortController()
  session.bind(controller)

  const onDelta = (text: string): void => {
    if (session.closed()) return
    messages[0]!.content += text
  }

  onDelta('前半句')
  session.close() // 相当于 onBeforeUnmount
  onDelta('后半句')

  assert.equal(messages[0]!.content, '前半句')
  assert.equal(controller.signal.aborted, true)
})

test('close 幂等：重复调用不会抛，也不会影响第二次', () => {
  const session = createStreamSession()
  const controller = new AbortController()
  session.bind(controller)

  session.close()
  assert.doesNotThrow(() => session.close())
  assert.equal(controller.signal.aborted, true)
  assert.equal(session.closed(), true)
})

test('先卸载后绑定：新 controller 当场被取消，不会留下一条活着的流', async () => {
  const session = createStreamSession()
  session.close()

  const controller = new AbortController()
  session.bind(controller)

  assert.equal(controller.signal.aborted, true)
  // fetch 立刻以 AbortError 结束，不会继续下载剩下的响应体。
  await assert.rejects(async () => {
    await new Promise((_, reject) => {
      controller.signal.addEventListener('abort', () => reject(new Error('aborted')))
      if (controller.signal.aborted) reject(new Error('aborted'))
    })
  })
})

test('每次发送用新会话：停掉上一轮不影响下一轮', () => {
  const first = createStreamSession()
  const firstController = new AbortController()
  first.bind(firstController)
  first.close()

  const second = createStreamSession()
  const secondController = new AbortController()
  second.bind(secondController)

  assert.equal(first.closed(), true)
  assert.equal(firstController.signal.aborted, true)
  assert.equal(second.closed(), false)
  assert.equal(secondController.signal.aborted, false)
})
