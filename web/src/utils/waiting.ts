// 等待态文案。
//
// qwen3.8-max 是推理模型：meta / tool_call / citation 在 0.1 秒内就到齐，
// 然后整整 30 多秒一个字节都没有。这段静止不是 bug，但演示时看着像卡死。
//
// 这里只做一件事：把"已经等了多久"翻译成一句人话。文案由真实耗时与真实
// citation 条数决定，没有任何"预计还剩 X 秒"——那种数字是编的，编的就会在
// 演示现场卡在 60% 不动，比空着更难看。

/** 检索阶段的窗口：2 秒。之后 tool_call 的 ok 会把文案切到"正在分析"。 */
export const WAITING_SEARCH_MS = 2000
/** 分析阶段的窗口：8 秒。 */
export const WAITING_ANALYZE_MS = 8000
/** 20 秒之后开始报秒数：20 秒以内报秒只会显得慢。 */
export const WAITING_COMPOSE_MS = 20000

export interface WaitingStage {
  /** 显示的那一行字。 */
  text: string
  /** 是否在文案里带"已用时 Ns"（调用方据此决定要不要每秒重算）。 */
  showSeconds: boolean
}

export interface WaitingStageOptions {
  /**
   * 检索是否已经结束（tool_call 的 status 变成 ok / failed）。
   *
   * 后端 0.03 秒就把 tool_call ok「命中 3 条法条」推过来了，如果这之后还写
   * 「正在检索相关法条…」，用户会以为文案卡住了——必须跟着真实事件走。
   */
  retrievalDone?: boolean
}

/**
 * waitingStage 把"已等待毫秒数 + 已收到几条 citation"翻成状态行文案。
 *
 * 四档：0–2s 检索、2–8s 分析、8–20s 梳理、20s 起报秒数。
 */
export function waitingStage(
  elapsedMs: number,
  citationCount: number,
  options: WaitingStageOptions = {},
): WaitingStage {
  const ms = Number.isFinite(elapsedMs) ? Math.max(0, elapsedMs) : 0
  const count = Number.isFinite(citationCount) ? Math.max(0, Math.floor(citationCount)) : 0

  if (ms < WAITING_SEARCH_MS && options.retrievalDone !== true) {
    return { text: '正在检索相关法条…', showSeconds: false }
  }

  if (ms < WAITING_ANALYZE_MS) {
    return {
      text: count > 0 ? `已找到 ${count} 条相关法条，正在分析` : '未检索到直接相关法条，正在分析',
      showSeconds: false,
    }
  }

  if (ms < WAITING_COMPOSE_MS) {
    return { text: '正在梳理法律关系与适用条款…', showSeconds: false }
  }

  return { text: `正在组织回答，请稍候（已用时 ${Math.floor(ms / 1000)}s）`, showSeconds: true }
}
