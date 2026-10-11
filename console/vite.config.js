import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 说明（阶段 1）：
// - base 使用相对路径 './'：产物将来会被 admin 服务挂载到 /console 之类的子路径，
//   相对 base 可以让同一份 dist 在任意挂载点下都能加载静态资源，无需重新构建。
// - server.proxy 只是本地联调便利项，默认仍然走 mock（VITE_USE_MOCK=true）。
//
// ⚠️ minify / cssMinify 为什么不使用 Vite 默认的 esbuild：
//   esbuild 的 JS API 是"常驻子进程 + 管道通信"架构（lib/main.js → ensureServiceIsRunning
//   用 child_process.spawn 且 stdio 为 pipe）。在本机 DSH 沙箱（workspace-write）里
//   禁止打开命名管道，直接报 `Error: spawn EPERM (errno -4048)`，构建无法完成。
//   因此这里改成两条**不产生子进程**的路径：
//     - JS 压缩：terser（纯 JS，跑在同一进程内）
//     - CSS 压缩：lightningcss（napi 原生插件，require 加载，同样不起子进程）
//   源代码全部是普通 .js / .vue（无 TS/JSX），构建期不需要 esbuild 做语法转译；
//   optimizeDeps 也只在 dev 阶段用 esbuild，build 阶段不触发。
//   这样在受限沙箱里也能跑通 `pnpm build`，且产物与默认配置等价（同为压缩后的 ESM）。
export default defineConfig({
  base: './',
  // ⚠️ esbuild: false 与上面的 minify/cssMinify 是一组：本沙箱禁止子进程管道通信，
  //    而 esbuild 的 JS API 依赖常驻子进程（spawn + pipe）。Vite 内部还有两个地方会用到 esbuild：
  //      1) 加载 vite.config.js 时用 esbuild 打包配置 → 通过 CLI 的 --configLoader runner 绕开
  //         （见 scripts/run-vite.mjs；本配置是纯 ESM，Node 可直接 import）；
  //      2) vite:define 插件做 define 替换时直接调用 esbuild.transform → 由
  //         scripts/esbuild-text-shim.mjs 顶替（没有配置开关，只能替换模块）。
  //    代价：产物不做语法降级（等价于 target=esnext），仅面向现代浏览器。
  //    这一点已在 README「未验证项/风险」中记录。
  esbuild: false,
  // 依赖预打包（dev）同样依赖 esbuild 的常驻子进程。scripts/run-vite.mjs 会探测
  // "能否以管道方式启动子进程"：不能时设置 CRONY_NO_DEP_OPT=1，这里据此关闭预打包。
  // Vite 5.1+ 已移除 optimizeDeps.disabled，官方替代写法是 noDiscovery + include 为空。
  // 普通机器上该变量不存在，optimizeDeps 保持 Vite 默认（dev 冷启动更快）。
  optimizeDeps:
    process.env.CRONY_NO_DEP_OPT === '1' ? { noDiscovery: true, include: [] } : undefined,
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5273,
    strictPort: false,
    proxy: {
      // 仅当 VITE_USE_MOCK=false 且 VITE_API_BASE 留空（走同源 /api）时才会用到。
      '/api': {
        target: process.env.VITE_PROXY_TARGET || 'http://127.0.0.1:8089',
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api/, ''),
      },
    },
  },
  build: {
    outDir: 'dist',
    assetsDir: 'assets',
    sourcemap: false,
    target: 'esnext',
    minify: 'terser',
    cssMinify: 'lightningcss',
    chunkSizeWarningLimit: 1200,
    terserOptions: {
      compress: { drop_debugger: true },
      format: { comments: false },
    },
    rollupOptions: {
      output: {
        manualChunks: {
          vue: ['vue', 'vue-router', 'pinia'],
          'element-plus': ['element-plus', '@element-plus/icons-vue'],
        },
      },
    },
  },
})