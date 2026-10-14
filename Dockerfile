# crony 多阶段构建镜像：同一镜像内含 cronyadmin 与 cronynode 两个可执行文件，
# 由 docker-compose 的 command 决定启动哪一个；控制台（Vue3 构建产物）也打进镜像。
#
# 这里**故意不写** `# syntax=docker/dockerfile:1`：本文件没有用到任何新版语法
# （无 heredoc / 无 RUN --mount），而那一行会要求 BuildKit 额外去 docker.io 拉取
# 前端镜像 —— 国内网络下经常报 "failed size validation / failed precondition"，
# 让"一条命令起得来"变成运气问题。默认的内置前端足够。
#
# 阶段 0 目标：任何环境下都能一条命令起得来（此前 README 只有零散的手工步骤），
# 并且镜像里"没有硬编码口令"——所有密钥都从环境变量注入。

# ---------- build（Go 服务端） ----------
FROM golang:1.23-alpine AS builder

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src
# 先只拷贝依赖清单，最大化利用构建缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags "-s -w" -o /out/cronyadmin ./admin/cmd \
 && go build -trimpath -ldflags "-s -w" -o /out/cronynode ./node/cmd

# ---------- build（控制台：Vue3 + Vite + Element Plus） ----------
#
# 为什么在镜像里构建前端：目标是"小白一条 `docker compose up -d --build` 就能看到界面"，
# 不该要求先在本机装 Node/pnpm 再手工构建再拷贝。这一步只影响镜像体积与首次构建时间。
#
# 为什么显式关掉 mock：console/.env 默认 VITE_USE_MOCK=true（为了"没有后端也能 pnpm dev 看界面"），
# 但那只是本地预览用的。镜像里必须连真后端，因此这里显式覆盖，并且 .dockerignore 已排除
# console/.env*，避免文件优先级把覆盖又改回 mock。
FROM node:22-slim AS webbuilder

ARG NPM_REGISTRY=https://registry.npmmirror.com
WORKDIR /web

# 先只拷贝依赖清单，最大化利用构建缓存
COPY console/package.json console/pnpm-lock.yaml console/pnpm-workspace.yaml console/.npmrc ./
RUN npm config set registry "$NPM_REGISTRY" \
 && corepack enable \
 && corepack prepare pnpm@11.28.5 --activate \
 && pnpm install --frozen-lockfile

COPY console/ ./
ENV VITE_USE_MOCK=false \
    VITE_API_BASE=http://127.0.0.1:8089
RUN pnpm build

# ---------- runtime ----------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl \
 && addgroup -S crony && adduser -S -G crony crony

ENV ENVIRONMENT=production \
    TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /out/cronyadmin /app/cronyadmin
COPY --from=builder /out/cronynode /app/cronynode
COPY admin/conf /app/admin/conf
COPY node/conf /app/node/conf
# 控制台静态文件：admin 从「当前目录下 ./console/dist」提供，访问路径 http://<host>:8089/console/
COPY --from=webbuilder /web/dist /app/console/dist

RUN mkdir -p /app/admin/logs /app/node/logs /app/dist \
 && chown -R crony:crony /app

# 以非 root 运行
USER crony

# admin API + 控制台（/console）
EXPOSE 8089

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8089/ping || exit 1

CMD ["/app/cronyadmin"]