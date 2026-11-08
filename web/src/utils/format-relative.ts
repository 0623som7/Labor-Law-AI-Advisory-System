/**
 * 把 ISO 时间格式化成"多久以前"。
 *
 * 侧边栏只有标题时，完全看不出会话是什么时候聊的：昨天问的和上周问的排在一起，
 * 顺序像是随机的。用相对时间是因为这里要的是"新不新"，不是精确时刻。
 *
 * now 可注入，测试才不用跟时钟赛跑。日期部分按本地时区输出：
 * 服务端给的是 UTC（带 Z），直接切字符串会差 8 小时。
 */
export function formatRelative(iso: string, now: number = Date.now()): string {
  const ts = Date.parse(iso)
  if (Number.isNaN(ts)) {
    return ''
  }

  const diffMs = now - ts
  // 服务端时钟比浏览器快一点时会算出负数：当成"刚刚"，别显示"-1 分钟前"。
  if (diffMs < 60_000) {
    return '刚刚'
  }

  const minutes = Math.floor(diffMs / 60_000)
  if (minutes < 60) {
    return `${minutes} 分钟前`
  }

  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return `${hours} 小时前`
  }

  const days = Math.floor(hours / 24)
  if (days < 7) {
    return `${days} 天前`
  }

  const d = new Date(ts)
  const pad = (n: number): string => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}
