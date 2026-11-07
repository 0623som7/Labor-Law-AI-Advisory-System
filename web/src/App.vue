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
</style>
