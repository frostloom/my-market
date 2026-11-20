# my-market mall 服务通用镜像（R1-T7）：多阶段构建 ./services/${SVC} → 静态二进制
# （CGO_ENABLED=0，运行层 alpine 无 libc 依赖）。
#
# SVC 由 deploy/compose/docker-compose.yml 的 build.args 传入：
#   mall-goods | mall-order | mall-bff
# 端口约定（见各服务 README / main.go）：
#   mall-goods  gRPC 9111 + HTTP 健康口 9110（/healthz）
#   mall-order  gRPC 9112 + HTTP 健康口 9120（/healthz）
#   mall-bff    HTTP 18090（/api/v1 + /healthz）
#
# 与 deploy/docker/go-service.Dockerfile 同源口径，差异两点：
#   1) SVC 语义：mall 侧为 services/<svc> 顶层包（无 cmd/ 层），ai 侧为
#      services/<svc>/cmd/<svc> 全路径；
#   2) ai 侧曾依赖的 services/ai-gateway replace/COPY（R2.6 跨模块形态）自 R2.7
#      起随单模块合并消失——billing proto 已在 pkg/proto/billing/v1，整树 COPY
#      已覆盖，无需任何额外放行。
#
# 注意：不要加 `# syntax=docker/dockerfile:1` 指令——那会从 docker.io 拉取 frontend
# 镜像，国内网络不可达，整个构建会卡死/DeadlineExceeded（ai 侧 2026-11-18 实测两次）。
# 内置 frontend 已支持本文件用到的全部特性（ARG before FROM / 多阶段 / COPY --from）。

# 国内 Docker Hub 直连不通，沿用 daocloud 镜像源（网络可达可换回 golang:1.26-alpine / alpine:3.20）。
ARG GO_IMAGE=docker.m.daocloud.io/library/golang:1.26-alpine
ARG RUN_IMAGE=docker.m.daocloud.io/library/alpine:3.20

FROM ${GO_IMAGE} AS build
# go 模块下载走 goproxy.cn（直连 proxy.golang.org 不通，与 ai 侧构建做法一致）
ENV GOPROXY=https://goproxy.cn,direct \
    GOWORK=off
WORKDIR /src
ARG SVC
COPY go.mod go.sum ./
RUN go mod download
# 根模块构建面：pkg/（mallerr/mallpage 公共库）+ proto/（契约 .proto 源）+
# services/（mallv1 生成码 + 各服务入口）。
COPY pkg ./pkg
COPY proto ./proto
COPY services ./services
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./services/${SVC}

FROM ${RUN_IMAGE}
# busybox wget 探活需要解析自身；CA 根证书备用（后续接真实支付/上游 https 时免 apk），
# 与 ai 侧运行层口径一致。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["app"]
