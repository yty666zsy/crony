# crony on Kubernetes（阶段 K4）

这套清单把 crony 从"一堆本机进程"变成"集群里的工作负载"，同时给 Agent 层
**最小权限**的集群接入。阅读顺序建议：本文件 → `PHASE20` 设计稿 → 三态验收报告。

## 目录

| 文件 | 内容 |
| --- | --- |
| `kind-cluster.yaml` | 本地验证集群（1 控制面 + 2 工作节点，两个 worker 带 `crony.io/zone` 标签） |
| `00-namespace.yaml` | 命名空间 `crony` + 两个独立 ServiceAccount |
| `10-config.yaml` | ConfigMap（非敏感配置）+ Secret（口令/密钥占位符） |
| `20-rbac.yaml` | 两份最小权限 Role（node 起 Job / admin 只读诊断 + 滚动重启） |
| `30-node.yaml` | crony-node Deployment（3 副本、按可用区打散） |
| `40-admin.yaml` | crony-admin Deployment + Service |

## 一次性准备

```bash
# 1) 构建镜像（根 Dockerfile 同时产出 cronyadmin 与 cronynode）
docker build -t crony:local .

# 2) 集群
#    a) 已有真集群：把镜像推到你的仓库，并把清单里的 image 改掉
#    b) 本地验证：kind（见下）
kind create cluster --config deploy/k8s/kind-cluster.yaml
kind load docker-image crony:local --name crony

# 3) 真实口令（不要用清单里的 REPLACE_ME）
kubectl -n crony create secret generic crony-secrets \
  --from-literal=CRONY_MYSQL_PASSWORD=... \
  --from-literal=CRONY_AUTH_JWT_SECRET=$(openssl rand -hex 32) \
  --from-literal=CRONY_AUTH_BOOTSTRAP_ADMIN_USER=admin \
  --from-literal=CRONY_AUTH_BOOTSTRAP_ADMIN_PASSWORD=... \
  --dry-run=client -o yaml | kubectl apply -f -

# 4) 数据面地址：编辑 10-config.yaml 里的 CRONY_MYSQL_PATH / CRONY_ETCD_ENDPOINTS
kubectl apply -f deploy/k8s/00-namespace.yaml
kubectl apply -f deploy/k8s/10-config.yaml
kubectl apply -f deploy/k8s/20-rbac.yaml
kubectl apply -f deploy/k8s/30-node.yaml
kubectl apply -f deploy/k8s/40-admin.yaml
```

访问控制台：`kubectl -n crony port-forward svc/crony-admin 8089:8089`，然后打开
`http://127.0.0.1:8089/console/`。

## 三个必须知道的边界

1. **镜像白名单默认是空的**（`CRONY_K8S_ALLOW_IMAGES`）。空 = 不允许任何镜像。
   这不是"没配好"，而是刻意的：创建 Pod 等于在集群里执行任意代码。
   写 `busybox*` 表示"允许 busybox 系"，写 `*` 表示"接受任意镜像"——后者要人签字。
2. **命名空间白名单默认只允许 `k8s.namespace` 一个**。任务 Pod 和 crony 自己同命名空间，
   于是"镜像白名单 / 命名空间边界 / RBAC"三者口径一致。
3. **写动作默认全关**：`CRONY_TOOLS_ALLOW_WRITE=false`、自治级别 `L1`。
   这时 `k8s_rollout_restart`（K3）**根本不会注册**——不是"注册了但拒绝"。

## 任务怎么在集群里跑

- 任务类型 `job_type=4`（`models.JobTypeK8s`）：`command` 是"镜像 + 容器内命令"，
  空格切分与 shell 类型逐字一致（第一个空格前是镜像）。
- 需要命名空间/环境变量时用 `steps_json`（`kind:"k8s"`，字段 `k8s_namespace` / `k8s_env`）。
- 执行时 crony 在 `k8s.namespace` 里创建一个 Job：
  - `BackoffLimit=0`——**重试归 crony**（`retry.Policy` 带退避与分类），
    不让 k8s 同时重试（否则 N×M 次执行，退出码说不清是第几次）；
  - `RestartPolicy=Never`、Pod 关闭 `automountServiceAccountToken`、禁提权；
  - `TTLSecondsAfterFinished=900` 保留失败现场，方便 `kubectl describe`；
    **超时/取消时只删 Pod**（停掉还在跑的容器），Job 与事件留到 TTL 到点——
    并且超时错误里会带上 Pod 卡住的**真实原因**（如 `Pod 卡在 ImagePullBackOff：... not found`），
    不会只给一句"超时"（真集群 E2E 抓出来的缺陷，见 PHASE20 §2.3.1）；
  - 标签 `app.kubernetes.io/managed-by=crony` + `crony/run-id|job-id|step`，可反查是哪个 run。
- 日志：Job 结束后收容器日志并按 run 落库，控制台的运行详情里能看到。

## 跨可用区

两层含义，别混：

- **crony 节点自己**跨区：`30-node.yaml` 用 `topologySpreadConstraints` 把副本摊到
  `topology.kubernetes.io/zone`（单区集群也能起，`ScheduleAnyway`）。
- **任务落到哪个节点**：由 crony 自己的调度器决定，用 `job.node_selector`
  的 `require_labels`/`require_tools` 做硬约束（例：`{"require_labels":{"zone":"az1"}}`），
  节点标签在控制台维护（见 `admin/internal/service/node.go` 的 `Labels`）。
  也就是说：**K8s 负责"节点在哪里"，crony 负责"任务去哪个节点"**，两层不互相越权。

## 本地验证：kind 集群（已实测可用）

`kind` 在本机是可以用的，唯一要绕的是这条**误导性报错**：

> `failed to list nodes: ... open //./pipe/dockerDesktopLinuxEngine: Access is denied`

它看起来像"Docker Desktop 不让第三方客户端连引擎"，其实**与 Docker Desktop 版本无关**，
是 **`kind.exe` 所在路径/句柄**导致的 named-pipe 连接被拒。**把 `kind.exe` 拷到别的目录再运行即可**：

```powershell
# 从会报错的目录（如仓库内 .gocache\gopath\bin）拷出来再跑
Copy-Item .\.gocache\gopath\bin\kind.exe $env:TEMP\kindtest\kind.exe -Force
& "$env:TEMP\kindtest\kind.exe" create cluster --config deploy\k8s\kind-cluster.yaml
```

起出来的集群（`kind-crony`，v1.29.2）`crony-control-plane` + `crony-worker`(zone-a) + `crony-worker2`(zone-b)，
两个 worker 都带 `crony.io/zone` 与 `topology.kubernetes.io/zone` 标签，可以演示跨节点调度。
这次验收就是在这个集群上跑通的（任务真跑、Agent 真诊断、滚动重启真换 Pod）。

> 不建议用 Docker Desktop 自带的 Kubernetes：它是**单节点**（演示不了跨节点调度），
> 开关藏在 GUI 里，且启停会影响这台机器上其它容器。

## 真集群集成测试（可选，无集群时自动跳过）

单测（fake clientset）证明"我们的逻辑"；下面这组证明"集群里真的成立"，尤其是**RBAC 够不够用**——
少一条 `pods/log`，fake 永远不会报错，线上才变成"任务跑成功了但日志取不回来"。

```powershell
# 1) 生成两个最小权限 SA 的 kubeconfig（token 有有效期，建议临时生成）
$server = kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}'
$ca     = kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}'
kubectl create token crony-node  -n crony --duration=2h   # → node-sa.kubeconfig
kubectl create token crony-admin -n crony --duration=2h   # → admin-sa.kubeconfig

# 2) 跑（缺省没有这些 env 时会 Skip，不会 Fail）
$env:CRONY_K8S_IT_NODE_KUBECONFIG  = '...\node-sa.kubeconfig'
$env:CRONY_K8S_IT_ADMIN_KUBECONFIG = '...\admin-sa.kubeconfig'
$env:CRONY_K8S_IT_NAMESPACE        = 'crony'
$env:CRONY_K8S_IT_DEPLOYMENT       = 'crony-demo-web'   # 滚动重启的目标
go test -tags integration ./common/pkg/k8sclient/... -run RealCluster -v
```

两条断言值得注意：
- `crony-node` 能真跑完一个 Job 并取回日志，但 patch Deployment 会被 **API 侧**拒绝；
- `crony-admin` 能重启，但建 Job 会被 **API 侧**拒绝（断言的是 `forbidden`，防止被自家配置闸门"假装拦住"）。

## 只想验代码路径

`go test ./common/pkg/k8sclient/...` 用 fake clientset 覆盖了
"建 Job → 轮询 → 判定 → 收日志 → 清理"的全链路，不需要任何集群。
