<script setup lang="ts">
// 应用外壳。鉴权提示条放在这里而不是各页面：契约与产品要求它全局可见，
// 写在对话页里的话，登录页就成了"看不到免责声明"的那一屏。
</script>

<template>
  <div class="app-shell">
    <header class="app-notice">本系统输出仅供参考，不构成法律意见。</header>
    <main class="app-main">
      <RouterView />
    </main>
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
  padding: 6px 16px;
  font-size: 12px;
  color: #8a6d3b;
  background: #fcf8e3;
  border-bottom: 1px solid #faebcc;
  text-align: center;
}

.app-main {
  flex: 1;
  min-height: 0;
  display: flex;
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
