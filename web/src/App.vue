<script setup lang="ts">
// 应用外壳。鉴权提示条放在这里而不是各页面：契约与产品要求它全局可见，
// 写在对话页里的话，登录页就成了"看不到免责声明"的那一屏。
import { ref } from 'vue'

import { THEME_KEY, applyTheme, nextTheme, readTheme, themeLabel } from '@/utils/theme'

// 主题按钮放在免责条右侧：全局可用，且不占任何页面的头部高度。
const theme = ref(readTheme())

function cycleTheme(): void {
  theme.value = nextTheme(theme.value)
  try {
    localStorage.setItem(THEME_KEY, theme.value)
  } catch {
    // 隐私模式写不进存储：这一轮照样生效，只是刷新后回落到跟随系统
  }
  applyTheme(theme.value)
}
</script>

<template>
  <div class="app-shell">
    <header class="app-notice">
      本系统输出仅供参考，不构成法律意见。
      <button
        type="button"
        class="theme-btn"
        :title="`主题：${themeLabel(theme)}（点击切换）`"
        :aria-label="`主题：${themeLabel(theme)}（点击切换）`"
        @click="cycleTheme"
      >
        {{ themeLabel(theme) }}
      </button>
    </header>
    <main class="app-main">
      <RouterView />
    </main>
    <footer class="icp">
      <a href="https://beian.miit.gov.cn/" target="_blank" rel="noopener">粤ICP备2026184096号-1</a>
    </footer>
  </div>
</template>

<style scoped>
.app-shell {
  display: flex;
  flex-direction: column;
  height: 100vh;
  min-height: 0;
}

.app-notice {
  flex: none;
  /* 按钮绝对定位挂在这里：免责声明仍是一条居中的文本，flex 布局不许改 */
  position: relative;
  padding: 8px 16px;
  /* 顶部免责声明：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-notice-text);
  background: var(--ll-notice-bg);
  border-bottom: 1px solid var(--ll-notice-line);
  text-align: center;
}

.theme-btn {
  position: absolute;
  right: 12px;
  top: 50%;
  transform: translateY(-50%);
  /* 徽标/按钮小字 */
  font-size: var(--ll-fs-tag);
  color: var(--ll-text-weak);
  background: none;
  border: 1px solid var(--ll-notice-line);
  border-radius: var(--ll-radius-sm);
  padding: 4px 8px;
  cursor: pointer;
  line-height: 1.2;
}

.theme-btn:hover {
  color: var(--ll-primary);
  border-color: var(--ll-primary);
}

.app-main {
  flex: 1;
  min-height: 0;
  display: flex;
}

.icp {
  flex: none;
  /* 页脚备案号：说明文字 */
  font-size: var(--ll-fs-meta);
  color: var(--ll-text-hint);
  text-align: center;
  padding: 8px 0;
}

.icp a {
  color: inherit;
  text-decoration: none;
}

.icp a:hover {
  text-decoration: underline;
}

/* 路由渲染出的页面是 .app-main 的 flex item，默认 flex: 0 1 auto 会收缩到内容宽度，
   于是只有内容宽、右边一片空白。在这里统一兜底，新页面不必自己记得写 flex: 1。
   scoped 样式必须用 :deep() 穿透到子组件根元素，直接写 > * 匹配不到。
   min-width: 0 是给 flex item 的收缩下限兜底，避免宽内容（如 PDF 画布）把容器顶宽。 */
.app-main > :deep(*) {
  flex: 1;
  min-width: 0;
}
</style>
