# ysp-live-go —— 央视频全频道直播代理（Go 单二进制版）
#
# 与原项目 Dockerfile 的区别：
#   原版需要 python:3.12-alpine + nodejs 运行两个进程（Python 网关 + Node 兜底引擎）。
#   Go 版把四层兜底全部编译进单个静态二进制，镜像里不需要 Python/Node，
#   Web/WASM 引擎用 wazero 在进程内执行（WASM 以 //go:embed 打包）。
FROM golang:1.26-alpine AS build

WORKDIR /src

# 只拷贝依赖清单，先做依赖缓存层
COPY go.mod go.sum ./
RUN go env -w GOPROXY=https://goproxy.cn,direct && go mod download

COPY . .
# CGO_ENABLED=0 + 静态链接，便于在软路由 / NAS 上直接运行
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ysp-live ./cmd/ysp-live

FROM alpine:3.20

LABEL description="央视频全频道直播代理（Go 版，四层兜底单进程）"

ENV TZ=Asia/Shanghai \
    YSP_DATA_DIR=/app/data

RUN apk add --no-cache tzdata ca-certificates && \
    mkdir -p /app/data

WORKDIR /app
COPY --from=build /out/ysp-live /app/ysp-live

VOLUME ["/app/data"]
EXPOSE 8767

# 与非 root 运行兼容：镜像内默认以 root 启动（与上游一致），
# 如需非 root 运行：docker run --user 1000:1000 -v ./data:/app/data ...
ENTRYPOINT ["/app/ysp-live"]
CMD ["8767"]
