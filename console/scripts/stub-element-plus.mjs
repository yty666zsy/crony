/**
 * element-plus 的极简替身，仅供 scripts/verify-live.mjs 在 Node 里跑控制台 API 层使用。
 *
 * 为什么需要它：client.js 只用到 ElMessage（业务错误提示），而完整加载 element-plus
 * 会牵进 Vue 组件树与样式副作用，在无 DOM 的 Node 里既慢又不必要。
 * 这里保持**同名同签名**，让 client.js / agent.js 的原代码一行不改地跑起来。
 * 真正的 UI 提示行为不在本脚本的验证范围内（已如实标注）。
 */
function noop() {}
const handler = { success: noop, error: noop, warning: noop, info: noop, closeAll: noop }
export const ElMessage = Object.assign(noop, handler)
export const ElMessageBox = { alert: async () => {}, confirm: async () => true, prompt: async () => '' }
export const ElNotification = Object.assign(noop, handler)
export default { ElMessage, ElMessageBox, ElNotification }