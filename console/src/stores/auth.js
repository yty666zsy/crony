import { defineStore } from 'pinia'
import { getToken, setToken, USE_MOCK } from '@/api/client'
import { login as loginApi, fetchCurrentUser } from '@/api/auth'

/**
 * 会话状态。
 *
 * Mock 模式（默认）下以固定用户（admin）进入，不强制登录页——目标是"没有后端也能独立运行"。
 * 真实模式（VITE_USE_MOCK=false）下由 LoginView 完成 `POST /login → JWT`，
 * 请求拦截器自动带 `Authorization` 头；令牌失效（业务码 1000）时由外壳切回登录页。
 */

/**
 * 从 JWT 载荷里取用户 ID。
 *
 * 后端没有 `GET /me`；"带着令牌刷新页面"时只能从令牌里取（载荷含 `ID`）。
 * 早期实现写死 `id=1`，而真实管理员往往不是 1 号用户，于是
 * `GET /user/find?id=1` 回 `[find_user] db error`，**每个页面加载都弹一次**。
 */
function userIdFromToken(token) {
  try {
    const payload = String(token).split('.')[1]
    if (!payload) return null
    const base64 = payload.replace(/-/g, '+').replace(/_/g, '/')
    const json = decodeURIComponent(
      atob(base64)
        .split('')
        .map((c) => '%' + ('00' + c.charCodeAt(0).toString(16)).slice(-2))
        .join(''),
    )
    const data = JSON.parse(json)
    const id = data.ID ?? data.id ?? data.user_id
    return id == null ? null : Number(id)
  } catch {
    return null
  }
}

/**
 * 归一化用户对象：真实后端只给 `role`（1 普通 / 2 管理员，见 common/models/user.go），
 * 而 UI 各处的判定用的是 `is_admin`；这里统一补齐，避免真实模式下把管理员显示成"运维"。
 */
function normalizeUser(u) {
  if (!u) return null
  const isAdmin = 'is_admin' in u ? Boolean(u.is_admin) : Number(u.role) === 2
  return {
    ...u,
    is_admin: isAdmin,
    nickname: u.nickname || (isAdmin ? '运维管理员' : u.username),
  }
}

const FALLBACK_USER = { username: 'admin', nickname: '运维管理员', is_admin: true }

export const useAuthStore = defineStore('auth', {
  state: () => ({
    token: getToken(),
    user: null,
    loading: false,
  }),

  getters: {
    isLoggedIn: (state) => Boolean(state.token),
    displayName: (state) => state.user?.nickname || state.user?.username || '运维管理员',
    isAdmin: (state) => Boolean(state.user?.is_admin),
  },

  actions: {
    /** 记录登录结果（真实登录响应自带 user；Mock 只有 token）。 */
    setSession(res) {
      if (res?.token) {
        this.token = res.token
        setToken(res.token)
      }
      if (res?.user) this.user = normalizeUser(res.user)
      return res
    },

    async loadProfile() {
      this.loading = true
      try {
        if (USE_MOCK) {
          this.user = normalizeUser(await fetchCurrentUser(1))
          return
        }
        const id = userIdFromToken(this.token)
        if (id == null) {
          // 没有可用令牌：不发请求（否则每个请求都会被回"未登录"刷屏），交给登录页。
          this.user = null
          return
        }
        this.user = normalizeUser(await fetchCurrentUser(id))
      } catch {
        // 后端不可达时退化为本地占位用户，保证控制台外壳可用
        this.user = FALLBACK_USER
      } finally {
        this.loading = false
      }
    },

    async login(payload) {
      const res = await loginApi(payload)
      this.setSession(res)
      // 真实登录响应已带 user；仅当缺失时才回查一次。
      if (!this.user) await this.loadProfile()
      return res
    },

    logout() {
      this.token = ''
      setToken('')
      this.user = null
    },
  },
})
