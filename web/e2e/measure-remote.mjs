// 演示导演卡里每个耗时数字的重跑器：对着「线上」量，不靠记忆。
//
//   node web/e2e/measure-remote.mjs                        # 默认打 https://111.230.152.155:8443
//   WEB_BASE=http://127.0.0.1:5173 API_BASE=http://127.0.0.1:8080 node web/e2e/measure-remote.mjs
//   RUNS=5 node web/e2e/measure-remote.mjs                 # 每项采 5 次
//
// 量的是什么，为什么这么量：
//   1. 检索出卡片 —— SSE 里第一个 tool_call(status=ok) 的到达时刻。前端把它翻成
//      「已找到 N 条相关法条，正在分析」，所以它才是那行文案的真实时刻。
//   2. 首字      —— SSE 里第一个 delta。状态行在这一刻整行消失，正文开始逐字出现。
//   3. 正文结束  —— done 帧。
//   4. 追问一轮  —— 同一会话第二轮（读得到上一轮，token 更多，所以单独量）。
//   5. 审查      —— POST /contracts/review 到 task succeeded；轮询间隔压到 100ms，
//                  这样量到的是审查本身，不是页面 2 秒轮询的量化误差。
//   6. PDF 渲染  —— 无头 Chrome 打开 /review/<file_id>，量到第 4 个 .page 挂上。
//                  这一步必须真开浏览器：渲染耗时在 pdfjs 与字体加载里，HTTP 量不到。
//
// 三次取中位数：单次会被上游排队、TLS 握手、本机 GC 干扰，中位数才是"下次大概多快"。
// 任何一步失败都非 0 退出——宁可没有数字，也不要一个看起来正常的假数字。

import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { setTimeout as sleep } from 'node:timers/promises'
import { fileURLToPath } from 'node:url'

// 线上是自签证书：不关校验，fetch 连不上；Chrome 侧用 --ignore-certificate-errors。
process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0'

const API = process.env.API_BASE || 'https://111.230.152.155:8443'
const WEB = process.env.WEB_BASE || 'https://111.230.152.155:8443'
const RUNS = Number(process.env.RUNS || 3)
const CHROME = process.env.CHROME_PATH || 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe'
const CDP_PORT = Number(process.env.CDP_PORT || 9491)
const NO_BROWSER = process.env.NO_BROWSER === '1'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '../..')
const PDF = path.resolve(process.env.PDF_PATH || path.join(repoRoot, '.dev', 'contract-demo.pdf'))
const tmpDir = path.join(here, '.tmp')
fs.mkdirSync(tmpDir, { recursive: true })

if (!fs.existsSync(PDF)) {
  console.error('找不到演示合同：' + PDF)
  process.exit(1)
}

const stamp = new Date()
// 手机号必须正好 11 位：137 + 时分秒（6 位）+ 2 位随机。
const phone = `137${String(stamp.getHours()).padStart(2, '0')}${String(stamp.getMinutes()).padStart(2, '0')}${String(stamp.getSeconds()).padStart(2, '0')}${String(Math.floor(Math.random() * 90) + 10)}`
const password = 'laborlaw123'

function must(cond, msg) {
  if (!cond) throw new Error(msg)
}

async function rest(pathname, { method = 'GET', token, body, form } = {}) {
  const headers = {}
  if (token) headers.Authorization = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const res = await fetch(`${API}/api/v1${pathname}`, {
    method,
    headers,
    body: form ?? (body !== undefined ? JSON.stringify(body) : undefined),
  })
  const json = await res.json()
  if (json.code !== 0) throw new Error(`${method} ${pathname} → code=${json.code} ${json.message}`)
  return json.data
}

// streamChat 发一轮咨询，返回每个关键时刻相对「按下发送」的毫秒数。
//
// 必须边读边记时间：等整轮结束再看 body，只能得到总耗时，首字与卡片时刻会丢。
async function streamChat(token, input, sessionId = '') {
  const t0 = Date.now()
  const res = await fetch(`${API}/api/v1/chat/completions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
    body: JSON.stringify({ input, session_id: sessionId }),
  })
  must(res.ok, `chat/completions HTTP ${res.status}`)
  const tHeaders = Date.now() - t0

  const out = { tHeaders, tToolOK: null, tFirstDelta: null, tDone: null, chars: 0, citations: 0, sessionId: '', text: '' }
  const decoder = new TextDecoder()
  let buf = ''
  let body = ''
  for await (const chunk of res.body) {
    buf += decoder.decode(chunk, { stream: true })
    // SSE 帧以空行分隔；半帧留在 buf 里等下一块。
    let idx
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      let ev = ''
      let data = ''
      for (const line of frame.split('\n')) {
        if (line.startsWith('event: ')) ev = line.slice(7)
        else if (line.startsWith('data: ')) data += line.slice(6)
      }
      if (!data) continue
      let payload
      try {
        payload = JSON.parse(data)
      } catch {
        continue
      }
      const at = Date.now() - t0
      body += data
      if (ev === 'meta') {
        out.sessionId = payload.session_id ?? ''
        out.tMeta = at
      }
      if (ev === 'tool_call' && payload.tool?.status === 'ok' && out.tToolOK === null) out.tToolOK = at
      if (ev === 'citation') out.citations += 1
      if (ev === 'delta' && (payload.text ?? '') !== '') {
        if (out.tFirstDelta === null) out.tFirstDelta = at
        out.text += payload.text
        out.chars = out.text.length
      }
      if (ev === 'done') out.tDone = at
      if (ev === 'error') throw new Error(`SSE error 帧：code=${payload.code} ${payload.message}`)
    }
    if (out.tDone !== null) break
  }
  must(out.tDone !== null, '没等到 done 帧')
  return out
}

const median = (xs) => {
  const a = [...xs].sort((x, y) => x - y)
  const m = a.length >> 1
  return a.length % 2 ? a[m] : (a[m - 1] + a[m]) / 2
}
const sec = (ms) => (ms / 1000).toFixed(2)
const med = (xs) => sec(median(xs))

// ── CDP：只为量 PDF 渲染 ────────────────────────────────────────
async function waitJSON(url, tries = 80) {
  for (let i = 0; i < tries; i++) {
    try {
      const r = await fetch(url)
      if (r.ok) return r.json()
    } catch {
      /* 端口还没起 */
    }
    await sleep(300)
  }
  throw new Error('Chrome 调试端口未就绪')
}

class CDP {
  #ws
  #id = 0
  #pending = new Map()
  static async connect(wsUrl) {
    const c = new CDP()
    c.#ws = new WebSocket(wsUrl)
    await new Promise((res, rej) => {
      c.#ws.onopen = res
      c.#ws.onerror = rej
    })
    c.#ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data)
      if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') {
        c.onConsoleError?.(msg.params.args.map((a) => a.value ?? a.description ?? '').join(' '))
      }
      if (msg.method === 'Runtime.exceptionThrown') {
        c.onConsoleError?.(msg.params.exceptionDetails.text + ' ' + (msg.params.exceptionDetails.exception?.description ?? ''))
      }
      if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') {
        c.onConsoleError?.(msg.params.entry.text)
      }
      const p = c.#pending.get(msg.id)
      if (!p) return
      c.#pending.delete(msg.id)
      msg.error ? p.reject(new Error(msg.error.message)) : p.resolve(msg.result)
    }
    return c
  }
  send(method, params = {}) {
    const id = ++this.#id
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject })
      this.#ws.send(JSON.stringify({ id, method, params }))
    })
  }
  async eval(expression) {
    const r = await this.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true })
    if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? r.exceptionDetails.text)
    return r.result.value
  }
  close() {
    this.#ws.close()
  }
}

async function waitFor(fn, timeoutMs, label, intervalMs = 50) {
  const started = Date.now()
  for (;;) {
    if (await fn()) return Date.now() - started
    if (Date.now() - started > timeoutMs) throw new Error(`等待超时：${label}`)
    await sleep(intervalMs)
  }
}

// ── 主流程 ─────────────────────────────────────────────────────
const rows = []
const raw = { api: API, web: WEB, runs: RUNS, at: stamp.toISOString(), samples: {} }

try {
  console.log(`# 目标 ${API}  采样 ${RUNS} 次  时间 ${stamp.toLocaleString('zh-CN')}`)
  const auth = await rest('/auth/register', { method: 'POST', body: { phone, password, nickname: '耗时采样' } })
  const token = auth.access_token
  console.log(`# 账号 ${phone}`)

  // ── 1–3：第一问（每轮一个新会话，避免历史越滚越长把后几次拖慢）
  const q = {}
  q.tool = []
  q.first = []
  q.done = []
  q.chars = []
  q.citations = []
  for (let i = 1; i <= RUNS; i++) {
    const r = await streamChat(token, '公司不给我交社保，我能直接走吗')
    q.tool.push(r.tToolOK)
    q.first.push(r.tFirstDelta)
    q.done.push(r.tDone)
    q.chars.push(r.chars)
    q.citations.push(r.citations)
    if (i === 1) raw.samples.firstQuestion = { tHeaders: r.tHeaders, tMeta: r.tMeta, tToolOK: r.tToolOK, tFirstDelta: r.tFirstDelta, tDone: r.tDone, chars: r.chars, citations: r.citations }
    console.log(`  [第一问 ${i}] 检索卡片=${sec(r.tToolOK)}s 首字=${sec(r.tFirstDelta)}s 全文=${sec(r.tDone)}s 字数=${r.chars} 卡片=${r.citations}`)
    must(r.tToolOK !== null && r.tFirstDelta !== null, '缺 tool_call ok 或 delta 帧')
  }
  rows.push(['第一问 · 检索卡片出现', med(q.tool), q.tool.map(sec).join(' / '), `${median(q.citations)} 张`])
  rows.push(['第一问 · 首个正文字', med(q.first), q.first.map(sec).join(' / '), `首字节之前还有 meta+检索`])
  rows.push(['第一问 · 正文结束', med(q.done), q.done.map(sec).join(' / '), `${median(q.chars)} 字`])

  // ── 4：追问（同会话第二轮）
  const f = { first: [], done: [], chars: [] }
  for (let i = 1; i <= RUNS; i++) {
    const first = await streamChat(token, '公司不给我交社保，我能直接走吗')
    const second = await streamChat(token, '那我还能要经济补偿吗', first.sessionId)
    f.first.push(second.tFirstDelta)
    f.done.push(second.tDone)
    f.chars.push(second.chars)
    console.log(`  [追问 ${i}] 首字=${sec(second.tFirstDelta)}s 全文=${sec(second.tDone)}s 字数=${second.chars}`)
    must(second.sessionId === first.sessionId, '追问不应新建会话')
  }
  rows.push(['追问 · 一轮结束', med(f.done), f.done.map(sec).join(' / '), `${median(f.chars)} 字`])

  // ── 5：上传 + 审查（轮询 100ms，量审查本身）
  const form = new FormData()
  form.append('file', new Blob([fs.readFileSync(PDF)], { type: 'application/pdf' }), path.basename(PDF))
  const tUp = Date.now()
  const file = await rest('/files', { method: 'POST', token, form })
  const uploadMs = Date.now() - tUp
  console.log(`  [上传] ${sec(uploadMs)}s file=${file.id} 页数=${file.page_count}`)

  const review = { post: [], poll: [] }
  let reviewId = ''
  for (let i = 1; i <= RUNS; i++) {
    const tPost = Date.now()
    const accepted = await rest('/contracts/review', { method: 'POST', token, body: { file_id: file.id, rule_scope: [], async: true } })
    const postMs = Date.now() - tPost
    reviewId = accepted.review_id ?? accepted.id
    let pollMs = 0
    if (accepted.task_id) {
      const tPoll = Date.now()
      for (;;) {
        const task = await rest(`/tasks/${accepted.task_id}`, { token })
        if (task.status === 'succeeded') break
        if (task.status === 'failed' || task.status === 'cancelled') throw new Error(`审查 ${task.status}`)
        if (Date.now() - tPoll > 180_000) throw new Error('审查超时')
        await sleep(100)
      }
      pollMs = Date.now() - tPoll
    }
    review.post.push(postMs)
    review.poll.push(pollMs)
    console.log(`  [审查 ${i}] 受理 ${sec(postMs)}s 到 succeeded ${sec(pollMs)}s review=${reviewId}`)
  }
  rows.push(['上传合同（4 页 PDF）', med([uploadMs]), sec(uploadMs), `${file.page_count} 页`])
  rows.push(['审查到 succeeded（100ms 轮询）', med(review.poll), review.poll.map(sec).join(' / '), `受理仅 ${sec(median(review.post))}s`])

  const findings = await rest(`/reviews/${reviewId}/findings`, { token })
  must(findings.length > 0, 'findings 为空')
  console.log(`  [findings] ${findings.length} 条`)

  // ── 6：PDF 渲染（真开无头 Chrome）
  if (!NO_BROWSER) {
    const chrome = spawn(
      CHROME,
      [
        `--remote-debugging-port=${CDP_PORT}`,
        `--user-data-dir=${path.join(tmpDir, `chrome-measure-${Date.now()}`)}`,
        '--remote-allow-origins=*',
        '--headless=new',
        '--disable-gpu',
        '--in-process-gpu',
        '--no-first-run',
        '--ignore-certificate-errors',
        '--window-size=1440,900',
        'about:blank',
      ],
      { stdio: 'ignore' },
    )
    let cdp
    try {
      await waitJSON(`http://127.0.0.1:${CDP_PORT}/json/version`)
      const target = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/new?about:blank`, { method: 'PUT' })).json()
      cdp = await CDP.connect(target.webSocketDebuggerUrl)
      await cdp.send('Page.enable')
      await cdp.send('Runtime.enable')
      await cdp.send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false })

      // 先进登录页把令牌塞进 localStorage，再进审查页——不然会被路由打回登录。
      await cdp.send('Page.navigate', { url: `${WEB}/login` })
      await waitFor(async () => (await cdp.eval('document.readyState')) === 'complete', 30_000, '登录页')
      await cdp.eval(`
        window.localStorage.setItem('laborlaw.access_token', ${JSON.stringify(auth.access_token)})
        window.localStorage.setItem('laborlaw.refresh_token', ${JSON.stringify(auth.refresh_token)})
        window.localStorage.setItem('laborlaw.expires_at', String(${Date.now()} + ${auth.expires_in} * 1000))
        true
      `)

      // 页面自己抛的错要收上来：dist 换新之后如果某个 chunk 404，控制台会先亮红，
      // 光看"页数出来了"是看不出来的。
      const consoleErrors = []
      cdp.onConsoleError = (t) => consoleErrors.push(t)

      // 对话页先走一遍：路由懒加载之后 ChatView / UploadView 都是独立 chunk，
      // 换 dist 最容易漏传的就是这几块——它们 404 时页面只剩一个空壳。
      await cdp.send('Page.navigate', { url: `${WEB}/chat` })
      await waitFor(async () => (await cdp.eval('document.readyState')) === 'complete', 30_000, '对话页加载')
      await waitFor(async () => await cdp.eval(`!!document.querySelector('.composer textarea')`), 30_000, '对话页输入框挂载')
      const chatSessions = await cdp.eval(`document.querySelectorAll('.session-item').length`)
      console.log(`  [对话页] 输入框已挂载 会话数=${chatSessions} 控制台报错=${consoleErrors.length}`)
      must(consoleErrors.length === 0, `对话页控制台有 ${consoleErrors.length} 条报错：${consoleErrors.join(' | ')}`)

      const render = []
      const cards = []
      const marks = []
      for (let i = 1; i <= RUNS; i++) {
        // 冷启动口径：每次都是新导航（新文档），等于"观众第一次进这个页面"。
        const tNav = Date.now()
        await cdp.send('Page.navigate', { url: `${WEB}/review/${file.id}` })
        await waitFor(async () => (await cdp.eval('document.readyState')) === 'complete', 30_000, '审查页加载')
        await waitFor(async () => (await cdp.eval(`document.querySelectorAll('.page').length`)) >= file.page_count, 60_000, 'PDF 渲染到最后一页', 25)
        render.push(Date.now() - tNav)
        // 风险卡与红框是 findings 回来之后才画的，单独量——它才是"观众看到结果"的时刻。
        await waitFor(async () => (await cdp.eval(`document.querySelectorAll('.mark').length`)) > 0, 30_000, '风险框出现', 25)
        cards.push(Date.now() - tNav)
        const state = await cdp.eval(
          `(() => ({ pages: document.querySelectorAll('.page').length, marks: document.querySelectorAll('.mark').length, cards: document.querySelectorAll('.card').length }))()`,
        )
        marks.push(state.marks)
        console.log(`  [PDF 渲染 ${i}] 4 页=${sec(render[render.length - 1])}s 风险框=${sec(cards[cards.length - 1])}s 页数=${state.pages} 框=${state.marks} 卡=${state.cards}`)
      }
      rows.push(['PDF 渲染到 4 页可见', med(render), render.map(sec).join(' / '), `含进站 TLS + 首屏 JS`])
      rows.push(['风险框出现（含 findings 请求）', med(cards), cards.map(sec).join(' / '), `${median(marks)} 个框`])
      raw.samples.render = render
      raw.samples.cards = cards
      if (consoleErrors.length) {
        console.error(`  [控制台报错] ${consoleErrors.length} 条：${consoleErrors.slice(0, 5).join(' | ')}`)
        must(false, '审查页控制台有报错，换构建后前端没跑通')
      }
    } finally {
      if (cdp) cdp.close()
      chrome.kill()
    }
  }

  // ── 出表
  console.log('\n| 时刻 | 中位数 | 各次（秒） | 备注 |')
  console.log('|---|---|---|---|')
  for (const [name, m, all, note] of rows) console.log(`| ${name} | ${m} s | ${all} | ${note} |`)

  const outFile = path.join(tmpDir, `measure-${stamp.toISOString().slice(0, 10)}.json`)
  fs.writeFileSync(outFile, JSON.stringify({ ...raw, rows }, null, 2), 'utf8')
  console.log(`\nMEASURE OK  原始样本 ${outFile}`)
} catch (err) {
  console.error('MEASURE FAILED: ' + (err?.stack || err))
  process.exit(1)
}
process.exit(0)
