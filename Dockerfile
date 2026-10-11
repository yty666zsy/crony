# syntax=docker/dockerfile:1
#
# crony 多阶段构建镜像：同一镜像内含 cronyadmin 与 cronynode 两个可执行文件，
# 由 docker-compose 的 command 决定启动哪一个。
#
# 阶段 0 目标：任何环境下都能一条命令起得来（此前 README 只有零散的手工步骤），
# 并且镜像里"没有硬编码口令"——所有密钥都从环境变量注入。

# ---------- build ----------
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

RUN mkdir -p /app/admin/logs /app/node/logs /app/dist \
 && chown -R crony:crony /app

# 以非 root 运行
USER crony

# admin API / 旧版前端
EXPOSE 8089

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8089/ping || exit 1

CMD ["/app/cronyadmin"]