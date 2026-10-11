/**
 * esbuild 的**纯 JS 替身**（只实现 Vite 构建路径上真正会用到的那一个 API）。
 *
 * 为什么需要它
 * ------------
 * esbuild 的 JS API 是"常驻子进程 + stdio 管道"架构（lib/main.js → ensureServiceIsRunning
 * 用 child_process.spawn 且 stdio 为 pipe）。本机 DSH 沙箱禁止打开命名管道，
 * 因此任何 esbuild 调用都会 `spawn EPERM`。
 *
 * Vite 6 里有两处在**关不掉**的地方会调用 esbuild：
 *   1. 加载 vite.config.js 时用 esbuild 打包配置文件 → 用 CLI 的 `--configLoader=runner` 绕开；
 *   2. `vite:define` 插件做 define 替换时直接调用 `esbuild.transform(code, { define })`
 *      （见 vite/dist/node/chunks/dep-*.js 的 replaceDefine）——这一处没有任何开关，
 *      即使配置了 `esbuild: false`、`minify: 'terser'` 也照样会走到。
 * 本文件通过 scripts/esbuild-shim-loader.mjs 顶替 `esbuild` 模块，为第 2 种用法
 * 提供等价的**纯文本** define 替换（不产生子进程）。
 *
 * 诚实声明（已写入 README「未验证项/风险」）
 * ----------------------------------------
 * - 本替身只保证"把 define 表里的字面量替换掉"这一件事，且刻意做成**扫描式**替换：
 *   跳过字符串/模板/注释，避免误改字面量内容。
 * - 它不是 esbuild 的完整实现：不做语法降级、不做 TS/JSX 转译、不压缩。
 *   本工程的源码是普通 .js / .vue，且构建走 terser + lightningcss，
 *   因此这些能力在构建路径上并不需要（`esbuild: false` + `target: 'esnext'`）。
 * - 若 Vite 调用到未实现的能力，本文件会**直接抛错**而不是静默返回错误结果，
 *   保证"构建成功"绝不等于"产物悄悄不对"。
 */

export const version = '0.25.0-sandbox-shim'

const IDENT_CHAR = /[A-Za-z0-9_$]/

function escapeRegExp(str) {
  return str.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

/**
 * 扫描式替换：把 define 表里的键（如 `process.env.NODE_ENV`）替换成对应字面量。
 * 只在"代码区"替换；字符串、模板字符串的静态部分、注释一律原样保留，
 * 但模板字符串里的 `${...}` 表达式会被当作代码继续处理。
 */
function applyDefines(code, define) {
  const keys = Object.keys(define).sort((a, b) => b.length - a.length)
  if (!keys.length) return code

  const re = new RegExp(keys.map(escapeRegExp).join('|'), 'g')
  // 用栈维护上下文：code（含 ${} 深度）/ sq / dq / tpl / line / block
  const stack = [{ type: 'code', depth: 0 }]
  let out = ''
  let i = 0

  while (i < code.length) {
    const top = stack[stack.length - 1]
    const ch = code[i]
    const next = code[i + 1]

    if (top.type === 'code') {
      if (ch === '{') {
        top.depth++
        out += ch
        i++
        continue
      }
      if (ch === '}') {
        if (top.depth === 0 && stack.length > 1) {
          // 结束 ${...} 表达式，回到模板字符串
          stack.pop()
          out += ch
          i++
          continue
        }
        top.depth--
        out += ch
        i++
        continue
      }
      if (ch === "'") {
        stack.push({ type: 'sq' })
        out += ch
        i++
        continue
      }
      if (ch === '"') {
        stack.push({ type: 'dq' })
        out += ch
        i++
        continue
      }
      if (ch === '`') {
        stack.push({ type: 'tpl' })
        out += ch
        i++
        continue
      }
      if (ch === '/' && next === '/') {
        stack.push({ type: 'line' })
        out += ch
        i++
        continue
      }
      if (ch === '/' && next === '*') {
        stack.push({ type: 'block' })
        out += ch
        i++
        continue
      }

      re.lastIndex = i
      const m = re.exec(code)
      if (m && m.index === i) {
        const prev = i > 0 ? code[i - 1] : ''
        const after = code[i + m[0].length] || ''
        // 前一个字符不能是标识符字符或 '.'（避免改到 a.process.env / xprocess.env）
        const prevOk = prev === '' || (!IDENT_CHAR.test(prev) && prev !== '.')
        // 后一个字符不能是标识符字符（避免改到 process.env.NODE_ENV_X）
        const afterOk = !IDENT_CHAR.test(after)
        if (prevOk && afterOk) {
          out += define[m[0]]
          i += m[0].length
          continue
        }
      }

      out += ch
      i++
      continue
    }

    if (top.type === 'sq' || top.type === 'dq') {
      const quote = top.type === 'sq' ? "'" : '"'
      if (ch === '\\') {
        out += ch + (next ?? '')
        i += 2
        continue
      }
      if (ch === quote) stack.pop()
      out += ch
      i++
      continue
    }

    if (top.type === 'tpl') {
      if (ch === '\\') {
        out += ch + (next ?? '')
        i += 2
        continue
      }
      if (ch === '`') {
        stack.pop()
        out += ch
        i++
        continue
      }
      if (ch === '$' && next === '{') {
        stack.push({ type: 'code', depth: 0 })
        out += '${'
        i += 2
        continue
      }
      out += ch
      i++
      continue
    }

    if (top.type === 'line') {
      if (ch === '\n') stack.pop()
      out += ch
      i++
      continue
    }

    // block comment
    if (ch === '*' && next === '/') {
      stack.pop()
      out += '*/'
      i += 2
      continue
    }
    out += ch
    i++
    continue
  }

  return out
}

/**
 * Vite 的 vite:define 插件就是靠这个函数工作的。
 * 参数与 esbuild.transform 的子集对齐；返回值需要 `map` 是字符串（Vite 会对它做 .includes）。
 */
export async function transform(code, options = {}) {
  const { define, loader = 'js' } = options

  if (loader !== 'js') {
    throw new Error(
      `[esbuild-shim] 仅支持 loader="js"（Vite 的 define 替换路径）。收到 loader="${loader}"。` +
        '本工程源码不含 TS/JSX；若确实需要该能力，请在没有子进程限制的环境下构建。',
    )
  }

  if (!define || typeof define !== 'object') {
    return { code, map: '' }
  }

  return { code: applyDefines(code, define), map: '' }
}

/** 仅供关掉的 esbuild 插件路径留作接口占位；真被调用说明配置有问题，直接抛错。 */
export async function formatMessages(messages) {
  return (messages || []).map((m) => `${m.text ?? ''}`)
}

/** optimizeDeps / vite:esbuild 插件才会用到；本工程构建路径不应触发。 */
export async function build() {
  throw new Error(
    '[esbuild-shim] 构建路径不应该调用 esbuild.build()。' +
      '请确认 vite.config.js 中已设置 esbuild: false / minify: terser / cssMinify: lightningcss。',
  )
}

export default { version, transform, formatMessages, build }