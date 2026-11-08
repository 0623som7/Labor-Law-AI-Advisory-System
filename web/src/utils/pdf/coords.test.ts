import { test } from 'node:test'
import assert from 'node:assert/strict'

import { HIGHLIGHT_PADDING, findingPage, inflateRect, pdfBBoxToViewport, rectToCss, type Rect } from './coords.ts'

/**
 * 假的 pdf.js viewport，行为照 PageViewport.convertToViewportRectangle 抄：
 * 先把 y 轴翻过来（height - y），再乘 scale。这样测试不依赖真 PDF 也能验证边界。
 */
function fakeViewport(widthPt: number, heightPt: number, scale: number) {
  return {
    width: widthPt * scale,
    height: heightPt * scale,
    convertToViewportRectangle(rect: number[]): number[] {
      const [x0, y0, x1, y1] = rect
      return [x0 * scale, (heightPt - y0) * scale, x1 * scale, (heightPt - y1) * scale]
    },
  }
}

const A4 = { width: 595.32, height: 841.92 }

test('pdfBBoxToViewport 把左下原点翻成左上原点', () => {
  const vp = fakeViewport(A4.width, A4.height, 1)
  // 贴近页面底部的一条 12pt 高的文字（y=270.99..282.99，从底往上数）
  const rect = pdfBBoxToViewport([80.99, 270.99, 539.23, 282.99], vp, 1)
  assert.deepEqual(rect, { left: 80.99, top: 558.93, width: 458.24, height: 12 })
})

test('pdfBBoxToViewport 在 scale=2 时给出像素值，而不是 pt', () => {
  const vp = fakeViewport(A4.width, A4.height, 2)
  const rect = pdfBBoxToViewport([80.99, 270.99, 539.23, 282.99], vp, 2)
  assert.deepEqual(rect, { left: 161.98, top: 1117.86, width: 916.48, height: 24 })
})

test('pdfBBoxToViewport 对倒序 bbox 也返回正的宽高', () => {
  const vp = fakeViewport(A4.width, A4.height, 1)
  // x0 > x1、y0 > y1：抽取器给出的框有时是反的
  const rect = pdfBBoxToViewport([539.23, 282.99, 80.99, 270.99], vp, 1)
  assert.deepEqual(rect, { left: 80.99, top: 558.93, width: 458.24, height: 12 })
})

test('pdfBBoxToViewport 拒绝 0 与负数 scale', () => {
  const vp = fakeViewport(A4.width, A4.height, 1)
  assert.throws(() => pdfBBoxToViewport([0, 0, 10, 10], vp, 0), /缩放比例必须是正数/)
  assert.throws(() => pdfBBoxToViewport([0, 0, 10, 10], vp, -1), /缩放比例必须是正数/)
  assert.throws(() => pdfBBoxToViewport([0, 0, 10, 10], vp, Number.NaN), /缩放比例必须是正数/)
})

test('pdfBBoxToViewport 遇到非法 viewport 返回值时抛错而不是画错位置', () => {
  const broken = {
    width: 100,
    height: 100,
    convertToViewportRectangle: () => [0, Number.NaN, 1, 1],
  }
  assert.throws(() => pdfBBoxToViewport([0, 0, 10, 10], broken, 1), /非法矩形/)
  const short = { width: 100, height: 100, convertToViewportRectangle: () => [0, 0, 1] }
  assert.throws(() => pdfBBoxToViewport([0, 0, 10, 10], short, 1), /非法矩形/)
})

test('rectToCss 输出 px 串', () => {
  const r: Rect = { left: 1.5, top: 2, width: 3, height: 4 }
  assert.deepEqual(rectToCss(r), { left: '1.5px', top: '2px', width: '3px', height: '4px' })
})

test('inflateRect 向外扩且不越出视口', () => {
  const vp = { width: 600, height: 842 }
  assert.deepEqual(inflateRect({ left: 100, top: 100, width: 50, height: 12 }, vp), {
    left: 98,
    top: 98,
    width: 54,
    height: 16,
  })
  // 贴边：左上角被夹到 0，右下角被夹到视口尺寸
  assert.deepEqual(inflateRect({ left: 0, top: 0, width: 600, height: 842 }, vp), {
    left: 0,
    top: 0,
    width: 600,
    height: 842,
  })
  assert.equal(HIGHLIGHT_PADDING, 2)
})

test('inflateRect 允许自定义 padding，且退化矩形宽度不为负', () => {
  const vp = { width: 100, height: 100 }
  const r = inflateRect({ left: 50, top: 50, width: 0, height: 0 }, vp, 0)
  assert.deepEqual(r, { left: 50, top: 50, width: 0, height: 0 })
  const wide = inflateRect({ left: 50, top: 50, width: 0, height: 0 }, vp, 200)
  assert.deepEqual(wide, { left: 0, top: 0, width: 100, height: 100 })
})

test('findingPage 缺字段归 0，负数与非整数都要收口', () => {
  assert.equal(findingPage({}), 0)
  assert.equal(findingPage({ clause_ref: {} }), 0)
  assert.equal(findingPage({ clause_ref: { page: 0 } }), 0)
  assert.equal(findingPage({ clause_ref: { page: 3 } }), 3)
  assert.equal(findingPage({ clause_ref: { page: 2.7 } }), 2)
  assert.equal(findingPage({ clause_ref: { page: -1 } }), 0)
  assert.equal(findingPage({ clause_ref: { page: Number.NaN } }), 0)
})
