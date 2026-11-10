import { ElMessageBox } from 'element-plus'
import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from './App.vue'
import { ensureFreshToken, getTokens } from './api/client'
import { onSessionExpired, resetSessionExpired } from './api/session-expiry'
import router from './router'
import { useAuthStore } from './stores/auth'
import './style.css'

const app = createApp(App)

app.use(createPinia())
app.use(router)

// 会话失效的唯一出口。弹框而不是立刻跳转：
// 用户可能正在读一段很长的回答，或者输入框里已经打了两百字。
onSessionExpired(() => {
  useAuthStore().reset()

  // 已经在登录页：不需要再弹，把标志位放开，等下次真的过期再提示。
  if (router.currentRoute.value.name === 'login') {
    resetSessionExpired()
    return
  }

  // 必须现在取：弹框是异步的，用户点完按钮时 currentRoute 可能已经变了。
  const from = router.currentRoute.value.fullPath

  void ElMessageBox.alert(
    '登录状态已失效，请重新登录后继续。当前页面不会丢失。',
    '需要重新登录',
    {
      confirmButtonText: '去登录',
      showClose: false,
      closeOnClickModal: false,
      closeOnPressEscape: false,
      type: 'warning',
    },
  )
    .then(() => router.replace({ name: 'login', query: { redirect: from } }))
    .catch(() => undefined)
})

// 静默续期。正常情况下 2 小时过期根本不该被用户察觉：
// access token 一过期就换新，只要 refresh token 还活着就永远不打断。
// 5 分钟一次足够——后台标签页的定时器会被浏览器节流到 1 分钟一次，仍然跑得到。
setInterval(() => {
  if (getTokens()) void ensureFreshToken()
}, 5 * 60_000)

// 笔记本合盖一晚上、标签页被冻结，定时器全停，回到前台时令牌早过期了。
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && getTokens()) void ensureFreshToken()
})

app.mount('#app')
