/**
 * Mock 数据源（阶段 1）。
 *
 * 设计原则：
 *  - 与后端字段**逐字段对齐**（crony/common/models/*.go + admin/internal/model/resp/*.go），
 *    这样未来把 VITE_USE_MOCK 切到 false 时，页面组件不需要改字段名；
 *  - "规划中" 的字段（并发策略、步骤、Agent 等）与阶段 1 已落库的
 *    crony/common/models/run.go（job_run / job_run_step）保持一致。
 */

// 状态/触发来源等枚举字典的唯一来源在 src/utils/dict.js（与 Go 枚举逐项对齐）。
// 这里 re-export，方便 mock 处理器与页面共用同一份，避免两处维护。
export { RUN_STATUS, TRIGGER_SOURCE } from '@/utils/dict'

const NOW = Date.now()
const SEC = 1000
const MIN = 60 * SEC
const HOUR = 60 * MIN

/** 相对当前时间生成 Unix 秒，保证界面上的时间永远是"最近"的。 */
function ago(ms) {
  return Math.floor((NOW - ms) / 1000)
}

function isoAgo(ms) {
  return new Date(NOW - ms).toISOString()
}

/* ------------------------------------------------------------------ *
 * 1. 节点（crony/common/models/node.go → models.Node + resp.RspNodeSearch）
 * ------------------------------------------------------------------ */

export const nodes = [
  {
    id: 1,
    pid: '18421',
    ip: '10.20.31.11',
    hostname: 'node-prod-01',
    uuid: 'n-8f3d1c2a-01',
    version: 'v1.4.2',
    status: 1, // NodeConnSuccess
    up: ago(6 * HOUR),
    down: 0,
    job_count: 5,
    // 规划中（阶段 1）：节点标签与资源水位，后端 /node/search 尚未返回
    tags: ['生产', '华东-1', '通用计算'],
    cpu: 42,
    mem: 61,
    disk: 73,
    agent_attached: true,
  },
  {
    id: 2,
    pid: '18455',
    ip: '10.20.31.12',
    hostname: 'node-prod-02',
    uuid: 'n-8f3d1c2a-02',
    version: 'v1.4.2',
    status: 1,
    up: ago(6 * HOUR + 12 * MIN),
    down: 0,
    job_count: 4,
    tags: ['生产', '华东-1', '数据库'],
    cpu: 67,
    mem: 78,
    disk: 54,
    agent_attached: true,
  },
  {
    id: 3,
    pid: '18502',
    ip: '10.20.31.13',
    hostname: 'node-prod-03',
    uuid: 'n-8f3d1c2a-03',
    version: 'v1.4.1',
    status: 1,
    up: ago(2 * HOUR + 40 * MIN),
    down: 0,
    job_count: 3,
    tags: ['生产', '华东-1', '中间件'],
    cpu: 28,
    mem: 45,
    disk: 39,
    agent_attached: true,
  },
  {
    id: 4,
    pid: '2210',
    ip: '10.20.42.21',
    hostname: 'node-stg-01',
    uuid: 'n-1a77be40-01',
    version: 'v1.4.2',
    status: 1,
    up: ago(31 * HOUR),
    down: 0,
    job_count: 2,
    tags: ['预发', '华东-1'],
    cpu: 12,
    mem: 33,
    disk: 22,
    agent_attached: false,
  },
  {
    id: 5,
    pid: '3301',
    ip: '10.30.7.8',
    hostname: 'node-edge-01',
    uuid: 'n-92cc5f10-01',
    version: 'v1.3.9',
    status: 1,
    up: ago(9 * HOUR + 5 * MIN),
    down: 0,
    job_count: 1,
    tags: ['边缘', '华南-2'],
    cpu: 88,
    mem: 82,
    disk: 91,
    agent_attached: true,
  },
  {
    id: 6,
    pid: '4410',
    ip: '10.20.31.51',
    hostname: 'node-db-01',
    uuid: 'n-7be2a904-01',
    version: 'v1.4.2',
    status: 2, // NodeConnFail
    up: ago(14 * HOUR),
    down: ago(23 * MIN),
    job_count: 0,
    tags: ['生产', '华东-1', '数据库'],
    cpu: 0,
    mem: 0,
    disk: 0,
    agent_attached: false,
  },
]

/* ------------------------------------------------------------------ *
 * 2. 定时任务（crony/common/models/job.go → models.Job）
 * ------------------------------------------------------------------ */

export const jobs = [
  {
    id: 1,
    name: '清理 90 天前日志',
    command: "find /var/log/app -name '*.log' -mtime +90 -delete",
    spec: '0 3 * * *',
    run_on: 'n-8f3d1c2a-01',
    host_name: 'node-prod-01',
    ip: '10.20.31.11',
    status: 1, // JobStatusAssigned
    job_type: 1, // JobTypeCmd
    http_method: 0,
    retry_times: 2,
    retry_interval: 5,
    timeout: 600,
    notify_type: 1,
    note: '只清理应用日志目录，不含审计日志；删除前会先统计待清理数量。',
    created: ago(30 * 24 * HOUR),
    updated: ago(2 * 24 * HOUR),
    // 规划中（阶段 1 任务模型 2.0）：并发策略 / 步骤编排，后端 job 表尚未落库
    concurrency_policy: 'skip',
    step_count: 4,
    last_run_status: 2,
    success_rate: 96.8,
  },
  {
    id: 2,
    name: 'MySQL 慢查询巡检',
    command: 'mysqlslow --top 20 --out /var/report/slow.html',
    spec: '*/30 * * * *',
    run_on: 'n-8f3d1c2a-02',
    host_name: 'node-prod-02',
    ip: '10.20.31.12',
    status: 1,
    job_type: 1,
    http_method: 0,
    retry_times: 3,
    retry_interval: 10,
    timeout: 300,
    notify_type: 2,
    note: '每 30 分钟采集一次慢查询 Top20，超过阈值触发告警。',
    created: ago(45 * 24 * HOUR),
    updated: ago(6 * HOUR),
    concurrency_policy: 'queue',
    step_count: 3,
    last_run_status: 3,
    success_rate: 88.2,
  },
  {
    id: 3,
    name: '容器健康检查',
    command: 'docker ps --filter health=unhealthy -q | xargs -r docker restart',
    spec: '*/5 * * * *',
    run_on: 'n-8f3d1c2a-03',
    host_name: 'node-prod-03',
    ip: '10.20.31.13',
    status: 1,
    job_type: 1,
    http_method: 0,
    retry_times: 1,
    retry_interval: 0,
    timeout: 120,
    notify_type: 1,
    note: '5 分钟一次；并发策略为 Skip，上一轮未结束时直接跳过本轮，避免重启风暴。',
    created: ago(60 * 24 * HOUR),
    updated: ago(30 * MIN),
    concurrency_policy: 'skip',
    step_count: 3,
    last_run_status: 5,
    success_rate: 99.1,
  },
  {
    id: 4,
    name: 'Nginx 配置热重载校验',
    command: 'nginx -t && nginx -s reload',
    spec: '0 4 * * 0',
    run_on: 'n-8f3d1c2a-01',
    host_name: 'node-prod-01',
    ip: '10.20.31.11',
    status: 1,
    job_type: 1,
    http_method: 0,
    retry_times: 0,
    retry_interval: 0,
    timeout: 60,
    notify_type: 1,
    note: '每周日凌晨校验配置并重载；校验失败不重载。',
    created: ago(20 * 24 * HOUR),
    updated: ago(7 * 24 * HOUR),
    concurrency_policy: 'forbid',
    step_count: 2,
    last_run_status: 2,
    success_rate: 100,
  },
  {
    id: 5,
    name: '磁盘水位巡检',
    command: "df -h | awk '$5+0>85 {print $6, $5}'",
    spec: '0 */2 * * *',
    run_on: 'n-8f3d1c2a-02',
    host_name: 'node-prod-02',
    ip: '10.20.31.12',
    status: 1,
    job_type: 1,
    http_method: 0,
    retry_times: 1,
    retry_interval: 3,
    timeout: 90,
    notify_type: 2,
    note: '两小时一次，>85% 上报；node-edge-01 已连续 3 次超阈值。',
    created: ago(50 * 24 * HOUR),
    updated: ago(4 * HOUR),
    concurrency_policy: 'skip',
    step_count: 2,
    last_run_status: 2,
    success_rate: 94.5,
  },
  {
    id: 6,
    name: '备份 PostgreSQL 全量',
    command: 'pg_dumpall > /backup/pg-$(date +%F).sql',
    spec: '30 1 * * *',
    run_on: 'n-8f3d1c2a-02',
    host_name: 'node-prod-02',
    ip: '10.20.31.12',
    status: 1,
    job_type: 1,
    http_method: 0,
    retry_times: 2,
    retry_interval: 30,
    timeout: 1800,
    notify_type: 2,
    note: '全量导出，超时 30 分钟；昨日因锁等待超时失败一次。',
    created: ago(80 * 24 * HOUR),
    updated: ago(11 * HOUR),
    concurrency_policy: 'forbid',
    step_count: 4,
    last_run_status: 6,
    success_rate: 91.4,
  },
  {
    id: 7,
    name: '证书到期提醒',
    command: 'https://intranet.crony.local/api/cert/check?days=30',
    spec: '0 9 * * 1',
    run_on: 'n-8f3d1c2a-01',
    host_name: 'node-prod-01',
    ip: '10.20.31.11',
    status: 1,
    job_type: 2, // JobTypeHttp
    http_method: 2, // HTTPMethodPost
    retry_times: 3,
    retry_interval: 60,
    timeout: 30,
    notify_type: 1,
    note: 'HTTP 类型任务，每周一 09:00 调用内部证书巡检接口。',
    created: ago(15 * 24 * HOUR),
    updated: ago(3 * 24 * HOUR),
    concurrency_policy: 'queue',
    step_count: 1,
    last_run_status: 2,
    success_rate: 100,
  },
  {
    id: 8,
    name: 'Kubernetes 节点排水检查',
    command: 'kubectl get nodes -o wide | grep -v Ready',
    spec: '*/15 * * * *',
    run_on: 'n-8f3d1c2a-03',
    host_name: 'node-prod-03',
    ip: '10.20.31.13',
    status: 1,
    job_type: 1,
    http_method: 0,
    retry_times: 1,
    retry_interval: 5,
    timeout: 60,
    notify_type: 1,
    note: '检测 NotReady 节点并上报给 Agent 做进一步根因分析。',
    created: ago(25 * 24 * HOUR),
    updated: ago(90 * MIN),
    concurrency_policy: 'skip',
    step_count: 3,
    last_run_status: 2,
    success_rate: 97.7,
  },
  {
    id: 9,
    name: '容器镜像垃圾回收',
    command: 'docker image prune -af --filter until=168h',
    spec: '0 5 * * 6',
    run_on: 'n-92cc5f10-01',
    host_name: 'node-edge-01',
    ip: '10.30.7.8',
    status: 1,
    job_type: 1,
    http_method: 0,
    retry_times: 1,
    retry_interval: 10,
    timeout: 900,
    notify_type: 1,
    note: '每周六清理 7 天前的悬空镜像；边缘节点磁盘吃紧时提前触发。',
    created: ago(40 * 24 * HOUR),
    updated: ago(5 * 24 * HOUR),
    concurrency_policy: 'forbid',
    step_count: 3,
    last_run_status: 2,
    success_rate: 92.0,
  },
  {
    id: 10,
    name: '应用探活失败自动回滚',
    command: 'bash /opt/app/rollback-if-unhealthy.sh',
    spec: '*/10 * * * *',
    run_on: 'n-92cc5f10-01',
    host_name: 'node-edge-01',
    ip: '10.30.7.8',
    status: 0, // JobStatusNotAssigned（等待分配）
    job_type: 1,
    http_method: 0,
    retry_times: 0,
    retry_interval: 0,
    timeout: 240,
    notify_type: 2,
    note: '高风险任务：需要人工确认后才会被分配节点执行（阶段 3 接入审批）。',
    created: ago(3 * 24 * HOUR),
    updated: ago(3 * 24 * HOUR),
    concurrency_policy: 'forbid',
    step_count: 5,
    last_run_status: 0,
    success_rate: 0,
  },
  {
    id: 11,
    name: 'Redis 内存碎片整理',
    command: 'redis-cli -h 10.20.31.13 -p 6379 memory purge',
    spec: '0 6 * * *',
    run_on: 'n-8f3d1c2a-03',
    host_name: 'node-prod-03',
    ip: '10.20.31.13',
    status: 1,
    job_type: 1,
    http_method: 0,
    retry_times: 1,
    retry_interval: 0,
    timeout: 300,
    notify_type: 1,
    note: '碎片率 >1.5 时才实际执行 purge。',
    created: ago(18 * 24 * HOUR),
    updated: ago(10 * HOUR),
    concurrency_policy: 'skip',
    step_count: 2,
    last_run_status: 2,
    success_rate: 98.3,
  },
  {
    id: 12,
    name: '安全基线巡检',
    command: 'bash /opt/security/baseline-audit.sh --level L2',
    spec: '0 2 * * 1',
    run_on: 'n-8f3d1c2a-01',
    host_name: 'node-prod-01',
    ip: '10.20.31.11',
    status: 1,
    job_type: 1,
    http_method: 0,
    retry_times: 0,
    retry_interval: 0,
    timeout: 1200,
    notify_type: 2,
    note: '等保基线 L2 巡检，产出结构化报告；结果进入审计链路。',
    created: ago(70 * 24 * HOUR),
    updated: ago(2 * 24 * HOUR),
    concurrency_policy: 'forbid',
    step_count: 6,
    last_run_status: 2,
    success_rate: 95.2,
  },
]

/* ------------------------------------------------------------------ *
 * 3. 执行记录 2.0：job_run + job_run_step
 *    （crony/common/models/run.go，阶段 1 已落库；接口为规划中）
 * ------------------------------------------------------------------ */

/** RunStatus：0 pending / 1 running / 2 success / 3 failed / 4 canceled / 5 skipped / 6 timeout */
export const runs = [
  {
    id: 9001,
    run_id: 'j1-t' + ago(3 * HOUR + 12 * MIN),
    job_id: 1,
    job_name: '清理 90 天前日志',
    spec: '0 3 * * *',
    command: "find /var/log/app -name '*.log' -mtime +90 -delete",
    job_type: 1,
    node_uuid: 'n-8f3d1c2a-01',
    hostname: 'node-prod-01',
    ip: '10.20.31.11',
    trigger_source: 1,
    triggered_by: 'scheduler',
    status: 2,
    attempt: 1,
    scheduled_time: ago(3 * HOUR + 12 * MIN),
    started_at: ago(3 * HOUR + 12 * MIN),
    finished_at: ago(3 * HOUR + 11 * MIN + 48 * SEC),
    duration_ms: 12480,
    exit_code: 0,
    step_count: 4,
    success_steps: 4,
    failed_steps: 0,
    retry_count: 1,
    error_msg: '',
    output: '已清理 1284 个日志文件，释放 3.7 GiB 空间。',
    trace_id: 'tr-7c1f9a2e4b',
    policy_snapshot: '{"concurrency":"skip","retry_times":2,"retry_interval":5,"timeout":600}',
    created: ago(3 * HOUR + 12 * MIN),
    updated: ago(3 * HOUR + 11 * MIN),
  },
  {
    id: 9002,
    run_id: 'j2-t' + ago(42 * MIN),
    job_id: 2,
    job_name: 'MySQL 慢查询巡检',
    spec: '*/30 * * * *',
    command: 'mysqlslow --top 20 --out /var/report/slow.html',
    job_type: 1,
    node_uuid: 'n-8f3d1c2a-02',
    hostname: 'node-prod-02',
    ip: '10.20.31.12',
    trigger_source: 1,
    triggered_by: 'scheduler',
    status: 3,
    attempt: 2,
    scheduled_time: ago(42 * MIN),
    started_at: ago(42 * MIN),
    finished_at: ago(38 * MIN + 20 * SEC),
    duration_ms: 220400,
    exit_code: 2,
    step_count: 3,
    success_steps: 1,
    failed_steps: 2,
    retry_count: 2,
    error_msg: '连接 MySQL 10.20.31.51:3306 超时（connect timeout），第 2 次重试后仍失败。',
    output: 'slow query 采集未完成，已保留部分结果（12 条）。',
    trace_id: 'tr-2ab88de110',
    policy_snapshot: '{"concurrency":"queue","retry_times":3,"retry_interval":10,"timeout":300}',
    created: ago(42 * MIN),
    updated: ago(38 * MIN),
  },
  {
    id: 9003,
    run_id: 'j3-t' + ago(15 * MIN),
    job_id: 3,
    job_name: '容器健康检查',
    spec: '*/5 * * * *',
    command: 'docker ps --filter health=unhealthy -q | xargs -r docker restart',
    job_type: 1,
    node_uuid: 'n-8f3d1c2a-03',
    hostname: 'node-prod-03',
    ip: '10.20.31.13',
    trigger_source: 1,
    triggered_by: 'scheduler',
    status: 5,
    attempt: 1,
    scheduled_time: ago(15 * MIN),
    started_at: 0,
    finished_at: ago(15 * MIN),
    duration_ms: 0,
    exit_code: 0,
    step_count: 0,
    success_steps: 0,
    failed_steps: 0,
    retry_count: 0,
    error_msg: '上一轮执行（run_id=j3-t…）尚未结束，并发策略 Skip：本轮直接跳过。',
    output: '',
    trace_id: 'tr-5d90c7a3f2',
    policy_snapshot: '{"concurrency":"skip","retry_times":1,"retry_interval":0,"timeout":120}',
    created: ago(15 * MIN),
    updated: ago(15 * MIN),
  },
  {
    id: 9004,
    run_id: 'j6-t' + ago(11 * HOUR),
    job_id: 6,
    job_name: '备份 PostgreSQL 全量',
    spec: '30 1 * * *',
    command: 'pg_dumpall > /backup/pg-$(date +%F).sql',
    job_type: 1,
    node_uuid: 'n-8f3d1c2a-02',
    hostname: 'node-prod-02',
    ip: '10.20.31.12',
    trigger_source: 1,
    triggered_by: 'scheduler',
    status: 6,
    attempt: 1,
    scheduled_time: ago(11 * HOUR),
    started_at: ago(11 * HOUR),
    finished_at: ago(10 * HOUR + 30 * MIN),
    duration_ms: 1800000,
    exit_code: 124,
    step_count: 4,
    success_steps: 2,
    failed_steps: 1,
    retry_count: 0,
    error_msg: '超过 timeout=1800s 被强制终止（pg_dumpall 等待锁释放）。',
    output: '已导出 42 张表，剩余 public.audit_log 等待 ACCESS SHARE 锁。',
    trace_id: 'tr-91ee2b7c05',
    policy_snapshot: '{"concurrency":"forbid","retry_times":2,"retry_interval":30,"timeout":1800}',
    created: ago(11 * HOUR),
    updated: ago(10 * HOUR + 30 * MIN),
  },
  {
    id: 9005,
    run_id: 'j2-m' + ago(5 * HOUR + 20 * MIN) * 1000000 + '-7',
    job_id: 2,
    job_name: 'MySQL 慢查询巡检',
    spec: '*/30 * * * *',
    command: 'mysqlslow --top 20 --out /var/report/slow.html',
    job_type: 1,
    node_uuid: 'n-8f3d1c2a-02',
    hostname: 'node-prod-02',
    ip: '10.20.31.12',
    trigger_source: 2,
    triggered_by: 'admin',
    status: 2,
    attempt: 1,
    scheduled_time: 0,
    started_at: ago(5 * HOUR + 20 * MIN),
    finished_at: ago(5 * HOUR + 19 * MIN + 12 * SEC),
    duration_ms: 48200,
    exit_code: 0,
    step_count: 3,
    success_steps: 3,
    failed_steps: 0,
    retry_count: 0,
    error_msg: '',
    output: '采集完成：慢查询 12 条，最慢 4.28s（sql_id=8f21c）。',
    trace_id: 'tr-33a1f0d9bb',
    policy_snapshot: '{"concurrency":"queue","retry_times":3,"retry_interval":10,"timeout":300}',
    created: ago(5 * HOUR + 20 * MIN),
    updated: ago(5 * HOUR + 19 * MIN),
  },
  {
    id: 9006,
    run_id: 'j5-t' + ago(2 * HOUR),
    job_id: 5,
    job_name: '磁盘水位巡检',
    spec: '0 */2 * * *',
    command: "df -h | awk '$5+0>85 {print $6, $5}'",
    job_type: 1,
    node_uuid: 'n-92cc5f10-01',
    hostname: 'node-edge-01',
    ip: '10.30.7.8',
    trigger_source: 1,
    triggered_by: 'scheduler',
    status: 2,
    attempt: 1,
    scheduled_time: ago(2 * HOUR),
    started_at: ago(2 * HOUR),
    finished_at: ago(2 * HOUR - 3 * SEC),
    duration_ms: 3180,
    exit_code: 0,
    step_count: 2,
    success_steps: 2,
    failed_steps: 0,
    retry_count: 0,
    error_msg: '',
    output: '/data 91% /var 88% —— 已上报告警（2 条）。',
    trace_id: 'tr-6f4b2c8811',
    policy_snapshot: '{"concurrency":"skip","retry_times":1,"retry_interval":3,"timeout":90}',
    created: ago(2 * HOUR),
    updated: ago(2 * HOUR),
  },
  {
    id: 9007,
    run_id: 'j8-t' + ago(30 * MIN),
    job_id: 8,
    job_name: 'Kubernetes 节点排水检查',
    spec: '*/15 * * * *',
    command: 'kubectl get nodes -o wide | grep -v Ready',
    job_type: 1,
    node_uuid: 'n-8f3d1c2a-03',
    hostname: 'node-prod-03',
    ip: '10.20.31.13',
    trigger_source: 5,
    triggered_by: 'agent-observe-01',
    status: 1,
    attempt: 1,
    scheduled_time: ago(30 * MIN),
    started_at: ago(30 * MIN),
    finished_at: 0,
    duration_ms: 0,
    exit_code: 0,
    step_count: 3,
    success_steps: 1,
    failed_steps: 0,
    retry_count: 0,
    error_msg: '',
    output: '',
    trace_id: 'tr-a0d5e77f31',
    policy_snapshot: '{"concurrency":"skip","retry_times":1,"retry_interval":5,"timeout":60}',
    created: ago(30 * MIN),
    updated: ago(29 * MIN),
  },
  {
    id: 9008,
    run_id: 'j12-t' + ago(2 * 24 * HOUR),
    job_id: 12,
    job_name: '安全基线巡检',
    spec: '0 2 * * 1',
    command: 'bash /opt/security/baseline-audit.sh --level L2',
    job_type: 1,
    node_uuid: 'n-8f3d1c2a-01',
    hostname: 'node-prod-01',
    ip: '10.20.31.11',
    trigger_source: 1,
    triggered_by: 'scheduler',
    status: 2,
    attempt: 1,
    scheduled_time: ago(2 * 24 * HOUR),
    started_at: ago(2 * 24 * HOUR),
    finished_at: ago(2 * 24 * HOUR - 6 * MIN),
    duration_ms: 364000,
    exit_code: 0,
    step_count: 6,
    success_steps: 6,
    failed_steps: 0,
    retry_count: 0,
    error_msg: '',
    output: '基线检查 68 项：通过 65，未通过 3（均已生成整改建议）。',
    trace_id: 'tr-b7c9e1d402',
    policy_snapshot: '{"concurrency":"forbid","retry_times":0,"retry_interval":0,"timeout":1200}',
    created: ago(2 * 24 * HOUR),
    updated: ago(2 * 24 * HOUR - 6 * MIN),
  },
  {
    id: 9009,
    run_id: 'j1-m' + ago(26 * HOUR) * 1000000 + '-3',
    job_id: 1,
    job_name: '清理 90 天前日志',
    spec: '0 3 * * *',
    command: "find /var/log/app -name '*.log' -mtime +90 -delete",
    job_type: 1,
    node_uuid: 'n-8f3d1c2a-01',
    hostname: 'node-prod-01',
    ip: '10.20.31.11',
    trigger_source: 6,
    triggered_by: 'self-heal:disk-high',
    status: 4,
    attempt: 1,
    scheduled_time: 0,
    started_at: ago(26 * HOUR),
    finished_at: ago(26 * HOUR - 42 * SEC),
    duration_ms: 42000,
    exit_code: 130,
    step_count: 4,
    success_steps: 1,
    failed_steps: 1,
    retry_count: 0,
    error_msg: '运维人员手动取消（审计单 AUD-2201）。',
    output: '已删除 210 个文件后被取消。',
    trace_id: 'tr-0e83ff2a19',
    policy_snapshot: '{"concurrency":"skip","retry_times":2,"retry_interval":5,"timeout":600}',
    created: ago(26 * HOUR),
    updated: ago(26 * HOUR - 42 * SEC),
  },
]

/** 步骤时间线（job_run_step） */
export const runSteps = {
  'j1-t97': null, // 占位说明：真实 key 由 run_id 动态匹配，见下方 buildSteps()
}

/**
 * 按 run_id 生成步骤时间线。
 * 这里用"模式匹配"而不是逐 run 硬编码，避免 9 条 run × 平均 4 步的巨大冗余。
 */
export function buildSteps(run) {
  const base = run.started_at || run.scheduled_time || Math.floor(NOW / SEC)
  const mk = (i, o) => ({
    id: run.id * 10 + i,
    run_id: run.run_id,
    step_index: i,
    created: base + (o.offset || 0),
    ...o,
  })

  switch (run.status) {
    case 5: // skipped：并发策略 Skip，没有产生任何步骤
      return []
    case 3: // failed：MySQL 慢查询巡检
      return [
        mk(1, {
          name: '加载巡检配置',
          kind: 'shell',
          condition: 'on_success',
          command: 'cat /etc/crony/mysqlslow.yml',
          status: 2,
          exit_code: 0,
          retry_count: 0,
          started_at: base,
          finished_at: base + 1,
          duration_ms: 820,
          output: '已加载配置：host=10.20.31.51 port=3306 top=20',
          error_msg: '',
          artifacts: '{}',
          offset: 0,
        }),
        mk(2, {
          name: '连接数据库并采集慢查询',
          kind: 'shell',
          condition: 'on_success',
          command: 'mysqlslow --top 20 --out /var/report/slow.html',
          status: 3,
          exit_code: 2,
          retry_count: 2,
          started_at: base + 2,
          finished_at: base + 62,
          duration_ms: 60000,
          output: '第 1 次尝试：connect timeout (10s)\n第 2 次尝试：退避 10s 后重试，仍 connect timeout\n第 3 次尝试：退避 10s 后重试，仍 connect timeout',
          error_msg: 'MySQL 连接超时；重试 2 次（间隔 10s 指数退避）后放弃。',
          artifacts: '{"attempts":3,"last_error":"connect timeout","target":"10.20.31.51:3306"}',
          offset: 2,
        }),
        mk(3, {
          name: '失败兜底：保留部分结果并上报',
          kind: 'http',
          condition: 'on_failure',
          command: 'POST https://intranet.crony.local/api/alert/notify',
          status: 3,
          exit_code: 7,
          retry_count: 1,
          started_at: base + 63,
          finished_at: base + 64,
          duration_ms: 1400,
          output: '告警网关返回 502（上游不可用），已降级写入本地队列。',
          error_msg: '告警投递失败，转为本地重投队列（阶段 1 重投队列尚未落库）。',
          artifacts: '{"http_status":502,"fallback":"local_queue"}',
          offset: 63,
        }),
      ]
    case 6: // timeout：PostgreSQL 备份
      return [
        mk(1, {
          name: '检查备份目录空间',
          kind: 'shell',
          condition: 'on_success',
          command: 'df -h /backup',
          status: 2,
          exit_code: 0,
          retry_count: 0,
          started_at: base,
          finished_at: base + 1,
          duration_ms: 640,
          output: '/backup 剩余 214 GiB（预计需要 180 GiB）',
          error_msg: '',
          artifacts: '{"free_gb":214}',
          offset: 0,
        }),
        mk(2, {
          name: '终止长时间运行的只读事务',
          kind: 'sql',
          condition: 'on_success',
          command: "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE state='idle in transaction' AND now()-state_change > interval '10 min'",
          status: 2,
          exit_code: 0,
          retry_count: 0,
          started_at: base + 1,
          finished_at: base + 3,
          duration_ms: 2100,
          output: '已终止 2 个空闲事务。',
          error_msg: '',
          artifacts: '{"terminated":2}',
          offset: 1,
        }),
        mk(3, {
          name: 'pg_dumpall 全量导出',
          kind: 'shell',
          condition: 'on_success',
          command: 'pg_dumpall > /backup/pg-$(date +%F).sql',
          status: 6,
          exit_code: 124,
          retry_count: 0,
          started_at: base + 3,
          finished_at: base + 1803,
          duration_ms: 1800000,
          output: '已导出 42 张表…\npublic.audit_log: 等待 ACCESS SHARE 锁（另有一个长事务持有 EXCLUSIVE 锁）\n[1800s] timeout，进程组被 SIGTERM 终止。',
          error_msg: '达到 timeout=1800s 上限，输出被截断；建议改为按表并行 + 快照读。',
          artifacts: '{"tables_done":42,"tables_total":43,"blocked_table":"public.audit_log","killed":"SIGTERM"}',
          offset: 3,
        }),
        mk(4, {
          name: '清理半成品文件',
          kind: 'shell',
          condition: 'always',
          command: 'find /backup -name "pg-*.sql" -size -1M -delete',
          status: 2,
          exit_code: 0,
          retry_count: 0,
          started_at: base + 1803,
          finished_at: base + 1804,
          duration_ms: 900,
          output: '已删除 1 个不完整备份文件。',
          error_msg: '',
          artifacts: '{"removed":1}',
          offset: 1803,
        }),
      ]
    case 4: // canceled：自愈触发的清理
      return [
        mk(1, {
          name: '自愈前置检查',
          kind: 'shell',
          condition: 'on_success',
          command: 'test -w /var/log/app && echo ok',
          status: 2,
          exit_code: 0,
          retry_count: 0,
          started_at: base,
          finished_at: base + 1,
          duration_ms: 310,
          output: 'ok',
          error_msg: '',
          artifacts: '{}',
          offset: 0,
        }),
        mk(2, {
          name: '执行清理',
          kind: 'shell',
          condition: 'on_success',
          command: "find /var/log/app -name '*.log' -mtime +90 -delete",
          status: 4,
          exit_code: 130,
          retry_count: 0,
          started_at: base + 1,
          finished_at: base + 42,
          duration_ms: 41000,
          output: '已删除 210 个文件后收到取消信号（SIGINT），进程组已退出。',
          error_msg: '运维人员手动取消（审计单 AUD-2201）。',
          artifacts: '{"deleted":210,"signal":"SIGINT"}',
          offset: 1,
        }),
      ]
    case 1: // running：K8s 节点检查
      return [
        mk(1, {
          name: '加载 kubeconfig',
          kind: 'shell',
          condition: 'on_success',
          command: 'export KUBECONFIG=/etc/kubernetes/admin.conf',
          status: 2,
          exit_code: 0,
          retry_count: 0,
          started_at: base,
          finished_at: base + 1,
          duration_ms: 420,
          output: '已加载集群 prod-east-1。',
          error_msg: '',
          artifacts: '{"cluster":"prod-east-1"}',
          offset: 0,
        }),
        mk(2, {
          name: '查询节点状态',
          kind: 'shell',
          condition: 'on_success',
          command: 'kubectl get nodes -o wide',
          status: 1,
          exit_code: 0,
          retry_count: 0,
          started_at: base + 1,
          finished_at: 0,
          duration_ms: 0,
          output: '（执行中）已返回 5 个节点，正在比对 Ready 状态…',
          error_msg: '',
          artifacts: '{}',
          offset: 1,
        }),
        mk(3, {
          name: '上报 NotReady 节点',
          kind: 'http',
          condition: 'on_failure',
          command: 'POST https://intranet.crony.local/api/k8s/notready',
          status: 0,
          exit_code: 0,
          retry_count: 0,
          started_at: 0,
          finished_at: 0,
          duration_ms: 0,
          output: '',
          error_msg: '',
          artifacts: '{}',
          offset: 2,
        }),
      ]
    default: {
      // success：按任务类型给出可读的步骤
      if (run.job_id === 1) {
        return [
          mk(1, {
            name: '预检日志目录可写',
            kind: 'shell',
            condition: 'on_success',
            command: 'test -w /var/log/app',
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base,
            finished_at: base + 1,
            duration_ms: 260,
            output: 'ok',
            error_msg: '',
            artifacts: '{}',
            offset: 0,
          }),
          mk(2, {
            name: '统计待清理文件',
            kind: 'shell',
            condition: 'on_success',
            command: "find /var/log/app -name '*.log' -mtime +90 | wc -l",
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base + 1,
            finished_at: base + 2,
            duration_ms: 1850,
            output: '1284',
            error_msg: '',
            artifacts: '{"pending_files":1284,"pending_bytes":3972844748}',
            offset: 1,
          }),
          mk(3, {
            name: '执行清理',
            kind: 'shell',
            condition: 'on_success',
            command: "find /var/log/app -name '*.log' -mtime +90 -delete",
            status: 2,
            exit_code: 0,
            retry_count: 1,
            started_at: base + 2,
            finished_at: base + 9,
            duration_ms: 7060,
            output: '第 1 次尝试：3 个文件被进程占用（EBUSY），按 retry_interval=5 退避 5s 后重试。\n第 2 次尝试：删除成功。',
            error_msg: '',
            artifacts: '{"deleted":1284,"freed_bytes":3972844748,"retried":1}',
            offset: 2,
          }),
          mk(4, {
            name: '汇总并上报',
            kind: 'http',
            condition: 'on_success',
            command: 'POST https://intranet.crony.local/api/report/cleanup',
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base + 10,
            finished_at: base + 11,
            duration_ms: 310,
            output: '上报成功。',
            error_msg: '',
            artifacts: '{"http_status":200,"report_id":"RPT-99120"}',
            offset: 10,
          }),
        ]
      }
      if (run.job_id === 12) {
        return [
          mk(1, {
            name: '身份与授权校验',
            kind: 'shell',
            condition: 'on_success',
            command: 'whoami && id -u',
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base,
            finished_at: base + 1,
            duration_ms: 180,
            output: 'root (0)',
            error_msg: '',
            artifacts: '{"uid":0}',
            offset: 0,
          }),
          mk(2, {
            name: 'SSH 与口令策略检查',
            kind: 'shell',
            condition: 'on_success',
            command: 'sshd -T | grep -E "permitrootlogin|passwordauthentication"',
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base + 1,
            finished_at: base + 3,
            duration_ms: 2400,
            output: 'permitrootlogin no\npasswordauthentication no',
            error_msg: '',
            artifacts: '{"ssh_items":12,"passed":12}',
            offset: 1,
          }),
          mk(3, {
            name: '内核参数加固项检查',
            kind: 'shell',
            condition: 'on_success',
            command: 'sysctl -a | grep -E "net.ipv4.conf.all.rp_filter"',
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base + 3,
            finished_at: base + 20,
            duration_ms: 17200,
            output: 'net.ipv4.conf.all.rp_filter = 1',
            error_msg: '',
            artifacts: '{"sysctl_items":24,"passed":23,"failed":1}',
            offset: 3,
          }),
          mk(4, {
            name: '文件权限基线检查',
            kind: 'shell',
            condition: 'on_success',
            command: 'find /etc -perm -o+w -type f',
            status: 2,
            exit_code: 0,
            retry_count: 1,
            started_at: base + 20,
            finished_at: base + 240,
            duration_ms: 220000,
            output: '第 1 次尝试：/etc/ssh/ssh_host_ecdsa_key 权限 644（期望 600）→ 已自动修正\n第 2 次尝试：复查通过。',
            error_msg: '',
            artifacts: '{"fixed":1,"remaining":0}',
            offset: 20,
          }),
          mk(5, {
            name: '生成结构化报告',
            kind: 'python',
            condition: 'on_success',
            command: 'python3 /opt/security/render_report.py --format json',
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base + 240,
            finished_at: base + 300,
            duration_ms: 60000,
            output: '报告已生成：/var/report/baseline-2026-L2.json',
            error_msg: '',
            artifacts: '{"report":"/var/report/baseline-2026-L2.json","checks":68}',
            offset: 240,
          }),
          mk(6, {
            name: '写入审计链路',
            kind: 'http',
            condition: 'always',
            command: 'POST https://intranet.crony.local/api/audit/append',
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base + 300,
            finished_at: base + 301,
            duration_ms: 420,
            output: '审计记录已写入（audit_id=AUD-2207）。',
            error_msg: '',
            artifacts: '{"http_status":200,"audit_id":"AUD-2207"}',
            offset: 300,
          }),
        ]
      }
      if (run.job_id === 5) {
        return [
          mk(1, {
            name: '采集磁盘水位',
            kind: 'shell',
            condition: 'on_success',
            command: 'df -h',
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base,
            finished_at: base + 1,
            duration_ms: 620,
            output: '/data 91%\n/var 88%\n/ 41%',
            error_msg: '',
            artifacts: '{"partitions":{"\\/data":91,"\\/var":88,"\\/":41}}',
            offset: 0,
          }),
          mk(2, {
            name: '超阈值项上报告警',
            kind: 'http',
            condition: 'on_success',
            command: 'POST https://intranet.crony.local/api/alert/notify',
            status: 2,
            exit_code: 0,
            retry_count: 0,
            started_at: base + 1,
            finished_at: base + 2,
            duration_ms: 700,
            output: '已上报 2 条告警（/data、/var）。',
            error_msg: '',
            artifacts: '{"http_status":200,"alerts":2}',
            offset: 1,
          }),
        ]
      }
      // 通用成功模板（手动触发的 MySQL 巡检等）
      return [
        mk(1, {
          name: '准备执行环境',
          kind: 'shell',
          condition: 'on_success',
          command: 'mkdir -p /var/report && cd /var/report',
          status: 2,
          exit_code: 0,
          retry_count: 0,
          started_at: base,
          finished_at: base + 1,
          duration_ms: 240,
          output: 'ok',
          error_msg: '',
          artifacts: '{}',
          offset: 0,
        }),
        mk(2, {
          name: run.job_name,
          kind: run.job_type === 2 ? 'http' : 'shell',
          condition: 'on_success',
          command: run.command,
          status: 2,
          exit_code: 0,
          retry_count: 0,
          started_at: base + 1,
          finished_at: base + 40,
          duration_ms: 39000,
          output: run.output || '执行成功。',
          error_msg: '',
          artifacts: '{"exit_code":0}',
          offset: 1,
        }),
        mk(3, {
          name: '结果归档',
          kind: 'http',
          condition: 'on_success',
          command: 'POST https://intranet.crony.local/api/report/archive',
          status: 2,
          exit_code: 0,
          retry_count: 0,
          started_at: base + 40,
          finished_at: base + 41,
          duration_ms: 380,
          output: '归档成功。',
          error_msg: '',
          artifacts: '{"http_status":200}',
          offset: 40,
        }),
      ]
    }
  }
}

/* ------------------------------------------------------------------ *
 * 4. 旧版执行日志（POST /job/log → models.JobLog），保留给兼容视图
 * ------------------------------------------------------------------ */

export const jobLogs = runs
  .filter((r) => r.status === 2 || r.status === 3)
  .map((r, i) => ({
    id: 5001 + i,
    name: r.job_name,
    job_id: r.job_id,
    command: r.command,
    ip: r.ip,
    hostname: r.hostname,
    node_uuid: r.node_uuid,
    success: r.status === 2,
    output: r.output,
    spec: r.spec,
    retry_times: r.retry_count,
    start_time: r.started_at,
    end_time: r.finished_at,
  }))

/* ------------------------------------------------------------------ *
 * 5. Agent 内核（阶段 2 · 《PHASE2-接口契约.md》§1–§5）
 *
 * 设计原则：
 *  - 字段名**逐字**对齐契约（seq / created / total_tokens / tool_args …），
 *    Mock 与真后端共用同一套字段，页面里没有任何"Mock 专用分支"；
 *  - run_id / node_uuid / job_id 全部从上面的 runs / nodes 派生，保证"会话里出现的 run
 *    在 /runs 里真的存在"，不另造一套平行假数据；
 *  - provider 一律 'offline'：本机只有确定性假 provider（agentconfig.ProviderOffline），
 *    它**不是模型**、也不上报 model 名。契约 §7 要求如实展示 provider，这里不写任何品牌名；
 *  - 时间用带本地偏移的 RFC3339（契约 §0），阶段 1 的 Unix 秒字段保持不变。
 * ------------------------------------------------------------------ */

const p2 = (n) => String(n).padStart(2, '0')

/** 本地时区名（IANA）。契约里的 next_trigger_text 用 "CST" 这种缩写，这里给更明确的 IANA 名。 */
export const AGENT_TZ = (() => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'Local'
  } catch {
    return 'Local'
  }
})()

/** Unix 毫秒 → RFC3339（带本地偏移，如 2026-03-02T02:05:00+08:00） */
export function rfc3339(ms) {
  const d = new Date(ms)
  const off = -d.getTimezoneOffset()
  const sign = off >= 0 ? '+' : '-'
  const abs = Math.abs(off)
  return (
    `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}` +
    `T${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}` +
    `${sign}${p2(Math.floor(abs / 60))}:${p2(abs % 60)}`
  )
}

/** 'YYYY-MM-DD HH:mm:ss (Asia/Shanghai)' —— 对应 Go agent.FormatTriggers 的可读形态 */
function triggerText(d) {
  return (
    `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ` +
    `${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())} (${AGENT_TZ})`
  )
}

/** 接下来 count 次"每小时第 minute 分" */
function hourlyTriggers(minute, count = 5, fromMs = NOW) {
  const out = []
  const base = new Date(fromMs)
  base.setSeconds(0, 0)
  base.setMinutes(minute)
  if (base.getTime() <= fromMs) base.setHours(base.getHours() + 1)
  for (let i = 0; i < count; i += 1) {
    out.push(new Date(base.getTime() + i * 3600 * 1000))
  }
  return out
}

/** 接下来 count 次"每天 hour:minute:00" */
function dailyTriggers(hour, minute, count = 5, fromMs = NOW) {
  const out = []
  const base = new Date(fromMs)
  base.setHours(hour, minute, 0, 0)
  if (base.getTime() <= fromMs) base.setDate(base.getDate() + 1)
  for (let i = 0; i < count; i += 1) {
    out.push(new Date(base.getTime() + i * 24 * 3600 * 1000))
  }
  return out
}

/** 与 Go agent.TriggerEcho 同形的确认文本（契约 §5.1 trigger_echo） */
export function triggerEcho(spec, times) {
  const lines = times.map((t, i) => `  ${i + 1}) ${triggerText(t)}`).join('\n')
  return (
    `表达式 "${spec}" 接下来 ${times.length} 次触发时间：\n` +
    lines +
    '\n确认无误后再创建任务；确认前不会写入调度器。'
  )
}

/**
 * 按**本库语义**估算接下来 count 次触发时间（确认页要用：用户改了 spec 之后必须重算）。
 *
 * 只覆盖日常会写的两种形态：6 段（秒 分 时 日 月 周）与 5 段（本库会解释成 秒 分 时 日 月）。
 * 其它形态（带步长、区间、枚举的字段）Mock 不去猜——真实实现由后端 cron 库计算，
 * 页面展示的永远是接口返回的 next_triggers，这里只服务于 Mock 的即时重算。
 */
export function specTriggers(spec, count = 5, fromMs = NOW) {
  const parts = String(spec ?? '').trim().split(/\s+/)
  const isInt = (s) => /^\d+$/.test(String(s ?? ''))
  if (parts.length === 6 && isInt(parts[1]) && isInt(parts[2])) {
    return dailyTriggers(Number(parts[2]), Number(parts[1]), count, fromMs)
  }
  // 5 段：第 2 段是"分"
  const minute = isInt(parts[1]) ? Number(parts[1]) : 0
  return hourlyTriggers(minute, count, fromMs)
}

/**
 * 停止原因的人话文案。与 Go agent.StopReason.Explain() 同义；
 * completed 用契约示例里的"正常完成"。
 */
export const AGENT_STOP_TEXT = {
  completed: '正常完成',
  max_steps: '达到最大步数上限（保护性终止，结论可能不完整）',
  tool_budget: '达到工具调用次数上限（保护性终止）',
  llm_budget: '模型预算耗尽（保护性终止）',
  canceled: '被主动取消',
  schema_invalid: '多次尝试后输出仍不符合要求的结构',
  no_provider: '没有可用的模型 provider',
  error: '执行过程中出现错误',
}

/** 会话级模型预算（llm.DefaultBudget：20 次调用 / 200000 token / 30 分钟） */
export const AGENT_BUDGET_MAX = { max_calls: 20, max_tokens: 200000, max_duration_ms: 1800000 }

/** 单会话工具调用额度（tools.DefaultPolicy：MaxCallsPerSession=12） */
export const AGENT_MAX_TOOL_CALLS = 12

/** 从既有 runs 里取样本，保证会话与 /runs 指向同一批记录 */
const runOf = (jobId, status) => runs.find((r) => r.job_id === jobId && r.status === status)
const RUN_DIAG_FULL = runOf(2, 3) // 失败：慢查询巡检连接超时 → 诊断证据充分
const RUN_DIAG_THIN = runOf(6, 6) // 超时：pg_dumpall 等锁 → 诊断证据不足
const RUN_DIAG_UNVERIFIED = runOf(1, 4) // 取消：人工取消 → 诊断引用了未观测证据
const RUN_JOB_OK = runOf(12, 2) // 成功：安全基线巡检（任务内嵌 Agent）
const RUN_JOB_BUDGET = runOf(5, 2) // 成功：磁盘水位巡检（内嵌 Agent 工具预算触顶）

/* ---------------- §2 轨迹步骤构造器（字段名对齐契约） ---------------- */

function modelStep(seq, ms, s) {
  return {
    seq,
    kind: 'model',
    provider: 'offline',
    model: '',
    prompt_preview: s.prompt_preview,
    prompt_bytes: s.prompt_bytes,
    content: s.content || '',
    tool_calls: s.tool_calls || [],
    total_tokens: s.total_tokens,
    latency_ms: s.latency_ms,
    finish_reason: s.finish_reason,
    truncated: false,
    created: rfc3339(ms),
  }
}

function toolStep(seq, ms, s) {
  return {
    seq,
    kind: 'tool',
    tool_name: s.tool_name,
    tool_args: s.tool_args,
    observation: s.observation,
    evidence: s.evidence || [],
    duration_ms: s.duration_ms,
    error: s.error || '',
    truncated: !!s.truncated,
    created: rfc3339(ms),
  }
}

function noticeStep(seq, ms, notice) {
  return { seq, kind: 'notice', notice, created: rfc3339(ms) }
}

/* ---------------- 诊断结果（§3.1 的 diagnosis 字段） ---------------- */

/** 样本 A：证据充分、置信度中等 */
const DIAG_FULL = {
  summary: 'MySQL 慢查询巡检连不上 10.20.31.51:3306：目标实例不可达（connect timeout）',
  category: 'network',
  category_text: '网络问题',
  cause:
    '任务在第 2 次重试后仍 connect timeout，节点日志显示 TCP 连接无响应，' +
    '且目标实例所在节点 node-db-01 的心跳已中断 23 分钟——两处观测互相印证"不是鉴权问题而是不可达"。',
  confidence: 72,
  evidence: [
    {
      ref: 'node.log:12',
      quote: 'mysqlslow: connect to 10.20.31.51:3306 timeout after 30000ms',
      why: '直连超时（而非鉴权失败）指向网络或实例不可达',
    },
    {
      ref: `run:${RUN_DIAG_FULL.run_id}#step-2`,
      quote: '步骤 2（采集慢查询）退出码 2，已重试 2 次',
      why: '失败稳定复现在采集阶段，说明不是偶发抖动',
    },
    {
      ref: 'node:n-7be2a904-01',
      quote: 'node-db-01 conn=NodeConnFail，down=23m',
      why: '目标实例所在节点已失联，与连接超时相互印证',
    },
  ],
  assumptions: ['实例侧可能正在重启或网络策略正在变更（本节点无法验证）'],
  ruled_out: [
    { hypothesis: '任务没有触发', reason: '本次 run 记录存在且状态为 failed，attempt=2' },
    { hypothesis: '命令写错', reason: `同一命令在 5 小时前的手动 run（${runOf(2, 2).run_id}）里执行成功` },
  ],
  suggested_actions: [
    {
      title: '确认 node-db-01 心跳与 3306 端口连通性',
      detail:
        '在 node-prod-02 上用 node_status 复核 node-db-01 状态，并在变更窗口内确认实例是否重启；' +
        '恢复心跳后再手动触发一次该任务，观察是否仍超时。',
      risk: 'low',
      risk_text: '低风险',
      rollback: '无（只读排查）',
    },
  ],
  evidence_sufficient: true,
  next_data_to_collect: [],
  unverified_refs: [],
  warnings: [],
  advisory_only: true,
  provider: 'offline',
  model: '',
  prompt_version: 'diagnose-v1',
  stop_reason: 'completed',
  stop_reason_text: '正常完成',
  duration_ms: 4210,
  tool_calls: 1,
  total_tokens: 1834,
}

DIAG_FULL.run_id = RUN_DIAG_FULL.run_id
DIAG_FULL.job_id = RUN_DIAG_FULL.job_id
DIAG_FULL.session_id = `diag-j${RUN_DIAG_FULL.job_id}-${RUN_DIAG_FULL.run_id}`
DIAG_FULL.created = rfc3339(RUN_DIAG_FULL.finished_at * 1000 + 60 * 1000)
DIAG_FULL.explain =
  `【诊断】任务 ${RUN_DIAG_FULL.job_id}（run ${RUN_DIAG_FULL.run_id}）\n` +
  `结论: ${DIAG_FULL.summary}\n` +
  `分类: ${DIAG_FULL.category_text}  置信度: ${DIAG_FULL.confidence}%（模型自评）  证据充分: 是\n` +
  `原因: ${DIAG_FULL.cause}\n` +
  '证据链:\n' +
  DIAG_FULL.evidence.map((e, i) => `  ${i + 1}. [${e.ref}] ${e.quote}\n     作用: ${e.why}`).join('\n') +
  '\n未验证假设:\n' +
  DIAG_FULL.assumptions.map((a) => `  - ${a}`).join('\n') +
  '\n已排除:\n' +
  DIAG_FULL.ruled_out.map((r) => `  - ${r.hypothesis}（${r.reason}）`).join('\n') +
  '\n建议动作（需人工执行）:\n' +
  DIAG_FULL.suggested_actions
    .map((a, i) => `  ${i + 1}. [${a.risk_text}] ${a.title} —— ${a.detail}\n     回滚: ${a.rollback}`)
    .join('\n') +
  `\n终止原因: ${DIAG_FULL.stop_reason}（${DIAG_FULL.stop_reason_text}）`

/** 样本 B：证据不足（evidence_sufficient=false，必须给出还要采集什么） */
const DIAG_THIN = {
  summary: 'PostgreSQL 全量备份在 timeout=1800s 被强制终止；现有证据只能定位到"等锁"，无法确认持锁来源',
  category: 'dependency',
  category_text: '依赖不可用',
  cause:
    'run 输出显示 public.audit_log 在等待 ACCESS SHARE 锁；但本节点没有数据库长事务视图，' +
    '无法确认是谁在持锁、持了多久，因此不能断言根因。',
  confidence: 41,
  evidence: [
    {
      ref: `run:${RUN_DIAG_THIN.run_id}`,
      quote: '超过 timeout=1800s 被强制终止（pg_dumpall 等待锁释放）',
      why: '只能说明"在等锁"，不能说明"谁在持锁"',
    },
  ],
  assumptions: ['存在长事务持有 audit_log 的锁（本次观测无法验证）'],
  ruled_out: [{ hypothesis: '备份盘写满', reason: '同节点其它写入类任务在同一时段正常完成' }],
  suggested_actions: [
    {
      title: '采集 pg_locks / pg_stat_activity 快照',
      detail:
        '在实例上执行只读查询，记录持锁 pid、query、事务开始时间；' +
        '拿到持锁会话后再决定是 kill 会话还是调整备份时间窗。',
      risk: 'low',
      risk_text: '低风险',
      rollback: '无（只读查询）',
    },
  ],
  evidence_sufficient: false,
  next_data_to_collect: [
    '目标实例的 pg_locks 快照（含 pid / query / 事务持续时长）',
    '本次 run 结束前后 5 分钟的数据库日志',
    '同一时间窗内该实例上的长事务审计记录',
  ],
  unverified_refs: [],
  warnings: ['证据不足以支撑明确结论：已按 evidence_sufficient=false 处理，未给出根因断言'],
  advisory_only: true,
  provider: 'offline',
  model: '',
  prompt_version: 'diagnose-v1',
  stop_reason: 'completed',
  stop_reason_text: '正常完成',
  duration_ms: 5120,
  tool_calls: 1,
  total_tokens: 2106,
}

DIAG_THIN.run_id = RUN_DIAG_THIN.run_id
DIAG_THIN.job_id = RUN_DIAG_THIN.job_id
DIAG_THIN.session_id = `diag-j${RUN_DIAG_THIN.job_id}-${RUN_DIAG_THIN.run_id}`
DIAG_THIN.created = rfc3339(RUN_DIAG_THIN.finished_at * 1000 + 90 * 1000)
DIAG_THIN.explain =
  `【诊断】任务 ${RUN_DIAG_THIN.job_id}（run ${RUN_DIAG_THIN.run_id}）\n` +
  `结论: ${DIAG_THIN.summary}\n` +
  `分类: ${DIAG_THIN.category_text}  置信度: ${DIAG_THIN.confidence}%（模型自评）  证据充分: 否\n` +
  `原因: ${DIAG_THIN.cause}\n` +
  '证据链:\n' +
  DIAG_THIN.evidence.map((e, i) => `  ${i + 1}. [${e.ref}] ${e.quote}\n     作用: ${e.why}`).join('\n') +
  '\n还需要采集:\n' +
  DIAG_THIN.next_data_to_collect.map((n) => `  - ${n}`).join('\n') +
  `\n终止原因: ${DIAG_THIN.stop_reason}（${DIAG_THIN.stop_reason_text}）`

/** 样本 C：模型引用了本次并未观测到的证据定位（unverified_refs 非空，必须醒目告警） */
const DIAG_UNVERIFIED = {
  summary: '日志清理任务被人工取消；现存观测只覆盖"取消"这一事实，取消原因不在本节点数据里',
  category: 'unknown',
  category_text: '未归类',
  cause:
    'run 记录为 status=canceled / exit_code=130，错误信息写明"运维人员手动取消（审计单 AUD-2201）"；' +
    '该审计单与取消原因都不在本节点可观测范围内。',
  confidence: 63,
  evidence: [
    {
      ref: `run:${RUN_DIAG_UNVERIFIED.run_id}`,
      quote: '运维人员手动取消（审计单 AUD-2201）',
      why: '这是取消事实的直接来源，但其中不含取消理由',
    },
    {
      ref: 'crony.log:88',
      quote: 'step 1 finished: removed 210 files',
      why: '取消发生在执行中途，说明前半段是正常执行的',
    },
  ],
  assumptions: ['存在对应的变更/审计单据（本节点无法读取审计系统）'],
  ruled_out: [{ hypothesis: '任务自身报错退出', reason: 'exit_code=130 是取消信号而非业务错误码' }],
  suggested_actions: [
    {
      title: '回填取消原因到知识库',
      detail: '从审计系统拉取 AUD-2201 的处理说明，补进本任务的复盘文档，避免下次重复排查。',
      risk: 'low',
      risk_text: '低风险',
      rollback: '无（文档操作）',
    },
  ],
  evidence_sufficient: true,
  next_data_to_collect: [],
  unverified_refs: ['syslog:999'],
  warnings: [
    '模型引用了 1 处本次并未观测到的证据定位：syslog:999',
    '取消原因不在本节点可观测范围内，结论按"未知"处理，未推断动机',
  ],
  advisory_only: true,
  provider: 'offline',
  model: '',
  prompt_version: 'diagnose-v1',
  stop_reason: 'completed',
  stop_reason_text: '正常完成',
  duration_ms: 3980,
  tool_calls: 1,
  total_tokens: 1760,
}

DIAG_UNVERIFIED.run_id = RUN_DIAG_UNVERIFIED.run_id
DIAG_UNVERIFIED.job_id = RUN_DIAG_UNVERIFIED.job_id
DIAG_UNVERIFIED.session_id = `diag-j${RUN_DIAG_UNVERIFIED.job_id}-${RUN_DIAG_UNVERIFIED.run_id}`
DIAG_UNVERIFIED.created = rfc3339(RUN_DIAG_UNVERIFIED.finished_at * 1000 + 120 * 1000)
DIAG_UNVERIFIED.explain =
  `【诊断】任务 ${RUN_DIAG_UNVERIFIED.job_id}（run ${RUN_DIAG_UNVERIFIED.run_id}）\n` +
  `结论: ${DIAG_UNVERIFIED.summary}\n` +
  `分类: ${DIAG_UNVERIFIED.category_text}  置信度: ${DIAG_UNVERIFIED.confidence}%（模型自评）  证据充分: 是\n` +
  `原因: ${DIAG_UNVERIFIED.cause}\n` +
  '证据链:\n' +
  DIAG_UNVERIFIED.evidence.map((e, i) => `  ${i + 1}. [${e.ref}] ${e.quote}\n     作用: ${e.why}`).join('\n') +
  '\n警告:\n' +
  DIAG_UNVERIFIED.warnings.map((w) => `  - ${w}`).join('\n') +
  '\n未观测到的证据定位（模型引用了但本次没观测到，不能采信）:\n' +
  DIAG_UNVERIFIED.unverified_refs.map((r) => `  - ${r}`).join('\n') +
  `\n终止原因: ${DIAG_UNVERIFIED.stop_reason}（${DIAG_UNVERIFIED.stop_reason_text}）`

/** 已存诊断（§3.1 的查询源）。键是 run_id，值是该 run 的会话与诊断。 */
export const agentDiagnoses = {
  [RUN_DIAG_FULL.run_id]: { session_id: DIAG_FULL.session_id, diagnosis: DIAG_FULL },
  [RUN_DIAG_THIN.run_id]: { session_id: DIAG_THIN.session_id, diagnosis: DIAG_THIN },
  [RUN_DIAG_UNVERIFIED.run_id]: { session_id: DIAG_UNVERIFIED.session_id, diagnosis: DIAG_UNVERIFIED },
}

/* ---------------- §5.1 草案样例（cmd / http / 五段警告） ---------------- */

const NL_BASE = Math.floor((NOW - 6 * MIN) / 1000)

const DRAFT_CMD_SPEC = '0 0 2 * * *'
const DRAFT_CMD_TIMES = dailyTriggers(2, 0, 5, NOW - 6 * MIN)

/** 草案 1：cmd（argv 模式，绝不拼接字符串） */
export const DRAFT_CMD = {
  session_id: `nl-job-${NL_BASE}-1`,
  name: '清理 /tmp 陈旧文件',
  description: '删除 /tmp 下 7 天未修改的文件，避免临时目录涨满',
  job_type: 'cmd',
  spec: DRAFT_CMD_SPEC,
  command: ['/usr/bin/find', '/tmp', '-type', 'f', '-mtime', '+7', '-delete'],
  raw_argv: true,
  http: null,
  timeout_seconds: 300,
  retry_times: 1,
  retry_interval: 60,
  concurrency_policy: 'skip',
  concurrency_policy_text: '上一次未跑完则跳过本次',
  concurrency_value: 0,
  rationale: '用户说"每天凌晨 2 点"，因此用 6 段表达式 0 0 2 * * *（秒 分 时 日 月 周）。',
  warnings: ['删除操作不可撤销：建议先去掉 -delete 跑一轮，确认文件清单无误后再创建正式任务'],
  next_triggers: DRAFT_CMD_TIMES.map((t) => rfc3339(t.getTime())),
  next_trigger_text: DRAFT_CMD_TIMES.map((t, i) => `${i + 1}) ${triggerText(t)}`),
  trigger_echo: triggerEcho(DRAFT_CMD_SPEC, DRAFT_CMD_TIMES),
  requires_confirmation: true,
  confirmed: false,
  timezone: AGENT_TZ,
  provider: 'offline',
  model: '',
  prompt_version: 'nljob-v1',
  stop_reason: 'completed',
  stop_reason_text: '正常完成',
  repairs: 0,
  duration_ms: 3120,
  total_tokens: 1420,
}

const DRAFT_HTTP_SPEC = '0 5 * * * *'
const DRAFT_HTTP_TIMES = hourlyTriggers(5, 5, NOW - 20 * MIN)

/** 草案 2：http 类型 */
export const DRAFT_HTTP = {
  session_id: `nl-job-${NL_BASE}-2`,
  name: '巡检内部健康检查接口',
  description: '每小时第 5 分钟请求一次健康检查接口，返回非 200 时记录下来',
  job_type: 'http',
  spec: DRAFT_HTTP_SPEC,
  command: [],
  raw_argv: false,
  http: { method: 'GET', url: 'http://10.20.31.11:8089/ping' },
  timeout_seconds: 30,
  retry_times: 0,
  retry_interval: 30,
  concurrency_policy: 'skip',
  concurrency_policy_text: '上一次未跑完则跳过本次',
  concurrency_value: 0,
  rationale: '用户说"每小时检查一次"，映射为每小时第 5 分钟触发（避开整点高峰）。',
  warnings: ['该地址是 crony-admin 自己的 /ping；确认这就是你要巡检的目标，否则请改成业务健康检查地址'],
  next_triggers: DRAFT_HTTP_TIMES.map((t) => rfc3339(t.getTime())),
  next_trigger_text: DRAFT_HTTP_TIMES.map((t, i) => `${i + 1}) ${triggerText(t)}`),
  trigger_echo: triggerEcho(DRAFT_HTTP_SPEC, DRAFT_HTTP_TIMES),
  requires_confirmation: true,
  confirmed: false,
  timezone: AGENT_TZ,
  provider: 'offline',
  model: '',
  prompt_version: 'nljob-v1',
  stop_reason: 'completed',
  stop_reason_text: '正常完成',
  repairs: 1,
  duration_ms: 5240,
  total_tokens: 2980,
}

/**
 * 草案 3：**五段表达式陷阱**样本。
 * 底层 cron 库把 5 段解释为「秒 分 时 日 月」，于是 '0 2 * * *' 是"每小时第 2 分钟"，
 * 而不是 Unix 的"每天 02:00"——下面的触发时间按本库真实语义算，肉眼一眼能看出不对。
 */
const DRAFT_FIVESEG_SPEC = '0 2 * * *'
const DRAFT_FIVESEG_TIMES = hourlyTriggers(2, 5, NOW - 40 * MIN)

export const DRAFT_FIVESEG = {
  session_id: `nl-job-${NL_BASE}-3`,
  name: '未命名任务',
  description: '执行日报脚本 /opt/scripts/report.sh',
  job_type: 'cmd',
  spec: DRAFT_FIVESEG_SPEC,
  command: ['/opt/scripts/report.sh'],
  raw_argv: true,
  http: null,
  timeout_seconds: 600,
  retry_times: 1,
  retry_interval: 60,
  concurrency_policy: 'skip',
  concurrency_policy_text: '上一次未跑完则跳过本次',
  concurrency_value: 0,
  rationale: '按用户原话"每天凌晨 2 点"直接采用了模型给出的表达式。',
  warnings: [
    '表达式 "0 2 * * *" 只有 5 段：本系统底层库把 5 段解释为「秒 分 时 日 月」，' +
      '不是 Unix 的「分 时 日 月 周」。请改用 6 段格式（例如每天 02:00 应写 "0 0 2 * * *"）。' +
      '下方触发时间按本库真实语义计算，请核对。',
    '模型未给出任务名，已使用占位名，请创建前修改',
  ],
  next_triggers: DRAFT_FIVESEG_TIMES.map((t) => rfc3339(t.getTime())),
  next_trigger_text: DRAFT_FIVESEG_TIMES.map((t, i) => `${i + 1}) ${triggerText(t)}`),
  trigger_echo: triggerEcho(DRAFT_FIVESEG_SPEC, DRAFT_FIVESEG_TIMES),
  requires_confirmation: true,
  confirmed: false,
  timezone: AGENT_TZ,
  provider: 'offline',
  model: '',
  prompt_version: 'nljob-v1',
  stop_reason: 'completed',
  stop_reason_text: '正常完成',
  repairs: 1,
  duration_ms: 6180,
  total_tokens: 3350,
}

/** 草案会话（§1 列表里的 nljob 会话，同时也是 §5.1 的返回体） */
export const agentDrafts = [DRAFT_CMD, DRAFT_HTTP, DRAFT_FIVESEG]

/* ---------------- §1 会话列表 ---------------- */

/** 按 run 生成一条诊断会话概要（§1 的单条字段） */
function diagSession(run, diag, { status, warnCount, evidenceCount, durationMs }) {
  const ms = run.finished_at * 1000 + 60 * 1000
  return {
    session_id: diag.session_id,
    kind: 'diagnose',
    job_id: run.job_id,
    run_id: run.run_id,
    node_uuid: run.node_uuid,
    goal: `诊断任务「${run.job_name}」（run ${run.run_id}）的失败原因`,
    status,
    stop_reason: diag.stop_reason,
    stop_reason_text: diag.stop_reason_text,
    provider: diag.provider,
    model: diag.model,
    prompt_version: diag.prompt_version,
    model_calls: 2,
    tool_calls: diag.tool_calls,
    total_tokens: diag.total_tokens,
    duration_ms: durationMs,
    warn_count: warnCount,
    evidence_count: evidenceCount,
    created: rfc3339(ms),
    created_ms: ms,
  }
}

/** 按草案生成一条 nljob 会话概要 */
function nljobSession(draft, warnCount, evidenceCount, toolCalls) {
  const ms = NOW - 6 * MIN
  return {
    session_id: draft.session_id,
    kind: 'nljob',
    job_id: 0,
    run_id: '',
    node_uuid: 'n-8f3d1c2a-01',
    goal: `把自然语言描述翻译成定时任务定义：${draft.description}`,
    status: 'completed',
    stop_reason: draft.stop_reason,
    stop_reason_text: draft.stop_reason_text,
    provider: draft.provider,
    model: draft.model,
    prompt_version: draft.prompt_version,
    model_calls: 1 + draft.repairs,
    tool_calls: toolCalls,
    total_tokens: draft.total_tokens,
    duration_ms: draft.duration_ms,
    warn_count: warnCount,
    evidence_count: evidenceCount,
    created: rfc3339(ms),
    created_ms: ms,
  }
}

/** 任务内嵌 Agent 会话概要（kind=job） */
function jobSession(run, { status, stopReason, warnCount, evidenceCount, toolCalls, tokens, durationMs, goal }) {
  const ms = run.finished_at * 1000 + 30 * 1000
  return {
    session_id: `agent-job-${run.run_id}`,
    kind: 'job',
    job_id: run.job_id,
    run_id: run.run_id,
    node_uuid: run.node_uuid,
    goal,
    status,
    stop_reason: stopReason,
    stop_reason_text: AGENT_STOP_TEXT[stopReason],
    provider: 'offline',
    model: '',
    prompt_version: 'diagnose-v1',
    model_calls: 3,
    tool_calls: toolCalls,
    total_tokens: tokens,
    duration_ms: durationMs,
    warn_count: warnCount,
    evidence_count: evidenceCount,
    created: rfc3339(ms),
    created_ms: ms,
  }
}

export const agentSessions = [
  diagSession(RUN_DIAG_FULL, DIAG_FULL, { status: 'completed', warnCount: 0, evidenceCount: 3, durationMs: 4210 }),
  diagSession(RUN_DIAG_THIN, DIAG_THIN, { status: 'completed', warnCount: 1, evidenceCount: 1, durationMs: 5120 }),
  diagSession(RUN_DIAG_UNVERIFIED, DIAG_UNVERIFIED, {
    status: 'canceled',
    warnCount: 2,
    evidenceCount: 2,
    durationMs: 3980,
  }),
  nljobSession(DRAFT_CMD, 1, 0, 1),
  nljobSession(DRAFT_HTTP, 1, 1, 1),
  nljobSession(DRAFT_FIVESEG, 2, 0, 1),
  jobSession(RUN_JOB_OK, {
    status: 'completed',
    stopReason: 'completed',
    warnCount: 0,
    evidenceCount: 3,
    toolCalls: 4,
    tokens: 3620,
    durationMs: 8600,
    goal: `在任务「${RUN_JOB_OK.job_name}」执行后给出未通过项的整改建议（只读、只建议）`,
  }),
  jobSession(RUN_JOB_BUDGET, {
    status: 'failed',
    stopReason: 'tool_budget',
    warnCount: 1,
    evidenceCount: 2,
    toolCalls: 12,
    tokens: 15800,
    durationMs: 96000,
    goal: `在任务「${RUN_JOB_BUDGET.job_name}」报出 /data 91% 后下钻目录占用`,
  }),
]

/* ---------------- §2 会话详情（轨迹回放） ---------------- */

/** 组装 §2 的完整详情：会话概要 + system/context/budget/warnings/evidence/tool_stats/steps/result */
function sessionDetail(session, extra) {
  return {
    ...session,
    system: extra.system,
    context: extra.context || [],
    budget: {
      calls: session.model_calls,
      tokens: session.total_tokens,
      duration_ms: session.duration_ms,
      ...AGENT_BUDGET_MAX,
    },
    warnings: extra.warnings || [],
    evidence: extra.evidence || [],
    tool_stats: {
      calls: session.tool_calls,
      denied: extra.denied || 0,
      truncated: extra.truncated || 0,
      remaining: Math.max(0, AGENT_MAX_TOOL_CALLS - session.tool_calls),
    },
    steps: extra.steps || [],
    result: extra.result ?? null,
  }
}

const SYSTEM_DIAGNOSE =
  '你是 crony 的失败诊断助手。只依据工具观测到的证据下结论；没有证据就明说"证据不足"并列出还需要采集什么。' +
  '每条结论必须挂上观测里出现过的证据定位（ref），不得引用未观测到的定位。'

const t0 = (run) => run.finished_at * 1000 - 4000

const DETAIL_DIAG_FULL = sessionDetail(agentSessions[0], {
  system: SYSTEM_DIAGNOSE,
  context: [
    `任务：${RUN_DIAG_FULL.job_name}（id=${RUN_DIAG_FULL.job_id}，job_type=cmd）`,
    `运行：run_id=${RUN_DIAG_FULL.run_id}，status=failed，exit_code=${RUN_DIAG_FULL.exit_code}，attempt=${RUN_DIAG_FULL.attempt}`,
    `节点：${RUN_DIAG_FULL.hostname}（${RUN_DIAG_FULL.node_uuid}）`,
    `命令：${RUN_DIAG_FULL.command}`,
    `输出尾部：${RUN_DIAG_FULL.output}`,
    `错误：${RUN_DIAG_FULL.error_msg}`,
  ],
  evidence: [
    {
      source: 'log',
      ref: 'node.log:12',
      quote: 'mysqlslow: connect to 10.20.31.51:3306 timeout after 30000ms',
    },
    { source: 'step', ref: `${RUN_DIAG_FULL.run_id}#step-2`, quote: '步骤 2 退出码 2，已重试 2 次' },
    { source: 'node', ref: 'node:n-7be2a904-01', quote: 'node-db-01 conn=NodeConnFail，down=23m' },
  ],
  steps: [
    modelStep(1, t0(RUN_DIAG_FULL), {
      prompt_preview: '你是 crony 的失败诊断助手……（任务上下文 + 可用只读工具清单）',
      prompt_bytes: 2140,
      content: '',
      tool_calls: [{ id: 'call_log_1', name: 'log_search', arguments: '{"run_id":"' + RUN_DIAG_FULL.run_id + '","level":"error"}' }],
      total_tokens: 880,
      latency_ms: 1180,
      finish_reason: 'tool_calls',
    }),
    toolStep(2, RUN_DIAG_FULL.finished_at * 1000 - 2800, {
      tool_name: 'log_search',
      tool_args: { run_id: RUN_DIAG_FULL.run_id, level: 'error' },
      observation:
        '命中 2 条\nnode.log:12 mysqlslow: connect to 10.20.31.51:3306 timeout after 30000ms\n' +
        'node.log:13 retry 2/2 failed, giving up',
      evidence: [
        {
          source: 'log',
          ref: 'node.log:12',
          quote: 'mysqlslow: connect to 10.20.31.51:3306 timeout after 30000ms',
        },
      ],
      duration_ms: 31,
    }),
    modelStep(3, RUN_DIAG_FULL.finished_at * 1000 - 900, {
      prompt_preview: '（第二轮：附上 log_search 观测）你是 crony 的失败诊断助手……',
      prompt_bytes: 3720,
      content: '',
      tool_calls: [],
      total_tokens: 954,
      latency_ms: 1520,
      finish_reason: 'stop',
    }),
  ],
  result: DIAG_FULL,
})

const DETAIL_DIAG_THIN = sessionDetail(agentSessions[1], {
  system: SYSTEM_DIAGNOSE,
  context: [
    `任务：${RUN_DIAG_THIN.job_name}（id=${RUN_DIAG_THIN.job_id}，job_type=cmd）`,
    `运行：run_id=${RUN_DIAG_THIN.run_id}，status=timeout，exit_code=${RUN_DIAG_THIN.exit_code}`,
    `命令：${RUN_DIAG_THIN.command}`,
    `输出尾部：${RUN_DIAG_THIN.output}`,
  ],
  warnings: DIAG_THIN.warnings,
  evidence: [
    {
      source: 'log',
      ref: `run:${RUN_DIAG_THIN.run_id}`,
      quote: '超过 timeout=1800s 被强制终止（pg_dumpall 等待锁释放）',
    },
  ],
  steps: [
    modelStep(1, t0(RUN_DIAG_THIN), {
      prompt_preview: '你是 crony 的失败诊断助手……（超时类失败）',
      prompt_bytes: 2050,
      content: '',
      tool_calls: [{ id: 'call_log_2', name: 'log_search', arguments: '{"job_id":' + RUN_DIAG_THIN.job_id + ',"level":"warn"}' }],
      total_tokens: 1020,
      latency_ms: 1260,
      finish_reason: 'tool_calls',
    }),
    toolStep(2, RUN_DIAG_THIN.finished_at * 1000 - 3000, {
      tool_name: 'log_search',
      tool_args: { job_id: RUN_DIAG_THIN.job_id, level: 'warn' },
      observation: '命中 1 条\ncrony.log:241 step 3 (pg_dumpall) still waiting for lock on public.audit_log',
      evidence: [
        {
          source: 'log',
          ref: 'crony.log:241',
          quote: 'step 3 (pg_dumpall) still waiting for lock on public.audit_log',
        },
      ],
      duration_ms: 27,
    }),
    modelStep(3, RUN_DIAG_THIN.finished_at * 1000 - 1200, {
      prompt_preview: '（第二轮：附上 log_search 观测）……',
      prompt_bytes: 3510,
      content: '',
      tool_calls: [],
      total_tokens: 1086,
      latency_ms: 1380,
      finish_reason: 'stop',
    }),
  ],
  result: DIAG_THIN,
})

const DETAIL_DIAG_UNVERIFIED = sessionDetail(agentSessions[2], {
  system: SYSTEM_DIAGNOSE,
  context: [
    `任务：${RUN_DIAG_UNVERIFIED.job_name}（id=${RUN_DIAG_UNVERIFIED.job_id}）`,
    `运行：run_id=${RUN_DIAG_UNVERIFIED.run_id}，trigger_source=self_heal，status=canceled`,
    `错误：${RUN_DIAG_UNVERIFIED.error_msg}`,
    `输出尾部：${RUN_DIAG_UNVERIFIED.output}`,
  ],
  warnings: DIAG_UNVERIFIED.warnings,
  evidence: [
    { source: 'log', ref: `run:${RUN_DIAG_UNVERIFIED.run_id}`, quote: '运维人员手动取消（审计单 AUD-2201）' },
    { source: 'log', ref: 'crony.log:88', quote: 'step 1 finished: removed 210 files' },
  ],
  steps: [
    modelStep(1, t0(RUN_DIAG_UNVERIFIED), {
      prompt_preview: '你是 crony 的失败诊断助手……（取消类记录）',
      prompt_bytes: 1990,
      content: '',
      tool_calls: [{ id: 'call_log_3', name: 'log_search', arguments: '{"run_id":"' + RUN_DIAG_UNVERIFIED.run_id + '"}' }],
      total_tokens: 860,
      latency_ms: 1120,
      finish_reason: 'tool_calls',
    }),
    toolStep(2, RUN_DIAG_UNVERIFIED.finished_at * 1000 - 2600, {
      tool_name: 'log_search',
      tool_args: { run_id: RUN_DIAG_UNVERIFIED.run_id },
      observation: '命中 2 条\ncrony.log:88 step 1 finished: removed 210 files\ncrony.log:90 canceled by operator (AUD-2201)',
      evidence: [{ source: 'log', ref: 'crony.log:88', quote: 'step 1 finished: removed 210 files' }],
      duration_ms: 24,
    }),
    // 这一条 notice 是"修复轮"的真实记录：模型输出了未观测到的定位，被本地校验打回重写
    noticeStep(3, RUN_DIAG_UNVERIFIED.finished_at * 1000 - 1800, '输出未通过结构校验，已发起第 1 次修复：$.evidence[2].ref "syslog:999" 不在本次观测集合内'),
    modelStep(4, RUN_DIAG_UNVERIFIED.finished_at * 1000 - 1400, {
      prompt_preview: '（修复轮：请只引用观测里出现过的 ref）……',
      prompt_bytes: 3110,
      content: '',
      tool_calls: [],
      total_tokens: 900,
      latency_ms: 1310,
      finish_reason: 'stop',
    }),
  ],
  result: DIAG_UNVERIFIED,
})

const DETAIL_NLJOB_CMD = sessionDetail(agentSessions[3], {
  system:
    '你是 crony 的任务翻译助手。把用户的口语描述翻译成字段完整的任务定义；cron 表达式必须符合本系统的 6 段格式' +
    '（秒 分 时 日 月 周），命令一律输出 argv 数组，不要拼接 shell 字符串。',
  context: [`用户原话：每天凌晨 2 点清理 /tmp 下 7 天前的文件`, '目标节点：n-8f3d1c2a-01'],
  warnings: DRAFT_CMD.warnings,
  evidence: [],
  steps: [
    modelStep(1, NOW - 6 * MIN - 3200, {
      prompt_preview: '你是 crony 的任务翻译助手……（用户原话 + 节点白名单）',
      prompt_bytes: 1680,
      content: '',
      tool_calls: [{ id: 'call_node_1', name: 'node_status', arguments: '{"node":"n-8f3d1c2a-01"}' }],
      total_tokens: 700,
      latency_ms: 980,
      finish_reason: 'tool_calls',
    }),
    toolStep(2, NOW - 6 * MIN - 2400, {
      tool_name: 'node_status',
      tool_args: { node: 'n-8f3d1c2a-01' },
      observation: 'node-prod-01 status=online up=6h disk=73% mem=61% jobs=5',
      evidence: [{ source: 'node', ref: 'node:n-8f3d1c2a-01', quote: 'status=online disk=73%' }],
      duration_ms: 12,
    }),
    modelStep(3, NOW - 6 * MIN - 900, {
      prompt_preview: '（第二轮：附上节点状态）……请输出 JSON 任务定义',
      prompt_bytes: 2980,
      content: '',
      tool_calls: [],
      total_tokens: 720,
      latency_ms: 1240,
      finish_reason: 'stop',
    }),
  ],
  result: DRAFT_CMD,
})

const DETAIL_NLJOB_HTTP = sessionDetail(agentSessions[4], {
  system:
    '你是 crony 的任务翻译助手。把用户的口语描述翻译成字段完整的任务定义；' +
    'http 类型必须给出 method 与完整 URL，不要编造不存在的域名。',
  context: ['用户原话：每小时检查一次内部健康检查接口', '目标节点：n-8f3d1c2a-01'],
  warnings: DRAFT_HTTP.warnings,
  evidence: [
    { source: 'http', ref: 'http://10.20.31.11:8089/ping', quote: '200 pong' },
  ],
  steps: [
    modelStep(1, NOW - 20 * MIN - 5200, {
      prompt_preview: '你是 crony 的任务翻译助手……（http 类任务）',
      prompt_bytes: 1720,
      content: '',
      tool_calls: [{ id: 'call_http_1', name: 'http_get', arguments: '{"url":"http://10.20.31.11:8089/ping"}' }],
      total_tokens: 760,
      latency_ms: 1040,
      finish_reason: 'tool_calls',
    }),
    toolStep(2, NOW - 20 * MIN - 4200, {
      tool_name: 'http_get',
      tool_args: { url: 'http://10.20.31.11:8089/ping' },
      observation: '状态: 成功\n摘要: HTTP 200\n证据:\n- [http] http://10.20.31.11:8089/ping 「200 pong」',
      evidence: [{ source: 'http', ref: 'http://10.20.31.11:8089/ping', quote: '200 pong' }],
      duration_ms: 46,
    }),
    noticeStep(3, NOW - 20 * MIN - 3000, '第 1 次起草不可用：$.spec "0 * * * *" 只有 5 段，无法按本库语义解释（已自动重试）'),
    modelStep(4, NOW - 20 * MIN - 1500, {
      prompt_preview: '（修复轮：请改用 6 段表达式）……',
      prompt_bytes: 3260,
      content: '',
      tool_calls: [],
      total_tokens: 1040,
      latency_ms: 1480,
      finish_reason: 'stop',
    }),
  ],
  result: DRAFT_HTTP,
})

const DETAIL_NLJOB_FIVESEG = sessionDetail(agentSessions[5], {
  system:
    '你是 crony 的任务翻译助手。把用户的口语描述翻译成字段完整的任务定义；' +
    '注意本系统的 cron 是 6 段（秒 分 时 日 月 周），5 段会被解释成完全不同的时间。',
  context: ['用户原话：每天凌晨 2 点执行日报脚本 /opt/scripts/report.sh'],
  warnings: DRAFT_FIVESEG.warnings,
  evidence: [],
  steps: [
    modelStep(1, NOW - 40 * MIN - 6400, {
      prompt_preview: '你是 crony 的任务翻译助手……（用户原话）',
      prompt_bytes: 1650,
      content: '',
      tool_calls: [{ id: 'call_shell_1', name: 'shell_readonly', arguments: '{"command":"ls","args":["/opt/scripts"]}' }],
      total_tokens: 820,
      latency_ms: 1180,
      finish_reason: 'tool_calls',
    }),
    toolStep(2, NOW - 40 * MIN - 5200, {
      tool_name: 'shell_readonly',
      tool_args: { command: 'ls', args: ['/opt/scripts'] },
      observation: 'report.sh\ncleanup.sh',
      evidence: [{ source: 'shell', ref: 'shell:ls /opt/scripts', quote: 'report.sh' }],
      duration_ms: 18,
    }),
    noticeStep(3, NOW - 40 * MIN - 3600, '第 1 次起草不可用：$.spec "0 2 * * *" 只有 5 段，按本库语义是"每小时第 2 分钟"，与用户意图不符（已自动重试）'),
    modelStep(4, NOW - 40 * MIN - 1800, {
      prompt_preview: '（修复轮：请改用 6 段表达式；若无法确定就保留原式并给出警告）……',
      prompt_bytes: 3420,
      content: '',
      tool_calls: [],
      total_tokens: 1210,
      latency_ms: 1620,
      finish_reason: 'stop',
    }),
  ],
  result: DRAFT_FIVESEG,
})

const DETAIL_JOB_OK = sessionDetail(agentSessions[6], {
  system: '你是 crony 的巡检结果解读助手。只依据任务输出与只读工具观测给建议，不执行任何变更。',
  context: [
    `任务：${RUN_JOB_OK.job_name}（id=${RUN_JOB_OK.job_id}）`,
    `输出：${RUN_JOB_OK.output}`,
  ],
  evidence: [
    { source: 'log', ref: `run:${RUN_JOB_OK.run_id}`, quote: '基线检查 68 项：通过 65，未通过 3' },
    { source: 'sql', ref: 'mysql_query_readonly:LIMIT 3', quote: '3 条未通过项（SSH 空口令登录 / 内核参数 / 日志权限）' },
    { source: 'node', ref: 'node:n-8f3d1c2a-01', quote: 'status=online disk=73%' },
  ],
  steps: [
    modelStep(1, t0(RUN_JOB_OK), {
      prompt_preview: '你是 crony 的巡检结果解读助手……（基线巡检输出）',
      prompt_bytes: 2260,
      content: '',
      tool_calls: [{ id: 'call_sql_1', name: 'mysql_query_readonly', arguments: '{"query":"SELECT item,result FROM audit_item WHERE run_id=\'...\' LIMIT 3"}' }],
      total_tokens: 1140,
      latency_ms: 1420,
      finish_reason: 'tool_calls',
    }),
    toolStep(2, RUN_JOB_OK.finished_at * 1000 - 4200, {
      tool_name: 'mysql_query_readonly',
      tool_args: { query: "SELECT item,result FROM audit_item WHERE run_id='...' LIMIT 3" },
      observation: '3 行\nSSH 空口令登录: fail\n内核参数 net.ipv4.tcp_syncookies: fail\n/var/log/app 权限 0777: fail',
      evidence: [{ source: 'sql', ref: 'mysql_query_readonly:LIMIT 3', quote: '3 条未通过项（SSH 空口令登录 / 内核参数 / 日志权限）' }],
      duration_ms: 38,
    }),
    modelStep(3, RUN_JOB_OK.finished_at * 1000 - 2400, {
      prompt_preview: '（第二轮：附上未通过项）……',
      prompt_bytes: 4020,
      content: '',
      tool_calls: [{ id: 'call_node_2', name: 'node_status', arguments: '{"node":"n-8f3d1c2a-01"}' }],
      total_tokens: 1180,
      latency_ms: 1360,
      finish_reason: 'tool_calls',
    }),
    toolStep(4, RUN_JOB_OK.finished_at * 1000 - 1400, {
      tool_name: 'node_status',
      tool_args: { node: 'n-8f3d1c2a-01' },
      observation: 'node-prod-01 status=online up=6h disk=73% mem=61% jobs=5',
      evidence: [{ source: 'node', ref: 'node:n-8f3d1c2a-01', quote: 'status=online disk=73%' }],
      duration_ms: 11,
    }),
    modelStep(5, RUN_JOB_OK.finished_at * 1000 - 500, {
      prompt_preview: '（第三轮：给出整改优先级建议，不执行）……',
      prompt_bytes: 4480,
      content: '',
      tool_calls: [],
      total_tokens: 1300,
      latency_ms: 1520,
      finish_reason: 'stop',
    }),
  ],
  result: null,
})

const DETAIL_JOB_BUDGET = sessionDetail(agentSessions[7], {
  system: '你是 crony 的巡检结果解读助手。只依据任务输出与只读工具观测给建议，不执行任何变更。',
  context: [
    `任务：${RUN_JOB_BUDGET.job_name}（id=${RUN_JOB_BUDGET.job_id}）`,
    `输出：${RUN_JOB_BUDGET.output}`,
  ],
  warnings: [
    '达到工具调用次数上限（12 次/会话）：下钻目录占用时反复尝试同一查询，已按保护性策略终止',
  ],
  evidence: [
    { source: 'log', ref: `run:${RUN_JOB_BUDGET.run_id}`, quote: '/data 91% /var 88% —— 已上报告警（2 条）' },
    { source: 'shell', ref: 'shell:df -h /data', quote: '/data 91%' },
  ],
  steps: [
    modelStep(1, t0(RUN_JOB_BUDGET), {
      prompt_preview: '你是 crony 的巡检结果解读助手……（磁盘告警输出）',
      prompt_bytes: 2180,
      content: '',
      tool_calls: [{ id: 'call_shell_2', name: 'shell_readonly', arguments: '{"command":"df","args":["-h","/data"]}' }],
      total_tokens: 980,
      latency_ms: 1180,
      finish_reason: 'tool_calls',
    }),
    toolStep(2, RUN_JOB_BUDGET.finished_at * 1000 - 80000, {
      tool_name: 'shell_readonly',
      tool_args: { command: 'df', args: ['-h', '/data'] },
      observation: 'Filesystem Size Used Avail Use% Mounted on\n/dev/vdb1 500G 455G 45G 91% /data',
      evidence: [{ source: 'shell', ref: 'shell:df -h /data', quote: '/data 91%' }],
      duration_ms: 22,
    }),
    noticeStep(3, RUN_JOB_BUDGET.finished_at * 1000 - 20000, '达到工具调用次数上限（12 次/会话），已按保护性策略终止本次会话'),
  ],
  result: null,
})

/** §2 会话详情表（键为 session_id）；未列出的会话由 index.js 返回兜底详情 */
export const agentSessionDetails = {
  [DETAIL_DIAG_FULL.session_id]: DETAIL_DIAG_FULL,
  [DETAIL_DIAG_THIN.session_id]: DETAIL_DIAG_THIN,
  [DETAIL_DIAG_UNVERIFIED.session_id]: DETAIL_DIAG_UNVERIFIED,
  [DETAIL_NLJOB_CMD.session_id]: DETAIL_NLJOB_CMD,
  [DETAIL_NLJOB_HTTP.session_id]: DETAIL_NLJOB_HTTP,
  [DETAIL_NLJOB_FIVESEG.session_id]: DETAIL_NLJOB_FIVESEG,
  [DETAIL_JOB_OK.session_id]: DETAIL_JOB_OK,
  [DETAIL_JOB_BUDGET.session_id]: DETAIL_JOB_BUDGET,
}

/* ---------------- §4 只读工具清单 ---------------- */

/**
 * 工具名与描述取自 common/pkg/tools/builtin.go（逐字对应，不要美化）。
 * 注意：真实部署里数据源为 nil 的工具**不会注册**（NewReadonlyRegistry），
 * 因此这个清单是"上限"；实际以 /agent/tool/list 的返回为准。
 */
export const agentTools = [
  {
    name: 'shell_readonly',
    description:
      '在 crony 节点上执行**只读**诊断命令并返回输出。允许的命令：df、ps、tail、journalctl' +
      '（df=磁盘使用率，ps=进程列表，tail=看日志尾部，journalctl=systemd 日志）。' +
      '不接受管道、重定向或其他命令；tail 只能读取 /var/log 下的文件。',
    readonly: true,
    risk: 'read',
    timeout_ms: 15000,
    schema: {
      type: 'object',
      properties: {
        command: { type: 'string', description: '命令名' },
        args: { type: 'array', items: { type: 'string' }, description: '参数' },
      },
      required: ['command'],
      additionalProperties: false,
    },
  },
  {
    name: 'http_get',
    description: '发起只读 HTTP GET 请求做健康检查（当前未配置 AllowHosts，**所有请求都会被拒绝**）',
    readonly: true,
    risk: 'read',
    timeout_ms: 15000,
    schema: {
      type: 'object',
      properties: { url: { type: 'string', description: '目标地址' } },
      required: ['url'],
      additionalProperties: false,
    },
  },
  {
    name: 'node_status',
    description: '查询 crony 节点的在线状态、心跳时间、正在运行的任务数与资源水位（CPU/内存/磁盘）。',
    readonly: true,
    risk: 'read',
    timeout_ms: 5000,
    schema: {
      type: 'object',
      properties: { node: { type: 'string', description: '节点 UUID；留空表示查询当前节点' } },
      additionalProperties: false,
    },
  },
  {
    name: 'log_search',
    description: '按关键字/任务/运行 ID 检索任务日志。至少给出 keyword、job_id、run_id 之一，避免全量拉取。',
    readonly: true,
    risk: 'read',
    timeout_ms: 10000,
    schema: {
      type: 'object',
      properties: {
        keyword: { type: 'string', description: '关键字（子串匹配）' },
        job_id: { type: 'integer', description: '任务 ID' },
        run_id: { type: 'string', description: '运行 ID（形如 j12-t1791708060）' },
        level: { type: 'string', enum: ['debug', 'info', 'warn', 'error'] },
      },
      additionalProperties: false,
    },
  },
  {
    name: 'mysql_query_readonly',
    description:
      '对 crony 元数据库执行**只读 SELECT** 查询（必须带 LIMIT，且不超过 100 行）。' +
      '禁止任何写操作、DDL、注释与多语句。',
    readonly: true,
    risk: 'read',
    timeout_ms: 10000,
    schema: {
      type: 'object',
      properties: { query: { type: 'string', description: '单条 SELECT 语句，必须以 LIMIT n 结尾' } },
      required: ['query'],
      additionalProperties: false,
    },
  },
  {
    name: 'node_probe',
    description:
      '在**指定节点**上执行只读诊断命令（端口监听、进程、服务状态、网络配置）。' +
      'admin 只能看到数据库，看不到节点本机事实；要断言"端口未监听/服务未运行"这类结论，必须先实际探测拿到证据。',
    readonly: true,
    risk: 'read',
    timeout_ms: 20000,
    schema: {
      type: 'object',
      properties: {
        node: { type: 'string', description: '目标节点 UUID' },
        command: { type: 'string', description: '只读命令名' },
        args: { type: 'array', items: { type: 'string' }, description: '参数列表' },
        reason: { type: 'string', description: '为什么要探测（写进审计）' },
      },
      required: ['node', 'command'],
      additionalProperties: false,
    },
  },
]

/**
 * tools.Policy 的默认值 + 当前生效的收窄项。
 *
 * 阶段 4 起策略多了"风险开关"：默认一律是**关着**的那一档。
 * 这些字段不是装饰——控制台的"能力边界"面板直接读它们，
 * 因此值必须与后端 toolPolicyView 的字段名逐字一致。
 */
export const agentToolPolicy = {
  max_calls_per_session: AGENT_MAX_TOOL_CALLS,
  max_calls_per_tool: 4,
  max_output_bytes: 65536,
  sql_max_rows: 100,
  allow_hosts: [],
  shell_read_paths: ['/var/log/', '/var/log/crony/'],
  allow_write: false,
  allow_destructive: false,
  autonomy: 'L1',
  auto_approve: [],
  approval_ttl_seconds: 900,
  max_calls_per_minute: 60,
  max_concurrent_calls: 4,
  max_write_actions_per_session: 2,
  max_affected_hosts_per_session: 1,
  default_timeout_ms: 15000,
  deny: [],
}

/* ------------------------------------------------------------------ *
 * 6. 知识库（规划中：阶段 2 接入向量检索）
 * ------------------------------------------------------------------ */

export const knowledgeDocs = [
  {
    id: 'KB-1001',
    title: 'MySQL 慢查询处置手册',
    summary: '从慢查询采集、索引建议到回滚的完整处置流程，含 12 个真实案例。',
    tags: ['数据库', '性能', 'Runbook'],
    type: 'runbook',
    source: '人工编写',
    chunks: 86,
    updated: isoAgo(2 * 24 * HOUR),
    owner: 'admin',
    status: 'published',
    hit_count: 342,
  },
  {
    id: 'KB-1002',
    title: '容器 unhealthy 自愈 Runbook',
    summary: '容器健康检查失败时的自愈动作序列：隔离 → 重启 → 回滚 → 通知。',
    tags: ['容器', '自愈', 'Runbook'],
    type: 'runbook',
    source: 'Agent 生成 + 人工复核',
    chunks: 54,
    updated: isoAgo(5 * HOUR),
    owner: 'agent-kb-01',
    status: 'published',
    hit_count: 208,
  },
  {
    id: 'KB-1003',
    title: '磁盘水位告警响应 SOP',
    summary: '>85% 的响应路径：定位大文件、清理策略、扩容申请、复盘要点。',
    tags: ['存储', 'SOP', '告警'],
    type: 'sop',
    source: '人工编写',
    chunks: 41,
    updated: isoAgo(9 * HOUR),
    owner: 'admin',
    status: 'published',
    hit_count: 176,
  },
  {
    id: 'KB-1004',
    title: 'PostgreSQL 备份恢复演练记录',
    summary: '最近 6 次全量备份恢复演练的耗时、失败点与改进项。',
    tags: ['数据库', '备份', '复盘'],
    type: 'review',
    source: 'Agent 生成',
    chunks: 33,
    updated: isoAgo(1 * 24 * HOUR + 3 * HOUR),
    owner: 'agent-report-01',
    status: 'published',
    hit_count: 98,
  },
  {
    id: 'KB-1005',
    title: 'Nginx reload 失败回滚步骤',
    summary: '配置校验失败与 reload 半成功的两条回滚路径，含 systemd 单元操作。',
    tags: ['中间件', '回滚', 'Runbook'],
    type: 'runbook',
    source: '人工编写',
    chunks: 27,
    updated: isoAgo(6 * 24 * HOUR),
    owner: 'admin',
    status: 'published',
    hit_count: 121,
  },
  {
    id: 'KB-1006',
    title: 'Kubernetes 节点 NotReady 排查树',
    summary: '按 kubelet / 网络 / 磁盘 / 证书四类分支的排查决策树。',
    tags: ['Kubernetes', '故障排查', '决策树'],
    type: 'decision_tree',
    source: 'Agent 生成 + 人工复核',
    chunks: 62,
    updated: isoAgo(4 * HOUR),
    owner: 'agent-rca-01',
    status: 'draft',
    hit_count: 45,
  },
  {
    id: 'KB-1007',
    title: '并发策略选择指南（Skip / Queue / Forbid）',
    summary: '三种并发策略的适用场景与反例，直接对应 job 的 concurrency_policy 字段。',
    tags: ['调度', '并发', '设计'],
    type: 'guide',
    source: '人工编写',
    chunks: 18,
    updated: isoAgo(3 * 24 * HOUR + 6 * HOUR),
    owner: 'admin',
    status: 'published',
    hit_count: 87,
  },
  {
    id: 'KB-1008',
    title: '等保 L2 基线检查项说明',
    summary: '68 项检查项的判定口径、修复命令与常见误报。',
    tags: ['安全', '合基', '基线'],
    type: 'guide',
    source: '人工编写',
    chunks: 74,
    updated: isoAgo(7 * 24 * HOUR),
    owner: 'admin',
    status: 'published',
    hit_count: 64,
  },
]

/* ------------------------------------------------------------------ *
 * 7. 审计日志（规划中：阶段 1-3，待后端 /audit/* 落地）
 * ------------------------------------------------------------------ */

export const auditLogs = [
  {
    id: 'AUD-2207',
    time: isoAgo(6 * MIN),
    operator: 'agent-guard-01',
    operator_type: 'agent',
    action: 'baseline.scan',
    action_text: '执行安全基线巡检',
    target: 'node-prod-01',
    target_type: 'node',
    result: 'success',
    detail: '68 项检查：通过 65，未通过 3',
    ip: '10.20.31.11',
    trace_id: 'tr-b7c9e1d402',
  },
  {
    id: 'AUD-2206',
    time: isoAgo(15 * MIN),
    operator: 'scheduler',
    operator_type: 'system',
    action: 'job.skip',
    action_text: '因并发策略 Skip 跳过本轮执行',
    target: '容器健康检查 (#3)',
    target_type: 'job',
    result: 'skipped',
    detail: '上一轮 run 未结束',
    ip: '10.20.31.13',
    trace_id: 'tr-5d90c7a3f2',
  },
  {
    id: 'AUD-2205',
    time: isoAgo(30 * MIN),
    operator: 'agent-observe-01',
    operator_type: 'agent',
    action: 'job.once',
    action_text: 'Agent 手动触发一次性执行',
    target: 'Kubernetes 节点排水检查 (#8)',
    target_type: 'job',
    result: 'success',
    detail: '触发来源 trigger_source=5 (agent)',
    ip: '10.20.31.13',
    trace_id: 'tr-a0d5e77f31',
  },
  {
    id: 'AUD-2204',
    time: isoAgo(42 * MIN),
    operator: 'scheduler',
    operator_type: 'system',
    action: 'job.failed',
    action_text: '任务执行失败',
    target: 'MySQL 慢查询巡检 (#2)',
    target_type: 'job',
    result: 'failed',
    detail: 'connect timeout，重试 2 次后放弃',
    ip: '10.20.31.12',
    trace_id: 'tr-2ab88de110',
  },
  {
    id: 'AUD-2203',
    time: isoAgo(3 * HOUR + 12 * MIN),
    operator: 'scheduler',
    operator_type: 'system',
    action: 'job.success',
    action_text: '任务执行成功',
    target: '清理 90 天前日志 (#1)',
    target_type: 'job',
    result: 'success',
    detail: '清理 1284 个文件，释放 3.7 GiB',
    ip: '10.20.31.11',
    trace_id: 'tr-7c1f9a2e4b',
  },
  {
    id: 'AUD-2202',
    time: isoAgo(5 * HOUR + 20 * MIN),
    operator: 'admin',
    operator_type: 'user',
    action: 'job.once',
    action_text: '手动触发一次性执行',
    target: 'MySQL 慢查询巡检 (#2)',
    target_type: 'job',
    result: 'success',
    detail: 'run_id=j2-m…',
    ip: '10.20.44.7',
    trace_id: 'tr-33a1f0d9bb',
  },
  {
    id: 'AUD-2201',
    time: isoAgo(26 * HOUR),
    operator: 'admin',
    operator_type: 'user',
    action: 'job.kill',
    action_text: '取消正在执行的 run',
    target: '清理 90 天前日志 (#1)',
    target_type: 'run',
    result: 'success',
    detail: 'SIGINT 已传播到进程组',
    ip: '10.20.44.7',
    trace_id: 'tr-0e83ff2a19',
  },
  {
    id: 'AUD-2200',
    time: isoAgo(28 * HOUR),
    operator: 'admin',
    operator_type: 'user',
    action: 'user.login',
    action_text: '登录控制台',
    target: 'admin',
    target_type: 'user',
    result: 'success',
    detail: 'JWT 签发成功',
    ip: '10.20.44.7',
    trace_id: '-',
  },
  {
    id: 'AUD-2199',
    time: isoAgo(30 * HOUR),
    operator: 'unknown',
    operator_type: 'anonymous',
    action: 'user.login',
    action_text: '登录失败',
    target: 'root',
    target_type: 'user',
    result: 'failed',
    detail: '口令错误（连续第 3 次）',
    ip: '203.0.113.44',
    trace_id: '-',
  },
  {
    id: 'AUD-2198',
    time: isoAgo(2 * 24 * HOUR),
    operator: 'admin',
    operator_type: 'user',
    action: 'job.add',
    action_text: '新建定时任务',
    target: '安全基线巡检 (#12)',
    target_type: 'job',
    result: 'success',
    detail: 'spec=0 2 * * 1',
    ip: '10.20.44.7',
    trace_id: '-',
  },
  {
    id: 'AUD-2197',
    time: isoAgo(2 * 24 * HOUR + 40 * MIN),
    operator: 'admin',
    operator_type: 'user',
    action: 'user.change_pw',
    action_text: '修改自身口令',
    target: 'admin',
    target_type: 'user',
    result: 'success',
    detail: '口令强度校验通过',
    ip: '10.20.44.7',
    trace_id: '-',
  },
  {
    id: 'AUD-2196',
    time: isoAgo(3 * 24 * HOUR),
    operator: 'admin',
    operator_type: 'user',
    action: 'node.del',
    action_text: '删除节点',
    target: 'node-old-07',
    target_type: 'node',
    result: 'forbidden',
    detail: 'RBAC 拦截：需要管理员令牌（实际已具备，示例为越权尝试）',
    ip: '203.0.113.44',
    trace_id: '-',
  },
]

/* ------------------------------------------------------------------ *
 * 8. 概览与健康（规划中：GET /statis/overview、GET /statis/health）
 * ------------------------------------------------------------------ */

export const overview = {
  kpi: {
    total_jobs: 12,
    enabled_jobs: 11,
    online_nodes: 5,
    total_nodes: 6,
    agents_online: 4,
    agents_total: 6,
    runs_today: 184,
    success_rate_today: 96.2,
    failed_today: 5,
    timeout_today: 1,
    skipped_today: 2,
    avg_duration_ms: 8420,
    kb_docs: 8,
    pending_approvals: 1,
  },
  // 迷你趋势图数据（近 12 个时间片）
  trends: {
    runs_today: [12, 9, 14, 18, 16, 21, 19, 24, 22, 18, 15, 12],
    success_rate_today: [94, 96, 95, 97, 98, 96, 93, 95, 97, 96, 98, 96],
    runs_failed: [1, 0, 2, 1, 0, 1, 2, 1, 0, 1, 1, 0],
    avg_duration: [7.2, 8.1, 6.4, 9.2, 8.8, 7.5, 0, 8.4, 9.1, 7.7, 8.2, 8.4],
  },
  servers: {
    total: 6,
    online: 5,
    offline: 1,
    // 观测站基线：Dashboard 的 "Servers" 卡片在未接入真实节点时展示空状态
    onboarded: 0,
  },
  recent_tasks: runs.slice(0, 6),
}

export const health = {
  status: 'degraded',
  checked_at: new Date().toISOString(),
  components: [
    { name: 'crony-admin (API)', status: 'ok', latency_ms: 12, detail: 'v1.4.2 · :8089' },
    { name: 'MySQL', status: 'ok', latency_ms: 4, detail: '10.20.31.51:3306 · 连接池 8/64' },
    { name: 'crony-node 集群', status: 'degraded', latency_ms: 0, detail: '5/6 在线，node-db-01 失联 23m' },
    { name: '调度器', status: 'ok', latency_ms: 1, detail: 'leader=node-prod-01' },
    { name: '通知渠道（钉钉/邮件）', status: 'ok', latency_ms: 88, detail: '最近投递 30s 前成功' },
    { name: 'Agent 运行时', status: 'planned', latency_ms: 0, detail: '阶段 2 交付，当前为 Mock' },
  ],
}

/* ------------------------------------------------------------------ *
 * 9. 告警与快捷操作（Dashboard 用）
 * ------------------------------------------------------------------ */

export const alerts = [
  {
    id: 'AL-501',
    level: 'critical',
    title: 'node-edge-01 /data 磁盘使用率 91%',
    source: '磁盘水位巡检',
    node: 'node-edge-01',
    time: isoAgo(2 * HOUR),
    status: 'firing',
    agent: 'agent-observe-01',
  },
  {
    id: 'AL-500',
    level: 'critical',
    title: 'MySQL 慢查询巡检连续 2 次连接超时',
    source: 'MySQL 慢查询巡检',
    node: 'node-prod-02',
    time: isoAgo(42 * MIN),
    status: 'firing',
    agent: 'agent-rca-01',
  },
  {
    id: 'AL-499',
    level: 'warning',
    title: 'node-db-01 心跳丢失超过 20 分钟',
    source: '节点心跳',
    node: 'node-db-01',
    time: isoAgo(23 * MIN),
    status: 'firing',
    agent: 'agent-observe-01',
  },
  {
    id: 'AL-498',
    level: 'warning',
    title: 'PostgreSQL 全量备份超时，未生成完整备份',
    source: '备份 PostgreSQL 全量',
    node: 'node-prod-02',
    time: isoAgo(10 * HOUR + 30 * MIN),
    status: 'ack',
    agent: 'agent-remediate-01',
  },
  {
    id: 'AL-497',
    level: 'info',
    title: '安全基线巡检发现 3 项未通过（L2）',
    source: '安全基线巡检',
    node: 'node-prod-01',
    time: isoAgo(2 * 24 * HOUR),
    status: 'resolved',
    agent: 'agent-guard-01',
  },
  {
    id: 'AL-496',
    level: 'warning',
    title: '容器 healthcheck 失败 3 次，已自动重启',
    source: '容器健康检查',
    node: 'node-prod-03',
    time: isoAgo(6 * HOUR),
    status: 'resolved',
    agent: 'agent-remediate-01',
  },
]

export const quickActions = [
  { key: 'run-job', label: '立即执行任务', desc: '选择已分配的任务并手动触发一次', icon: 'VideoPlay', to: '/jobs' },
  { key: 'new-job', label: '新建定时任务', desc: '定义 cron 表达式、目标节点与重试策略', icon: 'Plus', to: '/jobs' },
  { key: 'runs', label: '查看执行记录', desc: '按状态/触发来源检索 job_run 与步骤', icon: 'List', to: '/runs' },
  { key: 'terminal', label: '打开终端', desc: '进入节点交互式终端（阶段 2 接真实 PTY）', icon: 'Monitor', to: '/terminal' },
  { key: 'agent', label: '下发 Agent 任务', desc: '用自然语言描述运维目标交给 Agent', icon: 'MagicStick', to: '/agents' },
  { key: 'kb', label: '检索知识库', desc: '查询 Runbook / SOP / 复盘记录', icon: 'Search', to: '/knowledge' },
]

/* ------------------------------------------------------------------ *
 * 10. 统计（真实接口：/statis/today、/statis/week、/statis/system）
 * ------------------------------------------------------------------ */

export const statisToday = {
  job_total: 12,
  job_success: 177,
  job_fail: 5,
  job_running: 1,
  job_wait: 1,
  job_timeout: 1,
  node_total: 6,
  node_online: 5,
}

export const statisWeek = {
  date: Array.from({ length: 7 }, (_, i) => {
    const d = new Date(NOW - (6 - i) * 24 * HOUR)
    return `${d.getMonth() + 1}/${d.getDate()}`
  }),
  success: [162, 178, 171, 184, 190, 168, 177],
  fail: [8, 4, 9, 5, 3, 7, 5],
  timeout: [1, 0, 2, 1, 0, 1, 1],
  skipped: [3, 1, 2, 4, 2, 1, 2],
}

/* ------------------------------------------------------------------ *
 * 11. 脚本（真实接口：/script/search）
 * ------------------------------------------------------------------ */

export const scripts = [
  {
    id: 1,
    name: 'cleanup-old-logs.sh',
    content: "#!/bin/bash\n# 清理 90 天前日志\nfind /var/log/app -name '*.log' -mtime +90 -delete\n",
    note: '日志清理脚本，含 EBUSY 重试',
    created: ago(30 * 24 * HOUR),
    updated: ago(3 * 24 * HOUR),
  },
  {
    id: 2,
    name: 'baseline-audit.sh',
    content: '#!/bin/bash\n# 等保 L2 基线巡检\nset -euo pipefail\nbash /opt/security/checks/*.sh\n',
    note: '安全基线巡检入口',
    created: ago(70 * 24 * HOUR),
    updated: ago(2 * 24 * HOUR),
  },
]

/* ------------------------------------------------------------------ *
 * 12. 设置页（部分为后端已支持，部分为规划中）
 * ------------------------------------------------------------------ */

export const settingsSnapshot = {
  auth: {
    jwt_issuer: 'crony-admin',
    jwt_ttl_hours: 12,
    jwt_secret_source: 'admin/conf/app.ini（环境变量优先）',
    password_min_length: 8,
    force_password_change: true,
    bootstrap_admin: '首个 /register 请求自动成为管理员（引导模式，注册随后关闭）',
    rbac: '已启用：JWTAuth + RequireAdmin（阶段 0）',
  },
  scheduler: {
    default_concurrency_policy: 'skip',
    default_retry_times: 0,
    default_retry_interval: 0,
    max_retry_times: 100,
    default_timeout_seconds: 0,
    default_allocation: 'auto',
  },
  notify: {
    channels: [
      { key: 'dingtalk', label: '钉钉机器人', enabled: true, supported: true },
      { key: 'email', label: '邮件（SMTP）', enabled: true, supported: true },
      { key: 'webhook', label: '自定义 Webhook', enabled: false, supported: false },
      { key: 'wechat', label: '企业微信', enabled: false, supported: false },
    ],
  },
  // 阶段 4 真实口径：Agent 内核 + 只读工具集（含 admin→node 探测通道）已落地，
  // 工具全部 read 风险、诊断恒为 advisory_only；写工具默认**不注册**（allow-write=false）。
  agent: {
    enabled: true,
    provider: 'offline',
    model: '',
    autonomy_level: 'L1（写动作一律人工审批，不自动执行）',
    advisory_only: true,
    readonly_tools: 6,
    write_tools: 0,
    prompt_versions: ['diagnose-v1', 'nljob-v1'],
    // 阶段 2 是"单会话 ReAct 循环"，没有多 Agent 并行编排
    max_parallel_agents: 1,
    budget: { max_calls: 20, max_tokens: 200000, max_duration_ms: 1800000 },
    max_tool_calls_per_session: 12,
    require_approval_for: ['所有写动作（阶段 4 起存在：写工具默认不注册，注册后一律转人工审批）'],
  },
}

export const apiBaseInfo = { base: 'http://127.0.0.1:8089' }

/* ==========================================================================
 * 阶段 4.2：写动作审批单 + 工具调用审计（Mock）
 *
 * 这组样例刻意覆盖**全部终态**（pending / executed / execution_failed /
 * blocked / rejected / expired / canceled），因为审批页最容易骗自己的地方就是
 * "看起来都一样"：状态文案必须能让人一眼分清
 *   - executed         动作真的发生了；
 *   - execution_failed 动作发生了但失败；
 *   - blocked          动作**从未发生**（执行前复核没过：工具下线/策略收紧/参数被改）。
 * 字段与后端 agentApprovalView 逐字对齐（args_text 是脱敏后的存档参数）。
 * ========================================================================== */

// 后端契约里 created / expires_at / decided_at / executed_at 都是 **Unix 秒**
// （毫秒另有 created_ms 字段），而 data.js 顶部的 NOW 是毫秒：
// 这里必须换算，否则「还剩 11 分钟」会被算成天文数字。
const APPR_NOW = Math.floor(NOW / 1000)

export const approvalRequests = [
  {
    approval_id: 'ap-1791738000000000001',
    session_id: 'sess-4f2a91c8',
    run_id: 'r-20261012-0001',
    agent_kind: 'diagnose',
    tool: 'job_retry',
    risk: 'write',
    risk_text: '可逆写（有副作用，可回退）',
    args_text: '{"job_id":21,"reason":"磁盘告警且已确认清理脚本无副作用，重跑一次"}',
    args_hash: '9f2c1d4b7a8e5f6c3b2a1908d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8',
    reason: '磁盘告警且已确认清理脚本无副作用，重跑一次',
    target_node: 'node-it-01',
    impact: '影响 1 台主机 · 1 个任务',
    requested_by: 'agent:diagnose',
    status: 'pending',
    status_text: '待审批',
    decided_by: '',
    decided_at: 0,
    decision_note: '',
    executed_at: 0,
    execution_result: '',
    expires_at: APPR_NOW + 11 * 60,
    remain_seconds: 11 * 60,
    executed: false,
    created: APPR_NOW - 4 * 60,
    created_ms: (APPR_NOW - 4 * 60) * 1000,
  },
  {
    approval_id: 'ap-1791737000000000002',
    session_id: 'sess-77c10b3e',
    run_id: 'r-20261012-0002',
    agent_kind: 'diagnose',
    tool: 'job_retry',
    risk: 'write',
    risk_text: '可逆写（有副作用，可回退）',
    args_text: '{"job_id":18,"reason":"etcd 抖动导致的假失败，现场已恢复，重跑一次"}',
    args_hash: '1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809',
    reason: 'etcd 抖动导致的假失败，现场已恢复，重跑一次',
    target_node: 'node-it-01',
    impact: '影响 1 台主机 · 1 个任务',
    requested_by: 'agent:diagnose',
    status: 'executed',
    status_text: '已执行',
    decided_by: 'admin',
    decided_at: APPR_NOW - 26 * 60,
    decision_note: '确认现场一致，同意重跑',
    executed_at: APPR_NOW - 26 * 60 + 2,
    execution_result: '已向原节点投递一次重跑 | 输出: job 18 accepted by node-it-01',
    expires_at: APPR_NOW - 19 * 60,
    remain_seconds: 0,
    executed: true,
    created: APPR_NOW - 31 * 60,
    created_ms: (APPR_NOW - 31 * 60) * 1000,
  },
  {
    approval_id: 'ap-1791736000000000003',
    session_id: 'sess-91aa02d7',
    run_id: 'r-20261012-0003',
    agent_kind: 'diagnose',
    tool: 'job_retry',
    risk: 'write',
    risk_text: '可逆写（有副作用，可回退）',
    args_text: '{"job_id":12,"reason":"重跑一次验证"}',
    args_hash: 'abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789',
    reason: '重跑一次验证',
    target_node: 'node-it-01',
    impact: '影响 1 台主机 · 1 个任务',
    requested_by: 'agent:diagnose',
    status: 'execution_failed',
    status_text: '已执行但失败',
    decided_by: 'admin',
    decided_at: APPR_NOW - 52 * 60,
    decision_note: '批准',
    executed_at: APPR_NOW - 52 * 60 + 3,
    execution_result: '错误: 节点无响应（投递超时）',
    expires_at: APPR_NOW - 45 * 60,
    remain_seconds: 0,
    executed: true,
    created: APPR_NOW - 58 * 60,
    created_ms: (APPR_NOW - 58 * 60) * 1000,
  },
  {
    approval_id: 'ap-1791735000000000004',
    session_id: 'sess-5b6e88f1',
    run_id: 'r-20261012-0004',
    agent_kind: 'diagnose',
    tool: 'job_retry',
    risk: 'write',
    risk_text: '可逆写（有副作用，可回退）',
    args_text: '{"job_id":15,"reason":"清理 /tmp 过期文件的任务卡住了，重跑"}',
    args_hash: '00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff',
    reason: '清理 /tmp 过期文件的任务卡住了，重跑',
    target_node: 'node-it-01',
    impact: '影响 1 台主机 · 1 个任务',
    requested_by: 'agent:diagnose',
    status: 'blocked',
    status_text: '已拦截（执行前复核未通过，未执行）',
    decided_by: 'admin',
    decided_at: APPR_NOW - 71 * 60,
    decision_note: '批准',
    executed_at: 0,
    execution_result: '执行前复核未通过: 审批单参数与待执行参数不一致，拒绝执行（批准 A 执行 B）',
    expires_at: APPR_NOW - 66 * 60,
    remain_seconds: 0,
    executed: false,
    created: APPR_NOW - 78 * 60,
    created_ms: (APPR_NOW - 78 * 60) * 1000,
  },
  {
    approval_id: 'ap-1791734000000000005',
    session_id: 'sess-2d90ffab',
    run_id: 'r-20261012-0005',
    agent_kind: 'diagnose',
    tool: 'job_retry',
    risk: 'write',
    risk_text: '可逆写（有副作用，可回退）',
    args_text: '{"job_id":7,"reason":"失败告警误报，重跑确认"}',
    args_hash: 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef',
    reason: '失败告警误报，重跑确认',
    target_node: 'node-it-01',
    impact: '影响 1 台主机 · 1 个任务',
    requested_by: 'agent:diagnose',
    status: 'rejected',
    status_text: '已驳回',
    decided_by: 'admin',
    decided_at: APPR_NOW - 96 * 60,
    decision_note: '现场正在变更，等窗口期过后重新发起',
    executed_at: 0,
    execution_result: '',
    expires_at: APPR_NOW - 90 * 60,
    remain_seconds: 0,
    executed: false,
    created: APPR_NOW - 101 * 60,
    created_ms: (APPR_NOW - 101 * 60) * 1000,
  },
  {
    approval_id: 'ap-1791733000000000006',
    session_id: 'sess-6c31e2a4',
    run_id: 'r-20261012-0006',
    agent_kind: 'diagnose',
    tool: 'job_retry',
    risk: 'write',
    risk_text: '可逆写（有副作用，可回退）',
    args_text: '{"job_id":11,"reason":"重跑一次"}',
    args_hash: '5555555555555555555555555555555555555555555555555555555555555555',
    reason: '重跑一次',
    target_node: 'node-it-01',
    impact: '影响 1 台主机 · 1 个任务',
    requested_by: 'agent:diagnose',
    status: 'expired',
    status_text: '已过期（超时未决策，不可再执行）',
    decided_by: '',
    decided_at: 0,
    decision_note: '审批超时未决策（系统自动作废）：写动作的合理性依赖当时的现场，过期单子不允许再执行',
    executed_at: 0,
    execution_result: '',
    expires_at: APPR_NOW - 118 * 60,
    remain_seconds: 0,
    executed: false,
    created: APPR_NOW - 133 * 60,
    created_ms: (APPR_NOW - 133 * 60) * 1000,
  },
]

/**
 * 工具调用审计（网关每次调用一条，被拒的也在内）。
 * decision_text 与后端 auditDecisionText 一致：approval_required 是**成功的一步**，
 * 不是错误——这一点在页面上必须看得见，否则运维会把"转人工审批"当成故障。
 */
export const toolAuditRows = [
  {
    id: 4101,
    session_id: 'sess-4f2a91c8',
    run_id: 'r-20261012-0001',
    agent_kind: 'diagnose',
    tool: 'job_retry',
    risk: 'write',
    decision: 'approval_required',
    decision_text: '转人工审批（未执行）',
    ok: true,
    reason: 'L2 且白名单为空：写动作必须人工确认',
    approval_id: 'ap-1791738000000000001',
    duration_ms: 3,
    created: APPR_NOW - 4 * 60,
    created_ms: (APPR_NOW - 4 * 60) * 1000,
  },
  {
    id: 4100,
    session_id: 'sess-77c10b3e',
    run_id: 'r-20261012-0002',
    agent_kind: 'diagnose',
    tool: 'job_retry',
    risk: 'write',
    decision: 'approval_executed',
    decision_text: '已批准并执行',
    ok: true,
    reason: '人工批准（admin）后执行',
    approval_id: 'ap-1791737000000000002',
    duration_ms: 128,
    created: APPR_NOW - 26 * 60,
    created_ms: (APPR_NOW - 26 * 60) * 1000,
  },
  {
    id: 4099,
    session_id: 'sess-5b6e88f1',
    run_id: 'r-20261012-0004',
    agent_kind: 'diagnose',
    tool: 'job_retry',
    risk: 'write',
    decision: 'denied',
    decision_text: '已拒绝',
    ok: false,
    reason: '执行前复核未通过：审批单参数与待执行参数不一致（TOCTOU）',
    approval_id: 'ap-1791735000000000004',
    duration_ms: 1,
    created: APPR_NOW - 71 * 60,
    created_ms: (APPR_NOW - 71 * 60) * 1000,
  },
  {
    id: 4098,
    session_id: 'sess-2d90ffab',
    run_id: 'r-20261012-0005',
    agent_kind: 'diagnose',
    tool: 'mysql_query_readonly',
    risk: 'read',
    decision: 'allowed',
    decision_text: '已放行',
    ok: true,
    reason: '',
    approval_id: '',
    duration_ms: 41,
    created: APPR_NOW - 95 * 60,
    created_ms: (APPR_NOW - 95 * 60) * 1000,
  },
]
// decided_at / executed_at 在后端是 **RFC3339 字符串**（handler 的 unixText），
// 而上面的样例为了好读写成了 Unix 秒：这里统一成后端形状。
// 为什么较真：Mock 与真机的**字段类型**不一致时，页面在 Mock 下能跑、
// 切到真机会因为 Date.parse(数字) 之类的差异而显示错时间，属于典型的"开关一拨就翻车"。
for (const r of approvalRequests) {
  r.decided_at = r.decided_at ? new Date(r.decided_at * 1000).toISOString() : ''
  r.executed_at = r.executed_at ? new Date(r.executed_at * 1000).toISOString() : ''
}