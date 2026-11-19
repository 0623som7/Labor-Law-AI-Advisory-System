import assert from 'node:assert/strict'
import test from 'node:test'

import { THEME_KEY, applyTheme, nextTheme, readTheme, resolveTheme, themeLabel } from './theme.ts'

// node --test 里没有 DOM：只为这几个用例塞最小替身，跑完立刻还原。
class FakeStorage {
  map = new Map<string, string>()
  getItem(k: string) {
    return this.map.has(k) ? (this.map.get(k) as string) : null
  }
  setItem(k: string, v: string) {
    this.map.set(k, v)
  }
}

class FakeElement {
  attrs = new Map<string, string>()
  classes = new Set<string>()
  classList = {
    add: (c: string) => this.classes.add(c),
    remove: (c: string) => this.classes.delete(c),
    contains: (c: string) => this.classes.has(c),
  }
  setAttribute(k: string, v: string) {
    this.attrs.set(k, v)
  }
  getAttribute(k: string) {
    return this.attrs.get(k) ?? null
  }
}

test('readTheme 脏数据与异常都退回 system', () => {
  const store = new FakeStorage()
  const g = globalThis as unknown as { localStorage?: unknown }
  const saved = g.localStorage
  g.localStorage = store
  try {
    assert.equal(readTheme(), 'system')
    store.setItem(THEME_KEY, 'dark')
    assert.equal(readTheme(), 'dark')
    store.setItem(THEME_KEY, 'LIGHT')
    assert.equal(readTheme(), 'system')
    g.localStorage = {
      getItem() {
        throw new Error('blocked')
      },
    }
    assert.equal(readTheme(), 'system')
  } finally {
    g.localStorage = saved
  }
})

test('nextTheme 三态循环', () => {
  assert.equal(nextTheme('system'), 'light')
  assert.equal(nextTheme('light'), 'dark')
  assert.equal(nextTheme('dark'), 'system')
})

test('resolveTheme：显式值原样，system 跟着系统走', () => {
  assert.equal(resolveTheme('light'), 'light')
  assert.equal(resolveTheme('dark'), 'dark')
  const g = globalThis as unknown as { matchMedia?: unknown }
  const saved = g.matchMedia
  try {
    g.matchMedia = () => ({ matches: true })
    assert.equal(resolveTheme('system'), 'dark')
    g.matchMedia = () => ({ matches: false })
    assert.equal(resolveTheme('system'), 'light')
    g.matchMedia = undefined
    assert.equal(resolveTheme('system'), 'light')
  } finally {
    g.matchMedia = saved
  }
})

test('applyTheme 深色要同时挂 class 与 data-theme，浅色要把 class 摘掉', () => {
  const root = new FakeElement()
  const g = globalThis as unknown as { document?: unknown; matchMedia?: unknown }
  const savedDoc = g.document
  const savedMq = g.matchMedia
  g.document = { documentElement: root }
  g.matchMedia = () => ({ matches: false })
  try {
    applyTheme('dark')
    assert.equal(root.getAttribute('data-theme'), 'dark')
    assert.equal(root.classList.contains('dark'), true)
    assert.equal(themeLabel('dark'), '深色')

    applyTheme('light')
    assert.equal(root.getAttribute('data-theme'), 'light')
    assert.equal(root.classList.contains('dark'), false)

    applyTheme('system')
    assert.equal(root.getAttribute('data-theme'), 'light')
    assert.equal(themeLabel('system'), '跟随系统')
    assert.equal(themeLabel('light'), '浅色')
  } finally {
    g.document = savedDoc
    g.matchMedia = savedMq
  }
})
