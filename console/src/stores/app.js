import { defineStore } from 'pinia'

const THEME_KEY = 'crony_theme'

/**
 * 应用外壳状态：侧边栏折叠、视口断点、主题。
 *
 * 折叠逻辑刻意分成两个来源，避免"用户手动展开却被断点反复折叠"的抖动：
 *   - userCollapsed：用户点击折叠按钮 / Collapse All 的结果；
 *   - isNarrow：视口 <=1024px（硬性要求：此断点下侧边栏必须收成图标）。
 * 生效值 effectiveCollapsed = userCollapsed || isNarrow。
 */
export const useAppStore = defineStore('app', {
  state: () => ({
    userCollapsed: false,
    isNarrow: false,
    isMobile: false,
    /** 分组展开状态：key=分组 key */
    groupOpen: {
      overview: true,
      scheduling: true,
      infra: true,
      agent: true,
      governance: true,
      system: true,
    },
    theme: 'dark',
    chatOpen: false,
  }),

  getters: {
    collapsed: (state) => state.userCollapsed || state.isNarrow,
    sidebarWidth: (state) =>
      state.userCollapsed || state.isNarrow ? 'var(--sidebar-w-collapsed)' : 'var(--sidebar-w)',
  },

  actions: {
    syncViewport() {
      const w = window.innerWidth
      this.isNarrow = w <= 1024
      this.isMobile = w <= 768
    },

    toggleSidebar() {
      if (this.isNarrow) {
        // 窄屏下点击折叠按钮视为"临时展开"（覆盖断点折叠）
        this.userCollapsed = !this.userCollapsed
        return
      }
      this.userCollapsed = !this.userCollapsed
    },

    /** "Collapse All"：整体收起为图标栏，并关闭所有分组 */
    collapseAll() {
      this.userCollapsed = true
      Object.keys(this.groupOpen).forEach((k) => {
        this.groupOpen[k] = false
      })
    },

    expandAll() {
      this.userCollapsed = false
      Object.keys(this.groupOpen).forEach((k) => {
        this.groupOpen[k] = true
      })
    },

    toggleGroup(key) {
      this.groupOpen[key] = !this.groupOpen[key]
    },

    initTheme() {
      let saved = 'dark'
      try {
        saved = localStorage.getItem(THEME_KEY) || 'dark'
      } catch {
        saved = 'dark'
      }
      this.applyTheme(saved)
    },

    toggleTheme() {
      this.applyTheme(this.theme === 'dark' ? 'light' : 'dark')
    },

    applyTheme(theme) {
      this.theme = theme
      const root = document.documentElement
      root.classList.toggle('dark', theme === 'dark')
      try {
        localStorage.setItem(THEME_KEY, theme)
      } catch {
        /* 忽略隐私模式下的写入失败 */
      }
    },

    toggleChat() {
      this.chatOpen = !this.chatOpen
    },
  },
})