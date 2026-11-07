// SSE 解析器的单测。用 node 内置测试运行器 + 原生 TypeScript 类型擦除，
// 不为一个纯函数解析器拉一套测试框架进来（node --test 见 package.json 的 test 脚本）。
//
// 重点覆盖三件最容易写错的事：
//   1. 中文被 chunk 边界切断（不 stream: true 会出乱码）；
//   2. 一帧被切成多个 chunk / 一个 chunk 里有多帧；
//   3. 未知事件与坏 JSON 不能抛异常。

import assert from 'node:assert/strict'
import { test } from 'node:test'

import { SSEParser, decodeEvent, isKnownEvent, normalizeNewlines, parseFrame } from './sse.ts'

const enc = new TextEncoder()

function collect(raw: string): string[] {
  const parser = new SSEParser()
  const frames = [...parser.push(enc.encode(raw)), ...parser.end()]
  return frames.map((f) => `${f.event}|${f.data}`)
}

test('parseFrame 取 event 与 data，id 单独保留', () => {
  const frame = parseFrame('id: 3\nevent: delta\ndata: {"seq":3,"text":"根据"}')
  assert.ok(frame)
  assert.equal(frame.id, '3')
  assert.equal(frame.event, 'delta')
  assert.equal(frame.data, '{"seq":3,"text":"根据"}')
})

test('parseFrame 兼容多行 data，用 \\n 拼接', () => {
  const frame = parseFrame('event: x\ndata: 第一行\ndata: 第二行')
  assert.ok(frame)
  assert.equal(frame.data, '第一行\n第二行')
})

test('parseFrame 丢弃纯注释帧（心跳）', () => {
  assert.equal(parseFrame(': keep-alive'), null)
  assert.equal(parseFrame(''), null)
})

test('parseFrame 缺少空格也能解析（data:xxx 与 data: xxx 等价）', () => {
  const frame = parseFrame('event: delta\ndata:{"seq":1,"text":"a"}')
  assert.ok(frame)
  assert.equal(frame.data, '{"seq":1,"text":"a"}')
})

test('SSEParser 按空行切帧，一个 chunk 里多帧', () => {
  const got = collect('event: meta\ndata: {"a":1}\n\nevent: delta\ndata: {"b":2}\n\n')
  assert.deepEqual(got, ['meta|{"a":1}', 'delta|{"b":2}'])
})

test('SSEParser 一帧被拆成多个 chunk 也能拼回来', () => {
  const parser = new SSEParser()
  const whole = enc.encode('event: delta\ndata: {"seq":1,"text":"根据"}\n\n')
  const frames = [
    ...parser.push(whole.slice(0, 5)),
    ...parser.push(whole.slice(5, 20)),
    ...parser.push(whole.slice(20)),
    ...parser.end(),
  ]
  assert.equal(frames.length, 1)
  assert.equal(frames[0]?.event, 'delta')
  assert.equal(frames[0]?.data, '{"seq":1,"text":"根据"}')
})

test('SSEParser 跨 chunk 切断的汉字不会被解成乱码', () => {
  const parser = new SSEParser()
  const text = 'event: delta\ndata: {"seq":1,"text":"延长工作时间"}\n\n'
  const bytes = enc.encode(text)
  // 逐个字节喂，必然把每个汉字都切开。
  const frames: string[] = []
  for (const b of bytes) {
    for (const f of parser.push(new Uint8Array([b]))) frames.push(f.data)
  }
  frames.push(...parser.end().map((f) => f.data))
  assert.equal(frames[0], '{"seq":1,"text":"延长工作时间"}')
  assert.ok(!frames[0]?.includes('\uFFFD'))
})

test('SSEParser 兼容 CRLF 行尾', () => {
  const got = collect('event: delta\r\ndata: {"seq":1,"text":"a"}\r\n\r\n')
  assert.deepEqual(got, ['delta|{"seq":1,"text":"a"}'])
})

test('normalizeNewlines 把 \\r\\n 与裸 \\r 都变成 \\n', () => {
  assert.equal(normalizeNewlines('a\r\nb\rc'), 'a\nb\nc')
})

test('SSEParser 流结束时没有结尾空行，最后一帧也要吐出来', () => {
  const parser = new SSEParser()
  const frames = [...parser.push(enc.encode('event: done\ndata: {"seq":9}')), ...parser.end()]
  assert.equal(frames.length, 1)
  assert.equal(frames[0]?.event, 'done')
})

test('decodeEvent 解出六种事件的类型', () => {
  const meta = decodeEvent({ id: '', event: 'meta', data: '{"seq":0,"message_id":"M1","session_id":"S1","model":"consult"}' })
  assert.ok(meta && meta.type === 'meta')
  assert.equal(meta.session_id, 'S1')

  const delta = decodeEvent({ id: '', event: 'delta', data: '{"seq":1,"text":"根据"}' })
  assert.ok(delta && delta.type === 'delta')
  assert.equal(delta.text, '根据')

  const citation = decodeEvent({
    id: '',
    event: 'citation',
    data: '{"seq":2,"citation":{"statute_id":"LCL-31","law":"中华人民共和国劳动合同法","article":"第三十一条","text":"用人单位应当严格执行劳动定额标准","url":"https://example.com"}}',
  })
  assert.ok(citation && citation.type === 'citation')
  assert.equal(citation.citation.statute_id, 'LCL-31')
  assert.equal(citation.citation.url, 'https://example.com')

  const tool = decodeEvent({
    id: '',
    event: 'tool_call',
    data: '{"seq":3,"tool":{"tool":"retrieval","status":"ok","detail":"命中 3 条法条"}}',
  })
  assert.ok(tool && tool.type === 'tool_call')
  assert.equal(tool.tool.status, 'ok')
  assert.equal(tool.tool.detail, '命中 3 条法条')

  const done = decodeEvent({
    id: '',
    event: 'done',
    data: '{"seq":4,"message_id":"M1","finish_reason":"stop","usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}',
  })
  assert.ok(done && done.type === 'done')
  assert.equal(done.usage.total_tokens, 30)

  const err = decodeEvent({ id: '', event: 'error', data: '{"seq":5,"code":50201,"message":"模型超时"}' })
  assert.ok(err && err.type === 'error')
  assert.equal(err.code, 50201)
})

test('decodeEvent 对坏 JSON、未知事件、缺嵌套对象返回 null 而不抛', () => {
  assert.equal(decodeEvent({ id: '', event: 'delta', data: '{坏的' }), null)
  assert.equal(decodeEvent({ id: '', event: 'wat', data: '{"seq":1}' }), null)
  // 契约里 tool_call 是嵌套的 {tool:{...}}，扁平写法解不出来就是 null，不能崩。
  assert.equal(decodeEvent({ id: '', event: 'tool_call', data: '{"seq":1,"tool":"retrieval","status":"ok"}' }), null)
})

test('isKnownEvent 只认契约里的六个名字', () => {
  for (const name of ['meta', 'delta', 'citation', 'tool_call', 'done', 'error']) {
    assert.equal(isKnownEvent(name), true, name)
  }
  assert.equal(isKnownEvent('heartbeat'), false)
})
