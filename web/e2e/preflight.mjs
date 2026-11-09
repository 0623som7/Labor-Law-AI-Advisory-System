// 答辩前 30 秒自检：全绿再上台。
//   node web/e2e/preflight.mjs                 # 检查本机演示环境
//   API_BASE=https://111.230.152.155:8443 WEB_BASE=https://111.230.152.155:8443 \
//     node web/e2e/preflight.mjs --remote      # 检查线上
//
// 每一项只回答"能不能上台用"，所以失败只打一行人话，不打 JSON、不打堆栈：
// 这是给站在投影前的人看的，不是给调试的人看的。要看细节直接跑 demo.mjs。
//
// --remote 只放宽证书校验（线上是自签的 LaborLaw Local CA），不写
// NODE_TLS_REJECT_UNAUTHORIZED=0——那会把进程内所有 TLS 校验一起关掉。
// 不加 --remote 时仍然严格校验证书，避免把"证书过期"当成"环境正常"。

import fs from 'node:fs'
import http from 'node:http'
import https from 'node:https'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const REMOTE = process.argv.includes('--remote')
const API = (process.env.API_BASE || 'http://127.0.0.1:8080').replace(/\/+$/, '')
const WEB = (process.env.WEB_BASE || 'http://127.0.0.1:5173').replace(/\/+$/, '')

// 路径从脚本自身位置推，不依赖 cwd：仓库根、web 目录、任意位置跑都行。
const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '../..')
const PDF = path.join(repoRoot, '.dev', 'contract-demo.pdf')

const CHROME =
  process.env.CHROME_PATH ||
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe'

const insecureAgent = REMOTE ? new https.Agent({ rejectUnauthorized: false }) : undefined

// ── 最小 HTTP 客户端 ────────────────────────────────────────────
// 用 node:http(s) 而不是 fetch：自签证书需要 per-request 的 agent，
// 全局 fetch 只能靠 NODE_TLS_REJECT_UNAUTHORIZED 这种一刀切开关。
function request(url, { method = 'GET', headers = {}, body } = {}) {
  return new Promise((resolve, reject) => {
    const u = new URL(url)
    const mod = u.protocol === 'https:' ? https : http
    const req = mod.request(
      u,
      {
        method,
        headers,
        agent: u.protocol === 'https:' ? insecureAgent : undefined,
        timeout: 30_000,
      },
      resolve,
    )
    req.on('timeout', () => req.destroy(Object.assign(new Error('请求超时（30 秒）'), { code: 'ETIMEDOUT' })))
    req.on('error', reject)
    if (body !== undefined) req.write(body)
    req.end()
  })
}

async function readText(res) {
  let text = ''
  await new Promise((resolve, reject) => {
    res.setEncoding('utf8')
    res.on('data', (c) => (text += c))
    res.on('end', resolve)
    res.on('error', reject)
  })
  return text
}

async function apiJson(pathname, { method = 'GET', token, body } = {}) {
  const headers = {}
  if (token) headers.Authorization = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const res = await request(`${API}/api/v1${pathname}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await readText(res)
  let json
  try {
    json = JSON.parse(text)
  } catch {
    throw new Error(`返回的不是 JSON（HTTP ${res.statusCode}）`)
  }
  return { status: res.statusCode, json }
}

// SSE 咨询：只关心"有没有出字、引了几条、花了多久"。
async function sseChat(token, input) {
  const res = await request(`${API}/api/v1/chat/completions`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Accept: 'text/event-stream',
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({ input }),
  })
  if (res.statusCode !== 200) {
    const text = await readText(res)
    throw new Error(`HTTP ${res.statusCode}${text.includes('40101') ? '（登录状态失效）' : ''}`)
  }

  const started = Date.now()
  let buf = ''
  let firstDelta = null
  let citations = 0
  let chars = 0
  let done = false

  for await (const chunk of res) {
    buf += chunk.toString('utf8').replace(/\r\n?/g, '\n')
    let idx
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      const event = /^event: (.+)$/m.exec(frame)?.[1]
      if (event === 'error') {
        throw new Error('模型侧返回了 error 帧')
      }
      if (event === 'delta') {
        if (firstDelta === null) firstDelta = Date.now() - started
        chars++
      }
      if (event === 'citation') citations++
      if (event === 'done') done = true
    }
    if (done) break
  }
  res.destroy()
  return { firstDeltaMs: firstDelta, totalMs: Date.now() - started, citations, chars, done }
}

// 演示账号：固定号，先登录，账号不存在才注册。注册限流 5 次/分钟，
// 每次自检都新注册一个号，连跑几遍就会撞上 42901。
const PHONE = process.env.PREFLIGHT_PHONE || '13900009999'
const PASSWORD = process.env.PREFLIGHT_PASSWORD || 'laborlaw123'

async function demoToken() {
  const login = await apiJson('/auth/login', {
    method: 'POST',
    body: { phone: PHONE, password: PASSWORD },
  })
  if (login.status === 200 && login.json.code === 0) return login.json.data.access_token

  const reg = await apiJson('/auth/register', {
    method: 'POST',
    body: { phone: PHONE, password: PASSWORD, nickname: '演示自检' },
  })
  if (reg.status === 200 && reg.json.code === 0) return reg.json.data.access_token
  throw new Error(reg.json?.message || `注册与登录都失败（HTTP ${reg.status}）`)
}

// ── 失败原因转人话 ──────────────────────────────────────────────
const BY_CODE = {
  ECONNREFUSED: '连接被拒绝，服务没起来',
  ECONNRESET: '连接被重置',
  ENOTFOUND: '域名解析失败',
  EHOSTUNREACH: '主机不可达',
  ETIMEDOUT: '请求超时，网络或服务卡住',
  DEPTH_ZERO_SELF_SIGNED_CERT: '证书是自签的，检查时要加 --remote',
  SELF_SIGNED_CERT_IN_CHAIN: '证书链里有自签证书，检查时要加 --remote',
  UNABLE_TO_VERIFY_LEAF_SIGNATURE: '证书无法校验，检查时要加 --remote',
  UNABLE_TO_GET_ISSUER_CERT_LOCALLY: '证书签发者不在信任链里，检查时要加 --remote',
}

function human(err) {
  if (!err) return '未知错误'
  if (err.code && BY_CODE[err.code]) return BY_CODE[err.code]
  return err.message ? err.message.split('\n')[0] : String(err)
}

// ── 七项检查 ────────────────────────────────────────────────────
const checks = [
  {
    name: 'API 可达',
    async run() {
      const res = await request(`${API}/healthz`)
      const text = await readText(res)
      if (res.statusCode !== 200) throw new Error(`/healthz 返回 HTTP ${res.statusCode}`)
      let json
      try {
        json = JSON.parse(text)
      } catch {
        throw new Error('/healthz 返回的不是 JSON')
      }
      if (json.status !== 'ok') throw new Error(`/healthz 状态是 ${json.status || '(空)'}`)
      return `/healthz 200 · ${API}`
    },
  },
  {
    name: '依赖服务',
    async run() {
      const res = await request(`${API}/readyz`)
      const text = await readText(res)
      if (res.statusCode !== 200) throw new Error(`/readyz 返回 HTTP ${res.statusCode}`)
      let json
      try {
        json = JSON.parse(text)
      } catch {
        throw new Error('/readyz 返回的不是 JSON')
      }
      const mysql = json.checks?.mysql
      const redis = json.checks?.redis
      if (mysql !== 'up') throw new Error(`mysql 是 ${mysql || '未上报'}`)
      // Redis 只在检索缓存与限流上用到，但演示期间限流一挂就是"登录失败"，
      // 所以按硬失败处理，不降级成提示。
      if (redis !== 'up') throw new Error(`redis 是 ${redis || '未上报'}`)
      return 'mysql up · redis up'
    },
  },
  {
    name: '前端可达',
    async run() {
      const res = await request(`${WEB}/`)
      const type = String(res.headers['content-type'] || '')
      const text = await readText(res)
      if (res.statusCode !== 200) throw new Error(`首页返回 HTTP ${res.statusCode}`)
      if (!type.includes('text/html')) throw new Error(`首页 content-type 是 ${type || '(空)'}`)
      if (!/<div id="app"/.test(text) && !/src="\/assets\//.test(text)) {
        throw new Error('首页返回了 HTML，但不像打包后的前端（找不到挂载点或资源引用）')
      }
      return `${WEB} 200 · ${type.split(';')[0]}`
    },
  },
  {
    name: '法条库已灌',
    async run() {
      const token = await demoToken()
      const res = await apiJson('/statutes?q=' + encodeURIComponent('经济补偿') + '&top_k=1', { token })
      if (res.status !== 200 || res.json.code !== 0) {
        throw new Error(res.json?.message || `HTTP ${res.status}`)
      }
      const list = res.json.data || []
      if (list.length === 0) throw new Error('检索返回 0 条，法条表可能是空的')
      const hit = list[0].statute
      if (!hit?.law || !hit?.article) throw new Error('返回条目缺少 law / article，不是真实法条')
      return `${hit.id} · ${hit.law}${hit.article}`
    },
  },
  {
    name: '向量检索生效',
    async run() {
      const token = await demoToken()
      const question = '公司不给我交社保怎么办'
      const first = await sseChat(token, question)
      if (!first.done) throw new Error('第一遍没等到 done 帧')
      if (first.citations < 1) throw new Error('第一遍 0 条 citation')
      const second = await sseChat(token, question)
      if (!second.done) throw new Error('第二遍没等到 done 帧')
      if (second.citations < 1) throw new Error('第二遍 0 条 citation')

      const fmt = (r) => `${(r.totalMs / 1000).toFixed(1)}s`
      const faster = second.totalMs + 300 < first.totalMs
      const detail =
        `两遍各 ${first.citations} 条 citation · ` +
        `第一遍 ${fmt(first)}（首字 ${first.firstDeltaMs === null ? '无' : (first.firstDeltaMs / 1000).toFixed(1) + 's'}）· ` +
        `第二遍 ${fmt(second)}`
      if (faster) return { note: detail + ' · 查询向量缓存生效' }
      return {
        warn: true,
        note: detail + ' · 缓存未命中（不影响演示，但会慢 0.7s）',
      }
    },
  },
  {
    name: '演示合同在',
    async run() {
      if (!fs.existsSync(PDF)) throw new Error('.dev/contract-demo.pdf 不存在')
      const size = fs.statSync(PDF).size
      if (size <= 100_000) throw new Error(`合同只有 ${size} 字节，像是空文件或没生成完`)
      return `.dev/contract-demo.pdf · ${(size / 1024).toFixed(0)} KB`
    },
  },
  {
    name: 'Chrome 在',
    async run() {
      if (!fs.existsSync(CHROME)) throw new Error(`找不到 ${CHROME}，可用 CHROME_PATH 指定`)
      return CHROME
    },
  },
]

const results = []
const t0 = Date.now()

console.log(`自检目标：API ${API} · WEB ${WEB}${REMOTE ? ' · 已放宽证书校验（--remote）' : ''}`)

for (let i = 0; i < checks.length; i++) {
  const check = checks[i]
  const started = Date.now()
  try {
    const out = await check.run()
    const note = typeof out === 'string' ? out : out.note
    const warn = typeof out === 'object' && out.warn
    const took = ((Date.now() - started) / 1000).toFixed(1)
    console.log(`${warn ? '⚠' : '✓'} 检查 ${i + 1} · ${check.name}：${note}（${took}s）`)
    results.push({ ok: true, warn: !!warn, name: check.name, reason: note })
  } catch (err) {
    const took = ((Date.now() - started) / 1000).toFixed(1)
    console.log(`✗ 检查 ${i + 1} · ${check.name}：${human(err)}（${took}s）`)
    results.push({ ok: false, warn: false, name: check.name, reason: human(err) })
  }
}

const failed = results.filter((r) => !r.ok)
const warned = results.filter((r) => r.warn)
const total = ((Date.now() - t0) / 1000).toFixed(1)
console.log('')

if (failed.length === 0) {
  // ⚠ 不是失败：缓存未命中只是慢一点，功能是通的，上台前知道就行。
  if (warned.length > 0) {
    console.log(`DEMO READY（${checks.length} 项通过，${warned.length} 项提示，用时 ${total}s）`)
    for (const w of warned) console.log(`  ⚠ ${w.name}：${w.reason}`)
  } else {
    console.log(`DEMO READY（${checks.length} 项全绿，用时 ${total}s）`)
  }
  process.exit(0)
}
console.log(`NOT READY: ${failed.length} 项未通过（用时 ${total}s）`)
for (const f of failed) {
  console.log(`  - ${f.name}${f.reason ? '：' + f.reason : ''}`)
}
process.exit(1)
