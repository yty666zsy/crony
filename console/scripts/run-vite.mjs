/**
 * Vite CLI 启动器。
 *
 * package.json 里的 dev / build / preview 都通过这里启动 vite，做两件事：
 *
 * 1. 先加载 child-process-sandbox-shim.mjs，把沙箱"同步抛出 spawn EPERM"
 *    的异常行为还原成标准 Node 的异步 'error' 事件（否则 Vite 内部的
 *    `exec('net use')` 会让进程直接崩掉）。
 *
 * 2. 注册 esbuild-shim-loader.mjs，把 `esbuild` 模块换成纯 JS 替身：
 *    Vite 的 vite:define 插件在构建时会无条件调用 esbuild.transform()，
 *    而 esbuild 的 JS API 依赖"常驻子进程 + 管道"，在本沙箱必然 EPERM。
 *    替身只实现 define 替换这一个能力，其余 API 一旦被调用就抛错（不静默降级）。
 *
 * 3. 给 dev / build 追加 `--configLoader runner`：
 *    Vite 默认用 esbuild 打包 vite.config.js，runner 模式下改用 Node 的 import()
 *    直接加载（本工程配置是纯 ESM），从而绕开 esbuild。
 *
 * 这三项都是"受限环境兼容"，不影响产物语义；详见 README「未验证项/风险」。
 */
import { createRequire } from 'node:module'
import { register } from 'node:module'
import { execFileSync } from 'node:child_process'
import { dirname, join } from 'node:path'
import { pathToFileURL } from 'node:url'
import './child-process-sandbox-shim.mjs'

// 能力探测：本环境是否允许"以管道方式启动子进程"（esbuild / dep 预打包的前提）。
// 必须在安装 shim 之前探测——shim 会把同步 EPERM 变成异步事件，探测就失效了。
// 顺序上本文件顶部的 import 先执行，因此探测代码放在 import 之后即可。
function canSpawnWithPipes() {
  try {
    execFileSync(process.execPath, ['-e', '0'], { stdio: 'pipe' })
    return true
  } catch {
    return false
  }
}

if (!canSpawnWithPipes()) {
  // 交给 vite.config.js：该环境下关闭依赖预打包（预打包走 esbuild 的常驻子进程）
  process.env.CRONY_NO_DEP_OPT = '1'
}

// 必须先注册 loader，再 import vite 的 CLI（CLI 内部静态 import 'esbuild'）
register('./esbuild-shim-loader.mjs', import.meta.url)

const require = createRequire(import.meta.url)

// vite 的 package.json 在 exports 里是公开的，用它定位 CLI，避免写死版本号
const vitePkgPath = require.resolve('vite/package.json')
const viteCliPath = join(dirname(vitePkgPath), 'bin', 'vite.js')

const args = process.argv.slice(2)
const command = args[0]

// preview 不加载配置文件，也就不需要 configLoader
if (command !== 'preview' && !args.some((a) => a.startsWith('--configLoader'))) {
  args.push('--configLoader=runner')
}

process.argv = [process.argv[0], viteCliPath, ...args]

await import(pathToFileURL(viteCliPath).href)