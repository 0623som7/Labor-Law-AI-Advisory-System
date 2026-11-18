// 上传前的本地校验。
//
// 抽成纯函数是为了能单测：判据在组件里时，只能靠 e2e 起一个真浏览器才知道
// 「选错文件时到底有没有发请求」，而这条判据恰恰是最该天天跑的那条。
//
// 阈值与后端 cfg.Storage.MaxUploadMB 默认值、以及 413 的文案「文件超过 5 MB」
// 完全对齐——三处说三个数（前端 20MB / nginx 12m / 后端 5MB）时，
// 用户永远是"已经等上传走完"之后才被告知不行的那一个。

/** 与后端 41301 的口径一致；nginx 的 12m 是外层上限，留作余量，不是这里的口径。 */
export const MAX_UPLOAD_BYTES = 5 * 1024 * 1024

/** 提示语复用同一个标签，避免文案里再写死一遍数字。 */
export const MAX_UPLOAD_LABEL = '5 MB'

/**
 * 文件体积的可读说法：1MB 以上按 MB 留一位小数，以下按 KB。
 *
 * 提示里必须带实际体积：只说"超过上限"用户不知道自己超了多少，
 * 是 5.1MB 还是 40MB，压缩到什么程度才算够。
 */
export function uploadSizeLabel(bytes: number): string {
  const mb = bytes / 1024 / 1024
  if (mb >= 1) return `${mb.toFixed(1)} MB`
  return `${Math.max(1, Math.round(bytes / 1024))} KB`
}

/**
 * 返回第一条不通过的原因，通过则返回 null。
 *
 * file.type 在部分系统上是空串（尤其是从网盘拖下来的 pdf），所以后缀再判一次；
 * 体积判断用 `>` 而不是 `>=`：正好 5MB 的文件能传，和后端口径一致。
 */
export function validateUpload(file: File): string | null {
  if (file.size > MAX_UPLOAD_BYTES) {
    return `这份 ${uploadSizeLabel(file.size)}，超过 ${MAX_UPLOAD_LABEL} 上限，请压缩后再传`
  }
  const isPdf = file.type === 'application/pdf' || file.name.toLowerCase().endsWith('.pdf')
  if (!isPdf) return '只支持 PDF 文件'
  return null
}
