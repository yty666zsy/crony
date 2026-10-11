import { defineStore } from 'pinia'
import { getToken, setToken } from '@/api/client'
import { login as loginApi, fetchCurrentUser } from '@/api/auth'

/**
 * 会话状态。
 *
 * 阶段 1：控制台默认以 Mock 用户（admin）进入，不强制登录页——
 * 目标是"没有后端也能独立运行"。真实登录链路（POST /login → JWT → 后续请求带
 * Authorization 头）已经打通到 api/client.js 的请求拦截器，阶段 1 接入登录页即可启用。
 */
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
    async loadProfile() {
      this.loading = true
      try {
        this.user = await fetchCurrentUser(1)
      } catch {
        // 后端不可达时退化为本地占位用户，保证控制台外壳可用
        this.user = { username: 'admin', nickname: '运维管理员', is_admin: true }
      } finally {
        this.loading = false
      }
    },

    async login(payload) {
      const res = await loginApi(payload)
      if (res?.token) {
        this.token = res.token
        setToken(res.token)
      }
      await this.loadProfile()
      return res
    },

    logout() {
      this.token = ''
      setToken('')
      this.user = null
    },
  },
})