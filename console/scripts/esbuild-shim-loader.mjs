/**
 * Node ESM loader：把 `import ... from 'esbuild'` 重定向到纯 JS 替身。
 *
 * 背景：esbuild 的 JS API 依赖"常驻子进程 + 管道"，在本机 DSH 沙箱下必然
 * `spawn EPERM`。而 Vite 的 vite:define 插件在构建时无条件调用
 * `esbuild.transform()`（没有配置开关），所以只能在这一层顶替模块。
 *
 * 只在 specifier 正好是 'esbuild' 时重定向，其它一律交回默认解析。
 * 该 loader 由 scripts/run-vite.mjs 通过 module.register() 注册，
 * 因此只影响 `pnpm dev/build/preview` 这三个由本工程启动的进程。
 */
export async function resolve(specifier, context, nextResolve) {
  if (specifier === 'esbuild') {
    return {
      url: new URL('./esbuild-text-shim.mjs', import.meta.url).href,
      shortCircuit: true,
    }
  }
  return nextResolve(specifier, context)
}