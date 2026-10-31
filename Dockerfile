# syntax=docker/dockerfile:1

# ysp-live-go —— 央视频全频道直播代理（Go 单二进制版）
#
# 与原项目 Dockerfile 的区别：
#   原版需要 python:3.12-alpine + nodejs 运行两个进程（Python 网关 + Node 兜底引擎）。
#   Go 版把四层兜底全部编译进单个静态二进制，镜像里不需要 Python/Node，
#   Web/WASM 引擎用 wazero 在进程内执行（WASM 以 //go:embed 打包）。
#
# 多架构：构建阶段固定跑在构建机架构（$BUILDPLATFORM），靠 Go 的交叉编译
# 产出目标架构二进制，比用 QEMU 模拟编译快一个数量级。
#   docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 .
#
# 版本注入：
#   docker build --build-arg VERSION=1.2.3 --build-arg REVISION=$(git rev-parse HEAD) .
# 未传时 VERSION 为 dev，二进制的 /health 与启动横幅会显示该值。

# ---------- 构建阶段 ----------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# 只拷贝依赖清单，先做依赖缓存层（依赖不变时这一层可复用）
COPY go.mod go.sum ./
RUN go env -w GOPROXY=https://goproxy.cn,direct && go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev

# CGO_ENABLED=0 + 静态链接，便于在软路由 / NAS 上直接运行。
# -trimpath 去掉本地路径；-buildvcs=false 保证在无 .git 的构建上下文里也能复现。
RUN set -eux; \
    export CGO_ENABLED=0 \
           GOOS="${TARGETOS:-linux}" \
           GOARCH="${TARGETARCH:-amd64}"; \
    case "${GOARCH}/${TARGETVARIANT:-}" in \
      arm/v6) export GOARM=6 ;; \
      arm/v7) export GOARM=7 ;; \
    esac; \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X github.com/jacktea/ysp-live-go/internal/httpapi.Version=${VERSION}" \
      -o /out/ysp-live ./cmd/ysp-live

# ---------- 运行阶段 ----------
FROM alpine:3.20

ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="ysp-live-go" \
      org.opencontainers.image.description="央视频全频道直播代理（Go 版，四层兜底单进程）" \
      org.opencontainers.image.source="https://github.com/jacktea/ysp-live-go" \
      org.opencontainers.image.url="https://github.com/jacktea/ysp-live-go" \
      org.opencontainers.image.documentation="https://github.com/jacktea/ysp-live-go#readme" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

ENV TZ=Asia/Shanghai \
    YSP_DATA_DIR=/app/data

RUN apk add --no-cache tzdata ca-certificates && \
    mkdir -p /app/data

WORKDIR /app
COPY --from=build /out/ysp-live /app/ysp-live
# 许可证随镜像分发（MIT 要求在副本中保留版权声明）
COPY LICENSE /app/LICENSE

VOLUME ["/app/data"]
EXPOSE 8767

# 与非 root 运行兼容：镜像内默认以 root 启动（与上游一致），
# 如需非 root 运行：docker run --user 1000:1000 -v ./data:/app/data ...
# 注意：这里刻意不放 HEALTHCHECK —— 端口可由 CMD 位置参数改成任意值，
#       写死端口的健康检查会在换端口时把容器误判为 unhealthy。/health 路由照常可用。
ENTRYPOINT ["/app/ysp-live"]
CMD ["8767"]
