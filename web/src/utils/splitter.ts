// 两栏分隔条 / 追问抽屉的纯函数。
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

/** 布尔型开关（例如"风险清单是否折叠"）的读写，脏值一律当默认值。 */
export function readFlag(key: string, fallback: boolean): boolean {
  try {
    const raw = localStorage.getItem(key)
    if (raw === null) return fallback
    if (raw === '1' || raw === 'true') return true
    if (raw === '0' || raw === 'false') return false
    return fallback
  } catch {
    return fallback
  }
}

export function writeFlag(key: string, value: boolean): void {
  try {
    localStorage.setItem(key, value ? '1' : '0')
  } catch {
    // 同上：记不住不影响本次操作。
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

/**
 * 上下方向（抽屉高度）的键盘调整。ArrowUp 变高——手柄在抽屉顶上，
 * 往上推就是把抽屉拉高，方向跟鼠标拖动一致。
 */
export function nextHeightByKey(
  current: number,
  key: string,
  opts: { step?: number; min?: number; max?: number } = {},
): number | null {
  const step = opts.step ?? KEY_STEP
  const min = opts.min ?? 240
  const max = opts.max ?? 560

  switch (key) {
    case 'ArrowUp':
      return Math.min(max, Math.round(current + step))
    case 'ArrowDown':
      return Math.max(min, Math.round(current - step))
    case 'Home':
      return min
    case 'End':
      return max
    default:
      return null
  }
}

/**
 * 追问抽屉的高度夹取。
 *
 * 三个上限里取最紧的：视口 70%（否则高屏上抽屉能吃掉整页）、560（再高也不像抽屉了）、
 * 面板高 - 160（卡片列表必须留着能看两行摘要在哪）。
 * 下限跟着上限走：面板本身只有 300 高时，上限是 140，下限就不能还是 240——
 * 否则 lower > upper，夹取函数会返回一个比上限还大的值，抽屉直接盖住卡片。
 */
export function clampDrawerHeight(desired: number, panelHeight: number, viewportHeight: number): number {
  const panel = Number.isFinite(panelHeight) && panelHeight > 0 ? panelHeight : 0
  const viewport = Number.isFinite(viewportHeight) && viewportHeight > 0 ? viewportHeight : 0
  // 面板高度读不到（首次挂载时父级还在布局）就不做"给卡片留 160"这条限制：
  // 否则 panel=0 → upper=0 → 抽屉整块塌掉。退回只剩视口与 560 两个上限。
  const capByPanel = panel > 0 ? Math.round(panel - 160) : Number.POSITIVE_INFINITY
  // 视口也读不到（0）时 upper 仍是 0：两条信息都没有，宁可 0 也不猜一个可能更糟的高度。
  const upper = Math.max(0, Math.min(Math.round(viewport * 0.7), 560, capByPanel))
  const lower = Math.min(240, upper)
  const want = Number.isFinite(desired) ? desired : lower
  return Math.round(Math.max(lower, Math.min(upper, want)))
}

/** 抽屉默认高度：只在第一次进页面时算一次（0.4 视口高，封顶 360）。 */
export function defaultDrawerHeight(viewportHeight: number): number {
  const viewport = Number.isFinite(viewportHeight) && viewportHeight > 0 ? viewportHeight : 0
  return Math.min(360, Math.round(viewport * 0.4))
}
