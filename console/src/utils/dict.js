/**
 * UI 字典：把后端枚举值翻译成中文文案与配色。
 *
 * 枚举定义来源（逐项对应，不要凭印象改）：
 *   - RunStatus / TriggerSource → crony/common/models/run.go（阶段 1 已落库）
 *   - JobType / JobStatus / 分配方式 → crony/common/models/job.go
 *   - Node status → crony/common/models/node.go（NodeConnSuccess=1 / NodeConnFail=2）
 *   - 业务错误码 → crony/admin/internal/model/resp/rsp.go
 *
 * 本文件是唯一来源，api/mock/data.js 从这里复用，避免两处维护。
 */

/** 执行状态：0 pending / 1 running / 2 success / 3 failed / 4 canceled / 5 skipped / 6 timeout */
export const RUN_STATUS = {
  0: { key: 'pending', text: '排队中', color: '#7c8aa8', tag: 'info' },
  1: { key: 'running', text: '执行中', color: '#6366f1', tag: 'primary' },
  2: { key: 'success', text: '成功', color: '#22c55e', tag: 'success' },
  3: { key: 'failed', text: '失败', color: '#ef4444', tag: 'danger' },
  4: { key: 'canceled', text: '已取消', color: '#7c8aa8', tag: 'info' },
  5: { key: 'skipped', text: '已跳过', color: '#f59e0b', tag: 'warning' },
  6: { key: 'timeout', text: '超时', color: '#f59e0b', tag: 'warning' },
}

/** 触发来源：1 scheduler / 2 manual / 3 retry / 4 api / 5 agent / 6 self_heal */
export const TRIGGER_SOURCE = {
  1: { key: 'scheduler', text: '定时调度' },
  2: { key: 'manual', text: '手动触发' },
  3: { key: 'retry', text: '失败重试' },
  4: { key: 'api', text: 'API/WebHook' },
  5: { key: 'agent', text: 'AI Agent' },
  6: { key: 'self_heal', text: '自愈动作' },
}

/** 任务类型：1 cmd / 2 http */
export const JOB_TYPE = {
  1: { key: 'cmd', text: 'Shell 命令' },
  2: { key: 'http', text: 'HTTP 请求' },
}

/** 任务分配状态：0 未分配 / 1 已分配 */
export const JOB_STATUS = {
  0: { key: 'not_assigned', text: '未分配', tag: 'warning' },
  1: { key: 'assigned', text: '已分配', tag: 'success' },
}

/** 节点连接状态：1 正常 / 2 失联 */
export const NODE_STATUS = {
  1: { key: 'online', text: '在线', color: '#22c55e', tag: 'success' },
  2: { key: 'offline', text: '失联', color: '#ef4444', tag: 'danger' },
}

/** 并发策略（规划中，阶段 1 任务模型 2.0）：skip / queue / forbid */
export const CONCURRENCY_POLICY = {
  skip: { key: 'skip', text: 'Skip（跳过本轮）', tag: 'warning' },
  queue: { key: 'queue', text: 'Queue（排队等待）', tag: 'primary' },
  forbid: { key: 'forbid', text: 'Forbid（禁止并发）', tag: 'danger' },
}

/** Agent 状态（规划中，阶段 2） */
export const AGENT_STATUS = {
  online: { key: 'online', text: '在线', color: '#22c55e', tag: 'success' },
  busy: { key: 'busy', text: '工作中', color: '#6366f1', tag: 'primary' },
  idle: { key: 'idle', text: '空闲', color: '#7c8aa8', tag: 'info' },
  offline: { key: 'offline', text: '离线', color: '#ef4444', tag: 'danger' },
}

/** 告警级别 */
export const ALERT_LEVEL = {
  critical: { text: '严重', color: '#ef4444', tag: 'danger' },
  warning: { text: '警告', color: '#f59e0b', tag: 'warning' },
  info: { text: '提示', color: '#38bdf8', tag: 'info' },
}

/** 审计结果 */
export const AUDIT_RESULT = {
  success: { text: '成功', tag: 'success', color: '#22c55e' },
  failed: { text: '失败', tag: 'danger', color: '#ef4444' },
  skipped: { text: '已跳过', tag: 'warning', color: '#f59e0b' },
  forbidden: { text: '被拒绝', tag: 'danger', color: '#ef4444' },
}

/** 组件健康度（阶段 1 规划接口 /statis/health） */
export const HEALTH_STATUS = {
  ok: { text: '正常', tag: 'success', color: '#22c55e' },
  degraded: { text: '降级', tag: 'warning', color: '#f59e0b' },
  down: { text: '不可用', tag: 'danger', color: '#ef4444' },
  planned: { text: '规划中', tag: 'info', color: '#7c8aa8' },
}

/** 业务错误码（resp/rsp.go），用于把后端 code 翻译成可读提示 */
export const BIZ_CODE = {
  200: '成功',
  1000: '操作失败',
  1001: '请求参数错误',
  1002: '任务格式错误',
  1003: '令牌生成失败',
  1004: '用户名已存在',
  1005: '权限不足（需要管理员）',
  1006: '注册已关闭',
}

export const label = (dict, value, fallback = '—') => dict?.[value]?.text ?? fallback
export const color = (dict, value, fallback = '#7c8aa8') => dict?.[value]?.color ?? fallback
export const tagType = (dict, value, fallback = 'info') => dict?.[value]?.tag ?? fallback