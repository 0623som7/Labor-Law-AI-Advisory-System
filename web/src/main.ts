import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from './App.vue'
import { onUnauthorized } from './api/client'
import router from './router'
import { useAuthStore } from './stores/auth'
import './style.css'

const app = createApp(App)

app.use(createPinia())
app.use(router)

// 令牌失效的唯一出口。放在这里而不是拦截器内部，是因为跳转要用 router，
// 而 router 又依赖视图组件——写进 api 层会形成循环引用。
onUnauthorized(() => {
  useAuthStore().reset()
  if (router.currentRoute.value.name !== 'login') {
    void router.replace({ name: 'login' })
  }
})

app.mount('#app')
