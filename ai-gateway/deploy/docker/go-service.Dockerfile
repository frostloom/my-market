# my-market 通用 Go 服务镜像：多阶段构建 ./cmd/${SVC} → 静态二进制（CGO_ENABLED=0）。
# 注意：不要加 `# syntax=docker/dockerfile:1` 指令——那会从 docker.io 拉取 frontend 镜像，
# 国内网络不可达，整个构建会卡死/DeadlineExceeded（2026-11-18 实测两次复现）。
# 内置 frontend 已支持本文件用到的全部特性（ARG before FROM / 多阶段 / COPY --from）。
# SVC 由 deploy/compose/docker-compose.yml 的 build.args 传入
# （gateway | billing | router | agent | mock-provider）。
# 构建上下文 = ai-gateway/ 仓库根（go:embed 的 internal/gateway/web/dist 已入库）。

# 国内 Docker Hub 直连不通，沿用 daocloud 镜像源（网络可达可换回 golang:1.25-alpine / alpine:3.20）。
ARG GO_IMAGE=docker.m.daocloud.io/library/golang:1.25-alpine
ARG RUN_IMAGE=docker.m.daocloud.io/library/alpine:3.20

FROM ${GO_IMAGE} AS build
# go 模块下载走 goproxy.cn（直连 proxy.golang.org 不通，与旧仓 README 构建做法一致）
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
ARG SVC
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SVC}

FROM ${RUN_IMAGE}
# agent 直连 https 上游（DeepSeek）需要 CA 根证书；构建层（golang alpine）自带，复制即可，运行层免 apk。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["app"]
