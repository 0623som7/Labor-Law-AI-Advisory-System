import assert from 'node:assert/strict'
import test from 'node:test'

import {
  clampSideWidth,
  MAX_SIDE,
  MIN_SIDE,
  nextWidthByKey,
  readSideWidth,
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
