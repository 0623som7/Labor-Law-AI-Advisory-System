// auth 五条：注册、登录、刷新、当前用户、登出。
//
// 注册与登录都直接回 AuthResult（契约里写明注册即发令牌），
// 所以两处都能一步进 /chat，不用再补一次登录。

import { get, post } from './client'
import type { AuthResult, User } from './types'

export interface LoginPayload {
  phone: string
  password: string
}

export interface RegisterPayload extends LoginPayload {
  nickname: string
}

export function login(payload: LoginPayload): Promise<AuthResult> {
  return post<AuthResult>('/auth/login', payload)
}

export function register(payload: RegisterPayload): Promise<AuthResult> {
  return post<AuthResult>('/auth/register', payload)
}

export function refresh(refreshToken: string): Promise<AuthResult> {
  return post<AuthResult>('/auth/refresh', { refresh_token: refreshToken })
}

export function me(): Promise<User> {
  return get<User>('/auth/me')
}

/** 登出只让服务端作废令牌；本地令牌由 store 负责清，避免两处都清导致回调竞争。 */
export function logout(): Promise<null> {
  return post<null>('/auth/logout')
}
