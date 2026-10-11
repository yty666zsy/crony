import { createRouter, createWebHashHistory } from 'vue-router'

/**
 * 路由表。
 *
 * 使用 hash 模式（createWebHashHistory）：产物将来由 crony-admin 挂在 /console 之类的
 * 子路径下，hash 模式不需要服务端配合 rewrite，刷新任意深链接都不会 404。
 * 默认路由 /dashboard。
 *
 * meta 字段：
 *   title    页面标题（侧边栏 + 顶栏主标题）
 *   subtitle 顶栏副标题（面包屑风格）
 *   group    所属导航分组（用于顶栏面包屑前缀）
 *   icon     Element Plus 图标名（侧边栏使用，见 src/config/nav.js）
 */

// /big-screen 使用独立的全屏布局（不带侧边栏/顶栏）
const BARE_LAYOUT = { bare: true }

const routes = [
  { path: '/', redirect: '/dashboard' },

  {
    path: '/dashboard',
    name: 'dashboard',
    component: () => import('@/views/DashboardView.vue'),
    meta: { title: 'Dashboard', subtitle: '总览 · 任务与 Agent 运行态势', group: 'Home', icon: 'Odometer' },
  },
  {
    path: '/big-screen',
    name: 'big-screen',
    component: () => import('@/views/BigScreenView.vue'),
    meta: { title: 'Big Screen', subtitle: '大屏态势（阶段 2 完善）', group: 'Home', icon: 'Monitor', ...BARE_LAYOUT },
  },

  /* ------------------------- Automation ------------------------- */
  {
    path: '/jobs',
    name: 'jobs',
    component: () => import('@/views/JobsView.vue'),
    meta: { title: 'Scheduled Jobs', subtitle: '定时任务 · cron 表达式与重试策略', group: 'Automation', icon: 'Timer' },
  },
  {
    path: '/runs',
    name: 'runs',
    component: () => import('@/views/RunsView.vue'),
    meta: { title: 'Run History', subtitle: '执行记录 · job_run 与步骤时间线', group: 'Automation', icon: 'DataLine' },
  },

  /* ------------------------ Infrastructure ---------------------- */
  {
    path: '/servers',
    name: 'servers',
    component: () => import('@/views/ServersView.vue'),
    meta: { title: 'Server Management', subtitle: '服务器管理 · 节点心跳与任务分布', group: 'Infrastructure', icon: 'Monitor' },
  },
  {
    path: '/db-connections',
    name: 'db-connections',
    component: () => import('@/views/PlaceholderView.vue'),
    meta: {
      title: 'DB Connections',
      subtitle: '数据库连接（阶段 2 交付）',
      group: 'Infrastructure',
      icon: 'Coin',
      planned: '阶段 2',
      desc:
        '统一登记数据库连接（MySQL / PostgreSQL / Redis），供巡检类任务与 Agent 的只读工具复用。' +
        '当前后端没有对应模型与接口，本页为信息架构占位。',
      bullets: [
        '连接信息加密存储，口令不落前端（仅存引用 ID）',
        '支持连通性测试与权限最小化校验',
        '被 MySQL 慢查询巡检等任务引用，避免口令散落在 cron 命令里',
      ],
    },
  },
  {
    path: '/ssh-keys',
    name: 'ssh-keys',
    component: () => import('@/views/PlaceholderView.vue'),
    meta: {
      title: 'SSH Keys',
      subtitle: '密钥管理（阶段 2 交付）',
      group: 'Infrastructure',
      icon: 'Key',
      planned: '阶段 2',
      desc:
        '集中托管节点登录所用密钥与口令凭据，替代当前"节点上直接放私钥"的做法。' +
        '当前后端没有对应模型与接口，本页为信息架构占位。',
      bullets: [
        '私钥仅在后端加密存储，前端只能提交不能回读',
        '按节点/分组授权，配合阶段 0 的 RBAC 做最小权限',
        '支持密钥轮换提醒（与"证书到期提醒"任务同源）',
      ],
    },
  },
  {
    path: '/terminal',
    name: 'terminal',
    component: () => import('@/views/TerminalView.vue'),
    meta: { title: 'Terminal', subtitle: '终端 · 阶段 2 接入真实 PTY', group: 'Infrastructure', icon: 'Monitor' },
  },
  {
    path: '/remote-desktop',
    name: 'remote-desktop',
    component: () => import('@/views/PlaceholderView.vue'),
    meta: {
      title: 'Remote Desktop',
      subtitle: '远程桌面（阶段 3 交付）',
      group: 'Infrastructure',
      icon: 'FullScreen',
      planned: '阶段 3',
      desc:
        '基于 VNC/RDP 网关的图形化远程桌面，用于无法用命令行覆盖的运维场景。' +
        '当前后端没有对应能力，本页为信息架构占位。',
      bullets: [
        '会话录制与操作回放进入审计链路',
        '按节点授权，禁止共享账号',
        '与 Agent 的"人工接管"模式共用入口',
      ],
    },
  },
  {
    path: '/containers',
    name: 'containers',
    component: () => import('@/views/PlaceholderView.vue'),
    meta: {
      title: 'Containers & Virtualization',
      subtitle: '容器与虚拟化（阶段 3 交付）',
      group: 'Infrastructure',
      icon: 'Box',
      planned: '阶段 3',
      desc:
        '容器（Docker）与虚拟机（KVM/vSphere）的统一视图。当前 crony 只在节点上通过 shell 任务操作容器' +
        '（见"容器健康检查"任务），没有容器 API，本页为信息架构占位。',
      bullets: [
        '容器列表/日志/资源水位，复用节点上的 docker 能力',
        '虚拟机的生命周期动作纳入审批（阶段 3）',
        '与"容器镜像垃圾回收"任务联动展示清理结果',
      ],
    },
  },

  /* ---------------------------- AI Agent ------------------------ */
  {
    path: '/agents',
    name: 'agents',
    component: () => import('@/views/AgentsView.vue'),
    meta: {
      title: 'Agent Console',
      subtitle: 'Agent 会话 · 只读能力边界与轨迹（阶段 2 契约 §1/§3/§4）',
      group: 'AI Agent',
      icon: 'MagicStick',
    },
  },
  {
    // 阶段 2 新增：会话轨迹回放（契约 §2）。
    // 命名参数用 session_id（不是 id），与契约字段一致，避免复制链接时对不上号。
    path: '/agents/:session_id',
    name: 'agent-session',
    component: () => import('@/views/AgentSessionView.vue'),
    meta: {
      title: 'Agent Session',
      subtitle: '轨迹回放 · 只渲染后端记录的步骤（阶段 2 契约 §2）',
      group: 'AI Agent',
      icon: 'MagicStick',
    },
  },
  {
    path: '/knowledge',
    name: 'knowledge',
    component: () => import('@/views/KnowledgeView.vue'),
    meta: { title: 'Knowledge Base', subtitle: '知识库 · Runbook / SOP / 复盘（Mock）', group: 'AI Agent', icon: 'Notebook' },
  },
  {
    path: '/audit',
    name: 'audit',
    component: () => import('@/views/AuditView.vue'),
    meta: { title: 'Audit Trail', subtitle: '审计日志 · 谁在何时对什么做了什么（Mock）', group: 'AI Agent', icon: 'Files' },
  },
  {
    path: '/approvals',
    name: 'approvals',
    component: () => import('@/views/ApprovalsView.vue'),
    meta: {
      title: 'Approvals',
      subtitle: '写动作审批台 · 一次批准至多执行一次',
      group: 'AI Agent',
      icon: 'CircleCheck',
    },
  },

  /* ----------------------------- System ------------------------- */
  {
    path: '/settings',
    name: 'settings',
    component: () => import('@/views/SettingsView.vue'),
    meta: { title: 'Settings', subtitle: '设置 · 鉴权 / 调度默认值 / 通知 / Agent', group: 'System', icon: 'Setting' },
  },

  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { title: '404', subtitle: '页面不存在' },
  },
]

const router = createRouter({
  history: createWebHashHistory(),
  routes,
  scrollBehavior: () => ({ top: 0 }),
})

router.afterEach((to) => {
  const title = to.meta?.title
  document.title = title ? `${title} · ITOps Agent 控制台` : 'ITOps Agent 控制台'
})

export { routes }
export default router