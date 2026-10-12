import { createRouter, createWebHashHistory } from 'vue-router'

/**
 * 路由表。
 *
 * 使用 hash 模式（createWebHashHistory）：产物将来由 crony-admin 挂在 /console 之类的
 * 子路径下，hash 模式不需要服务端配合 rewrite，刷新任意深链接都不会 404。
 * 默认路由 /dashboard。
 *
 * meta 字段：
 *   title    页面标题（顶栏主标题）
 *   subtitle 顶栏副标题（面包屑风格）
 *   group    所属导航分组（顶栏面包屑前缀，取值与 src/config/nav.js 的分组标签一致）
 *   icon     Element Plus 图标名（侧边栏使用，见 src/config/nav.js）
 *
 * 说明：早期版本曾保留 4 个无后端支撑的演示占位路由（/db-connections、/ssh-keys、
 * /remote-desktop、/containers），现按"控制台只承载真实能力"的原则移除。
 */

// /big-screen 使用独立的全屏布局（不带侧边栏/顶栏）
const BARE_LAYOUT = { bare: true }

const routes = [
  { path: '/', redirect: '/dashboard' },

  /* ---------------------------- 概览 ---------------------------- */
  {
    path: '/dashboard',
    name: 'dashboard',
    component: () => import('@/views/DashboardView.vue'),
    meta: { title: '概览', subtitle: '总览 · 任务与 Agent 运行态势', group: '概览', icon: 'Odometer' },
  },
  {
    path: '/big-screen',
    name: 'big-screen',
    component: () => import('@/views/BigScreenView.vue'),
    meta: { title: '值班大屏', subtitle: '大屏态势（阶段 2 完善）', group: '概览', icon: 'Monitor', ...BARE_LAYOUT },
  },

  /* ------------------------- 调度与执行 ------------------------- */
  {
    path: '/jobs',
    name: 'jobs',
    component: () => import('@/views/JobsView.vue'),
    meta: { title: '定时任务', subtitle: 'cron 表达式与重试策略', group: '调度与执行', icon: 'Timer' },
  },
  {
    path: '/runs',
    name: 'runs',
    component: () => import('@/views/RunsView.vue'),
    meta: { title: '执行记录', subtitle: 'job_run 与步骤时间线', group: '调度与执行', icon: 'DataLine' },
  },
  {
    path: '/orchestration',
    name: 'orchestration',
    component: () => import('@/views/OrchestrationView.vue'),
    meta: {
      title: '任务编排',
      subtitle: '多步骤 DAG（依赖 / 汇聚 / 层深）',
      group: '调度与执行',
      icon: 'Share',
    },
  },

  /* ------------------------- 节点与资源 ------------------------- */
  {
    path: '/servers',
    name: 'servers',
    component: () => import('@/views/ServersView.vue'),
    meta: { title: '节点管理', subtitle: '节点心跳与任务分布', group: '节点与资源', icon: 'Monitor' },
  },
  {
    path: '/terminal',
    name: 'terminal',
    component: () => import('@/views/TerminalView.vue'),
    meta: { title: '终端', subtitle: '阶段 2 接入真实 PTY', group: '节点与资源', icon: 'Monitor' },
  },

  /* -------------------------- Agent 智能 ------------------------ */
  {
    path: '/agents',
    name: 'agents',
    component: () => import('@/views/AgentsView.vue'),
    meta: {
      title: 'Agent 会话',
      subtitle: '只读能力边界与轨迹（阶段 2 契约 §1/§3/§4）',
      group: 'Agent 智能',
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
      title: 'Agent 会话详情',
      subtitle: '轨迹回放 · 只渲染后端记录的步骤（阶段 2 契约 §2）',
      group: 'Agent 智能',
      icon: 'MagicStick',
    },
  },
  {
    path: '/knowledge',
    name: 'knowledge',
    component: () => import('@/views/KnowledgeView.vue'),
    meta: { title: '知识库', subtitle: 'Runbook / SOP / 复盘（Mock）', group: 'Agent 智能', icon: 'Notebook' },
  },
  {
    path: '/memory',
    name: 'memory',
    component: () => import('@/views/MemoryView.vue'),
    meta: {
      title: '经验库',
      subtitle: 'Agent 经验库 · 症状/动作/结果沉淀，只作线索不作证据；坏经验只能停用',
      group: 'Agent 智能',
      icon: 'Collection',
    },
  },

  /* ------------------------- 治理与运维 ------------------------- */
  {
    path: '/approvals',
    name: 'approvals',
    component: () => import('@/views/ApprovalsView.vue'),
    meta: {
      title: '审批中心',
      subtitle: '写动作审批台 · 一次批准至多执行一次',
      group: '治理与运维',
      icon: 'CircleCheck',
    },
  },
  {
    path: '/audit',
    name: 'audit',
    component: () => import('@/views/AuditView.vue'),
    meta: { title: '审计轨迹', subtitle: '谁在何时对什么做了什么（Mock）', group: '治理与运维', icon: 'Files' },
  },
  {
    path: '/selfheal',
    name: 'selfheal',
    component: () => import('@/views/SelfHealView.vue'),
    meta: {
      title: '自愈调度',
      subtitle: '失败自动处置（白名单动作 + 三道闸门），只读与手动催一轮',
      group: '治理与运维',
      icon: 'FirstAidKit',
    },
  },
  {
    path: '/ops',
    name: 'ops',
    component: () => import('@/views/OpsView.vue'),
    meta: {
      title: '智能运维',
      subtitle: '多目标调度 / SLO 契约 / 指标看板 / 自治信用 / 混沌考核 / 影子对账',
      group: '治理与运维',
      icon: 'DataAnalysis',
    },
  },

  /* ---------------------------- 系统 ---------------------------- */
  {
    path: '/settings',
    name: 'settings',
    component: () => import('@/views/SettingsView.vue'),
    meta: { title: '系统设置', subtitle: '鉴权 / 调度默认值 / 通知 / Agent', group: '系统', icon: 'Setting' },
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
  document.title = title ? `${title} · crony 控制台` : 'crony 控制台'
})

export { routes }
export default router
