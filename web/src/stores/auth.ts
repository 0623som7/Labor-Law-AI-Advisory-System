// 登录态。
//
// 只放"当前用户是谁 + 令牌在不在"，不放开哪个会话、哪个消息：
// 那些属于页面自己的状态，进 store 会让刷新逻辑绕一圈还得再理一遍。
//
// 刷新页面后 setTokens 已经写过 localStorage，所以这里初始化时先用
// getTokens() 决定 isLoggedIn，再异步补一次 /auth/me 拉昵称——
// 否则首屏会有一瞬间显示"未登录"，路由守卫会把人踢回登录页。

import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import * as authApi from '@/api/auth'
import type { LoginPayload, RegisterPayload } from '@/api/auth'
import { clearTokens, getTokens, setTokens } from '@/api/client'
import type { TokenPair, User } from '@/api/types'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<User | null>(null)
  const tokens = ref<TokenPair | null>(getTokens())
  const loading = ref(false)

  const isLoggedIn = computed(() => tokens.value !== null)

  function applyResult(result: { user: User; access_token: string; refresh_token: string; expires_in: number }): void {
    setTokens(result)
    tokens.value = getTokens()
    user.value = result.user
  }

  async function login(payload: LoginPayload): Promise<void> {
    loading.value = true
    try {
      applyResult(await authApi.login(payload))
    } finally {
      loading.value = false
    }
  }

  async function register(payload: RegisterPayload): Promise<void> {
    loading.value = true
    try {
      applyResult(await authApi.register(payload))
    } finally {
      loading.value = false
    }
  }

  /** 刷新页面后补拉用户信息。失败不抛：令牌失效由拦截器统一处理。 */
  async function loadMe(): Promise<void> {
    if (!tokens.value) return
    try {
      user.value = await authApi.me()
    } catch {
      // 拦截器已经把 40101 转成跳登录页；这里只是安静地不打断首屏。
    }
  }

  async function logout(): Promise<void> {
    try {
      // 服务端失败也要清本地，否则用户被困在"点登出没反应"的状态里。
      await authApi.logout()
    } catch {
      // 忽略：本地清理在下面无条件执行。
    } finally {
      reset()
    }
  }

  function reset(): void {
    clearTokens()
    tokens.value = null
    user.value = null
  }

  return { user, tokens, loading, isLoggedIn, login, register, loadMe, logout, reset }
})
