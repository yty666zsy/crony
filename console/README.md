# crony AI Agent 控制台（console）

阶段 2 前端交付：在阶段 1 的"信息架构 + Mock 骨架"之上，把 AI Agent 部分从**设想中的接口**
换成**冻结契约**（`../PHASE2-接口契约.md` §1–§5）的真实实现——Agent 会话列表与轨迹回放、
只读能力边界、AI 诊断（读 + 触发）、自然语言建任务（草案 → 二次确认）。

- **技术栈**：Vue 3 + Vite + Element Plus + Tailwind CSS + Vue Router + Pinia（无 React / Nuxt / Ant Design）
- **独立运行**：默认走内置 Mock，**不需要后端**即可完整浏览全部页面；关掉开关即切到真实接口
- **诚实标注**：阶段 2 的 7 个 Agent 接口后端 handler **尚未注册**（Go 内核已在 `crony/common/pkg/agent` 落地），
  所以本控制台的 Agent 数据全部来自 Mock，页面上处处标注；`advisory_only=true`（只给建议、不执行）在 UI 里体现为**没有任何执行按钮**
- **写入范围**：本目录自包含，未改动 `crony/` 下任何 `.go`、`Dockerfile`、`docker-compose.yml`、`conf/*`、`go.mod`，
  也未改动 `../PHASE2-接口契约.md`（只读契约）

---

## 1. 快速开始

```bash
# 依赖安装（本仓库使用 pnpm；npm 仅作兜底：npm install）
pnpm install

# 开发（默认 http://127.0.0.1:5273，/api 代理到 VITE_API_BASE）
pnpm dev

# 生产构建 → dist/
pnpm build

# 预览构建产物（http://127.0.0.1:4173）
pnpm preview

# Mock 数据层冒烟测试（无需后端、无需浏览器，Node 内真实执行 mock 处理器）
pnpm smoke

# 阶段 2 契约自检（无需后端：Node 内真实执行 src/api/agent.js → mock 的完整调用链，
# 逐条断言 §1–§5 的字段、分页/过滤、三种诊断样本、1007/1008/1009/1010/1011/1012 分支，
# 以及 §9 的 SSE 帧解析器与订阅帧序列；当前 13/13）
node verify-phase2.mjs

# 真实联调自检（**需要后端已在跑**，VITE_USE_MOCK=false）：
# Node 内加载控制台自己的 API 层（stores/auth.js → api/client.js 拦截器 → api/agent.js
# → api/stream.js），对真实 admin 打全部 7 个 /agent/* 接口 + SSE；2026-10-11 实测 15/15
pnpm verify:live
```

> 受限/沙箱环境（无命名管道、禁止子进程管道）下，开发服务器需要显式绑定回环地址：
> `pnpm dev -- --host 127.0.0.1`。原因见第 7 节。

### 环境变量

| 变量 | 默认值（`.env`） | 说明 |
| --- | --- | --- |
| `VITE_USE_MOCK` | `true` | `true` 走内置 Mock；`false` 走真实 HTTP |
| `VITE_API_BASE` | `http://127.0.0.1:8089` | 后端 admin 服务地址；`pnpm dev` 下 `/api` 代理目标 |
| `VITE_SITE_URL` | `https://github.com/tmnhs/crony` | 顶栏 "Visit Website" 跳转地址 |

`VITE_USE_MOCK=false` 时无需改任何页面代码：`src/api/*.js` 里每个函数都是
`if (USE_MOCK) → mock` / `else → request`，Mock 返回结构与后端 DTO 同名同形。

#### ⚠️ 真实联调必须用 `.env.local`，不能用命令行环境变量

2026-10-11 实测：即使命令行设了 `$env:VITE_USE_MOCK='false'`，`pnpm dev` 注入浏览器的
`import.meta.env` 仍然是 `"VITE_USE_MOCK": "true"` —— 页面看上去正常，实际全是 Mock 数据，
**等于假联调**。

根因在 Vite 6.4.4 的 `loadEnv`（`vite/dist/node/chunks/dep-*.js`）：

1. 读 `.env*` 得到 `parsed`；
2. `expand({ parsed, processEnv })` —— dotenv-expand **把 `parsed` 的值写回 `process.env`**
   （`processEnv[key] = parsed[key]`）；
3. 先放 `parsed`，再用 `process.env` 覆盖（设计意图是"行内环境变量优先"）。

第 2 步已经把 `.env` 的 `VITE_USE_MOCK=true` 写进了 `process.env`，第 3 步"优先"覆盖回来的
自然还是 `true`。所以 `.env` 里已有的键，命令行无法覆盖。

正确做法（`.env.local` 优先级高于 `.env`，且已被 `console/.gitignore` 与根 `.gitignore` 忽略）：

```bash
printf 'VITE_USE_MOCK=false\nVITE_API_BASE=http://127.0.0.1:8089\n' > console/.env.local
pnpm dev     # 打开 http://localhost:5273/，此时是真实后端数据
```

验证是否真的生效（不要凭页面观感判断）：

```bash
curl -s http://127.0.0.1:5273/src/api/client.js | head -c 400
# 应看到 "VITE_USE_MOCK": "false"
```

**2026-10-11 阶段 3 复跑时的实测结果**（真 admin 8089 + 真 MySQL 13306 + 真模型）：

```
import.meta.env = {"BASE_URL": "/", "DEV": true, "MODE": "development", "PROD": false,
 "SSR": false, "VITE_API_BASE": "http://127.0.0.1:8089", "VITE_USE_MOCK": "false"}
```

即运行中的 dev server（<http://127.0.0.1:5273/>）**确实是真后端模式**，
不是"看起来像联调"。注意 `.env` 里仍是 `VITE_USE_MOCK=true`——
**没有 `.env.local` 的机器上，`pnpm dev` 默认是 Mock**，这是最容易误判的一处。

---

## 2. 目录结构

```
console/
├── index.html                    # 入口（<html class="dark">，hash 路由）
├── verify-phase2.mjs             # 阶段 2 契约自检（Node + Vite SSR 加载器，不需要浏览器/后端）
├── vite.config.js                # base:'./'、@ 别名、/api 代理、构建产物拆分
├── tailwind.config.js            # navy/brand 色板、sidebar 宽度、glow 阴影、动画
├── postcss.config.js
├── pnpm-workspace.yaml           # 环境适配：nodeLinker: hoisted、allowBuilds 开关
├── .npmrc                        # 仅镜像源
├── .env / .env.example
├── scripts/
│   ├── run-vite.mjs              # dev/build/preview 入口（受限环境的启动适配）
│   ├── child-process-sandbox-shim.mjs  # 恢复 ChildProcess 的错误事件语义
│   ├── esbuild-shim-loader.mjs   # 把裸导入 'esbuild' 重定向到下面的替代实现
│   ├── esbuild-text-shim.mjs     # 纯 JS 的 esbuild.transform 替代（仅 define 替换）
│   └── smoke-mock.mjs            # pnpm smoke：Mock 数据层冒烟测试
└── src/
    ├── main.js                   # 挂载 Vue + Pinia + Router + Element Plus(zh-cn) + 全部图标
    ├── App.vue                   # → components/AppLayout.vue
    ├── api/
    │   ├── client.js             # axios 实例、Token、{code,data,msg} 解包、业务错误弹窗
    │   ├── mock/{index.js,data.js}  # Mock 处理器 + 可信中文样例数据（含阶段 2 §1–§5）
    │   ├── agent.js              # ★ 阶段 2 契约接口：§1/§2/§3.1/§3.2/§4/§5.1/§5.2
    │   ├── stream.js             # ★ 阶段 2 §9：SSE 订阅（fetch + ReadableStream）+ 可单测帧解析器
    │   ├── auth.js job.js run.js node.js script.js statis.js
    │   ├── knowledge.js          # 仍规划中（平台无文档/RAG 存储，见 §4.2）
    │   └── audit.js settings.js  # 已接真实接口（合并审计 / 只读配置快照，见 §4.1）
    ├── router/index.js           # 路由表（hash 模式）
    ├── config/nav.js             # 侧边栏导航分组（唯一导航来源）
    ├── stores/{app,auth,agentPanel}.js   # 布局/视图宽度、登录态、右下角会话速查面板
    ├── styles/index.css          # Tailwind 入口 + Element Plus 深色变量覆盖
    ├── utils/{dict.js,format.js} # 枚举字典（与 Go 枚举逐项对应）、格式化
    ├── components/
    │   ├── AppLayout.vue AppSidebar.vue AppTopbar.vue
    │   ├── AgentSessionWidget.vue  # 右下角青色圆形入口 → Agent 会话速查（§1，不是聊天）
    │   ├── AgentStepCard.vue     # 单条轨迹步骤：model / tool / notice 视觉可分（§2）
    │   ├── DiagnosisPanel.vue    # AI 诊断面板：只读只建议、模型自评、未观测引用告警（§3）
    │   ├── NlJobWizard.vue       # 自然语言建任务向导：草案 → 5 次触发时间 → 确认（§5）
    │   ├── MockBadge.vue         # 统一的 Mock/真实数据标注
    │   └── StatCard.vue Sparkline.vue StatusBadge.vue EmptyState.vue
    └── views/                    # 见第 3 节页面清单
```

---

## 3. 页面路由（hash 模式）

路由使用 `createWebHashHistory`，配合 `base: './'`，产物可由 crony-admin 直接挂在
`/console` 子路径下（与 `admin/internal/handler/router.go:141` 的说明一致），刷新任意深链接都不会 404。

| 路由 | 页面 | 说明 |
| --- | --- | --- |
| `/dashboard` | 总览 | 4 张 KPI + Latest Alerts + Quick Actions + 三栏（服务器/在线 Agent/知识库）+ 最近任务表 |
| `/big-screen` | 大屏 | 独立全屏布局（无侧栏/顶栏），等宽数字卡片与条形图（阶段 2 完善） |
| `/jobs` | 定时任务 | 列表检索、cron 表达式、并发策略、重试策略、立即执行、增删改抽屉；**用自然语言建任务向导**（§5.1 草案 → 5 次触发时间与警告 → §5.2 确认） |
| `/runs` | 执行记录 | `job_run` 检索（状态/触发来源/节点/跟踪号）+ 抽屉内步骤时间线、取消执行；抽屉内 **AI 诊断面板**（§3.1 读 / §3.2 触发，force 需二次确认） |
| `/servers` | 服务器管理 | 节点心跳、负载水位、任务分布、下线（管理员） |
| `/terminal` | 终端 | 节点选择 + 命令回显型终端（阶段 2 接真实 PTY） |
| `/agents` | Agent 会话 | **当前能力边界**（§4 真实只读工具 + 硬配额）、会话列表（§1：kind/status/job_id/run_id 过滤 + 分页）、就地"立即诊断"（§3.2） |
| `/agents/:session_id` | 会话轨迹 | **轨迹回放**（§2）：model / tool / notice 三类步骤视觉可分，tool 步骤展示 `tool_args` 与 `observation`，顶部展示 `budget` 与 `warnings`，结论只读 |
| `/knowledge` | 知识库 | **规划中**：后端没有文档/RAG 存储（无 chunks / 向量索引），不拿经验库冒充；真实经验检索见 `/agents` 与经验库 |
| `/audit` | 审计轨迹 | 检索两个**真实**事实源（工具调用审计 + 写动作审批），每行带"来源"标注与 `notice` 合并窗口说明 |
| `/settings` | 设置 | 鉴权（issuer/有效期/密钥来源，**只报是否配置、不返回密钥**）、调度默认值、通知渠道、Agent 能力与预算（provider/model/自治级别/预算上限） |
| `/:pathMatch(.*)*` | 404 | 兜底页 |

响应式：`≥1280px` 完整三栏；`≤1024px` 侧栏收为图标栏（可手动展开/收起/“收起分组”）；
`≤768px` 卡片单列、表格横向滚动。

---

## 4. 页面 ↔ 后端接口映射

统一信封 `{ code, data, msg }`，`code = 200` 为成功；**业务错误同样是 HTTP 200**，
由 `src/api/client.js` 的解包层抛 `BizError` 并弹 `ElMessage`。分页契约与后端
`PageResult` 一致：`{ list, total, page, page_size }`，入参 `page<=0 → 1`、`page_size<=0 → 20`。

### 4.1 已实现（后端 `admin/internal/handler/router.go` 已注册，全部需要 `JWTAuth`）

| 页面 | 前端 API（`src/api/`） | 后端路由 | router.go |
| --- | --- | --- | --- |
| 全局探活 | `auth.ping` | `GET /ping`（返回裸字符串 `"pong"`，无信封） | :32 |
| 登录/注册 | `auth.login` / `auth.register` | `POST /login`、`POST /register` | :51-52 |
| 用户栏 | `auth.currentUser` | `GET /user/find?id=` | :92 |
| 设置 · 改密 | `auth.changePassword` | `POST /user/change_pw` | :91 |
| 总览/大屏 | `statis.fetchTodayStatistics` | `GET /statis/today` | :58 |
| 大屏 | `statis.fetchWeekStatistics` | `GET /statis/week` | :59 |
| 设置 · 系统信息 | `statis.fetchSystemInfo` | `GET /statis/system` | :60 |
| **总览 KPI（执行次数/成功率）** | `run.fetchRunOverview` | `GET /job/run/overview?since=` | :83 |
| 任务列表 | `job.searchJobs` | `POST /job/search` | :70 |
| 任务详情 | `job.findJob` | `GET /job/find?id=` | :69 |
| 任务增改 | `job.saveJob` | `POST /job/add` | :67 |
| 任务删除 | `job.deleteJob` | `POST /job/del` | :68 |
| 立即执行一次 | `job.runJobOnce` | `POST /job/once` | :72 |
| 任务日志 | `job.searchJobLogs` | `POST /job/log`（旧 `job_log` 模型） | :71 |
| 执行记录 · 取消 | `job.killJob` / `run.killRun` | `POST /job/kill`（阶段 1 已恢复） | :74 |
| 执行记录 · 列表 | `run.searchRuns` | `POST /job/run/search` | :80 |
| 执行记录 · 详情 | `run.findRun` | `GET /job/run/find?run_id=` | :81 |
| 执行记录 · 步骤 | `run.fetchRunSteps` | `POST /job/run/steps` | :82 |
| 服务器/终端 | `node.searchNodes` | `POST /node/search` | :107 |
| 服务器 · 下线 | `node.deleteNode` | `POST /node/del`（**需管理员**） | :112 |
| 任务 · 脚本选择 | `script.searchScripts` / `script.findScript` | `POST /script/search`、`GET /script/find?id=` | :118-119 |
| 脚本增删（规划中页面未接） | `script.saveScript` / `script.deleteScript` | `POST /script/add`、`POST /script/del`（**需管理员**） | :124-125 |
| — | — | `POST /user/search`、`POST /user/del`、`POST /user/update`（**需管理员**） | :98-100 |

`GET /job/run/overview` 的响应口径（`admin/internal/service/job_run.go:118`）：
`{ total, success, failed, timeout, canceled, skipped, running, pending, retry_total,
success_rate, skip_rate, by_status, since }`，其中
`success_rate = 成功 / (total - skipped)`（被并发策略跳过不算失败）、`skip_rate = 跳过 / total`，
`since` 缺省为最近 24 小时。总览页前两张 KPI 卡直接使用该接口，卡片底部标注数据来源。

### 4.2 规划中（**后端当前没有这个路由**，前端先约定契约 + Mock；不得当成已实现）

| 页面 | 前端 API | 约定路由 | 状态 |
| --- | --- | --- | --- |
| 知识库 | `knowledge.searchKnowledge` | `POST /knowledge/search` | **仍规划中**：平台没有文档/RAG 存储（无 chunks / 向量索引） |

> `GET /statis/overview`、`GET /statis/health`、`GET /settings`、`POST /audit/search` **已实现**
> （后端 `admin/internal/handler/{insight.go,router.go}`，阶段 5 收尾补齐），本表不再列它们。
> 知识库**有意不实现**：用经验库（`agent_memory`）冒充文档检索会把"没有"说成"有"；
> 真实的经验检索走 `/agent/memory/*` 与 MemoryView。

切到真实后端时，**仍规划中**的接口会命中 404。前端对它打了 `planned: true` 标记
（`api/knowledge.js`）：`api/client.js` 命中 404 时**不弹错误提示**，
而是抛 `NotImplementedError`，由页面渲染成"规划中"占位（列表为空）。
早期版本不是这样——它会把 404 当故障逐条弹"请求失败（HTTP 404）"，单页可弹 2~3 条。

### 4.3 阶段 2 Agent 契约接口（冻结契约 `PHASE2-接口契约.md` §1–§5）

这 7 个接口**已双向落地**：Go 内核在
`crony/common/pkg/agent/{trace,diagnose,nljob,cronnext,agent,prompt}.go` 与
`crony/common/pkg/llm`、`crony/common/pkg/tools`，
admin HTTP handler 已注册在 `admin/internal/handler/router.go` 的 `/agent` 组（第 96–105 行，
`JWTAuth` 保护）。**默认 `VITE_USE_MOCK=true` 时前端仍走 Mock**（无需后端即可运行）；
用 `VITE_USE_MOCK=false` 启动才会打到真后端——此时浏览器看到的字段由 admin 视图层保证与契约一致。

> 本节此前写着"admin 层 HTTP handler 尚未注册"，那是 `e2829cd` 之前的旧状态，已更正。

| 契约 | 前端 API（`src/api/agent.js`） | 路由 | 页面 |
| --- | --- | --- | --- |
| §1 会话检索 | `searchAgentSessions` | `POST /agent/session/search` | `/agents`（列表 + 过滤 + 分页）、右下角速查面板 |
| §2 会话详情（轨迹） | `findAgentSession` | `POST /agent/session/find` | `/agents/:session_id` |
| §3.1 读诊断 | `findAgentDiagnosis` | `POST /agent/diagnosis/find` | `/runs` 抽屉、`/agents` 就地诊断 |
| §3.2 触发诊断 | `runAgentDiagnosis` | `POST /agent/diagnosis/run` | 同上（`force=true` 前有二次确认） |
| §4 工具与配额 | `listAgentTools` | `POST /agent/tool/list` | `/agents` 顶部"当前能力边界" |
| §5.1 草案 | `draftNlJob` | `POST /agent/nljob/draft` | `/jobs` 自然语言建任务向导 |
| §5.2 确认创建 | `confirmNlJob` | `POST /agent/nljob/confirm` | 同上（二次确认步骤） |

### 4.4 执行事实订阅（`PHASE2-接口契约.md` §9，SSE）

| 能力 | 前端 API（`src/api/stream.js`） | 路由 | 页面 |
| --- | --- | --- | --- |
| 实时进度（run/step/heartbeat/end） | `openRunStream(run_id, …)` → `{ close() }` | `GET /job/run/stream?run_id=&interval_ms=&max_seconds=&after_step=` | `/runs` 抽屉「实时进度」卡片 |
| 纯函数帧解析器（可单测） | `createSSEParser()` | — | 由 `verify-phase2.mjs` §9 三项断言覆盖 |

要点（细节见契约 §9，**不要当成消息总线**）：

- **不能用 `EventSource`**：它无法设置 `Authorization` 头，而本项目的鉴权是 Bearer JWT；
  因此用 `fetch` + `ReadableStream` 手工解析（`src/api/stream.js`）。把 token 放 query 会进
  浏览器历史/代理日志/`Referer`，明确不做。
- 服务端按 `interval_ms` 做 **DB（`job_run`/`job_run_step`）差异对比**，没有进程内事件总线；
  延迟上界 = 间隔。`snapshot` 是**重置**语义（换执行对象时会重发），前端必须清空既有步骤再应用。
- 终态后服务端发 `end` 并关闭，客户端**不得**重连；连接异常中断才退避重连（重连拿新快照）。
- 未做：历史事件重放、多订阅者广播、**Agent 会话轨迹的逐步推送**（阶段 3）。
- `VITE_USE_MOCK=true` 时该卡片是**模拟回放**（有 Mock 角标），不得据此认为"已实时推送过"。

`src/api/agent.js` 导出 13 个符号：上表 7 个契约函数 + 2 个归一化视图
（`budgetView`、`toolStatsView`，用于吸收"契约字段 vs Go 内核字段"差异）+ 4 个步骤取值兜底函数
（`stepSeq` / `stepTime` / `stepError` / `stepTokens`）。字段差异清单见第 10 节。

阶段 2 相关业务码（契约 §0）：`1007` 本节点未配置模型、`1008` 会话不存在、`1009` 草案本地复验不通过、
`1010` 指定的运行记录不存在、`1011` Agent 能力被配置关闭、`1012` 节点离线/未注册。
`src/utils/dict.js` 的 `AGENT_BIZ_HINT` 把它们翻译成"人话 + 该怎么办"，弹窗里同时给出 code，
便于对着契约排查。

---

## 5. Mock 数据

`src/api/mock/data.js` 提供可信的中文样例：节点 `node-prod-01…`、任务
"清理 90 天前日志"（`0 3 * * *`）、"MySQL 慢查询巡检"（`*/30 * * * *`）、"容器健康检查"
（`*/5 * * * *`）等；执行记录覆盖 成功 / 失败 / 超时 / 跳过 / 执行中，跳过用例明确写出
"上一轮尚未结束，并发策略 Skip：本轮直接跳过"；步骤明细带重试次数与退避说明。
枚举值全部从 `src/utils/dict.js` 复用（与 `crony/common/models/{job,run,node}.go` 逐项对应），
不在两处维护。

Mock 的行为约束（与真实后端一致，便于无感切换）：

- 返回同样的 `{ code, data, msg }` 信封，同样走 `client.js` 的解包与错误弹窗；
- 分页默认值与 `PageInfo.Check()` 一致；
- `login` 的口令长度不足返回业务错误 `code: 1000`（HTTP 200），正常口令返回 `token`；
- `GET /ping` 返回裸字符串（真实后端这条没有信封，故 Mock 也返回裸值）。

### 5.1 阶段 2 的 Mock 样例（对应契约 §1–§5）

`src/api/mock/data.js` 里的 Agent 数据**不是**编出来的好看数字，而是从既有 `runs` 派生
（`runOf(job_id, status)`），因此会话的 `run_id / job_id / node_uuid / 时间` 与执行记录页一致：

| 契约 | 样例 | 关键点 |
| --- | --- | --- |
| §1 | 8 条会话（3 种 kind × 3 种 status 全覆盖） | 字段与契约逐字对应，`provider='offline'`、`model=''`（离线 FakeProvider 从不上报 model，UI 显示"—"） |
| §2 | 8 份完整轨迹 | model / tool / notice 三类步骤都有；tool 步骤带 `tool_args` + `observation`；边界样本含 `tool_budget` 与 schema 修复的 notice |
| §3.1 | 3 份诊断样本 | ①证据充分（confidence 72，3 条证据）②证据不足（`evidence_sufficient=false`，列出还要采集什么）③**引用了未观测到的证据定位**（`unverified_refs=['syslog:999']` + 醒目警告） |
| §3.2 | 即时诊断 | 未诊断过的 run 返回"证据不足"模板（confidence 35，标注 Mock），不复用已有诊断；`force=false` 幂等、`force=true` 新增会话 |
| §4 | 6 个真实只读工具 | 名称/说明照抄 `common/pkg/tools/builtin.go`：`shell_readonly` / `http_get` / `node_status` / `node_probe` / `log_search` / `mysql_query_readonly`（`node_probe` 为阶段 3 第二段新增，提交 `0fcc81a`） |
| §5.1 | 3 份草案 | cmd（每天 2 点清理）、http（健康检查探活）、**5 段表达式**（`0 2 * * *`，带"不是每天 2 点"的醒目警告） |
| §5.2 | 确认创建 | 真的往 `jobs` 里插入一条 `status:0`（未分配节点）的任务，任务列表页立即可见；非法 spec / 越界 timeout / `confirmed=false` 一律 1009 |

Mock 里**可达**的业务错误分支（不是装饰）：`1008`（未知 session_id）、`1009`（空文本草案 /
非法覆盖项 / `confirmed!==true`）、`1010`（未知 run_id，需手工改参数）、
`1011`/`1007`（把 `settingsSnapshot.agent.enabled` / `provider` 改掉才会命中）、
`1012`（把 run 指到 `status=2` 的失联节点）。

> 另外保留了 `agentChat` / `agentDispatch` 两个**兼容垫片**：阶段 1 的冒烟脚本
> （`scripts/smoke-mock.mjs`，本期不允许改动）会断言它们返回可读文案。
> 它们现在返回的是"阶段 2 没有 `/agent/chat`，Agent 的产物是会话与轨迹"的说明，
> 不再假装存在对话/下发接口。

---

## 6. 设计取舍

- **导航信息架构按 crony 工作流重排**：侧边栏分六组 —— `概览`（概览 / 值班大屏）、
  `调度与执行`（定时任务 / 执行记录 / 任务编排）、`节点与资源`（节点管理 / 终端）、
  `Agent 智能`（Agent 会话 / 知识库 / 经验库）、`治理与运维`（审批中心 / 审计轨迹 / 自愈调度 /
  智能运维）、`系统`（系统设置）。早期版本沿用过演示站基线里的 `DB Connections` / `SSH Keys` /
  `Remote Desktop` / `Containers` 等**无后端支撑的占位项**，已连同对应路由与 `PlaceholderView.vue`
  一并移除——控制台只承载有真实接口或明确契约的能力。
- **真实模式的登录页（Mock 模式没有）**：Mock 模式仍免登录，底部用户栏展示 `admin`。
  真实模式（`VITE_USE_MOCK=false`）新增 `views/LoginView.vue`：`POST /login → JWT → 回查用户`，
  令牌存 `localStorage.crony_token`，请求自动带 `Bearer` 头。此前真实模式在无令牌时，
  每个接口都回业务码 1000（未登录），前端**逐条弹窗**——实测单页 4 条；现在：
  无令牌先登录（不发无谓请求）；令牌失效（1000 且 `data.reload`）统一清令牌、去重提示一次并切回登录页。
- **外壳身份取自 JWT，后端刷新静默兜底**：后端没有 `GET /me`，`loadProfile()` 直接用令牌载荷的
  `ID / UserName / Role` 构造用户身份（**不发请求**），再**静默**回查一次 `GET /user/find` 做刷新
  （`silent: true`，失败保持令牌身份、不弹错误）。早期写死 `id=1` 去查，而真实管理员往往不是
  1 号用户，`GET /user/find?id=1` 回 `[find_user] db error`，导致**每个页面加载都弹一次**该错误。
- **侧边栏状态徽标不再编数字**：阶段 1 写死过 `6 Agent · 12 Job`（Go 侧没有 Agent 实体模型，无从核对）；
  阶段 2 改为 `系统正常 · N Agent 会话`，N 取契约 §1 `POST /agent/session/search` 的 `total`，
  接口不可用时只显示 `系统正常`。
- **不做聊天框**：阶段 1 右下角是"AI 对话"（`/agent/chat`），但契约里没有这个接口——Agent 的产物是
  **会话 + 轨迹**。阶段 2 换成"Agent 会话速查"（按 run_id/job_id 检索会话 → 跳轨迹），删除了
  `AiChatWidget.vue` 与 `stores/chat.js`，避免用一个不存在的接口撑交互。
- **服务器页空状态**：节点列表为空时展示"尚未接入资产台账"的引导态，而不是编造服务器列表。
- **KPI 不编造"Agent 在线数"**：`agent_session` 只有终态（completed / failed / canceled），平台没有
  "Agent 在线"这种状态；因此概览与大屏展示的是**会话计数**（累计 / 今日 / 今日失败），
  而不是像早期 Mock 那样编一个 `4/6`。
- **图表不引第三方库**：趋势用内联 SVG（`Sparkline.vue`），大屏条形图用 div 高度实现，
  避免为两张小图引入图表库体积。
- **成功率的颜色阈值**：`≥95%` 绿、`≥90%` 橙、否则红，卡片与进度条一致。

---

## 7. 受限环境适配（沙箱内构建通过的原因）

在禁止命名管道、禁止子进程管道（`spawn EPERM`）的沙箱里，Vite 的标准链路有几处会直接失败。
以下适配**全部限制在本目录内**，对正常环境无副作用（`run-vite.mjs` 会自行探测能力）：

| 现象 | 根因 | 适配 |
| --- | --- | --- |
| `ERR_MODULE_NOT_FOUND: picomatch … from vite/dist/node/cli.js` | 本机 `fs.realpathSync` 不解析目录联接，Node 从 links 目录向上找到了顶层 `node_modules` | `pnpm-workspace.yaml` 里 `nodeLinker: hoisted`（**必须写在此文件**；`.npmrc` 的 `node-linker` 在 pnpm 11 被忽略） |
| 加载 `vite.config.js` 时 `spawn EPERM (ensureServiceIsRunning)` | Vite 用 esbuild 打包配置文件，需要管道 | CLI 追加 `--configLoader=runner`（用 Node 原生加载配置） |
| `spawn EPERM … optimizeSafeRealPathSync → exec("net use")` | Vite 在 Windows 上探测网络盘 | `scripts/child-process-sandbox-shim.mjs`：把同步抛出的 `spawn` 错误改回标准 `error` 事件语义 |
| `Cannot read properties of undefined (reading 'on')` ← `replaceDefine → esbuild.transform` | Vite 的 `vite:define` 无条件调用 `esbuild.transform`；而该属性是**不可配置的 getter**，无法打补丁 | `scripts/esbuild-shim-loader.mjs` 用 `module.register()` 把裸导入 `esbuild` 重定向到纯 JS 实现 `scripts/esbuild-text-shim.mjs`（只实现 define 替换，逐字符扫描、跳过字符串/注释/模板串；`transform('...')` 之外的能力会显式抛错，不静默降级） |
| 依赖预打包同样依赖 esbuild 子进程 | 同上 | `run-vite.mjs` 探测到管道不可用时设 `CRONY_NO_DEP_OPT=1`，`vite.config.js` 据此关闭 `optimizeDeps` 发现（`{ noDiscovery: true, include: [] }`） |
| `pnpm` 安装时 `Ignored build scripts: esbuild, vue-demi` | pnpm 11 的构建脚本拦截；强行允许会 `spawn EPERM` | `pnpm-workspace.yaml` 里 `allowBuilds: { esbuild: false, vue-demi: false }`——两者在构建链路中已被上面的替代实现覆盖，**实际不需要其 postinstall** |

**未采用的替代方案**：`esbuild-wasm`（WASM 版 esbuild，可绕开子进程）。它在当前 `package.json`
中以 devDependency 形式存在（版本 `0.28.2`），但本控制台的构建链路**并未使用它**——需要额外的
`initialize()` 引导、WASM 运行时与 worker 语义，而 define 替换这一个 API 用纯 JS 实现即可完全覆盖，
引入它只会增加体积与失败面。如需清理可执行 `pnpm remove esbuild-wasm`（会同步更新 lockfile）。

---

## 8. 版本（`pnpm install` 实际解析结果）

| 依赖 | package.json | 实测安装版本 |
| --- | --- | --- |
| vue | ^3.5.13 | 3.5.43 |
| vue-router | ^4.5.0 | 4.6.4 |
| pinia | ^2.3.1 | 2.3.1 |
| element-plus | ^2.9.3 | 2.14.7 |
| @element-plus/icons-vue | ^2.3.1 | 2.3.2 |
| axios | ^1.7.9 | 1.20.0 |
| vite | ^6.0.7 | 6.4.4 |
| @vitejs/plugin-vue | ^5.2.1 | 5.2.1 |
| tailwindcss | ^3.4.17 | 3.4.19（**v3，不使用 v4 / @tailwindcss/vite**） |
| postcss | ^8.4.49 | 8.5.29 |
| autoprefixer | ^10.4.20 | 10.6.1 |
| terser | ^5.37.0 | 5.51.2 |
| lightningcss | ^1.29.1 | 1.33.0 |
| esbuild-wasm（未使用，见第 7 节） | ^0.28.2 | 0.28.2 |

构建产物 `dist/`（`pnpm build`，实测 **24 个文件 / 1.81 MB**）：`index.html` 0.8 kB、
`assets/index-*.css` 385 kB（构建日志 394.48 kB / gzip 55.7 kB）、`element-plus-*.js` 1023 kB
（构建日志 1046.99 kB / gzip 318.7 kB，按需拆分后可显著下降）、`vue-*.js` 108 kB、
`index-*.js` 167 kB（构建日志 170.91 kB），以及各页面独立 chunk：
`JobsView` 30.1 kB、`DashboardView` 16.5 kB、`RunsView` 16.3 kB、`AgentSessionView` 16.3 kB、
`SettingsView` 14.7 kB、`AgentsView` 14.2 kB、`DiagnosisPanel` 10.7 kB 等。

---

## 9. 未验证项与风险（如实记录）

1. **未做浏览器渲染验证**：当前环境没有可用浏览器，`dist/` 只做了产物级校验（构建成功、
   相对路径、Tailwind 类与 Element Plus 深色变量均已进入 CSS、无残留 `process.env` /
   `import.meta.env`），**没有真实截图**。像素级视觉仍需人工打开确认。
2. **深色主题覆盖的完整性**：Element Plus 深色通过官方 `dark/css-vars.css` + `html.dark`
   下的变量覆盖实现，已在 CSS 产物中确认关键变量存在，但个别组件（如日期选择器面板、
   级联菜单）在深色下的对比度需人工过一眼。
3. **受限环境适配的取舍**：`nodeLinker: hoisted` 牺牲了 pnpm 的严格隔离（平铺 node_modules）；
   `--configLoader=runner`、`optimizeDeps` 关闭、esbuild 替代实现都只在沙箱探测命中时生效，
   正常开发机不需要也不受影响。若把本项目搬到标准环境，建议删除 `pnpm-workspace.yaml` 中的
   `nodeLinker`/`allowBuilds` 并恢复 esbuild 原链路。
4. **规划接口未做联调**：第 4.2 节 5 个接口后端尚未实现，真实模式下会报错属预期。
   `/job/run/overview` 只按 `service.RunOverview` 的字段与口径对接（含成功率分母扣除 skipped），
   但**未与真实数据库联调过数值**。
5. **鉴权链路**：`Bearer` 头与 401 处理只做了代码级实现与 Mock 验证（无登录页、无真实联调）。
   连带发现并修复：后端 `JWTAuth` 原先只接受**裸 token**，收到标准 `Bearer <jwt>` 会返回
   `That's not even a token`——即 `VITE_USE_MOCK=false` 时**所有**带鉴权的接口都会失败。
   修复后中间件同时接受两种写法（`BearerToken`，含单测）。此缺陷由后端的真 HTTP 端到端用例抓出，
   **前端的 Mock 模式与 `verify-phase2.mjs` 都测不到**，这也是"Mock 通过 ≠ 联调通过"的实证。
6. **开发服务器需显式 `--host 127.0.0.1`**：沙箱内不显式指定 host 时不会监听回环地址（已实测
   非沙箱回环 TCP 本身可用），显式指定后页面与模块均正常返回（40/40 模块 HTTP 200）。
7. **`esbuild-wasm` 为环境遗留依赖**（见第 7 节），当前构建不依赖它。

### 9.1 阶段 2 专项：真实联调结果 + 剩余未验证项（2026-10-11 更新）

> **本节结论已从"未对真后端跑过"推进为"API 层已真实联调通过，视觉/交互未验"。**
> 联调环境：`admin`（Go，真实 MySQL 13306）+ 控制台 API 层，`VITE_USE_MOCK=false`，
> 模型为 DashScope `qwen-plus`（真实凭据）。执行方式：`pnpm verify:live`，结果 **15/15 通过**。
>
> 阶段 3 改动（`node_status` 工具 + `unverified_node_claims` 契约字段 + 诊断面板告警块）
> 之后复跑：**17/17 通过**（同一后端，用例数随断言增加；`pnpm build` 亦通过）。
> 阶段 3 第二段（`node_probe` 节点侧只读探测通道，提交 `0fcc81a`）之后**再次复跑：17/17 通过**，
> 且 `tool/list` 实测已是 **6 个工具**（含 `node_probe`）；`node_probe` 的真联调证据
> 见 `PHASE2-接口契约.md` §4.1（通道可用 → 结论引用真 `netstat`/`tasklist` 输出）。
>
> ⚠️ **复跑前务必确认 `CRONY_LIVE_RUN_A`/`CRONY_LIVE_RUN_B` 是库里真实存在的 run_id**：
> 本次复跑第一次写成 `j910005-m1791721595539400-1`（库里没有这条），
> 结果 `diagnosis/find(B)` 与 `diagnosis/run(B)` 两项失败、报告 15/17——
> **看起来像回归，其实是用例参数写错**。正确写法只能在库里核对：
> `select run_id from job_run where job_id=<job>`。
>
> 注意 `verify:live` 是**控制台代码路径 + 真实 API**的联调，**不是浏览器 E2E**：
> 诊断面板新增的告警块是否在浏览器里渲染正确，仍未验证（需要真实浏览器）。

**新增：真实联调自检 `pnpm verify:live`（`scripts/verify-live.mjs`）**

它在 Node 内通过 Vite SSR 加载器执行**控制台自己的代码路径**：`stores/auth.js` 的 `login`
action → `api/auth.js` → `api/client.js` 的 axios 拦截器（真的往 localStorage 写 token、
真的加 `Authorization: Bearer`）→ `api/agent.js` → `api/stream.js` 的 SSE 解析与订阅，
对真实 admin 打全部 7 个 `/agent/*` 接口 + SSE。覆盖与实测结果：

| # | 检查项 | 实测 |
| --- | --- | --- |
| 1 | `VITE_USE_MOCK=false` 真的生效（断言，不看观感） | `USE_MOCK=false` ✓ |
| 2 | `POST /login`（走 `stores/auth.js`，含 `setToken` 与 `isLoggedIn`） | token 219 字符 ✓ |
| 3 | `POST /agent/session/search` | `total=7`，首条 `kind=diagnose` ✓ |
| 4 | `POST /agent/session/find`（轨迹回放） | 1 step / 4 evidence / 0 warnings ✓ |
| 5 | `POST /agent/tool/list` | 4 个工具（`http_get`/`log_search`/`mysql_query_readonly`/`shell_readonly`），**无 `node_status`**，配额 12 ✓ |
| 6 | `POST /agent/diagnosis/find` ×2 | `found=true`，`provider=openai`、`model=qwen-plus`、置信度 95、证据 2、`unverified_refs=0` ✓ |
| 7 | `POST /agent/diagnosis/run(force=false)` ×2 | `replayed=true`、`persist_error` 为空（幂等不烧 token）✓ |
| 8 | SSE 终态运行 | `snapshot → step → end(reason=final, status=failed)`，12ms ✓ |
| 9 | SSE 未知 `run_id` | 只发 `heartbeat`、不报 1010、不提前 `end` ✓ |
| 10 | **SSE 长连接 >20s** | 连接存活 **32.0s**，心跳 @7.0s/14.5s/22.0s/29.5s，客户端主动断开 ✓ |
| 11 | `POST /agent/nljob/draft`（真跑一次模型） | `spec=0 0 3 * * *`、`argv=["/usr/bin/find","/tmp","-mtime","+7","-delete"]`、5 次触发时间 ✓ |
| 12 | `POST /agent/nljob/confirm` | 落库 `job_id=4`、`status=0`（未分配）、`requires_assign=true` ✓ |

**本条联调直接抓出并修掉的 3 个真实缺陷（Mock 与 `verify-phase2.mjs` 都测不到）**

1. **`/agent/nljob/draft` 响应多一层 `data.draft`**：后端返回 `{draft:{…}, session_id}`，
   而前端按扁平结构读 `draft.value.name`。修复：`api/agent.js` 新增并导出纯函数
   `normalizeDraft()`（有 `draft` 就摊平、保留外层 `session_id`/`error`、草案同名字段优先）。
   **修前**：向导页所有字段空白、确认拿不到 `session_id`。
2. **axios 超时 15s < 真实模型耗时**：实测一次诊断 19.6–22.8s，原来前端会先被 axios 掐断，
   用户只看到"无法连接到后端"，而后端其实还在正常推理。修复：`api/client.js` 默认超时 → **60s**
   （注释里写明依据与"后端已取消全局 20s WriteTimeout"的配对关系）。
3. **`overrides.retry_times=-1` 与"不改"语义不符**：前端注释/契约写"0 / -1 表示不改"，
   但 `applyDraftOverrides` 只要非 `nil` 就写入，负数必然触发 1009。
   修复：后端 `admin/internal/handler/agent.go` 改为 `*RetryTimes >= 0` 才生效
   （与 `timeout_seconds <= 0` 保持一致），并补单测；同时更正前端注释。

顺带确认为**正确设计、不是缺陷**的一点：草案产出 6 段表达式 `0 0 3 * * *`，
而前端只对"恰好 5 段"弹红色告警——这是对的，底层 `jakecoffman/cron` 把 5 段解释为
「秒 分 时 日 月」，6 段才是"每天 03:00:00"。该告警逻辑与后端 `nljob.go` 的
`len(strings.Fields(spec)) == 5` 判断一致。

**第二次复跑（阶段 3 第二段之后，2026-10-11；同一脚本、同一后端、真模型）**

上表是阶段 2 的原始记录（当时 4 个工具、`total=7`）。引入 `node_probe` 与两个结论
字段后的实测值如下（**17/17 通过**）：

| 复跑项 | 实测值 |
| --- | --- |
| `POST /agent/tool/list` | **6 个工具**：`http_get, log_search, mysql_query_readonly, node_probe, node_status, shell_readonly`；配额 `max_calls_per_session=12` |
| `POST /agent/session/search` | `total=19`，首条 `kind=nljob` |
| `POST /agent/diagnosis/find(A)` | `provider=openai`、`model=qwen-plus`、置信度 95、证据 3、**未验证引用=2**（r55 那次模型编造了 2 条 `shell_readonly` 引用） |
| `POST /agent/diagnosis/find(B)` | `found=true`、证据 2、**未验证引用=0**（负向对照：该次没有伪造引用） |
| `POST /agent/diagnosis/run(force=false)` ×2 | `replayed=true`、`persist_error` 为空 |
| SSE 长连接 | 存活 32.0s，心跳 @7.0s/14.5s/22.0s/29.5s |
| `POST /agent/nljob/draft` / `confirm` | `spec=0 0 3 * * *`；`confirm` 落库 `job_id=12`、`status=0`、`requires_assign=true` |

> `unverified_refs=2` 不是控制台的缺陷，而是**后端如实上报了模型的编造行为**；
> 前端诊断面板会把它渲染成告警块（浏览器内是否渲染正确仍未验证）。

**第三次复跑（"进程级预算"缺陷修复之后，2026-10-11 23:03；同一脚本、同一后端）**

修复的是后端 `common/pkg/llm` 的预算记账归属（详见根目录
`AI-AGENT-改造方案.md` 3.4e）：此前预算挂在进程级 Gateway 上，
`max-duration-seconds=300` 实际表示"admin 运行超过 5 分钟后 Agent 永久不可用"。
本次复跑刻意在 **admin 已运行 ~440 秒**时执行，结果 **17/17 通过**：

| 复跑项 | 实测值 |
| --- | --- |
| 整体 | **17/17 通过，失败 0** |
| `POST /agent/tool/list` | 6 个工具不变；`max_calls_per_session=12` |
| `POST /agent/session/search` | `total=26`（诊断/巡检/草案累积） |
| `POST /agent/session/find` | `#r84`、`steps=6`、`evidence=9`、`warnings=0` |
| `POST /agent/diagnosis/find(A)` | `#r84`、`qwen-plus`、置信度 95、证据 3、未验证引用 0 |
| `POST /agent/diagnosis/find(B)` | 置信度 95、证据 2、未验证引用 0 |
| `POST /agent/nljob/draft` | ✅ 通过——**这一条是关键**：它要真的调模型，旧实现此刻必然失败 |
| `POST /agent/nljob/confirm` | `job_id=15`、`spec=0 0 3 * * *`、`status=0`、`requires_assign=true` |

> 结论口径：这份 17/17 只能证明"控制台 API 层 ↔ 真实后端"这条链路在长运行进程下依然可用；
> **不能**证明浏览器里的渲染/交互正确（仍未点验）。

**仍未验证（如实保留）**

1. **视觉与交互没有人工点验**：本机没有可用浏览器/无头内核，`verify:live` 走的是
   "控制台 API 层 + 真实后端"，**不覆盖**渲染、点击、路由跳转、二次确认弹窗。
   开发服务器本身已验证：`pnpm dev` 首页 200（当时标题 `ITOps Agent · crony 控制台`，后经前端适配
   改为 `crony · 分布式定时任务与 AI Agent 运维平台`）、
   `/src/main.js`、`/src/App.vue`、`/src/views/{Dashboard,Agents,Runs}View.vue`、
   `/src/components/NlJobWizard.vue` 均 HTTP 200 可编译，且注入的 env 为
   `"VITE_USE_MOCK": "false"`（即 dev 服务器确实处于真实模式）。
2. **SSE 断线重连的浏览器侧可见性未点验**：后端断线→前端退避重连→新快照收敛这条路径只有
   代码与单测支撑；本次只验证了"连接能活过 20s（原缺陷已修）"与"未知 run 只发心跳"。
3. **没有准确率评测**：真实 provider 只跑了 2 个播种用例（n=2），两个结论都与独立核验的
   地面真值一致，但**样本量为 2，不构成任何准确率主张**；页面上也不显示准确率。
4. **`confidence` 仍是模型自评**，前端展示的 `confidence_note` 明确写了
   "模型自评置信度（未经验证，不代表准确率）"。
5. **`advisory_only=true` 是阶段 2 的固有条**：UI 里"没有任何执行按钮"是按契约设计的，
   不是漏做——阶段 3 才会出现变更类动作与审批流。
6. **未实现 SSE / 流式**：契约里诊断与草案都是"一次请求拿到最终结果"，所以页面是请求-等待-渲染，
   没有逐 token 流式输出，也没有"运行中"的进度回显。诊断按钮的 loading 是唯一的进行中反馈。
7. **JSON Schema 展示为原文**：§4 的 `tools[].schema` 直接 `<pre>` 展示，未做表单化/校验器；
   阶段 2 也没有前端触发工具调用的入口（工具由内核调度）。
8. **`trigger_echo` / `next_trigger_text` 的边界情形未验证**：真实内核用 `cronnext` 库计算，
   月末、闰年、时区夏令时等边界**未验证**（本次只看到近 5 次正常结果）。
9. ~~**admin 缺节点侧证据通道**~~ **已在阶段 3 补上**：`tool/list` 实测 **6 个工具**，
   含 `node_status`（节点存活/心跳/水位）与 `node_probe`（在指定节点上跑只读诊断命令）。
   实测闭合：通道在服务时模型引用真 `netstat`/`tasklist` 输出、`unverified_node_claims=[]`；
   通道不可用时模型退回编造、守卫如实标记（见 `PHASE2-接口契约.md` §4.1）。
   **仍缺**的部分：节点水位发布器未实现（`node_metric` 真机上恒为"未上报"）；
   探测通道要求节点能连到同一个 MySQL，跨网段拓扑未支持。
10. **`pnpm smoke` 不受本期改动影响的部分**：`scripts/smoke-mock.mjs` 本期**不允许修改**，
    它仍断言旧的 `agentChat` / `agentDispatch`，因此这两个垫片只能保留（见第 5.1 节末尾）。
11. **无 lint 可跑**：`package.json` 没有 `lint` 脚本、也没有 eslint 依赖，本期未执行任何 lint，
    只做了 `pnpm build`（编译期校验）与三个自检脚本。

#### 附：`verify-phase2.mjs`（Mock 契约自检，仍然保留）

它验证的是**前端 API 层 + Mock 的契约一致性**，当前 13/13 通过，覆盖：

   | # | 检查项 | 断言要点 |
   | --- | --- | --- |
   | 1 | §1 会话列表 | `PageResult` 形状、20 个契约字段齐全、kind/status 过滤、分页 |
   | 2 | §2 会话详情 | 8 个顶层字段 + `budget` 6 项 + `tool_stats` 4 项；tool 步骤必须有 `tool_args`/`observation`；notice 必须有文本 |
   | 3 | §2 兜底详情 | 临时新增的会话也能取到可渲染详情（不依赖手工补数据） |
   | 4 | §3.1 三种诊断样本 | 证据充分 / 证据不足（含 `next_data_to_collect`）/ 引用未观测定位（含警告）；未诊断的 run 返回 `found=false`、`session_id=""`、`diagnosis=null` |
   | 5 | §3.2 触发诊断 | `force=false` 幂等返回同一条；`force=true` 新增会话且同一 run 不产生重复会话 |
   | 6 | §4 工具与配额 | 工具全部 `readonly`、5 个字段齐全；policy 6 项齐全 |
   | 7 | §5.1 草案 | cmd/http/五段三份样例字段齐全；`next_triggers` 与 `next_trigger_text` 各 5 条；`requires_confirmation=true`、`confirmed=false`；`trigger_echo` 含"确认前不会写入调度器"且列出 1)–5) 五次时间 |
   | 8 | §5.2 确认创建 | 创建出的任务出现在任务列表且 `status=0`；非法 spec / 越界 timeout / `confirmed=false` 走 1009；空文本草案走 1009 |
   | 9 | 错误码 | 1008（未知会话）、1010（未知 run）、1011（enabled=false）、1007（provider 为空） |
   | 10 | 错误码 | 1012（把 run 指到失联节点） |
   | 11 | §9 SSE 解析器 | 跨 chunk 半帧不提前吐出；CRLF 切帧；注释行忽略；多行 data；未闭合内容留在缓冲 |
   | 12 | §9 帧序列 | 首帧 `snapshot` → `step`… → `run` → `end` 且 `end` 必须在最后、`end` 后不再重连 |
   | 13 | §9 缺失执行 | run 不存在时给出 `error` + `end`，不得静默挂起 |

   **§9 的实时进度仍有明确的未验证面**：真实浏览器下"服务端主动断线 → 自动重连 → 新快照收敛"
   与心跳可见性未人工点验（Mock 模式是模拟回放，不是实时能力）；
   后端侧的 SSE 行为有单元测试 9 例 + 真实 MySQL 集成 3 例（含真 HTTP + JWT 端到端）兜底，
   见契约 §9.6。

---

## 10. 阶段 2 契约问题与字段差异（只报告，未改契约）

阅读 `crony/common/pkg/agent` 等 Go 实现后，发现契约与内核结构存在以下差异。
**前端一律按契约字段渲染**，只在契约字段缺失时回落到内核字段（`src/api/agent.js` 的
`budgetView` / `toolStatsView` / `stepSeq` / `stepTime` / `stepError` / `stepTokens`），
契约本身**未做任何修改**。建议后端 handler 负责补齐这些字段（而不是让前端猜）：

> **后续进展（后端已补齐 1~7）**：`admin/internal/handler/agent_view.go`（提交 `e2829cd`）
> 已按契约 v1 在视图层统一补齐 `seq`/`created`/`error`/`total_tokens`、`status_text`、
> `stop_reason_text`、`budget`（毫秒）、`tool_stats.truncated|remaining`、`category_text`/
> `risk_text`/`concurrency_policy_text`/`confidence_note`/`explain`，并把 `confidence` 统一为
> 0–100。**一旦接上真后端，表中的 1~7 项回落逻辑不会被触发**（保留它只为兼容 Mock 与旧后端）。
> 第 8~11 项是文档/枚举约定，以 `*_text` 与后端告警文本为准。详见 `PHASE2-接口契约.md` §8.5。

| # | 位置 | 契约（`PHASE2-接口契约.md`） | Go 内核实现 | 前端处理 |
| --- | --- | --- | --- | --- |
| 1 | §2 `steps[]` 标识 | `seq` / `created` / `error` | `Step.Index` / `Step.At` / `Step.Err`（`json:"err,omitempty"`） | 契约优先，缺失回落内核字段 |
| 2 | §2 `steps[]` token | `total_tokens` | 嵌套 `usage.total_tokens`（`prompt_tokens`/`completion_tokens`） | 两级取值，都缺失则不显示（不写 0） |
| 3 | §2 顶层 | `stop_reason_text` | `Trace.StopExplain` | 契约优先，`AGENT_STOP_REASON` 兜底 |
| 4 | §2 `budget` | `{calls,tokens,duration_ms,max_calls,max_tokens,max_duration_ms}` | `BudgetSnapshot{budget,calls,tokens,prompt_tokens,completion_tokens,elapsed(纳秒),remaining_*,exceeded,reason}`，且 `llm.Budget` **没有 json tag**（会序列化成 `MaxCalls/MaxTokens/MaxDuration`） | `budgetView` 同时认两套命名并把纳秒换算成毫秒 |
| 5 | §2 `tool_stats` | `{calls,denied,truncated,remaining}` | `SessionStats` 只有 `calls/denied/evidence/per_tool/…`，**没有** `truncated`/`remaining` | 缺失时显示 `—`，**不编造 0** |
| 6 | §1/§3 的 `*_text` | `category_text` / `risk_text` / `concurrency_policy_text` / `explain` | 内核结构里没有这些字段（应是 handler 层附加） | 缺失时用 `dict.js` 的枚举中文兜底 |
| 7 | §3.1 `confidence` | 示例 `72`（0–100 整数） | `clamp01(percent/100)`，即 0–1 小数 | `formatConfidence` 两种量纲都接受 |
| 8 | `stop_reason_text` 文案 | 示例写 `"正常完成"` | `Explain()` 返回 `已得到结构化结论` | 以契约文案为准（`AGENT_STOP_REASON.completed`），后端原文优先 |
| 9 | §5.1 `concurrency_policy` 取值 | 契约未列枚举 | `nljob.go`：`skip\|buffer_one\|buffer_all\|cancel_other\|terminate_other` | 与阶段 1 `dict.js` 的 `skip\|queue\|forbid` 不一致，**优先显示后端 `concurrency_policy_text`** |
| 10 | §5.1 五段表达式语义 | 只要求给出警告 | `nljob.go` 的警告措辞与"库按秒/分/时/日/周解释"有关 | Mock 采用更准确的"「秒 分 时 日 月」→ `0 2 * * *` 是每小时第 2 分钟"表述；**真实文案以后端为准** |
| 11 | 会话与 run 的关系 | §1 有 `job_id`/`run_id`（可为空） | `nljob` 会话没有 run_id | UI 对空值显示 `—`，不假设一定存在 |
| 12 | §5.1 `/agent/nljob/draft` 响应形状 | 契约示例是**扁平**的草案字段 | handler 实际返回 `{ draft: {…}, error, session_id }`（多一层） | **2026-10-11 真实联调抓到**：前端按扁平读会全空。已在 `api/agent.js` 加 `normalizeDraft()` 摊平（保留外层 `session_id`）；契约与 Mock 未改 |

另外两个"契约没写、但实现必须有"的点，前端按保守方式处理，建议契约后续补充说明：

- **§2 的 `system` / `context` 是原文字段**：可能很长（含完整 prompt）。页面默认折叠，点击才展开，
  避免首屏被 prompt 淹没。
- **§3.2 的 `force` 语义**：契约只写"是否强制重新诊断"，未写是否新增会话。Mock 按
  "`force=false` 已有则原样返回、`force=true` 新增一条会话"实现，UI 的二次确认文案也按这个语义写。

---

## 11. 与后端的衔接

`crony-admin` 当前在 `configNoRoute`（`router.go:134`）中挂载旧版 Vue2 产物 `./dist`；
AI Agent 控制台的产物可直接放到 `admin` 的静态目录并在 `/console` 下挂载：

```go
r.StaticFile("/console", "./console/dist/index.html")
r.Static("/console/assets", "./console/dist/assets")
```

因为使用了 hash 路由 + `base: './'`，无需服务端 rewrite；接口地址由 `VITE_API_BASE` 决定，
跨域可用后端已有的 `middlerware.Cors()`（`router.go:14`）。