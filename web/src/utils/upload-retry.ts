// 上传失败后该不该给「重试」按钮。
//
// 判据只看 ApiError.code，不看文案：同一句话在不同环境下表达的意思不一样，
// 码是后端给的唯一稳定口径（见 api/client.ts 的 toApiError）。
//
// 纯函数、不 import api/client：那边的 `@/` 别名 node --test 解析不了，
// 而这些判断本身不需要 ApiError 这个类，只需要它身上的数字。

/** 网络根本没发出去 / 请求超时：toApiError 把这两种都收敛成 -1。 */
const CODE_NETWORK = -1

/**
 * 值得再点一次：断网、超时、网关或服务端故障。
 *
 * - -1 网络/超时，过几秒再试是合理的；
 * - 500–599 HTTP 状态（反向代理没带包络时才会露出这一档）；
 * - 5xxxx 业务码（50001 未接上链路、50201 上游 LLM 故障）——是后端/上游的问题，
 *   不以"这次请求本身错了"为前提。
 */
export function canRetryUpload(code: number): boolean {
  if (code === CODE_NETWORK) return true
  if (code >= 500 && code <= 599) return true
  if (code >= 50000 && code <= 59999) return true
  return false
}
