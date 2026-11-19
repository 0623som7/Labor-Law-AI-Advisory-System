// 主题三态：跟随系统 / 浅色 / 深色。
// 变量只认 [data-theme='dark']，element-plus 只认 html.dark，所以深色时两者都要挂上。
export type Theme = 'system' | 'light' | 'dark'

export const THEME_KEY = 'laborlaw.theme'

const VALID: Theme[] = ['system', 'light', 'dark']

/** 读用户选择；localStorage 不可用或值是脏数据时退回「跟随系统」。 */
export function readTheme(): Theme {
  try {
    const raw = globalThis.localStorage?.getItem(THEME_KEY)
    if (raw === 'light' || raw === 'dark' || raw === 'system') return raw
    return 'system'
  } catch {
    return 'system'
  }
}

/** 把三态解析成实际生效的两态；system 时问系统。 */
export function resolveTheme(theme: Theme): 'light' | 'dark' {
  if (theme === 'light' || theme === 'dark') return theme
  const mq = globalThis.matchMedia?.('(prefers-color-scheme: dark)')
  return mq?.matches ? 'dark' : 'light'
}

/** 点一次前进一格：跟随系统 → 浅色 → 深色 → 跟随系统。 */
export function nextTheme(theme: Theme): Theme {
  const i = VALID.indexOf(theme)
  return VALID[(i + 1) % VALID.length] ?? 'system'
}

/**
 * 落到 DOM：深色时 html 同时有 class="dark" 与 data-theme="dark"。
 * 浅色时 class 必须清掉，否则 element-plus 的深色变量还在生效。
 */
export function applyTheme(theme: Theme): void {
  const root = globalThis.document?.documentElement
  if (!root) return
  const resolved = resolveTheme(theme)
  if (resolved === 'dark') {
    root.classList.add('dark')
  } else {
    root.classList.remove('dark')
  }
  root.setAttribute('data-theme', resolved)
}

/** 主题按钮上的文案；title 与 aria-label 用同一句。 */
export function themeLabel(theme: Theme): string {
  if (theme === 'light') return '浅色'
  if (theme === 'dark') return '深色'
  return '跟随系统'
}
