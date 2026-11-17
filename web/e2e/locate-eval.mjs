// 合同审查定位评测：五份已知违法条款的样本，跑真链路，量「识别对不对、页跳得对不对、框画得准不准」。
//
//   node web/e2e/locate-eval.mjs
//   API_BASE=http://127.0.0.1:8080 node web/e2e/locate-eval.mjs
//   REMOTE=1 API_BASE=https://111.230.152.155:8443 node web/e2e/locate-eval.mjs   # 线上（自签证书）
//
// 为什么要有这个脚本：审查链路早就能出 findings 与 bbox，但从来没有量过它准不准。
// 答辩被问「框画得准吗、多页合同的页跳得对吗」时，只有这个脚本的输出能回答。
//
// 四个指标（定义写死，改口径就等于改考卷）：
//   召回   —— 8 个目标点（file, rule_id 去重后）里，findings 出现了同 rule_id 的个数
//   条款命中 —— 命中目标的 clause_ref.section 是否等于 gold 里同 rule 的植入条款之一（C 有两处，命中任一即算）
//   页命中 —— 命中目标的 finding，clause_ref.page 是否等于 gold 里 planted 的 page（0-based）
//   框命中 —— 命中目标的 bbox 是否四个有限数、x0<x1、y0<y1、且整框落在该页 width/height 内
//   误报   —— findings 里 rule_id 不在该文件 gold 目标集合的条数，按文件统计后取最大值
//
// 门槛：召回 8/8 · 条款命中 100% · 页命中 100% · 框命中 100% · 误报 ≤ 1 份 · 深页命中 ≥ 2 个
// 条款命中为什么单独一条：历史上真出现过这个 bug——R-NO-SOCIAL-INS 的框指到「第一条」而违规在
// 「第六条」，两条框同页、bbox 也一模一样，页命中抓不到，只有条款名能抓到。
// 深页那条是关键：如果五份的框全落在第 0 页，这个评测就没验到「多页跳转」，等于摆设。
//
// 任何一项不过都非 0 退出——宁可红着，也不要一个看起来正常的假绿。

import fs from 'node:fs'
import path from 'node:path'
import { setTimeout as sleep } from 'node:timers/promises'
import { fileURLToPath } from 'node:url'

// 线上是自签证书。preflight.mjs 用 node:https 的 per-request agent 只放宽单次请求；
// 这里用 fetch（要 FormData/Blob 上传），fetch 没有 per-request agent 的口子，
// 只能在进程级关掉校验——和 measure-remote.mjs 的做法一致，代价写在这里而不是藏着。
if (process.env.REMOTE === '1') process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0'

const API = (process.env.API_BASE || 'http://127.0.0.1:8080').replace(/\/+$/, '')
const PHONE = process.env.PREFLIGHT_PHONE || '13900009999'
const PASSWORD = process.env.PREFLIGHT_PASSWORD || 'laborlaw123'

const here = path.dirname(fileURLToPath(import.meta.url))
const fixturesDir = path.join(here, 'fixtures', 'contracts')
const goldPath = path.join(fixturesDir, 'gold.json')
const outPath = path.join(here, '.tmp', 'locate-eval.json')

// 门槛（写死）
const GATE_RECALL = 8
const GATE_CLAUSE_HIT = 1.0
const GATE_PAGE_HIT = 1.0
const GATE_BOX_HIT = 1.0
const GATE_MAX_FALSE_POSITIVE_FILES = 1
const GATE_DEEP_PAGE = 2

function must(cond, msg) {
  if (!cond) throw new Error(msg)
}

const pct = (x) => `${(x * 100).toFixed(1)}%`
const f1 = (n) => (Number.isFinite(n) ? n.toFixed(1) : String(n))

async function rest(pathname, { method = 'GET', token, body, form } = {}) {
  const headers = {}
  if (token) headers.Authorization = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const res = await fetch(API + pathname, {
    method,
    headers,
    body: form ?? (body !== undefined ? JSON.stringify(body) : undefined),
  })
  const text = await res.text()
  let json = null
  try {
    json = text ? JSON.parse(text) : null
  } catch {
    throw new Error(`${method} ${pathname} 返回的不是 JSON：${text.slice(0, 200)}`)
  }
  return { status: res.status, json }
}

// 统一信封：code === 0 才算成功；40901 是「已有在跑/已完成的审查」，由调用方处理。
async function ok(pathname, opts) {
  const r = await rest(pathname, opts)
  if (r.json?.code !== 0) {
    throw new Error(`${opts?.method || 'GET'} ${pathname} 失败：code=${r.json?.code} ${r.json?.message ?? ''}`)
  }
  return r.json.data
}

async function login() {
  const r = await rest('/api/v1/auth/login', {
    method: 'POST',
    body: { phone: PHONE, password: PASSWORD },
  })
  if (r.json?.code === 0) return r.json.data.access_token
  await rest('/api/v1/auth/register', {
    method: 'POST',
    body: { phone: PHONE, password: PASSWORD, nickname: '定位评测' },
  })
  const again = await rest('/api/v1/auth/login', {
    method: 'POST',
    body: { phone: PHONE, password: PASSWORD },
  })
  must(again.json?.code === 0, `登录失败：${JSON.stringify(again.json)}`)
  return again.json.data.access_token
}

async function upload(token, file) {
  const buf = fs.readFileSync(path.join(fixturesDir, file))
  const form = new FormData()
  form.append('file', new Blob([buf], { type: 'application/pdf' }), file)
  return ok('/api/v1/files', { method: 'POST', token, form })
}

async function ensureReview(token, fileId) {
  const r = await rest('/api/v1/contracts/review', {
    method: 'POST',
    token,
    body: { file_id: fileId },
  })
  if (r.json?.code === 0) return { reviewId: r.json.data.review_id ?? r.json.data.id, reused: false }
  // 40901：这个文件已经有审查在跑或已跑完，复用最新那条，不当错误。
  if (r.json?.code === 40901) {
    const list = await ok(`/api/v1/reviews?file_id=${encodeURIComponent(fileId)}`, { token })
    must(Array.isArray(list) && list.length > 0, `40901 但没有可复用的审查：${fileId}`)
    return { reviewId: list[0].id, reused: true }
  }
  throw new Error(`创建审查失败：code=${r.json?.code} ${r.json?.message ?? ''}`)
}

async function waitSucceeded(token, reviewId) {
  const t0 = Date.now()
  for (;;) {
    const rv = await ok(`/api/v1/reviews/${reviewId}`, { token })
    if (rv.status === 'succeeded') return rv
    must(rv.status !== 'failed', `审查失败：${reviewId}`)
    must(Date.now() - t0 < 30_000, `审查 30 秒未完成：${reviewId}（最后状态 ${rv.status}）`)
    await sleep(200)
  }
}

// 框命中四条：四个有限数、x0<x1 且 y0<y1、0<=x0 且 x1<=width、0<=y0 且 y1<=height
function boxOk(bbox, width, height) {
  if (!Array.isArray(bbox) || bbox.length !== 4) return { ok: false, why: 'bbox 不是四个数' }
  const [x0, y0, x1, y1] = bbox
  if (![x0, y0, x1, y1].every((n) => typeof n === 'number' && Number.isFinite(n))) {
    return { ok: false, why: 'bbox 含非有限数' }
  }
  if (!(x0 < x1 && y0 < y1)) return { ok: false, why: `x0<x1 / y0<y1 不成立（${bbox.map(f1).join(',')}）` }
  if (!(0 <= x0 && x1 <= width)) return { ok: false, why: `横向越界（x1=${f1(x1)} > width=${f1(width)}）` }
  if (!(0 <= y0 && y1 <= height)) return { ok: false, why: `纵向越界（y1=${f1(y1)} > height=${f1(height)}）` }
  return { ok: true, why: '' }
}

// ── 主流程 ────────────────────────────────────────────────────
const gold = JSON.parse(fs.readFileSync(goldPath, 'utf8'))
const token = await login()
console.log(`登录成功：${PHONE}  API=${API}`)
console.log(`gold：${gold.files.length} 份样本，${gold.files.reduce((s, f) => s + f.planted.length, 0)} 个植入点\n`)

// 目标点：按 (file, rule_id) 去重。同一规则在同一文件里埋两处（C 的押金与扣押证件）只算一个目标，
// 因为规则引擎一条规则只出一个框——框落在任一植入点所在条款上就算命中。
// clauses 保留该规则在本文件里的**全部**植入条款，条款命中判定用（C 有两处，命中任一即算）。
const targets = []
for (const f of gold.files) {
  const byRule = new Map()
  for (const p of f.planted) {
    if (!byRule.has(p.rule_id)) byRule.set(p.rule_id, [])
    byRule.get(p.rule_id).push(p)
  }
  for (const [ruleId, list] of byRule) {
    targets.push({
      file: f.file,
      ruleId,
      page: list[0].page,
      clause: list[0].clause,
      clauses: list.map((p) => p.clause),
    })
  }
}

const perFile = []
for (const f of gold.files) {
  const file = await upload(token, f.file)
  must(file.page_count === f.page_count, `${f.file} 页数变了：gold ${f.page_count}，实际上传 ${file.page_count}`)
  must(Math.abs(file.confidence - f.confidence) < 1e-9, `${f.file} confidence 变了：gold ${f.confidence}，实际 ${file.confidence}`)

  const pages = await ok(`/api/v1/files/${file.id}/pages`, { token })
  const pageSizes = new Map(pages.pages.map((p) => [p.index, p]))

  const { reviewId } = await ensureReview(token, file.id)
  const before = await ok(`/api/v1/reviews/${reviewId}`, { token })
  // 第二次跑同一份 PDF：上传命中 digest 缓存拿回同一个 file_id，审查也直接返回已有那条。
  // 这不是失败，是幂等——记下来，免得报告里把"没新建"当成"没跑"。
  const reused = before.status === 'succeeded'
  const review = reused ? before : await waitSucceeded(token, reviewId)
  const findings = await ok(`/api/v1/reviews/${reviewId}/findings?limit=200`, { token })

  const fileTargets = targets.filter((t) => t.file === f.file)
  const targetRuleIds = new Set(fileTargets.map((t) => t.ruleId))

  // 同一 rule_id 可能返回多条 finding（理论上不该，规则一条一个框）——取第一条判命中，多余的不算误报。
  const firstByRule = new Map()
  for (const fd of findings) if (!firstByRule.has(fd.rule_id)) firstByRule.set(fd.rule_id, fd)

  const hits = []
  for (const t of fileTargets) {
    const fd = firstByRule.get(t.ruleId)
    if (!fd) continue
    const pageIdx = fd.clause_ref?.page
    const size = pageSizes.get(pageIdx)
    const box = boxOk(fd.clause_ref?.bbox, size?.width ?? NaN, size?.height ?? NaN)
    const section = fd.clause_ref?.section ?? ''
    hits.push({
      ...t,
      finding: fd,
      section,
      clauseHit: t.clauses.includes(section),
      pageHit: pageIdx === t.page,
      boxOk: box.ok,
      boxWhy: box.why,
    })
  }
  const falsePositives = findings.filter((fd) => !targetRuleIds.has(fd.rule_id))

  perFile.push({
    file: f.file,
    fileId: file.id,
    reviewId,
    reused,
    pageCount: file.page_count,
    confidence: file.confidence,
    counts: review.counts,
    score: review.score,
    clausePassRate: review.clause_pass_rate,
    findings: findings.map((fd) => ({
      rule_id: fd.rule_id,
      section: fd.clause_ref?.section ?? '',
      page: fd.clause_ref?.page ?? null,
      bbox: fd.clause_ref?.bbox ?? null,
    })),
    targets: fileTargets.map((t) => t.ruleId),
    missed: fileTargets.filter((t) => !firstByRule.has(t.ruleId)).map((t) => t.ruleId),
    extra: falsePositives.map((fd) => fd.rule_id),
    hits,
  })
}

// ── 指标 ──────────────────────────────────────────────────────
const totalTargets = targets.length
const hitCount = perFile.reduce((s, f) => s + f.hits.length, 0)
const clauseHitCount = perFile.reduce((s, f) => s + f.hits.filter((h) => h.clauseHit).length, 0)
const pageHitCount = perFile.reduce((s, f) => s + f.hits.filter((h) => h.pageHit).length, 0)
const boxHitCount = perFile.reduce((s, f) => s + f.hits.filter((h) => h.boxOk).length, 0)
const recall = hitCount / totalTargets
const clauseHit = hitCount ? clauseHitCount / hitCount : 0
const pageHit = hitCount ? pageHitCount / hitCount : 0
const boxHit = hitCount ? boxHitCount / hitCount : 0
const falsePositiveFiles = perFile.filter((f) => f.extra.length > 0).length
const maxFalsePositive = Math.max(...perFile.map((f) => f.extra.length), 0)
const deepPageHits = perFile.flatMap((f) => f.hits.filter((h) => h.pageHit && h.page > 0))

// ── 逐份诊断 ──────────────────────────────────────────────────
console.log('── 逐份 findings ─────────────────────────────────────────────')
for (const f of perFile) {
  console.log(
    `\n${f.file}  page_count=${f.pageCount} confidence=${f.confidence} review=${f.reviewId}${f.reused ? '（复用）' : ''}`,
  )
  console.log(
    `  counts: high=${f.counts?.high ?? '-'} medium=${f.counts?.medium ?? '-'} low=${f.counts?.low ?? '-'}  score=${f.score ?? '-'}  clause_pass_rate=${f.clausePassRate ?? '-'}`,
  )
  for (const fd of f.findings) {
    const bb = Array.isArray(fd.bbox) ? fd.bbox.map(f1).join(', ') : String(fd.bbox)
    console.log(`  · ${fd.rule_id}  ${fd.section}  page=${fd.page}  bbox=[${bb}]`)
  }
  for (const h of f.hits) {
    const bad = []
    if (!h.clauseHit) bad.push(`条款指到「${h.section}」，gold 是 ${h.clauses.join(' / ')}`)
    if (!h.pageHit) bad.push(`页 ${h.finding.clause_ref?.page} ≠ gold ${h.page}`)
    if (!h.boxOk) bad.push(`框不合格：${h.boxWhy}`)
    if (bad.length) console.log(`  ✗ ${h.ruleId}：${bad.join('；')}`)
  }
  if (f.missed.length) console.log(`  漏报：${f.missed.join(', ')}`)
  if (f.extra.length) console.log(`  误报：${f.extra.join(', ')}`)
  if (!f.missed.length && !f.extra.length) console.log('  expected vs actual：完全一致')
}

// ── 汇总表 ────────────────────────────────────────────────────
console.log('\n── 汇总 ─────────────────────────────────────────────────────')
console.log('文件    目标  命中  条款命中  页命中  框命中  误报')
for (const f of perFile) {
  const hit = f.hits.length
  const ch = f.hits.filter((h) => h.clauseHit).length
  const ph = f.hits.filter((h) => h.pageHit).length
  const bh = f.hits.filter((h) => h.boxOk).length
  console.log(
    `${f.file.padEnd(7)} ${String(f.targets.length).padEnd(5)} ${String(hit).padEnd(5)} ${String(ch).padEnd(9)} ${String(ph).padEnd(6)} ${String(bh).padEnd(6)} ${String(f.extra.length).padEnd(5)}`,
  )
}

console.log('')
console.log(`召回      ${hitCount}/${totalTargets} = ${pct(recall)}        门槛 ${GATE_RECALL}/${totalTargets}`)
console.log(`条款命中  ${clauseHitCount}/${hitCount} = ${pct(clauseHit)}        门槛 ${pct(GATE_CLAUSE_HIT)}`)
console.log(`页命中    ${pageHitCount}/${hitCount} = ${pct(pageHit)}        门槛 ${pct(GATE_PAGE_HIT)}`)
console.log(`框命中    ${boxHitCount}/${hitCount} = ${pct(boxHit)}        门槛 ${pct(GATE_BOX_HIT)}`)
console.log(
  `误报      ${falsePositiveFiles} 份文件有误报（最多 ${maxFalsePositive} 条）  门槛 ≤ ${GATE_MAX_FALSE_POSITIVE_FILES} 份`,
)
console.log(
  `深页命中  ${deepPageHits.length} 个（page > 0）${deepPageHits.length ? '：' + deepPageHits.map((h) => `${h.file}/${h.ruleId}@page${h.page}`).join(', ') : ''}  门槛 ≥ ${GATE_DEEP_PAGE} 个`,
)

// ── 门槛判定 ──────────────────────────────────────────────────
const checks = [
  ['召回 8/8', hitCount === GATE_RECALL, `${hitCount}/${totalTargets}`],
  ['条款命中 100%', clauseHitCount === hitCount && hitCount > 0, `${clauseHitCount}/${hitCount}`],
  ['页命中 100%', pageHitCount === hitCount && hitCount > 0, `${pageHitCount}/${hitCount}`],
  ['框命中 100%', boxHitCount === hitCount && hitCount > 0, `${boxHitCount}/${hitCount}`],
  ['误报 ≤ 1 份', falsePositiveFiles <= GATE_MAX_FALSE_POSITIVE_FILES, `${falsePositiveFiles} 份`],
  [`深页命中 ≥ ${GATE_DEEP_PAGE} 个`, deepPageHits.length >= GATE_DEEP_PAGE, `${deepPageHits.length} 个`],
]
console.log('\n── 门槛 ─────────────────────────────────────────────────────')
for (const [name, pass, detail] of checks) console.log(`${pass ? '✓' : '✗'} ${name.padEnd(18)} ${detail}`)

const failed = checks.filter(([, pass]) => !pass)
const summary = {
  ran_at: new Date().toISOString(),
  api: API,
  gold: path.relative(path.resolve(here, '../..'), goldPath).split(path.sep).join('/'),
  targets: totalTargets,
  recall: { hits: hitCount, total: totalTargets, ratio: recall },
  clause_hit: { hits: clauseHitCount, of: hitCount, ratio: clauseHit },
  page_hit: { hits: pageHitCount, of: hitCount, ratio: pageHit },
  box_hit: { hits: boxHitCount, of: hitCount, ratio: boxHit },
  false_positives: { files: falsePositiveFiles, max_per_file: maxFalsePositive },
  deep_page_hits: deepPageHits.map((h) => ({ file: h.file, rule_id: h.ruleId, page: h.page })),
  gates: checks.map(([name, pass, detail]) => ({ name, pass, detail })),
  passed: failed.length === 0,
  per_file: perFile.map((f) => ({
    file: f.file,
    file_id: f.fileId,
    review_id: f.reviewId,
    reused: f.reused,
    page_count: f.pageCount,
    confidence: f.confidence,
    counts: f.counts,
    score: f.score,
    clause_pass_rate: f.clausePassRate,
    findings: f.findings,
    missed: f.missed,
    extra: f.extra,
    clause_hit: f.hits.filter((h) => h.clauseHit).length,
    page_hit: f.hits.filter((h) => h.pageHit).length,
    box_hit: f.hits.filter((h) => h.boxOk).length,
  })),
}

fs.mkdirSync(path.dirname(outPath), { recursive: true })
fs.writeFileSync(outPath, JSON.stringify(summary, null, 2) + '\n', 'utf8')
console.log(`\n明细已写入 ${path.relative(path.resolve(here, '../..'), outPath).split(path.sep).join('/')}`)
console.log(`结论：${summary.passed ? '全部门槛通过' : `未通过 ${failed.length} 条：${failed.map(([n]) => n).join('、')}`}`)

process.exit(summary.passed ? 0 : 1)
