import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'
import AutoImport from 'unplugin-auto-import/vite'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'

// Element Plus 按需引入：组件与 ElMessage/ElMessageBox 这类命令式 API 都靠
// resolver 在编译期补 import 与样式，运行时不额外加载整包 CSS。
export default defineConfig({
  plugins: [
    vue(),
    AutoImport({
      resolvers: [ElementPlusResolver()],
      dts: 'auto-imports.d.ts',
    }),
    Components({
      resolvers: [ElementPlusResolver()],
      dts: 'components.d.ts',
    }),
  ],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5173,
    // 同源代理，不做 rewrite：前端一律请求 /api/v1/...，本地由 Vite 转发到 Go，
    // 上云由 nginx 转发，两条路径的路径前缀与行为一致，也就不用给后端开 CORS。
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false,
      },
    },
  },
})
