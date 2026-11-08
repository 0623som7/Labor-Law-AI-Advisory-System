// 前端全链路验收脚本：一条命令验证
// 注册 → 进对话页 → 首问（占位气泡/状态条/法条卡片/流式正文）→
// 追问（同一会话）→ 刷新后历史仍在 → 令牌预续期 → 令牌作废后跳登录页。
//
// 为什么自己写 CDP 而不用 --dump-dom：--dump-dom 拍一次就退出，
// 看不到"占位气泡 → 逐字出现 → loading 消失"这段时序，而那正是要验收的东西。
// Node 22+ 自带 WebSocket 与 fetch，所以零依赖。
//
// ── 前置条件（缺一个都会失败，且报错不一定指向真正原因）
//   1. api 在 http://127.0.0.1:8080 跑着（本机 go run，或 compose 起的容器都行）
//   2. 前端 dev server 在 http://127.0.0.1:5173（pnpm dev；它把 /api 代理到 8080）
//   3. statutes 表有 205 条法条——否则检索恒 0 命中，"3 张法条卡片"这条必然失败
//   4. 本机装了 Chrome（路径可用 CHROME_PATH 覆盖）
//   5. 上游模型可用（LLM_API_KEY 有效）
//
// ── 为什么要等 300 秒
//   qwen3.8-max 是推理模型，一次咨询实测 44 秒起步，复杂问题更久，
//   而上游偶发 50301 时 llm 层会自己重试两次——单轮实测最慢见过 180 秒。
//   之前设 180 秒上限，测出来的其实是"模型排队时间"而不是前端问题（假失败）。
//
// ── 可配置项（都有默认值，跑本地默认不用给）
//   WEB_BASE        前端地址，默认 http://127.0.0.1:5173
//   CHROME_PATH     浏览器可执行文件，默认 Windows 上的 Chrome 安装路径
//   CDP_PORT        Chrome 远程调试端口，默认 9333
//   E2E_PHONE       注册用的手机号；不给就按时间戳生成一个
//   E2E_PASSWORD    注册用的密码，默认 laborlaw123（≥8 位，后端会校验）
//   E2E_NICKNAME    昵称，默认"验收用户"
//
// 产物（Chrome 临时 profile、日志）落在 web/e2e/.tmp/，已 gitignore。
// 脚本不落任何 token 到磁盘：令牌只在浏览器 localStorage 里流转。

import { spawn } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as sleep } from 'node:timers/promises'

const here = path.dirname(fileURLToPath(import.meta.url))
const tmpDir = path.join(here, '.tmp')

const CHROME = process.env.CHROME_PATH || 'C:/Program Files/Google/Chrome/Application/chrome.exe'
const PORT = Number(process.env.CDP_PORT || 9333)
const BASE = process.env.WEB_BASE || 'http://127.0.0.1:5173'
// Chrome 对相对路径的 user-data-dir 不可靠，这里一律用绝对路径。
const profileDir = path.join(tmpDir, 'chrome-profile')
const chromeLog = path.join(tmpDir, 'chrome.log')

const consoleErrors = []
const consoleWarnings = []

function log(step, message) {
  console.log(`[${step}] ${message}`)
}

async function waitFor(fn, timeoutMs, label, everyMs = 400) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const value = await fn()
    if (value) return value
    if (Date.now() > deadline) throw new Error(`等待超时：${label}`)
    await sleep(everyMs)
  }
}

class CDP {
  #ws
  #id = 0
  #pending = new Map()

  constructor(ws) {
    this.#ws = ws
    ws.addEventListener('message', (event) => {
      const message = JSON.parse(event.data)
      if (message.id !== undefined) {
        const entry = this.#pending.get(message.id)
        if (!entry) return
        this.#pending.delete(message.id)
        if (message.error) entry.reject(new Error(`${message.error.message} (${entry.method})`))
        else entry.resolve(message.result)
        return
      }
      this.#onEvent(message)
    })
  }

  #onEvent(message) {
    if (message.method === 'Runtime.consoleAPICalled') {
      const text = message.params.args
        .map((a) => (a.value !== undefined ? String(a.value) : (a.description ?? a.type)))
        .join(' ')
      if (message.params.type === 'error') consoleErrors.push(text)
      if (message.params.type === 'warning') consoleWarnings.push(text)
    }
    if (message.method === 'Runtime.exceptionThrown') {
      const d = message.params.exceptionDetails
      consoleErrors.push(d.exception?.description ?? d.text)
    }
  }

  send(method, params = {}) {
    this.#id += 1
    const id = this.#id
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject, method })
      this.#ws.send(JSON.stringify({ id, method, params }))
    })
  }

  async eval(expression) {
    const result = await this.send('Runtime.evaluate', {
      expression,
      awaitPromise: true,
      returnByValue: true,
    })
    if (result.exceptionDetails) {
      throw new Error(`页面脚本异常：${result.exceptionDetails.exception?.description ?? result.exceptionDetails.text}`)
    }
    return result.result.value
  }
}

// 页面里用得到的小工具：改 Element Plus 的输入框必须走原生 setter，
// 否则 Vue 收不到 input 事件（el-input 是受控组件，直接改 value 会被下一次渲染覆盖）。
const HELPERS = `
  window.__setInput = (el, value) => {
    const proto = el.tagName === 'TEXTAREA' ? window.HTMLTextAreaElement.prototype : window.HTMLInputElement.prototype
    Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, value)
    el.dispatchEvent(new Event('input', { bubbles: true }))
  };
  window.__clickText = (selector, text) => {
    const hit = [...document.querySelectorAll(selector)].find((el) => el.textContent.includes(text))
    if (!hit) return false
    hit.click()
    return true
  };
  window.__texts = (selector) => [...document.querySelectorAll(selector)].map((el) => el.textContent.trim());
  true
`

async function main() {
  fs.mkdirSync(tmpDir, { recursive: true })
  fs.rmSync(profileDir, { recursive: true, force: true })

  const child = spawn(
    CHROME,
    [
      '--headless=new',
      `--remote-debugging-port=${PORT}`,
      `--user-data-dir=${profileDir}`,
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-gpu',
      '--window-size=1440,900',
      'about:blank',
    ],
    { stdio: ['ignore', 'ignore', fs.openSync(chromeLog, 'w')] },
  )

  try {
    await waitFor(async () => {
      try {
        const res = await fetch(`http://127.0.0.1:${PORT}/json/version`)
        return res.ok
      } catch {
        return false
      }
    }, 20000, 'Chrome 调试端口就绪')

    const target = await (
      await fetch(`http://127.0.0.1:${PORT}/json/new?${encodeURIComponent('about:blank')}`, { method: 'PUT' })
    ).json()

    const ws = new WebSocket(target.webSocketDebuggerUrl)
    await new Promise((resolve, reject) => {
      ws.addEventListener('open', resolve, { once: true })
      ws.addEventListener('error', reject, { once: true })
    })
    const cdp = new CDP(ws)

    await cdp.send('Runtime.enable')
    await cdp.send('Page.enable')
    await cdp.send('Log.enable')

    const navigate = async (url) => {
      await cdp.send('Page.navigate', { url })
      await waitFor(() => cdp.eval(`document.readyState === 'complete'`), 20000, `打开 ${url}`)
      await sleep(600)
    }

    // ── 0. 干净状态 ───────────────────────────────────────────────
    await navigate(`${BASE}/login`)
    await cdp.eval(`localStorage.clear(); sessionStorage.clear(); true`)
    await navigate(`${BASE}/login`)
    await cdp.eval(HELPERS)

    const title = await cdp.eval(`document.querySelector('.login-card h1')?.textContent ?? ''`)
    const notice = await cdp.eval(`document.querySelector('.app-notice')?.textContent ?? ''`)
    log('0 首屏', `title=${JSON.stringify(title)} notice=${JSON.stringify(notice)}`)

    // ── 1. 注册新号并自动进 /chat ─────────────────────────────────
    const phone = process.env.E2E_PHONE || `139${String(Date.now()).slice(-8)}`
    const password = process.env.E2E_PASSWORD || 'laborlaw123'
    const nickname = process.env.E2E_NICKNAME || '验收用户'

    await cdp.eval(`window.__clickText('.login-tab', '注册')`)
    await sleep(300)
    await cdp.eval(`
      (() => {
        const inputs = [...document.querySelectorAll('.login-form input')]
        window.__setInput(inputs[0], '${phone}')
        window.__setInput(inputs[1], '${password}')
        window.__setInput(inputs[2], '${nickname}')
        return inputs.length
      })()
    `)
    await sleep(200)
    await cdp.eval(`window.__clickText('.login-form button', '注册并进入')`)

    await waitFor(
      () => cdp.eval(`location.pathname === '/chat'`),
      30000,
      '注册后跳转到 /chat',
    )
    await sleep(1200)
    const emptyHint = await cdp.eval(`document.querySelector('.messages .empty h3')?.textContent ?? ''`)
    log('1 注册', `phone=${phone} 已进入 /chat，空状态文案=${JSON.stringify(emptyHint)}`)

    // ── 1b. 令牌预续期：把过期时间改到 30 秒后，随便点一下就应换新令牌 ──
    const beforeRefresh = await cdp.eval(`
      (() => {
        localStorage.setItem('laborlaw.expires_at', String(Date.now() + 30_000))
        return {
          refresh: localStorage.getItem('laborlaw.refresh_token'),
          expires: localStorage.getItem('laborlaw.expires_at'),
        }
      })()
    `)
    await cdp.eval(`localStorage.setItem('laborlaw.expires_at', String(Date.now() + 30_000))`)
    // 刷新页面就会打 loadMe + listSessions 两个 REST 请求，正好撞上"该续期了"。
    await navigate(`${BASE}/chat`)
    await cdp.eval(HELPERS)
    await waitFor(
      async () =>
        (await cdp.eval(`localStorage.getItem('laborlaw.refresh_token')`)) !== beforeRefresh.refresh,
      30000,
      '预续期换掉了 refresh_token',
      200,
    )
    const afterRefresh = await cdp.eval(`
      (() => ({
        refresh: localStorage.getItem('laborlaw.refresh_token'),
        expires: Number(localStorage.getItem('laborlaw.expires_at')),
        access: (localStorage.getItem('laborlaw.access_token') ?? '').slice(-8),
      }))()
    `)
    log(
      '1b 预续期',
      `refresh 已轮换=${afterRefresh.refresh !== beforeRefresh.refresh} 新过期时间剩余=${Math.round((afterRefresh.expires - Date.now()) / 1000)}s access 尾号=${afterRefresh.access}`,
    )

    // ── 2. 首问：占位气泡 → 状态条 → 法条卡片 → 流式正文 ──────────
    const firstQuestion = '公司不给交社保，我能直接走吗'

    const sendMessage = async (text) => {
      await cdp.eval(`
        (() => {
          const ta = document.querySelector('.composer textarea')
          window.__setInput(ta, ${JSON.stringify(text)})
          return true
        })()
      `)
      await sleep(200)
      const clicked = await cdp.eval(`window.__clickText('.composer-actions button', '发送')`)
      if (!clicked) throw new Error('发送按钮不可点')
    }

    const state = () =>
      cdp.eval(`
        (() => ({
          thinking: !!document.querySelector('.thinking'),
          thinkingText: document.querySelector('.thinking')?.textContent ?? '',
          statusBar: document.querySelector('.tool-label')?.textContent ?? '',
          statusBarVisible: !!document.querySelector('.tool-bar:not(.faded)'),
          citations: [...document.querySelectorAll('.citation-card')].map((el) => ({
            title: el.querySelector('.citation-title')?.textContent ?? '',
            text: (el.querySelector('.citation-text')?.textContent ?? '').slice(0, 40),
            link: el.querySelector('.citation-link')?.getAttribute('href') ?? '',
          })),
          bubbles: [...document.querySelectorAll('.bubble-assistant .content')].map((el) => el.textContent.length),
          lastText: (document.querySelectorAll('.bubble-assistant .content')[0]?.textContent ?? '').slice(0, 60),
          stopVisible: !!window.__clickText0,
          sending: [...document.querySelectorAll('.composer-actions button')].some((b) => b.textContent.includes('停止')),
        }))()
      `)

    await sendMessage(firstQuestion)

    // 状态条是时序问题：running 只闪一下就被 ok 覆盖，单点采样很容易只抓到
    // "正在思考…"。所以整段回答期间持续采样，最后打印见过的所有文案。
    const statusLabels = []
    let barFaded = false
    const sampler = (async () => {
      const started = Date.now()

      while (Date.now() - started < 150000) {
        const snap = await cdp
          .eval(`
            (() => ({
              label: document.querySelector('.tool-label')?.textContent ?? '',
              faded: !!document.querySelector('.tool-row.faded'),
              citations: document.querySelectorAll('.citation-card').length,
            }))()
          `)
          .catch(() => null)
        if (!snap) return
        if (snap.label !== '' && !statusLabels.includes(snap.label)) statusLabels.push(snap.label)
        if (snap.faded) barFaded = true
        // 淡出发生在首个 delta（可能在法条卡片之后 1–3 秒），所以等到真淡出再收工。
        if (snap.citations > 0 && snap.faded) return
        await sleep(100)
      }
    })()

    const sawThinking = await waitFor(
      async () => (await state()).thinking,
      5000,
      '占位气泡出现',
      100,
    )
    const placeholder = await cdp.eval(`document.querySelector('.thinking')?.textContent ?? ''`)
    log('2a 占位', `占位气泡出现=${sawThinking} 文案=${JSON.stringify(placeholder)}`)

    const sawStatus = await waitFor(
      async () => (await state()).statusBarVisible,
      45000,
      '状态条出现',
      200,
    ).catch(() => false)
    log('2b 状态条', `出现=${!!sawStatus}`)

    await waitFor(
      async () => {
        const s = await state()
        return s.citations.length > 0 && s.bubbles[0] >= 20
      },
      120000,
      '法条卡片与正文出现',
      500,
    )
    const streaming = await state()
    log('2c 法条卡片', `张数=${streaming.citations.length}`)
    for (const [i, c] of streaming.citations.entries()) {
      log('2c 卡片', `${i + 1}. ${c.title} ｜ ${c.text}… ｜ ${c.link || '(无链接)'}`)
    }

    await waitFor(
      async () => !(await state()).sending,
      120000,
      '第一条回答结束',
      500,
    )
    const afterFirst = await state()
    await sampler
    log('2b 状态条', `文案序列=${JSON.stringify(statusLabels)} 首个 delta 后淡出=${barFaded}`)
    log(
      '2d 回答结束',
      `正文字数=${afterFirst.bubbles[0]} loading 已消失=${!afterFirst.thinking} 首段=${JSON.stringify(afterFirst.lastText)}`,
    )

    // ── 3. 追问同一会话 ──────────────────────────────────────────
    const before = await cdp.eval(`[...document.querySelectorAll('.session-item')].length`)
    await sendMessage('那我能要经济补偿吗')
    await waitFor(
      async () => {
        const s = await state()
        return s.bubbles.length >= 2 && s.bubbles[1] > 20 && !s.sending
      },
      // 上游偶发 50301，llm 层会自己重试两次，单轮实测最慢见过 180s；
      // 给到 300s，否则测出来的是模型排队时间，不是前端问题。
      300000,
      '第二轮回答结束',
      500,
    )
    const afterSecond = await state()
    const after = await cdp.eval(`[...document.querySelectorAll('.session-item')].length`)
    log(
      '3 追问',
      `会话数 ${before} → ${after}（应不变）；历史消息条数=${await cdp.eval(`document.querySelectorAll('.bubble').length`)}；第二轮字数=${afterSecond.bubbles[1]}`,
    )

    // ── 3.5 停止按钮：发送中可中断，气泡标记已中断 ────────────────
    await sendMessage('那我还能要求补缴社保吗')
    await waitFor(() => cdp.eval(`!!window.__stopBtn`), 15000, '停止按钮出现', 100).catch(async () => {
      await cdp.eval(`
        (() => {
          window.__stopBtn = [...document.querySelectorAll('.composer-actions button')].find((b) =>
            b.textContent.includes('停止'),
          )
          return !!window.__stopBtn
        })()
      `)
    })
    const stopClicked = await cdp.eval(`
      (() => {
        const btn = [...document.querySelectorAll('.composer-actions button')].find((b) =>
          b.textContent.includes('停止'),
        )
        if (!btn) return false
        btn.click()
        return true
      })()
    `)
    await waitFor(
      async () => !(await cdp.eval(`[...document.querySelectorAll('.composer-actions button')].some((b) => b.textContent.includes('停止'))`)),
      15000,
      '中断后发送按钮恢复',
      150,
    )
    const stopped = await cdp.eval(`
      (() => ({
        interrupted: document.querySelectorAll('.meta-line').length,
        text: (document.querySelector('.meta-line')?.textContent ?? '').trim(),
        thinking: !!document.querySelector('.thinking'),
      }))()
    `)
    log(
      '3.5 停止',
      `点击=${stopClicked} 中断标记=${stopped.interrupted} 文案=${JSON.stringify(stopped.text)} 占位已清=${!stopped.thinking}`,
    )

    // ── 4. 刷新后会话与历史仍在 ──────────────────────────────────
    await navigate(`${BASE}/chat`)
    await cdp.eval(HELPERS)
    await waitFor(
      async () => (await cdp.eval(`document.querySelectorAll('.session-item').length`)) > 0,
      20000,
      '刷新后会话列表恢复',
    )
    const restored = await cdp.eval(`
      (() => ({
        sessions: [...document.querySelectorAll('.session-title')].map((el) => el.textContent.trim()),
        activeId: document.querySelector('.session-item.active') ? true : false,
      }))()
    `)
    log('4 刷新', `会话列表=${JSON.stringify(restored.sessions)} 有选中项=${restored.activeId}`)

    const reopened = await cdp.eval(`
      (() => {
        const first = document.querySelector('.session-title')
        if (first) first.click()
        return !!first
      })()
    `)
    await waitFor(
      async () => (await cdp.eval(`document.querySelectorAll('.bubble').length`)) >= 2,
      20000,
      '打开会话后历史消息恢复',
    )
    const history = await cdp.eval(`
      (() => ({
        bubbles: document.querySelectorAll('.bubble').length,
        citations: document.querySelectorAll('.citation-card').length,
      }))()
    `)
    log('4 历史', `点开会话=${reopened} 气泡=${history.bubbles} 引用卡片=${history.citations}（历史消息应为 0）`)

    // ── 5. 令牌被改坏 → 跳登录页，不白屏 ─────────────────────────
    await cdp.eval(`localStorage.setItem('laborlaw.access_token', 'broken-token'); true`)
    await sendMessage('这条应该被拒绝')
    await waitFor(
      () => cdp.eval(`location.pathname === '/login'`),
      60000,
      '令牌失效后跳登录页',
      300,
    )
    const backToLogin = await cdp.eval(`
      (() => ({
        path: location.pathname,
        hasLoginCard: !!document.querySelector('.login-card'),
        tokenLeft: localStorage.getItem('laborlaw.access_token'),
        bodyText: document.body.innerText.length,
      }))()
    `)
    log(
      '5 失效',
      `已跳 ${backToLogin.path}，登录卡片在=${backToLogin.hasLoginCard}，令牌已清=${backToLogin.tokenLeft === null}，页面文字长度=${backToLogin.bodyText}`,
    )

    // ── 5b 登录失败必须原样显示后端文案 ──────────────────────────
    // 用一个必然不存在的号和错密码：这里要验的是"错误文案透传"，
    // 不是鉴权逻辑，所以不碰真实账号（也就不需要任何凭据）。
    await cdp.eval(`
      (() => {
        const [phone, password] = document.querySelectorAll('.login-form input')
        window.__setInput(phone, '13999999999')
        window.__setInput(password, 'wrongpass123')
        return true
      })()
    `)
    await sleep(150)
    await cdp.eval(`document.querySelector('.login-form .submit')?.click() || true`)
    await waitFor(
      async () => (await cdp.eval(`document.querySelector('.form-error')?.textContent ?? ''`)).length > 0,
      20000,
      '登录失败提示出现',
      200,
    )
    const formError = await cdp.eval(`document.querySelector('.form-error')?.textContent ?? ''`)
    log('5b 登录失败', `提示=${JSON.stringify(formError)} 路径仍为=${await cdp.eval(`location.pathname`)}`)

    // ── 6. 控制台 ────────────────────────────────────────────────
    log('6 控制台', `error=${consoleErrors.length} warning=${consoleWarnings.length}`)
    for (const e of consoleErrors.slice(0, 10)) log('6 error', e.replace(/\s+/g, ' ').slice(0, 200))
    for (const w of consoleWarnings.slice(0, 10)) log('6 warn', w.replace(/\s+/g, ' ').slice(0, 160))

    console.log('\nRESULT: PASS')
  } finally {
    child.kill()
  }
}

main().catch((error) => {
  console.error('\nRESULT: FAIL')
  console.error(error)
  for (const e of consoleErrors.slice(0, 10)) console.error('[console error]', e.replace(/\s+/g, ' ').slice(0, 300))
  process.exitCode = 1
})
