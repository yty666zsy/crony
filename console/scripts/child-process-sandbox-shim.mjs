/**
 * child_process 沙箱兼容 shim（仅影响"spawn 失败"这一种情况）。
 *
 * 背景
 * ----
 * 本机 DSH 沙箱（workspace-write）不允许子进程以管道方式通信：任何
 * `child_process.spawn/exec/execFile`（默认 stdio: 'pipe'）都会失败。
 * 但更麻烦的是**失败方式**：标准 Node 在 spawn 失败时不会同步抛异常，
 * 而是通过 `process.nextTick` 触发子进程的 'error' 事件
 * （见 node:internal/child_process 的 onErrorNT）。本沙箱则直接同步抛出
 * `Error: spawn EPERM`，于是调用方**根本没有机会**处理这个错误。
 *
 * Vite 里就有一处必然会踩到：
 *   optimizeSafeRealPathSync() → exec('net use', cb)   // 探测 Windows 网络驱动器
 * 这个 exec 的返回值本来是被 `if (error) return` 正常处理的，
 * 但同步抛出会一路冒泡，导致 `pnpm build` 在"解析第一个文件"时就中断，
 * 报 `failed to load config from .../vite.config.js`。
 *
 * 本 shim 的做法
 * --------------
 * 只把"同步抛出"还原成"异步 emit('error')"，即标准 Node 的语义，
 * **不伪造成功、不吞掉错误**：上层仍然会收到同一个 EPERM 错误对象。
 * 在没有沙箱限制的普通机器上，这段代码永远不会被触发（spawn 不抛异常）。
 *
 * 注意：不能改成给子进程 'error' 事件兜底排序/静默忽略——Vite 依赖
 * "拿到 error 就退回到非 native 的 realpathSync" 这一行为。
 */
import cp from 'node:child_process'

const proto = cp.ChildProcess && cp.ChildProcess.prototype

if (proto && typeof proto.spawn === 'function' && !proto.__cronySpawnShimInstalled) {
  const originalSpawn = proto.spawn

  proto.spawn = function patchedSpawn(options) {
    try {
      return originalSpawn.call(this, options)
    } catch (err) {
      // 还原标准 Node 语义：异步派发 'error'，让调用方的回调/监听器能拿到它。
      process.nextTick(() => {
        this.emit('error', err)
      })
      return this
    }
  }

  Object.defineProperty(proto, '__cronySpawnShimInstalled', { value: true })
}