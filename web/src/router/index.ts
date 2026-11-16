// 路由。四个页面，守卫写清楚：
//   - 没登录访问需要登录的页面 → 去 /login；
//   - 已登录访问 /login → 去 /chat（否则登录成功后回退一步又看到登录页）。

import { createRouter, createWebHistory } from 'vue-router'

import { getTokens } from '@/api/client'

// 页面一律懒加载：静态 import 会把四个页面连同 pdfjs-dist（约 400KB）一起塞进首屏
// 主包，只用聊天的用户也要为 PDF 库付流量。getTokens 保持静态——守卫要在页面
// 模块下载前执行，懒加载它等于让首屏多一次往返。


const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    { path: '/', redirect: '/chat' },
    { path: '/login', name: 'login', component: () => import('@/views/LoginView.vue'), meta: { requiresAuth: false } },
    { path: '/chat', name: 'chat', component: () => import('@/views/ChatView.vue'), meta: { requiresAuth: true } },
    { path: '/upload', name: 'upload', component: () => import('@/views/UploadView.vue'), meta: { requiresAuth: true } },
    { path: '/review/:fileId', name: 'review', component: () => import('@/views/ReviewView.vue'), meta: { requiresAuth: true } },
    // 审查历史并进上传工作台的左栏，独立页取消。
    // 保留这条路由是为了别让旧书签和外部链接 404。
    { path: '/reviews', redirect: '/upload' },
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
