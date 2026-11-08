// 答辩演示的一键脚本：一条命令跑完要演的完整路径，每步打印耗时。
//
//   node web/e2e/demo.mjs
//
// 前置条件与 full-flow.mjs 一样：api 在 8080、vite dev 在 5173、法条库 205 条、
// 本机有 Chrome、上游 LLM 可用（qwen3.8-max 单轮 20–45 秒，要给够等待时间）。
//
// 任何一步失败都非 0 退出；最后打印一行 DEMO OK … 方便直接贴到答辩笔记里。

import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { setTimeout as sleep } from 'node:timers/promises'
import { fileURLToPath } from 'node:url'

const API = process.env.API_BASE || 'http://127.0.0.1:8080'
const WEB = process.env.WEB_BASE || 'http://127.0.0.1:5173'
const CHROME = process.env.CHROME_PATH || 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe'
const CDP_PORT = Number(process.env.CDP_PORT || 9481)
const CONSOLE_WAIT = 60_000

// 路径一律从脚本自身位置推，不依赖 cwd：从仓库根、从 web 目录、从任意地方跑都行。
const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '../..')
const PDF = path.resolve(process.env.PDF_PATH || path.join(repoRoot, '.dev', 'contract-demo.pdf'))

const tmpDir = path.join(here, '.tmp')
fs.mkdirSync(tmpDir, { recursive: true })

if (!fs.existsSync(PDF)) {
  console.error('找不到演示合同：' + PDF + '（应从仓库根或 web 目录执行均可）')
  process.exit(1)
}

const tag = 'demo'
const stamp = new Date()
const phone = `139${String(stamp.getHours()).padStart(2, '0')}${String(stamp.getMinutes()).padStart(2, '0')}${String(stamp.getSeconds()).padStart(2, '0')}${String(Math.floor(Math.random() * 10))}9`
const password = 'laborlaw123'

const t0 = Date.now()
const step = (name, extra = '') => console.log(`[${((Date.now() - t0) / 1000).toFixed(1)}s] ${name}${extra ? ' ' + extra : ''}`)

function must(cond, message) {
  if (!cond) throw new Error(message)
}

// ── REST 小工具：注册、上传、审查、轮询 ──────────────────────────
async function rest(pathname, { method = 'GET', token, body, form, raw } = {}) {
  const headers = {}
  if (token) headers.Authorization = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const res = await fetch(`${API}/api/v1${pathname}`, {
    method,
    headers,
    body: form ?? (body !== undefined ? JSON.stringify(body) : undefined),
  })
  if (raw) return res
  const json = await res.json()
  if (json.code !== 0) throw new Error(`${method} ${pathname} → code=${json.code} ${json.message}`)
  return json.data
}

async function pollTask(token, taskId, timeoutMs = 180_000) {
  const started = Date.now()
  for (;;) {
    const task = await rest(`/tasks/${taskId}`, { token })
    if (task.status === 'succeeded') return Date.now() - started
    if (task.status === 'failed' || task.status === 'cancelled') {
      throw new Error(`审查任务 ${task.status}：${task.error || '(无错误信息)'}`)
    }
    if (Date.now() - started > timeoutMs) throw new Error(`审查超时 ${timeoutMs}ms`)
    await sleep(2000)
  }
}

// ── CDP ────────────────────────────────────────────────────────
const chrome = spawn(
  CHROME,
  [
    `--remote-debugging-port=${CDP_PORT}`,
    `--user-data-dir=${path.join(tmpDir, `chrome-${tag}-${Date.now()}`)}`,
    '--headless=new',
    '--disable-gpu',
    '--no-first-run',
    '--window-size=1440,900',
    'about:blank',
  ],
  { stdio: 'ignore' },
)

async function waitJSON(url, tries = 80) {
  for (let i = 0; i < tries; i++) {
    try {
      const res = await fetch(url)
      if (res.ok) return res.json()
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
  consoleErrors = []
  consoleWarnings = []
  static async connect(wsUrl) {
    const c = new CDP()
    c.#ws = new WebSocket(wsUrl)
    await new Promise((res, rej) => {
      c.#ws.onopen = res
      c.#ws.onerror = rej
    })
    c.#ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data)
      if (msg.method === 'Runtime.consoleAPICalled') {
        const text = msg.params.args.map((a) => a.value ?? a.description ?? '').join(' ')
        if (msg.params.type === 'error') c.consoleErrors.push(text)
        if (msg.params.type === 'warning') c.consoleWarnings.push(text)
      }
      if (msg.method === 'Runtime.exceptionThrown') {
        c.consoleErrors.push(msg.params.exceptionDetails.text + ' ' + (msg.params.exceptionDetails.exception?.description ?? ''))
      }
      if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') {
        c.consoleErrors.push(msg.params.entry.text)
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

const HELPERS = `
  window.__setInput = (el, value) => {
    if (!el) return false
    const desc = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value')
    desc.set.call(el, value)
    el.dispatchEvent(new Event('input', { bubbles: true }))
    return true
  }
  window.__clickText = (selector, text) => {
    const btn = [...document.querySelectorAll(selector)].find((b) => b.textContent.trim().includes(text))
    if (!btn) return false
    btn.click()
    return true
  }
  true
`

async function waitFor(fn, timeoutMs, label, intervalMs = 250) {
  const started = Date.now()
  for (;;) {
    if (await fn()) return true
    if (Date.now() - started > timeoutMs) throw new Error(`等待超时：${label}`)
    await sleep(intervalMs)
  }
}

let cdp
let summary = ''
try {
  // ── 1. 注册新账号 ───────────────────────────────────────────
  const auth = await rest('/auth/register', {
    method: 'POST',
    body: { phone, password, nickname: '演示账号' },
  })
  const token = auth.access_token
  step('1 注册', `${phone} user=${auth.user?.id ?? '?'}`)

  // ── 2. 起浏览器，注入令牌，进对话页 ──────────────────────────
  await waitJSON(`http://127.0.0.1:${CDP_PORT}/json/version`)
  const target = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/new?about:blank`, { method: 'PUT' })).json()
  cdp = await CDP.connect(target.webSocketDebuggerUrl)
  await cdp.send('Page.enable')
  await cdp.send('Runtime.enable')
  await cdp.send('Log.enable')
  await cdp.send('DOM.enable')
  await cdp.send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false })

  await cdp.send('Page.navigate', { url: `${WEB}/login` })
  await waitFor(async () => (await cdp.eval(`document.readyState`)) === 'complete', 30_000, '登录页加载')
  await cdp.eval(`
    window.localStorage.setItem('laborlaw.access_token', ${JSON.stringify(auth.access_token)})
    window.localStorage.setItem('laborlaw.refresh_token', ${JSON.stringify(auth.refresh_token)})
    window.localStorage.setItem('laborlaw.expires_at', String(${Date.now()} + ${auth.expires_in} * 1000))
    true
  `)
  await cdp.send('Page.navigate', { url: `${WEB}/chat` })
  await waitFor(async () => (await cdp.eval(`location.pathname`)) === '/chat', 30_000, '进入对话页')
  // pathname 在文档还没建完时就已经是 /chat 了，必须等真实的输入框挂上再继续。
  await waitFor(async () => await cdp.eval(`!!document.querySelector('.composer textarea')`), 30_000, '输入框挂载')
  await cdp.eval(HELPERS)
  step('2 浏览器就绪', `会话数=${await cdp.eval(`document.querySelectorAll('.session-item').length`)}`)

  // ── 3. 第一问：等 done，量首字延迟与总耗时 ───────────────────
  const question = '公司不给我交社保，我能直接走吗'
  const sendStart = Date.now()
  must(
    await cdp.eval(`
      (() => {
        const ta = document.querySelector('.composer textarea')
        return window.__setInput(ta, ${JSON.stringify(question)})
      })()
    `),
    '输入框不存在',
  )
  await sleep(150)
  must(await cdp.eval(`window.__clickText('.composer-actions button', '发送')`), '发送按钮不可点')

  const statusSeen = []
  let firstDeltaAt = null
  const deadline = Date.now() + 180_000
  for (;;) {
    const snap = await cdp.eval(`
      (() => ({
        status: document.querySelector('.status-text')?.textContent ?? '',
        chars: (document.querySelectorAll('.bubble-assistant .content')[0]?.textContent ?? '').length,
        citations: document.querySelectorAll('.citation-card').length,
        sending: [...document.querySelectorAll('.composer-actions button')].some((b) => b.textContent.includes('停止')),
      }))()
    `)
    if (snap.status !== '' && !statusSeen.includes(snap.status)) statusSeen.push(snap.status)
    if (firstDeltaAt === null && snap.chars > 0) firstDeltaAt = Date.now() - sendStart
    if (!snap.sending && snap.chars > 0) break
    if (Date.now() > deadline) throw new Error('第一问等待超时')
    await sleep(100)
  }
  const chatTotal = Date.now() - sendStart
  const citations = await cdp.eval(`document.querySelectorAll('.citation-card').length`)
  must(citations >= 1, `法条卡片应当出现，实际 ${citations} 张`)
  step(
    '3 第一问',
    `首字=${(firstDeltaAt / 1000).toFixed(1)}s 总耗时=${(chatTotal / 1000).toFixed(1)}s 卡片=${citations} 状态行=${JSON.stringify(statusSeen)}`,
  )

  // ── 4. 追问：上下文生效（会话数不增、气泡数增加） ─────────────
  const sessionsBefore = await cdp.eval(`document.querySelectorAll('.session-item').length`)
  const bubblesBefore = await cdp.eval(`document.querySelectorAll('.bubble').length`)
  await cdp.eval(`
    (() => {
      const ta = document.querySelector('.composer textarea')
      window.__setInput(ta, '那我能要经济补偿吗')
      return true
    })()
  `)
  await sleep(150)
  must(await cdp.eval(`window.__clickText('.composer-actions button', '发送')`), '追问发送失败')
  await waitFor(
    async () =>
      (await cdp.eval(`document.querySelectorAll('.bubble').length`)) >= bubblesBefore + 2 &&
      !(await cdp.eval(`[...document.querySelectorAll('.composer-actions button')].some((b) => b.textContent.includes('停止'))`)),
    180_000,
    '追问回答结束',
    300,
  )
  const sessionsAfter = await cdp.eval(`document.querySelectorAll('.session-item').length`)
  must(sessionsAfter === sessionsBefore, `追问不应新建会话：${sessionsBefore} → ${sessionsAfter}`)
  const secondAnswer = await cdp.eval(`(document.querySelectorAll('.bubble-assistant .content')[1]?.textContent ?? '')`)
  must(secondAnswer.length > 20, '追问的回答是空的')
  const contextKept = /经济补偿|补偿|第四十六条|第四十七条/.test(secondAnswer)
  must(contextKept, '追问的回答没有接上上下文（没提经济补偿）')
  step('4 追问', `会话数=${sessionsAfter}（未新增） 回答字数=${secondAnswer.length} 提经济补偿=${contextKept}`)

  // ── 5. 上传合同 → 审查 → 轮询 succeeded ──────────────────────
  const form = new FormData()
  form.append('file', new Blob([fs.readFileSync(PDF)], { type: 'application/pdf' }), path.basename(PDF))
  const uploadStart = Date.now()
  const file = await rest('/files', { method: 'POST', token, form })
  step('5a 上传', `file=${file.id} 页数=${file.page_count} 耗时=${((Date.now() - uploadStart) / 1000).toFixed(1)}s`)

  const reviewStart = Date.now()
  const accepted = await rest('/contracts/review', {
    method: 'POST',
    token,
    body: { file_id: file.id, rule_scope: [], async: true },
  })
  // 已有结果时后端回 200 整个 Review（没有 task_id），这条路也要能跑通。
  const reviewId = accepted.review_id ?? accepted.id
  let reviewMs = Date.now() - reviewStart
  if (accepted.task_id) reviewMs = await pollTask(token, accepted.task_id)
  step('5b 审查', `review=${reviewId} 耗时=${(reviewMs / 1000).toFixed(1)}s`)

  // ── 6. findings 断言 ────────────────────────────────────────
  const findings = await rest(`/reviews/${reviewId}/findings`, { token })
  must(findings.length === 5, `findings 应为 5 条，实际 ${findings.length}`)
  must(
    findings.some((f) => f.rule_id === 'R-PROBATION-MATCH'),
    'findings 里没有 R-PROBATION-MATCH',
  )
  step('6 findings', `${findings.length} 条，含 R-PROBATION-MATCH=${findings.some((f) => f.rule_id === 'R-PROBATION-MATCH')}`)

  // ── 7. 打开审查页：不该有「开始审查」，框数 = 5 ──────────────
  const openPage = async () => {
    await cdp.send('Page.navigate', { url: `${WEB}/review/${file.id}` })
    await waitFor(async () => (await cdp.eval(`document.readyState`)) === 'complete', 30_000, '审查页加载')
    await waitFor(async () => (await cdp.eval(`document.querySelectorAll('.page').length`)) >= 1, 30_000, 'PDF 渲染')
    await waitFor(async () => (await cdp.eval(`document.querySelectorAll('.mark').length`)) > 0, 20_000, '风险框出现')
    const state = await cdp.eval(`
      (() => ({
        start: [...document.querySelectorAll('button')].some((b) => b.textContent.trim() === '开始审查' && b.offsetParent !== null),
        marks: document.querySelectorAll('.mark').length,
        cards: document.querySelectorAll('.card').length,
        pages: document.querySelectorAll('.page').length,
        summary: (document.querySelector('.summary')?.innerText ?? '').replace(/\\n/g, ' '),
      }))()
    `)
    return state
  }

  const fresh = await openPage()
  must(!fresh.start, '已有审查结果时不应显示「开始审查」')
  must(fresh.marks === 5, `风险框应为 5 个，实际 ${fresh.marks}`)
  step('7 审查页', `页数=${fresh.pages} 框=${fresh.marks} 卡片=${fresh.cards} 无开始按钮=${!fresh.start} 汇总=${fresh.summary}`)

  // ── 8. 刷新后再来一次 ───────────────────────────────────────
  await cdp.send('Page.reload', { ignoreCache: false })
  await waitFor(async () => (await cdp.eval(`document.readyState`)) === 'complete', 30_000, '刷新完成')
  await waitFor(async () => (await cdp.eval(`document.querySelectorAll('.mark').length`)) > 0, 30_000, '刷新后风险框恢复')
  const reloaded = await cdp.eval(`
    (() => ({
      start: [...document.querySelectorAll('button')].some((b) => b.textContent.trim() === '开始审查' && b.offsetParent !== null),
      marks: document.querySelectorAll('.mark').length,
      localKeys: Object.keys(window.localStorage).filter((k) => k.startsWith('laborlaw.review.')).length,
    }))()
  `)
  must(!reloaded.start, '刷新后不应出现「开始审查」')
  must(reloaded.marks === 5, `刷新后风险框应为 5 个，实际 ${reloaded.marks}`)
  must(reloaded.localKeys === 0, '刷新后不应依赖 localStorage 里的审查指针')
  step('8 刷新', `框=${reloaded.marks} 无开始按钮=${!reloaded.start} 本地指针=${reloaded.localKeys}`)

  // ── 9. 控制台 ─────────────────────────────────────────────
  await sleep(1500)
  const ignored = cdp.consoleErrors.filter((t) => t.includes('favicon'))
  must(ignored.length === cdp.consoleErrors.length, `控制台有 error：${JSON.stringify(cdp.consoleErrors.slice(0, 3))}`)
  must(cdp.consoleWarnings.length === 0, `控制台有 warning：${JSON.stringify(cdp.consoleWarnings.slice(0, 3))}`)
  step('9 控制台', `error=${cdp.consoleErrors.length} warning=${cdp.consoleWarnings.length}`)

  summary = `DEMO OK  chat_first=${(firstDeltaAt / 1000).toFixed(1)}s chat_total=${(chatTotal / 1000).toFixed(1)}s review=${(reviewMs / 1000).toFixed(1)}s marks=${reloaded.marks}`
  console.log(summary)
  console.log(`账号 ${phone} / ${password}；file=${file.id} review=${reviewId}`)
} catch (err) {
  console.error(`DEMO FAIL  ${err.message}`)
  if (cdp) {
    try {
      console.error('页面：', (await cdp.eval(`document.body.innerText`)).slice(0, 300))
    } catch {
      /* 页面已经没了 */
    }
  }
  process.exitCode = 1
} finally {
  cdp?.close()
  chrome.kill()
}
