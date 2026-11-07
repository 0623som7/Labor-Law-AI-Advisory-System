// 路由。目前只有登录页一个入口，对话页在下一提交里加。

import { createRouter, createWebHistory } from 'vue-router'

import { getTokens } from '@/api/client'
import LoginView from '@/views/LoginView.vue'

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    { path: '/', redirect: '/login' },
    { path: '/login', name: 'login', component: LoginView, meta: { requiresAuth: false } },
    { path: '/:pathMatch(.*)*', redirect: '/login' },
  ],
})

router.beforeEach((to) => {
  // 直接读令牌而不是 store：守卫在 pinia 就绪前也可能被触发（硬刷新深链）。
  const loggedIn = getTokens() !== null
  if (to.meta.requiresAuth && !loggedIn) {
    return { name: 'login' }
  }
  if (to.name === 'login' && loggedIn) {
    return { name: 'login' }
  }
  return true
})

export default router
