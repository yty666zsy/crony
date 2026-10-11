/**
 * 侧边栏导航配置（信息架构 Baseline：ITOps Agent 演示站 + 本工程新增项）。
 *
 * 分组：
 *   Home           Dashboard / Big Screen
 *   Automation     新增：定时任务 / 执行记录（crony 的核心能力，演示站没有）
 *   Infrastructure Server Management / DB Connections / SSH Keys / Terminal /
 *                  Remote Desktop / Containers & Virtualization
 *   AI Agent       新增：Agent Console / Knowledge Base / Audit Trail
 *   System         Settings
 */
export const navGroups = [
  {
    key: 'home',
    label: 'Home',
    icon: 'HomeFilled',
    items: [
      { label: 'Dashboard', to: '/dashboard', icon: 'Odometer' },
      { label: 'Big Screen', to: '/big-screen', icon: 'Monitor' },
    ],
  },
  {
    key: 'automation',
    label: 'Automation',
    icon: 'Timer',
    items: [
      { label: 'Scheduled Jobs', to: '/jobs', icon: 'Timer' },
      { label: 'Run History', to: '/runs', icon: 'DataLine' },
    ],
  },
  {
    key: 'infrastructure',
    label: 'Infrastructure',
    icon: 'Monitor',
    items: [
      { label: 'Server Management', to: '/servers', icon: 'Monitor' },
      { label: 'DB Connections', to: '/db-connections', icon: 'Coin' },
      { label: 'SSH Keys', to: '/ssh-keys', icon: 'Key' },
      { label: 'Terminal', to: '/terminal', icon: 'Monitor' },
      { label: 'Remote Desktop', to: '/remote-desktop', icon: 'FullScreen' },
      { label: 'Containers & Virtualization', to: '/containers', icon: 'Box' },
    ],
  },
  {
    key: 'ai',
    label: 'AI Agent',
    icon: 'MagicStick',
    items: [
      { label: 'Agent Console', to: '/agents', icon: 'MagicStick' },
      { label: 'Knowledge Base', to: '/knowledge', icon: 'Notebook' },
      { label: 'Audit Trail', to: '/audit', icon: 'Files' },
      { label: 'Approvals', to: '/approvals', icon: 'CircleCheck' },
      { label: 'Memory', to: '/memory', icon: 'Collection' },
    ],
  },
  {
    key: 'system',
    label: 'System',
    icon: 'Setting',
    items: [{ label: 'Settings', to: '/settings', icon: 'Setting' }],
  },
]

export default navGroups