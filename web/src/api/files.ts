// 文件上传 / 解析结果 / 审查 / 任务查询。
//
// 上传单独走 postForm：axios 的实例默认头是 application/json，FormData 撞上
// 这个头会被 transformRequest 序列化成 JSON（文件变成 {}），所以必须显式声明
// multipart，让浏览器自己补 boundary。
//
// 审查是异步的：POST 只回 task_id，前端每 2 秒查一次 /tasks/{id}，
// 拿到 succeeded 再取 findings。轮询实现放在视图里，这里只提供三个原子动作。

import { get, getBinary, post, postForm } from './client'
import type { Finding, ParsedDoc, Review, Task } from './types'

/** POST /files，字段名固定为 file。进度回调拿不到总量时回调 null，不要假装百分比。 */
export function uploadFile(file: File, onProgress?: (percent: number | null) => void): Promise<ReviewFile> {
  const form = new FormData()
  form.append('file', file)
  return postForm<ReviewFile>('/files', form, onProgress)
}

// 后端 DTO 与本文件用到的字段一致；单独起名避免与 types.FileRecord 混淆。
export type ReviewFile = import('./types').FileRecord

/** GET /files/{id}。不存在或不属于当前用户时后端回 40401。 */
export function getFile(fileId: string): Promise<ReviewFile> {
  return get<ReviewFile>(`/files/${encodeURIComponent(fileId)}`)
}

/**
 * GET /files/{id}/pages。
 *
 * 已上传但没解析出文字层时后端回 42201（不是 404），调用方要单独提示"可能是扫描件"。
 */
export function getPages(fileId: string): Promise<ParsedDoc> {
  return get<ParsedDoc>(`/files/${encodeURIComponent(fileId)}/pages`)
}

/**
 * GET /files/{id}/content，原始字节。
 *
 * 不能把 URL 直接交给 pdf.js：它内部用 XHR，不带 Authorization。
 * 这里自己取 arrayBuffer，再喂给 getDocument({ data })。
 */
export function getFileBytes(fileId: string): Promise<ArrayBuffer> {
  return getBinary(`/files/${encodeURIComponent(fileId)}/content`)
}

/**
 * POST /contracts/review。
 *
 * 后端有两条正常路径，形状不一样，别只按契约里的 202 写：
 *   - 202 {task_id, review_id}：新起了一次审查，要轮询 /tasks；
 *   - 200 Review：这份文件已经有审查结果（后端直接复用），没有再排任务。
 * 已在审查中时后端回 40901。
 */
export type StartReviewResult =
  | { kind: 'accepted'; taskId: string; reviewId: string }
  | { kind: 'existing'; reviewId: string }

export async function startReview(fileId: string): Promise<StartReviewResult> {
  const data = await post<Record<string, unknown>>('/contracts/review', {
    file_id: fileId,
    rule_scope: [],
    async: true,
  })
  if (typeof data.task_id === 'string' && typeof data.review_id === 'string') {
    return { kind: 'accepted', taskId: data.task_id, reviewId: data.review_id }
  }
  const replay = data as unknown as Review
  if (typeof replay.id === 'string') {
    return { kind: 'existing', reviewId: replay.id }
  }
  throw new Error('审查接口返回了无法识别的结构')
}

/** GET /tasks/{id}。轮询用它。 */
export function getTask(taskId: string): Promise<Task> {
  return get<Task>(`/tasks/${encodeURIComponent(taskId)}`)
}

/** GET /reviews/{id}。刷新后用它把已完成的审查结果重新拉回来。 */
export function getReview(reviewId: string): Promise<Review> {
  return get<Review>(`/reviews/${encodeURIComponent(reviewId)}`)
}

/** GET /reviews/{id}/findings。 */
export function listFindings(reviewId: string): Promise<Finding[]> {
  return get<Finding[]>(`/reviews/${encodeURIComponent(reviewId)}/findings`)
}
