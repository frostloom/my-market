# my-market ai 服务通用镜像（R2.7 服务拆分起）：多阶段构建 ./services/${SVC} →
# 静态二进制（CGO_ENABLED=0，运行层 alpine 无 libc 依赖）。
# 注意：不要加 `# syntax=docker/dockerfile:1` 指令——那会从 docker.io 拉取 frontend 镜像，
# 国内网络不可达，整个构建会卡死/DeadlineExceeded（2026-11-18 实测两次复现）。
# 内置 frontend 已支持本文件用到的全部特性（ARG before FROM / 多阶段 / COPY --from）。
# SVC 由 deploy/compose/docker-compose.yml 的 build.args 传入（全包路径）：
#   services/billing/cmd/billing | services/router/cmd/router | services/agent/cmd/agent
#   | services/gateway/cmd/gateway | services/mock-provider/cmd/mock-provider
# 构建上下文 = my-market 仓库根（单模块 github.com/frostloom/my-market；
# go:embed 的 services/gateway/internal/web/dist 已入库）。与 mall-service.Dockerfile
# 同位同口径，差异仅 SVC 语义（ai 侧为 services/<svc>/cmd/<svc> 全路径）。

# 国内 Docker Hub 直连不通，沿用 daocloud 镜像源（网络可达可换回 golang:1.26-alpine / alpine:3.20）。
ARG GO_IMAGE=docker.m.daocloud.io/library/golang:1.26-alpine
ARG RUN_IMAGE=docker.m.daocloud.io/library/alpine:3.20

FROM ${GO_IMAGE} AS build
# go 模块下载走 goproxy.cn（直连 proxy.golang.org 不通，与 mall 侧构建做法一致）
ENV GOPROXY=https://goproxy.cn,direct \
    GOWORK=off
WORKDIR /src
ARG SVC
COPY go.mod go.sum ./
RUN go mod download
# 单模块构建面：pkg/（mall 与 ai 公共库 + billing proto）+ proto/（mall 契约源）+
# services/（各服务入口与 internal，含 go:embed 的 web/dist）。tools/、eval/、docs/、
# deploy/ 由 .dockerignore 排除，不进上下文。
COPY pkg ./pkg
COPY proto ./proto
COPY services ./services
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./${SVC}

FROM ${RUN_IMAGE}
# agent 直连 https 上游（DeepSeek）需要 CA 根证书；构建层（golang alpine）自带，复制即可，运行层免 apk。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["app"]
