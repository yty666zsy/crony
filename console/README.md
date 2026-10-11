# crony AI Agent 控制台（console）

阶段 1 前端交付：把 crony（企业级定时任务 / 运维平台）的控制台从"任务表格"升级为
**ITOps Agent / Multi-Agent Automation Platform** 的观感——深色导航、总览 KPI、执行记录与
步骤时间线、Agent 控制台、知识库与审计。

- **技术栈**：Vue 3 + Vite + Element Plus + Tailwind CSS + Vue Router + Pinia（无 React / Nuxt / Ant Design）
- **独立运行**：默认走内置 Mock，**不需要后端**即可完整浏览全部页面；关掉开关即切到真实接口
- **写入范围**：本目录自包含，未改动 `crony/` 下任何 `.go`、`Dockerfile`、`docker-compose.yml`、`conf/*`、`go.mod`

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

# Mock 数据层冒烟测试（无需后端、无需浏览器，Node 内真实执行 api 层）
pnpm smoke
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

---

## 2. 目录结构

```
console/
├── index.html                    # 入口（<html class="dark">，hash 路由）
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
    │   ├── mock/{index.js,data.js}  # Mock 处理器 + 可信中文样例数据
    │   ├── auth.js job.js run.js node.js script.js statis.js
    │   └── agent.js knowledge.js audit.js settings.js   # 规划中接口
    ├── router/index.js           # 路由表（hash 模式）
    ├── config/nav.js             # 侧边栏导航分组（唯一导航来源）
    ├── stores/{app,auth,chat}.js # 布局/视图宽度、登录态、AI 浮窗会话
    ├── styles/index.css          # Tailwind 入口 + Element Plus 深色变量覆盖
    ├── utils/{dict.js,format.js} # 枚举字典（与 Go 枚举逐项对应）、格式化
    ├── components/
    │   ├── AppLayout.vue AppSidebar.vue AppTopbar.vue
    │   ├── AiChatWidget.vue      # 右下角紫色圆形入口 → 展开对话面板
    │   ├── StatCard.vue Sparkline.vue StatusBadge.vue EmptyState.vue
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
| `/jobs` | 定时任务 | 列表检索、cron 表达式、并发策略、重试策略、立即执行、增删改抽屉 |
| `/runs` | 执行记录 | `job_run` 检索（状态/触发来源/节点/跟踪号）+ 抽屉内步骤时间线与取消执行 |
| `/servers` | 服务器管理 | 节点心跳、负载水位、任务分布、下线（管理员） |
| `/terminal` | 终端 | 节点选择 + 命令回显型终端（阶段 2 接真实 PTY） |
| `/agents` | Agent 控制台 | Agent 能力/状态、目标下发（Mock 编排过程与结果） |
| `/knowledge` | 知识库 | Runbook / SOP / 复盘文档检索与正文预览 |
| `/audit` | 审计日志 | 操作审计检索（操作者、动作、对象、结果） |
| `/settings` | 设置 | 鉴权/改密、调度默认值、通知、Agent 开关、系统健康 |
| `/db-connections` `/ssh-keys` `/remote-desktop` `/containers` | 占位页 | 有目标说明、交付阶段与要点清单，**非空白页** |
| `/:pathMatch(.*)*` | 404 | 兜底页 |

响应式：`≥1280px` 完整三栏；`≤1024px` 侧栏收为图标栏（可手动展开/收起/"Collapse All"）；
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

### 4.2 规划中（**后端当前没有这些路由**，前端先约定契约 + Mock；不得当成已实现）

| 页面 | 前端 API | 约定路由 | 阶段 |
| --- | --- | --- | --- |
| 总览/大屏 · 节点与耗时 KPI、告警、最近任务 | `statis.fetchOverview` | `GET /statis/overview` | 阶段 1 |
| 设置 · 系统健康组件 | `statis.fetchHealth` | `GET /statis/health` | 阶段 1 |
| Agent 控制台 | `agent.searchAgents` | `POST /agent/search` | 阶段 2 |
| Agent 控制台 / 右下角浮窗 | `agent.chatWithAgent` | `POST /agent/chat` | 阶段 2 |
| Agent 控制台 · 目标下发 | `agent.dispatchAgentTask` | `POST /agent/dispatch` | 阶段 2 |
| 知识库 | `knowledge.searchKnowledge` | `POST /knowledge/search` | 阶段 2 |
| 审计日志 | `audit.searchAuditLogs` | `POST /audit/search` | 阶段 2 |
| 设置 | `settings.fetchSettings` | `GET /settings` | 阶段 2 |

页面中所有使用规划接口的位置都带 "Mock"/阶段标注（卡片 `planned` 徽标或副标题），
不使用户误以为后端已具备该能力。切到真实后端时，未被实现的接口会返回 404/业务错误，
解包层会弹错误提示——这是预期行为，路由补齐后即恢复正常。

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

---

## 6. 设计取舍

- **导航分组扩展**：基线只有 `Home` / `Infrastructure`，本控制台新增 `Automation`（定时任务、
  执行记录）、`AI Agent`（Agent 控制台、知识库、审计）、`System`（设置）三组，把 crony 原有的
  任务/执行能力与新增的 Agent 能力分开，避免"基础设施"分组被撑成杂项。
- **无独立登录页**：本期 9 个必交付页面不含登录；底部用户栏展示当前用户（Mock 为 `admin`），
  真实登录链路已在 `client.js` 就绪（`Bearer` 头 + `localStorage` 的 `crony_token`），
  后续把登录页接到 `/login` 即可。
- **侧边栏状态徽标为数据驱动**：显示 `System Normal · N Agent · M Job`，N/M 由
  `/agent/search`、`/job/search` 的实际条数决定，而不是写死基线截图里的 `12 Agent · 10 Workflow`。
- **服务器页空状态**：`/statis/overview` 未实现时，服务器卡片展示"尚未接入资产台账"的引导态，
  而不是编造服务器列表。
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

构建产物 `dist/`（`pnpm build`，约 1.7 MB / 22 个文件）：`index.html` 0.8 kB、
`assets/index-*.css` 394 kB（gzip 55.6 kB）、`element-plus-*.js` 1047 kB（gzip 318.7 kB，
按需拆分后可显著下降）、`vue-*.js` 110 kB、`index-*.js` 130 kB，各页面独立 chunk。

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
4. **规划接口未做联调**：第 4.2 节 8 个接口后端尚未实现，真实模式下会报错属预期。
   `/job/run/overview` 只按 `service.RunOverview` 的字段与口径对接（含成功率分母扣除 skipped），
   但**未与真实数据库联调过数值**。
5. **鉴权链路未端到端验证**：没有登录页，`Bearer` 头与 401 处理只做了代码级实现与 Mock 验证。
6. **开发服务器需显式 `--host 127.0.0.1`**：沙箱内不显式指定 host 时不会监听回环地址（已实测
   非沙箱回环 TCP 本身可用），显式指定后页面与模块均正常返回（40/40 模块 HTTP 200）。
7. **`esbuild-wasm` 为环境遗留依赖**（见第 7 节），当前构建不依赖它。

---

## 10. 与后端的衔接

`crony-admin` 当前在 `configNoRoute`（`router.go:134`）中挂载旧版 Vue2 产物 `./dist`；
AI Agent 控制台的产物可直接放到 `admin` 的静态目录并在 `/console` 下挂载：

```go
r.StaticFile("/console", "./console/dist/index.html")
r.Static("/console/assets", "./console/dist/assets")
```

因为使用了 hash 路由 + `base: './'`，无需服务端 rewrite；接口地址由 `VITE_API_BASE` 决定，
跨域可用后端已有的 `middlerware.Cors()`（`router.go:14`）。