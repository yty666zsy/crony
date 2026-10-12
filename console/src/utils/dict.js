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
  1: { key: 'running', text: '执行中', color: '#06b6d4', tag: 'primary' },
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

/**
 * 【阶段 1 遗留·仅兼容】多 Agent 编排的"Agent 在线/空闲"枚举。
 * 阶段 2 的 Agent 内核是**单进程内的 ReAct 循环**（契约 §1–§5），后端不产出
 * "agent 实体上线状态"，因此这个字典不再有接口与之对应；保留只为阶段 1 组件不炸。
 * 新页面请使用下面的 AGENT_SESSION_* / AGENT_STEP_KIND 等契约字典。
 */
export const AGENT_STATUS = {
  online: { key: 'online', text: '在线', color: '#22c55e', tag: 'success' },
  busy: { key: 'busy', text: '工作中', color: '#06b6d4', tag: 'primary' },
  idle: { key: 'idle', text: '空闲', color: '#7c8aa8', tag: 'info' },
  offline: { key: 'offline', text: '离线', color: '#ef4444', tag: 'danger' },
}

/* ------------------------------------------------------------------ *
 * 阶段 2 Agent 内核字典（《PHASE2-接口契约.md》§1–§5）
 *
 * 每个取值都对应 Go 侧枚举，不要凭印象增删：
 *   kind/status        → 契约 §1（admin 层会话记录）
 *   stop_reason        → common/pkg/agent/trace.go: StopReason
 *   category/risk      → common/pkg/agent/diagnose.go: DiagnosisSchema 的 enum
 *   step kind          → common/pkg/agent/trace.go: StepKind
 * ------------------------------------------------------------------ */

/** 会话类型：契约 §1（diagnose 诊断 / nljob 自然语言建任务 / job 任务内嵌 Agent） */
export const AGENT_SESSION_KIND = {
  diagnose: { key: 'diagnose', text: '失败诊断', color: '#3b82f6', tag: 'primary' },
  nljob: { key: 'nljob', text: '自然语言建任务', color: '#38bdf8', tag: 'info' },
  job: { key: 'job', text: '任务内嵌 Agent', color: '#f59e0b', tag: 'warning' },
}

/** 会话状态：契约 §1（completed / failed / canceled） */
export const AGENT_SESSION_STATUS = {
  completed: { key: 'completed', text: '已完成', color: '#22c55e', tag: 'success' },
  failed: { key: 'failed', text: '失败', color: '#ef4444', tag: 'danger' },
  canceled: { key: 'canceled', text: '已取消', color: '#7c8aa8', tag: 'info' },
}

/** 停止原因：common/pkg/agent/trace.go（warn 用于"保护性终止"，需要人注意） */
export const AGENT_STOP_REASON = {
  completed: { key: 'completed', text: '正常完成', color: '#22c55e' },
  max_steps: { key: 'max_steps', text: '达到最大步数上限（保护性终止）', color: '#f59e0b' },
  tool_budget: { key: 'tool_budget', text: '达到工具调用次数上限', color: '#f59e0b' },
  llm_budget: { key: 'llm_budget', text: '模型预算耗尽', color: '#f59e0b' },
  canceled: { key: 'canceled', text: '被主动取消', color: '#7c8aa8' },
  schema_invalid: { key: 'schema_invalid', text: '多次尝试后输出仍不符合结构要求', color: '#ef4444' },
  no_provider: { key: 'no_provider', text: '没有可用的模型 provider', color: '#ef4444' },
  error: { key: 'error', text: '执行过程中出现错误', color: '#ef4444' },
}

/** 轨迹步骤类型：common/pkg/agent/trace.go: StepKind(model/tool/notice) */
export const AGENT_STEP_KIND = {
  model: { key: 'model', text: '模型步骤', color: '#06b6d4', icon: 'MagicStick' },
  tool: { key: 'tool', text: '工具步骤', color: '#f59e0b', icon: 'Tools' },
  notice: { key: 'notice', text: '修复提示', color: '#ef4444', icon: 'WarnTriangleFilled' },
}

/** 诊断失败分类：common/pkg/agent/diagnose.go（category 的 enum） */
export const DIAGNOSIS_CATEGORY = {
  config: { key: 'config', text: '配置错误', color: '#f59e0b' },
  dependency: { key: 'dependency', text: '依赖不可用', color: '#3b82f6' },
  environment: { key: 'environment', text: '环境问题', color: '#38bdf8' },
  network: { key: 'network', text: '网络问题', color: '#0ea5e9' },
  resource: { key: 'resource', text: '资源不足', color: '#ef4444' },
  permission: { key: 'permission', text: '权限问题', color: '#f97316' },
  code: { key: 'code', text: '脚本/命令缺陷', color: '#a855f7' },
  schedule: { key: 'schedule', text: '调度问题', color: '#22c55e' },
  unknown: { key: 'unknown', text: '未归类', color: '#7c8aa8' },
}

/** 建议动作风险等级：common/pkg/agent/diagnose.go（risk 的 enum: low/medium/high） */
export const DIAGNOSIS_RISK = {
  low: { key: 'low', text: '低风险', color: '#22c55e', tag: 'success' },
  medium: { key: 'medium', text: '中风险', color: '#f59e0b', tag: 'warning' },
  high: { key: 'high', text: '高风险', color: '#ef4444', tag: 'danger' },
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

/** 业务错误码（resp/rsp.go + 阶段 2 契约 §0），用于把后端 code 翻译成可读提示 */
export const BIZ_CODE = {
  200: '成功',
  1000: '操作失败',
  1001: '请求参数错误',
  1002: '任务格式错误',
  1003: '令牌生成失败',
  1004: '用户名已存在',
  1005: '权限不足（需要管理员）',
  1006: '注册已关闭',
  1007: '本节点未配置模型',
  1008: '会话不存在',
  1009: '草案本地复验不通过',
  1010: '指定的运行记录不存在',
  1011: 'Agent 能力被配置关闭',
  1012: '节点离线/未注册，无法在其上执行',
  1013: '审批单不存在',
  1014: '审批单状态已变更（已被处理）',
  1015: '审批单已过期',
  1016: '参数与审批单不一致（拒绝执行）',
  1017: '执行前复核未通过（工具/策略已变）',
  1018: '已执行但失败',
}

/**
 * 阶段 2 Agent 业务码的"人话 + 该怎么办"。
 *
 * 为什么单列一张表：client.js 的 unwrap 已经把后端 msg 弹出来了，但 msg 是**后端视角**的
 * 措辞（例如 "agent: not configured"）。运维需要的是"我该改什么"。两个都展示，
 * 且把 code 一起显示，方便对着契约 §0 排查。
 */
export const AGENT_BIZ_HINT = {
  1007: {
    title: '本节点未配置模型',
    desc: '后端配置的 llm 段缺失或未启用（provider / model / api_key）。补齐配置并重启 admin 后 Agent 类接口才会工作；在此之前诊断与自然语言建任务都会失败。',
  },
  1008: {
    title: '会话不存在',
    desc: 'session_id 在当前节点查不到（可能来自其它环境或已被清理）。请回到 /agents 的会话列表里重新选择一条。',
  },
  1009: {
    title: '草案本地复验不通过',
    desc: 'cron 表达式、命令/URL、超时或重试取值至少有一项不合法。后端 msg 里会指出具体字段与原因，按其提示修改后重新起草。',
  },
  1010: {
    title: '指定的运行记录不存在',
    desc: 'run_id 在当前节点查不到。请在 /runs 里选择一条真实存在的运行记录后再点诊断。',
  },
  1011: {
    title: 'Agent 能力被配置关闭',
    desc: '配置里 llm.enabled=false。需要显式启用后才会执行模型调用，UI 无法绕过。',
  },
  1012: {
    title: '节点离线/未注册，无法在其上执行',
    desc: '该运行记录所在节点当前不可达，采集不到只读证据。先确认节点心跳恢复，或改在其它节点上重跑任务。',
  },
  1013: {
    title: '审批单不存在',
    desc: 'approval_id 在当前节点查不到（可能来自其它环境或已被清理）。回到 /approvals 列表里重新选择一张单子。',
  },
  1014: {
    title: '审批单状态已变更',
    desc: '这张单子已被别人决策/已执行，或已不是"待审批"。批准动作**不会**重复执行（一次批准一次执行），请刷新列表查看最新状态。',
  },
  1015: {
    title: '审批单已过期',
    desc: '超过有效期的单子不允许再执行：写动作的合理性依赖当时的现场，过期判断可能已经失效。需要重跑请让 Agent 重新发起一次写动作。',
  },
  1016: {
    title: '参数与审批单不一致',
    desc: '批准的是 A、执行的是 B（TOCTOU）。系统已拒绝执行并把这张单子标记为"已执行但失败"。请人工核对这次动作的来源。',
  },
  1017: {
    title: '执行前复核未通过',
    desc: '批准之后工具已下线，或策略被收紧（allow_write=false / 自治级别降级），或执行器未装配。复核不通过一律不执行，请确认当前部署的工具策略。',
  },
  1018: {
    title: '已执行但失败',
    desc: '动作真的被执行了，但执行结果是失败（例如目标节点连接异常、任务不存在）。单子已落"已执行但失败"，详情看执行结果；不要直接重试同一张单子。',
  },
}

/**
 * 把 BizError / 任意错误翻译成 { code, title, desc, msg }。
 * 调用方负责决定以 toast 还是页面内联方式呈现（本函数不弹窗）。
 */
export function bizHint(err) {
  const code = Number(err?.code)
  const msg = err?.message || ''
  const known = AGENT_BIZ_HINT[code]
  if (known) return { code, title: known.title, desc: known.desc, msg }
  return {
    code: Number.isFinite(code) && code > 0 ? code : 0,
    title: BIZ_CODE[code] || '请求失败',
    desc: msg || '后端没有返回可读原因，请查看 admin 日志。',
    msg,
  }
}

export const label = (dict, value, fallback = '—') => dict?.[value]?.text ?? fallback
export const color = (dict, value, fallback = '#7c8aa8') => dict?.[value]?.color ?? fallback
export const tagType = (dict, value, fallback = 'info') => dict?.[value]?.tag ?? fallback