/**
 * 侧边栏导航配置（crony 控制台信息架构）。
 *
 * 分组与 crony 的真实工作流对齐（概览 → 调度 → 节点 → 智能 → 治理），
 * 不再保留早期演示站里没有后端支撑的基础设施占位项
 * （数据库连接 / SSH 密钥 / 远程桌面 / 容器与虚拟化，对应路由已一并移除）。
 *
 *   概览       概览 / 值班大屏
 *   调度与执行  定时任务 / 执行记录 / 任务编排     —— crony 的核心能力
 *   节点与资源  节点管理 / 终端
 *   Agent 智能  Agent 会话 / 知识库 / 经验库 / 评测台
 *   治理与运维  审批中心 / 审计轨迹 / 自愈调度 / 智能运维
 *   系统       系统设置
 */
export const navGroups = [
  {
    key: 'overview',
    label: '概览',
    icon: 'HomeFilled',
    items: [
      { label: '概览', to: '/dashboard', icon: 'Odometer' },
      { label: '值班大屏', to: '/big-screen', icon: 'Monitor' },
    ],
  },
  {
    key: 'scheduling',
    label: '调度与执行',
    icon: 'Timer',
    items: [
      { label: '定时任务', to: '/jobs', icon: 'Timer' },
      { label: '执行记录', to: '/runs', icon: 'DataLine' },
      { label: '任务编排', to: '/orchestration', icon: 'Share' },
    ],
  },
  {
    key: 'infra',
    label: '节点与资源',
    icon: 'Monitor',
    items: [
      { label: '节点管理', to: '/servers', icon: 'Monitor' },
      { label: '终端', to: '/terminal', icon: 'Monitor' },
    ],
  },
  {
    key: 'agent',
    label: 'Agent 智能',
    icon: 'MagicStick',
    items: [
      { label: 'Agent 会话', to: '/agents', icon: 'MagicStick' },
      { label: '知识库', to: '/knowledge', icon: 'Notebook' },
      { label: '经验库', to: '/memory', icon: 'Collection' },
      { label: '评测台', to: '/eval', icon: 'TrendCharts' },
    ],
  },
  {
    key: 'governance',
    label: '治理与运维',
    icon: 'CircleCheck',
    items: [
      { label: '审批中心', to: '/approvals', icon: 'CircleCheck' },
      { label: '审计轨迹', to: '/audit', icon: 'Files' },
      { label: '自愈调度', to: '/selfheal', icon: 'FirstAidKit' },
      { label: '智能运维', to: '/ops', icon: 'DataAnalysis' },
    ],
  },
  {
    key: 'system',
    label: '系统',
    icon: 'Setting',
    items: [{ label: '系统设置', to: '/settings', icon: 'Setting' }],
  },
]

export default navGroups
