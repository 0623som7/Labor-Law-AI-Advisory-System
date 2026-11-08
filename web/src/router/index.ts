// 路由。四个页面，守卫写清楚：
//   - 没登录访问需要登录的页面 → 去 /login；
//   - 已登录访问 /login → 去 /chat（否则登录成功后回退一步又看到登录页）。

import { createRouter, createWebHistory } from 'vue-router'

import { getTokens } from '@/api/client'
import ChatView from '@/views/ChatView.vue'
import LoginView from '@/views/LoginView.vue'
import ReviewView from '@/views/ReviewView.vue'
import UploadView from '@/views/UploadView.vue'

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    { path: '/', redirect: '/chat' },
    { path: '/login', name: 'login', component: LoginView, meta: { requiresAuth: false } },
    { path: '/chat', name: 'chat', component: ChatView, meta: { requiresAuth: true } },
    { path: '/upload', name: 'upload', component: UploadView, meta: { requiresAuth: true } },
    { path: '/review/:fileId', name: 'review', component: ReviewView, meta: { requiresAuth: true } },
    // 直接读令牌而不是 store：守卫在 pinia 就绪前也可能被触发（硬刷新深链）。
    { path: '/:pathMatch(.*)*', redirect: '/chat' },
  ],
})

router.beforeEach((to) => {
  const loggedIn = getTokens() !== null
  if (to.meta.requiresAuth && !loggedIn) {
    return { name: 'login' }
  }
  if (to.name === 'login' && loggedIn) {
    return { name: 'chat' }
  }
  return true
})

export default router
