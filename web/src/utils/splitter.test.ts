import assert from 'node:assert/strict'
import test from 'node:test'

import {
  clampDrawerHeight,
  clampSideWidth,
  defaultDrawerHeight,
  MAX_SIDE,
  MIN_SIDE,
  nextHeightByKey,
  nextWidthByKey,
  readFlag,
  readSideWidth,
  writeFlag,
  writeSideWidth,
} from './splitter.ts'

// node --test 里没有 localStorage（那是浏览器 API），这里塞一个最小的替身，
// 只为让 readSideWidth / writeSideWidth 的边界分支能跑。
const store = new Map<string, string>()
;(globalThis as unknown as { localStorage: Storage }).localStorage = {
  getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
  setItem: (k: string, v: string) => void store.set(k, String(v)),
  removeItem: (k: string) => void store.delete(k),
  clear: () => store.clear(),
  key: (i: number) => [...store.keys()][i] ?? null,
  get length() {
    return store.size
  },
} as Storage

test('clampSideWidth：正常值原样返回', () => {
  assert.equal(clampSideWidth(500, 1440), 500)
})

test('clampSideWidth：低于下限夹到 MIN_SIDE', () => {
  assert.equal(clampSideWidth(100, 1440), MIN_SIDE)
  assert.equal(clampSideWidth(-999, 1440), MIN_SIDE)
})

test('clampSideWidth：容器很窄时被 total * 0.6 限制', () => {
  // 1000 * 0.6 = 600，比 MAX_SIDE 小，所以上限落在 600
  assert.equal(clampSideWidth(700, 1000), 600)
  assert.equal(clampSideWidth(720, 800), 480)
})

test('clampSideWidth：对侧至少留 minOpposite', () => {
  // 容器 900，maxRatio 给 540，但留 400 给对侧后上限是 500
  assert.equal(clampSideWidth(720, 900, { minOpposite: 400 }), 500)
})

test('clampSideWidth：自定义上下限（对话页侧栏 220/420）', () => {
  assert.equal(clampSideWidth(120, 1440, { min: 220, max: 420 }), 220)
  assert.equal(clampSideWidth(999, 1440, { min: 220, max: 420 }), 420)
  assert.equal(clampSideWidth(300, 1440, { min: 220, max: 420 }), 300)
})

test('clampSideWidth：容器尺寸不可用时退回下限，不产生 NaN', () => {
  assert.equal(clampSideWidth(500, 0), MIN_SIDE)
  assert.equal(clampSideWidth(Number.NaN, 1440), MIN_SIDE)
})

test('readSideWidth：正常值读回，脏值一律退回 fallback', () => {
  const key = 'laborlaw.test.sideWidth'
  writeSideWidth(key, 560)
  assert.equal(readSideWidth(key, 420, MIN_SIDE, MAX_SIDE), 560)

  localStorage.setItem(key, '不是数字')
  assert.equal(readSideWidth(key, 420, MIN_SIDE, MAX_SIDE), 420)

  localStorage.setItem(key, 'NaN')
  assert.equal(readSideWidth(key, 420, MIN_SIDE, MAX_SIDE), 420)

  localStorage.setItem(key, '9999') // 越界
  assert.equal(readSideWidth(key, 420, MIN_SIDE, MAX_SIDE), 420)

  localStorage.removeItem(key)
  assert.equal(readSideWidth(key, 420, MIN_SIDE, MAX_SIDE), 420)
})

test('nextWidthByKey：左右平移、Home/End 到两端、其余返回 null', () => {
  assert.equal(nextWidthByKey(420, 'ArrowLeft'), 396)
  assert.equal(nextWidthByKey(396, 'ArrowLeft'), 372)
  assert.equal(nextWidthByKey(420, 'ArrowRight'), 444)
  assert.equal(nextWidthByKey(340, 'ArrowLeft'), MIN_SIDE) // 不越过下限
  assert.equal(nextWidthByKey(700, 'ArrowRight'), MAX_SIDE) // 不越过上限
  assert.equal(nextWidthByKey(420, 'Home'), MIN_SIDE)
  assert.equal(nextWidthByKey(420, 'End', { max: 500 }), 500)
  assert.equal(nextWidthByKey(420, 'Enter'), null)
  assert.equal(nextWidthByKey(420, 'a'), null)
})

test('nextHeightByKey：向上变高、向下变矮，Home/End 到两端', () => {
  assert.equal(nextHeightByKey(360, 'ArrowUp'), 384)
  assert.equal(nextHeightByKey(384, 'ArrowDown'), 360)
  assert.equal(nextHeightByKey(240, 'ArrowDown'), 240) // 不越过下限
  assert.equal(nextHeightByKey(540, 'ArrowUp'), 560) // 不越过上限
  assert.equal(nextHeightByKey(360, 'Home'), 240)
  assert.equal(nextHeightByKey(360, 'End', { max: 520 }), 520)
  assert.equal(nextHeightByKey(360, 'ArrowLeft'), null) // 横向键不归它管
})

test('clampDrawerHeight：正常区间原样返回', () => {
  // 视口 900 → 0.7*900=630；面板 780 → 780-160=620；上限 560 最紧
  assert.equal(clampDrawerHeight(400, 780, 900), 400)
  assert.equal(clampDrawerHeight(600, 780, 900), 560)
})

test('clampDrawerHeight：面板过矮时下限跟着压，绝不返回颠倒区间', () => {
  // 面板 300 → upper = 140；lower 必须 ≤ upper
  const h = clampDrawerHeight(400, 300, 900)
  assert.equal(h, 140)
  assert.ok(h <= 140)
  assert.equal(clampDrawerHeight(10, 300, 900), 140) // 低于下限也抬到 lower=upper=140
})

test('clampDrawerHeight：视口过矮时上限被 0.7 * 视口 压住', () => {
  // 视口 500 → 350；面板 780 → 620；560 → 上限 350
  assert.equal(clampDrawerHeight(500, 780, 500), 350)
})

test('clampDrawerHeight：脏输入不产生 NaN', () => {
  assert.equal(clampDrawerHeight(Number.NaN, 780, 900), 240)
  // 面板高未知时不再塌成 0：只丢"给卡片留 160"这条限制，560 与视口上限仍在
  assert.equal(clampDrawerHeight(300, Number.NaN, 900), 300)
})

test('clampDrawerHeight：面板还没量到高度时不吃 160 的亏', () => {
  // 首次挂载那几毫秒父级 clientHeight 是 0；此时若仍算 panel-160 就等于 -160，
  // upper 被压到 0，抽屉整块塌掉。
  // 400 落在 [240, 560] 里，夹取结果就是 400 本身（不是 360——360 是默认高度，
  // 不是这个函数的输出）。
  assert.equal(clampDrawerHeight(400, 0, 900), 400)
})

test('clampDrawerHeight：面板读不到时仍受 560 硬上限约束', () => {
  assert.equal(clampDrawerHeight(9999, 0, 900), 560)
})

test('defaultDrawerHeight：0.4 视口高，封顶 360', () => {
  assert.equal(defaultDrawerHeight(900), 360)
  assert.equal(defaultDrawerHeight(700), 280)
  assert.equal(defaultDrawerHeight(Number.NaN), 0)
})

test('readFlag / writeFlag：只有 1/0/true/false 认，其余回 fallback', () => {
  const key = 'laborlaw.test.flag'
  writeFlag(key, true)
  assert.equal(readFlag(key, false), true)
  writeFlag(key, false)
  assert.equal(readFlag(key, true), false)
  localStorage.setItem(key, '随便什么')
  assert.equal(readFlag(key, true), true)
  localStorage.removeItem(key)
  assert.equal(readFlag(key, true), true)
})
