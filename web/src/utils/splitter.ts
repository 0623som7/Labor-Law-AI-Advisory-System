// 两栏分隔条的纯函数。
//
// 为什么不把它写进组件里：拖拽、键盘、持久化这三件事各自都有边界条件（负宽度、
// 对侧被压没、localStorage 里存了脏值），留在 SFC 里就只能靠浏览器实测，
// 而这些函数可以一条一条用 node --test 钉住。

export const MIN_SIDE = 320
export const MAX_SIDE = 720
export const KEY_STEP = 24

/** 任何一侧最宽占容器 60%，剩下的留给另一侧——再宽就不像两栏了。 */
const MAX_RATIO = 0.6

/**
 * 把期望宽度夹到合法区间：[min, min(max, total * maxRatio)]，并保证对侧至少留 minOpposite。
 *
 * 三处限制取最紧的那个，是因为它们各自会单独失效：容器很窄时 maxRatio 生效，
 * 容器很宽时 max 生效，而 minOpposite 管的是另一种情况——用户把分隔条拖到底，
 * 另一侧不能塌成 0 宽（那看起来像页面坏了，而且拖不回来）。
 */
export function clampSideWidth(
  desired: number,
  total: number,
  opts: {
    min?: number
    max?: number
    minOpposite?: number
    maxRatio?: number
  } = {},
): number {
  const min = opts.min ?? MIN_SIDE
  const max = opts.max ?? MAX_SIDE
  const minOpposite = opts.minOpposite ?? MIN_SIDE
  const maxRatio = opts.maxRatio ?? MAX_RATIO

  if (!Number.isFinite(total) || total <= 0) return min

  const upper = Math.max(
    min,
    Math.min(max, Math.floor(total * maxRatio), Math.floor(total - minOpposite)),
  )
  const want = Number.isFinite(desired) ? desired : min
  return Math.max(min, Math.min(upper, Math.round(want)))
}

/** 读持久化宽度。读到 NaN / 越界 / 非数字一律当没存过，返回 fallback。 */
export function readSideWidth(key: string, fallback: number, min: number, max: number): number {
  try {
    const raw = localStorage.getItem(key)
    if (raw === null || raw.trim() === '') return fallback
    const n = Number(raw)
    if (!Number.isFinite(n)) return fallback
    if (n < min || n > max) return fallback
    return Math.round(n)
  } catch {
    // 隐私模式 / 存储被禁用时 localStorage 会抛，不能让它把整页带崩。
    return fallback
  }
}

export function writeSideWidth(key: string, width: number): void {
  try {
    localStorage.setItem(key, String(Math.round(width)))
  } catch {
    // 写不进去就当没记住，不影响本次拖拽。
  }
}

/**
 * 键盘调整：ArrowLeft/ArrowRight 平移一步，Home/End 到两端，其余键返回 null
 * （null 表示"这个键不归我管"，调用方据此决定要不要 preventDefault）。
 */
export function nextWidthByKey(
  current: number,
  key: string,
  opts: { step?: number; min?: number; max?: number } = {},
): number | null {
  const step = opts.step ?? KEY_STEP
  const min = opts.min ?? MIN_SIDE
  const max = opts.max ?? MAX_SIDE

  switch (key) {
    case 'ArrowLeft':
      return Math.max(min, Math.round(current - step))
    case 'ArrowRight':
      return Math.min(max, Math.round(current + step))
    case 'Home':
      return min
    case 'End':
      return max
    default:
      return null
  }
}
