# ===== 构建阶段 =====
FROM golang:1.24-alpine AS builder

WORKDIR /src

# 先拷贝依赖清单并下载，利用层缓存：仅改业务代码时无需重下依赖
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0  静态链接，运行阶段可基于极简镜像
# -s -w          去掉符号表与调试信息，减小体积
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/server ./cmd/server

# ===== 运行阶段 =====
FROM alpine:latest

# ca-certificates：请求官方 HTTPS 接口必需，缺失会报证书错误
# tzdata：日志时间戳按本地时区显示
RUN apk add --no-cache ca-certificates tzdata

# 默认以非 root 用户运行。
# uid 1000 是多数 Linux 发行版首个普通用户的 uid，让「不指定 --user 直接跑」
# 与「挂载宿主目录给当前用户」两种场景尽量对上，减少权限摩擦。
# 推荐用法是运行时用 --user "$(id -u):$(id -g)" 覆盖成宿主机当前用户，
# 那样挂载目录天然可写，无需 chown。
RUN adduser -D -u 1000 appuser

WORKDIR /app

COPY --from=builder /out/server /app/server
COPY config.example.yaml /app/config.example.yaml

# 目录 755 保证任何 uid 都能读取二进制；数据目录归默认用户所有，
# 仅在「不挂载 /app/data」时才用得到（挂载后属主由宿主机决定）
RUN mkdir -p /app/data && chown -R appuser:appuser /app && chmod -R a+rX /app

USER appuser

VOLUME ["/app/data"]
ENV DATA_DIR=/app/data

EXPOSE 8080

ENTRYPOINT ["/app/server"]
