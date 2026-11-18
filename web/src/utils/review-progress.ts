// 审查进行中的阶段文案（纯函数）。
//
// 为什么按"已等毫秒"而不是按后端返回的 stage：后端 stage 是任务级状态
// （pending/running），一次审查里大半天不变，用户看到的就是一行不动的字，
// 容易以为卡死。而"已经等了多久"是本机就能算的，且和用户体感直接对应。
//
// 抽成纯函数是为了能单测边界：4 档的分界线（1500/4000/9000）写在模板里
// 就只能靠肉眼盯秒表。

/** 四档文案，逐字固定——对话页那两档也是这么写的，同一个产品不要两种口气。 */
const STAGE_PARSE = '正在解析文字层…'
const STAGE_RULES = '正在比对试用期、社保、押金、最低工资等 8 类风险…'
const STAGE_LOCATE = '正在逐条定位原文位置…'
const STAGE_SLOW = '这一步比平时慢，仍在处理，请不要关闭页面…'

/** 两个分界线：解析→规则、规则→定位、定位→偏慢。 */
export const REVIEW_STAGE_MS = {
  parse: 1500,
  rules: 4000,
  locate: 9000,
} as const

/**
 * 按已等毫秒给出此刻该显示的那一句。
 *
 * 负数与 NaN 按 0 处理：定时器被清掉或时钟回拨时，宁可按第一档说话，
 * 也不要让界面出现空白或 "NaN 毫秒"。
 */
export function reviewStage(elapsedMs: number): string {
  const ms = Number.isFinite(elapsedMs) && elapsedMs > 0 ? elapsedMs : 0
  if (ms < REVIEW_STAGE_MS.parse) return STAGE_PARSE
  if (ms < REVIEW_STAGE_MS.rules) return STAGE_RULES
  if (ms < REVIEW_STAGE_MS.locate) return STAGE_LOCATE
  return STAGE_SLOW
}

export interface ReviewProgressLine {
  /** false 时整行不渲染：审查结束、失败、或者根本没在跑。 */
  visible: boolean
  text: string
}

/**
 * 审查进行中该不该显示这一行、显示哪一句。
 *
 * 结束时立刻不可见（不留残影）：卡片区马上会被结果占满，
 * 再挂一句"正在比对…"就是自相矛盾的画面。
 */
export function reviewProgressLine(input: { elapsedMs: number; running: boolean }): ReviewProgressLine {
  if (!input.running) return { visible: false, text: '' }
  return { visible: true, text: reviewStage(input.elapsedMs) }
}
