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

$env:CRONY_ACC_BASE='http://127.0.0.1:8089'
$env:CRONY_ACC_USER='admin'
$env:CRONY_ACC_PASS='<你的登录口令>'                     # 必填；不填则整套 SKIP
$env:CRONY_ACC_RUN_ID='<一个真实存在且失败的 run_id>'      # 诊断类用例的前置条件
$env:CRONY_ACC_EXPECT_REAL='1'                            # 要求 provider 不得是 offline
$env:CRONY_ACC_NODE_UUID='node-it-01'
$env:CRONY_TEST_MYSQL_DSN='root:@tcp(127.0.0.1:13306)/crony_live?charset=utf8mb4&parseTime=True&loc=Local&timeout=5s'
$env:CRONY_ACC_REPORT='E:\documents\crony\验收报告.md'     # 报告输出路径（可留空：只打印）

go test -tags acceptance ./acceptance/ -v -count=1 -timeout 900s
```

可选：`CRONY_ACC_SSE_SHORT_SECONDS`（默认 8）、`CRONY_ACC_SSE_RUN_SECONDS`（默认 20）。

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
| AC-03 | 能力面 = 6 个**只读**工具 + 配额字段来自后端注册表 | 登录 |
| AC-04 | 会话检索分页生效且条目含控制台契约字段 | 登录 |
| AC-05 | `run_id` 不存在时 SSE 只发心跳、**不提前**下终态 | 登录（8s 窗口） |
| AC-06 | 真实执行的 SSE 能观测到 snapshot/step 与终态 `end` | 真 run_id |
| AC-07 | 诊断正路径：真模型有调用、有工具轨迹、有结构化结论 | 真 run_id + 模型凭据 |
| AC-08 | **预算按运行计**：`budget.duration_ms` 只覆盖本次运行（对"进程级预算"缺陷的判别） | 真 run_id（依赖 AC-05/06/07 先跑，使进程运行时长 ≫ 单次耗时） |
| AC-09 | 幂等回放：`force=false` 不重复烧模型、不新增会话 | 真 run_id |
| AC-10 | NL 起草产出结构化草案且**不**创建任务 | 登录 + 模型凭据 |
| AC-11 | NL 确认只落 `status=0` 未分配任务，且会回收自己造的 job | 依赖 AC-10 |
| AC-12 | 探测审计行字段完整：`done` 行必须带 `exit_code` 与节点主机名 | DSN |
| AC-13 | `expired` 必须自述「没人领取/被取消」，**不得**被读成命令执行失败 | DSN + 库里存在 expired 行 |
| AC-14 | 节点水位 `node_metric` 有真数据且可归属节点 | DSN（**当前为已知缺口 → SKIP**） |
| AC-15 | 会话详情 18 个契约字段齐全（控制台回放面板依赖） | 依赖 AC-07/08 |

## 4. 验收 ≠ 单测

单测证明"代码逻辑符合预期"，验收证明"部署起来的系统对外行为符合承诺"。
两者都要有，但**不能互相替代**：本目录刻意不 import 内部包，
就是为了让"验收"这件事无法被"改一下内部假设"糊过去。

## 5. 已知边界

- AC-13/AC-14 是**环境相关**用例：库里没有 `expired` 行、没有水位数据时会 SKIP，
  这是口径上的诚实，不代表相应能力已经验收过。
- 浏览器内的渲染与交互**不在此目录覆盖范围**（本机无浏览器）；控制台侧另有
  `console/scripts/verify-live.mjs` 覆盖"控制台 API 层 ↔ 真实后端"。