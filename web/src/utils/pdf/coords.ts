/**
 * PDF 坐标换算。
 *
 * 项目约定的坐标协议（写死在契约里，不许在这里"顺手修正"）：
 *   - bbox = [x0, y0, x1, y1]，单位 pt
 *   - 原点在左下角，y 轴向上（PDF 原生坐标系）
 *   - 浏览器 / canvas 的坐标系原点在左上角、y 轴向下
 *
 * pdf.js 的 viewport.convertToViewportRectangle() 已经吃掉了页面旋转、缩放与翻转，
 * 所以这里只做"取左上角 + 取绝对值当宽高"，不再自己乘 scale 或减 height，
 * 否则页面旋转 90° 的文档会翻到画布外面去。
 */

/** 视口像素矩形：left/top 是左上角坐标，width/height 恒为正。 */
export interface Rect {
  left: number
  top: number
  width: number
  height: number
}

/** 只依赖 pdf.js viewport 的这两个成员，单测里可以用假对象替代真的 PageViewport。 */
export interface ViewportLike {
  convertToViewportRectangle(rect: number[]): number[]
  width: number
  height: number
}

/** 保留两位小数：pt 换算到 px 之后会出现 80.99000000000001 这种尾巴，渲染时会抖。 */
function round2(v: number): number {
  return Math.round(v * 100) / 100
}

/**
 * 把 PDF 的 bbox 换算成视口像素矩形。
 *
 * @param bbox     [x0, y0, x1, y1]，pt，原点左下、y 向上
 * @param viewport page.getViewport({ scale }) 的返回值
 * @param scale    该 viewport 的缩放比例，只用于参数自检（<1 时仍然是合法缩放，
 *                 这里只拒绝 0 与负数），换算本身由 viewport 完成
 */
export function pdfBBoxToViewport(
  bbox: [number, number, number, number],
  viewport: ViewportLike,
  scale: number,
): Rect {
  if (!Number.isFinite(scale) || scale <= 0) {
    throw new Error(`缩放比例必须是正数，收到 ${scale}`)
  }
  const [x0, y0, x1, y1] = bbox
  const rect = viewport.convertToViewportRectangle([x0, y0, x1, y1])
  if (rect.length !== 4 || rect.some((v) => !Number.isFinite(v))) {
    throw new Error(`viewport 返回了非法矩形：${JSON.stringify(rect)}`)
  }
  const [vx0, vy0, vx1, vy1] = rect
  const left = Math.min(vx0, vx1)
  const top = Math.min(vy0, vy1)
  return {
    left: round2(left),
    top: round2(top),
    width: round2(Math.abs(vx1 - vx0)),
    height: round2(Math.abs(vy1 - vy0)),
  }
}

/** 把 bbox 直接表示成 CSS 的定位串，省得每个组件自己拼 left/top/width/height。 */
export function rectToCss(rect: Rect): { left: string; top: string; width: string; height: string } {
  return {
    left: `${rect.left}px`,
    top: `${rect.top}px`,
    width: `${rect.width}px`,
    height: `${rect.height}px`,
  }
}

/** 高亮框的外扩（px）：字号小的时候 2pt 的框只有 2px 高，肉眼看不见。 */
export const HIGHLIGHT_PADDING = 2

/** 把矩形向外扩 padding，并保证不越出视口（负坐标会被浏览器裁掉）。 */
export function inflateRect(rect: Rect, viewport: { width: number; height: number }, padding = HIGHLIGHT_PADDING): Rect {
  const left = Math.max(0, rect.left - padding)
  const top = Math.max(0, rect.top - padding)
  const right = Math.min(viewport.width, rect.left + rect.width + padding)
  const bottom = Math.min(viewport.height, rect.top + rect.height + padding)
  return {
    left: round2(left),
    top: round2(top),
    width: round2(Math.max(0, right - left)),
    height: round2(Math.max(0, bottom - top)),
  }
}

/**
 * 风险条目落在第几页。
 *
 * clause_ref.page 是 0 起；缺字段归 0（第 1 页），不返回 -1 让每个调用方自己兜。
 * 注意与 pdf.js 的 getPage() 区分：那边页号 1 起，差一页就会把框画到下一页。
 */
export function findingPage(f: { clause_ref?: { page?: number } }): number {
  const p = f.clause_ref?.page
  return typeof p === 'number' && Number.isFinite(p) && p >= 0 ? Math.floor(p) : 0
}
