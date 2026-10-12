# acceptance —— crony AI-Agent 改造的案例验收（黑盒）

一句话：**对着"已经跑起来的系统"按下按钮，看它是不是真的做到了它承诺的事**，
并产出一份带三态口径的验收报告。

- 位置：`crony/acceptance/`，门控构建标签 `acceptance`（不带 tag 时只是一个空包，不影响 `go build ./...`）
- 只通过 **HTTP + 只读 SQL** 观察行为，不 import 任何内部包 → 可以对着任意一版已部署的 admin 跑
- 真模型、真 MySQL、真节点侧探测：**没有前置条件就 SKIP 并写明原因**，绝不把"没测到"算成"通过了"

## 1. 怎么跑

```powershell
# 被测系统（另一个终端）：
#   admin:    cd <repo>\.livetest\run ; .\admin\admin.exe -e testing     （配置见 conf/testing/main.json）
#   临时库:   MySQL 13306（见方案文档，**不要**动宿主 3306）
#   节点侧:   probeagent.exe（PROBE_AGENT_UUID=node-it-01）
#             + 水位发布器（AC-14 必需，否则节点水位恒为「未上报」→ AC-14 显式 SKIP）
#             载体见 .livetest/metricagent-harness/main.go（真 StartMetricPublisher，每 10s 一次）

$env:CRONY_ACC_BASE='http://127.0.0.1:8089'
$env:CRONY_ACC_USER='admin'
$env:CRONY_ACC_PASS='<你的登录口令>'                     # 必填；不填则整套 SKIP
$env:CRONY_ACC_RUN_ID=''                                  # 可留空：套件会自建 run 夹具（跑完自删）
$env:CRONY_ACC_RUN_TAG='acc<时间戳>'                       # 可选：夹具 run_id 前缀，便于自查残留
$env:CRONY_ACC_EXPECT_REAL='1'                            # 要求 provider 不得是 offline
$env:CRONY_ACC_NODE_UUID='node-it-01'
$env:CRONY_TEST_MYSQL_DSN='root:@tcp(127.0.0.1:13306)/crony_live?charset=utf8mb4&parseTime=True&loc=Local&timeout=5s'
$env:CRONY_ACC_REPORT='E:\documents\crony\验收报告.md'     # 报告输出路径（可留空：只打印）

go test -tags acceptance ./acceptance/ -v -count=1 -timeout 900s
```

可选：`CRONY_ACC_SSE_SHORT_SECONDS`（默认 16）、`CRONY_ACC_SSE_RUN_SECONDS`（默认 20）。

### 1.1 run 夹具：为什么允许套件自己插数据

AC-06/AC-07/AC-08/AC-09/AC-15 都需要**一条已结束的执行记录**。两种来源：

| 来源 | 触发条件 | 说明 |
| --- | --- | --- |
| 调用方指定 | 设了 `CRONY_ACC_RUN_ID` | 例如刚跑过一次真任务，报告里会写"调用方提供" |
| **套件自建夹具** | 没设且 DSN 可写 | 插 1 条 `job_run`（`status=3` 失败）+ 2 条 `job_run_step`，`t.Cleanup` 自删 |

为什么必须支持自建：**验收要"一条命令可复跑"**。此前这两个用例依赖一个"只在开发机上存在过"
的 run_id，换台机器就 FAIL，甚至可能被写成"查不到就跳过"而**悄悄变成通过**。
夹具只写 `job_run`/`job_run_step`（诊断材料正是从这两张表取，见 `common/pkg/agentruntime/loadrun.go`），
不碰 `job` 表，跑完自删（实测残留 0 行；清理失败会 `t.Errorf` 报错，不静默）。

> 顺带一个踩过的坑：主用例里 `defer db.Close()` 会**早于** `t.Cleanup` 执行，
> 于是清理撞上 `database is closed` 而静默失败。现在关连接也走 `t.Cleanup`（后进先出）。

## 2. 三态口径（读报告前必看）

| 状态 | 含义 | 允许的解读 |
| --- | --- | --- |
| ✅ PASS | 在**本次环境**里观测到了承诺的行为 | 只能说明"这次真做到了"，不等于"所有情况都对" |
| ❌ FAIL | 观测到与承诺相矛盾的行为 | 视为缺陷；报告里会写出期望与实际 |
| ⏭️ SKIP | **前置条件不存在，什么都没观测到** | **不等于通过**。报告逐条写明原因（缺凭据 / 缺 run_id / 库里没有 expired 行…） |

## 3. 用例目录

| 用例 | 验收点 | 依赖 |
| --- | --- | --- |
| AC-01 | 未鉴权访问 `/agent/*` 被拒（鉴权在业务之前） | 无 |
| AC-02 | 正确口令签发 token；错误口令被拒 | 口令 |
| AC-03 | 能力面 = 6 个**只读**工具（`readonly=true` 且 `risk=read`）+ 配额/风险策略字段（`allow_write=false`、`autonomy=L1`、审批 TTL、速率/并发上限）来自后端注册表 | 登录 |
| AC-04 | 会话检索分页生效且条目含控制台契约字段 | 登录 |
| AC-05 | `run_id` 不存在时 SSE 只发心跳、**不提前**下终态 | 登录（8s 窗口） |
| AC-06 | 真实执行的 SSE 能观测到 snapshot/step 与终态 `end` | run 夹具（或 `CRONY_ACC_RUN_ID`） |
| AC-07 | 诊断正路径：真模型有调用、有工具轨迹、有结构化结论 | run 夹具 + 模型凭据 |
| AC-08 | **预算按运行计**：`budget.duration_ms` 只覆盖本次运行（对"进程级预算"缺陷的判别） | 依赖 AC-05/06/07 先跑，使进程运行时长 ≫ 单次耗时 |
| AC-09 | 幂等回放：`force=false` 不重复烧模型、不新增会话 | run 夹具 |
| AC-10 | NL 起草产出结构化草案且**不**创建任务 | 登录 + 模型凭据 |
| AC-11 | NL 确认只落 `status=0` 未分配任务，且会回收自己造的 job | 依赖 AC-10 |
| AC-12 | 探测审计行字段完整：`done` 行必须带 `exit_code` 与节点主机名 | DSN |
| AC-13 | `expired` 必须自述「没人领取/被取消」，**不得**被读成命令执行失败 | DSN + 库里存在 expired 行 |
| AC-14 | 节点水位：目标节点有**新鲜**数据（<10min）且容量非零，三项百分比不得全 0 | DSN + 有节点进程在跑**水位发布器** |
| AC-15 | 会话详情 18 个契约字段齐全，且 `tool_stats.per_tool` 合计与 `calls` 自洽 | 依赖 AC-07/08 |
| AC-16 | 工具调用审计落库：会话有工具调用就必须有对应事实行，逐行自洽（决策、原因、双口径时间、可追到会话/运行） | DSN；模型若全程不调工具，套件会**主动触发**一次低信息量诊断（最多 2 次），仍无调用才 SKIP 并写明原因 |
| AC-34 | 自愈策略边界可读：动作白名单闭合、三道闸门可见、分类→动作映射完整（阶段 4-C） | 登录 |
| AC-35 | 自愈手动扫描**不绕过闸门**：回执字段齐全，指定不存在的 run_id 被拒（1025） | 登录 |
| AC-36 | 自愈记录查询契约：条目含复盘字段；不存在单号回 1023、缺锚点回 1001 | 登录 |
| AC-37 | 自愈留痕自洽：`job_heal` 行含策略/证据快照；中间态 `executed` 不写 `finished_at` | DSN + 库里存在自愈行 |

### 3.1 AC-16 为什么要"主动触发"工具调用

审计行只在**真的发生工具调用**时产生，而是否调用工具由模型决定（实测 60 个历史会话里 16 个调了工具）。
若 AC-16 只查"本轮 AC-07/AC-10 会话的审计行"，它会随模型心情时红时绿 —— 这正是本目录最反对的
"看起来全绿其实没兜住"。因此当本轮会话没有工具调用时，AC-16 自己造一条 run 夹具把调用逼出来：

- 夹具形态是**实测调出来的**：`error_msg` 只有「命令退出码 1」、无步骤无日志、命令是脚本调用
  （历史上唯一能稳定触发取证的形态）。第一版夹具把失败原因写进 `error_msg`，模型直接下结论、0 次工具调用。
- 主动触发最多 2 次（换 run、换说法），**重试封顶**，不做"烧 token 抽卡"。
- 两次都不调工具时，按三态口径显式 SKIP，原因里写清"无法从黑盒侧触发"，并指向真库集成测试
  `common/pkg/agentruntime/audit_mysql_test.go`（它在 13306 上用真实 MySQL 验证了写入、脱敏、列宽、预算不变式）。

## 4. 验收 ≠ 单测

单测证明"代码逻辑符合预期"，验收证明"部署起来的系统对外行为符合承诺"。
两者都要有，但**不能互相替代**：本目录刻意不 import 内部包，
就是为了让"验收"这件事无法被"改一下内部假设"糊过去。

## 5. 已知边界

- AC-13/AC-14 是**环境相关**用例：库里没有 `expired` 行、没有水位数据时会 SKIP，
  这是口径上的诚实，不代表相应能力已经验收过。AC-14 还要求**有节点进程真的在跑发布器**。
- AC-16 依赖模型是否调用工具（见 §3.1）：套件会主动触发最多 2 次，仍不调用时 SKIP。
  **SKIP 不等于审计链路没问题**——它意味着这一轮"没观测到"，链路本身的真库证据在集成测试里。
- 报告必须**是合法 UTF-8**：`truncate` 按字节切会切断多字节字符（真实发生过：
  报告第 1831 字节悬挂 `E6`，编辑器打不开整个文件），现在改为不切多字节字符 + 落盘 `ToValidUTF8` 兜底，
  并有 2 条回归单测（含"任意长度截断后都必须是合法 UTF-8"）。
- 浏览器内的渲染与交互**不在此目录覆盖范围**（本机无浏览器）；控制台侧另有
  `console/scripts/verify-live.mjs` 覆盖"控制台 API 层 ↔ 真实后端"。