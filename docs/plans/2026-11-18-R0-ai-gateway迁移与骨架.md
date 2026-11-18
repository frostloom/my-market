# R0：ai-gateway 迁移与工程骨架 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 ai-gateway 整仓迁入 my-market 形成双模块 monorepo，compose 分 profile 一键起 gateway 全家并通过冒烟。

**Architecture:** 整仓目录拷贝（模块路径不变）+ go.work 串联根模块与嵌套模块；compose 以 profile 划分 infra/ai/mall 三组（本计划交付 infra/ai，mall 组由 R1 添加）。

**Tech Stack:** Go 1.25 · docker compose v2 · MySQL 8(3307) · Redis(6381) · Kafka(宿主 29092)

**Spec:** [PRD.md](../PRD.md) §3/§10.1/§12 + [acceptance.md](../spec/acceptance.md) R0 节 + [ADR-001](../spec/adr/ADR-001-整仓迁移与双模块布局.md)

## Global Constraints

- 模块路径不变：`github.com/frostloom/ai-gateway`；根模块 `github.com/frostloom/my-market`
- 不迁：`.git/`、`logs/`、`.gocache/`、`worktrees/`、`web/node_modules/`、`bin/`、`.env`（密钥不进新仓，提供 `.env.example`）
- 必迁：`cmd/ internal/ web/(含 dist，go:embed 依赖) docs/ scripts/ deploy/ eval/ go.mod go.sum docker-compose.yml`（参考原件）
- dev 端口：MySQL 3307 · Redis 6381 · Kafka 容器 9092/宿主 **29092** · gateway 显式 `GATEWAY_PORT=18080`
- 新仓 git 从零初始化；提交信息用 conventional commits（`chore:`/`feat:` 前缀）

## Review Focus

1. **密钥泄漏**（.env / 内嵌 key 入新仓）→ Task 1 步骤 4 的 `git log --all --diff-filter=A -- '.env*'` 断言 + Task 4 验收项
2. **误迁大目录**（node_modules/.gocache/logs）→ Task 1 步骤 3 的体积断言
3. **go.work 破坏 ai-gateway 构建/测试**（路径或版本冲突）→ Task 2 步骤 5 双模块 `go build && go test` 断言
4. **gateway 默认端口 8080 踩坑**（compose 未显式注入）→ Task 3 步骤 5 健康检查必含 18080
5. **Kafka 宿主端口误用 9092**（连不上）→ Task 3 步骤 4 以 29092 探测

---

### Task 1: 迁移 ai-gateway 代码并初始化新仓

**Files:**
- Create: `ai-gateway/**`（拷贝）
- Create: `.gitignore`（根，含 `.env`、`logs/`、`*.log`、`.DS_Store`、`node_modules/`）

**Interfaces:**
- Produces: 新仓 git 仓库与 ai-gateway 目录（后续所有任务的容器）

- [ ] **Step 1: 拷贝代码（排除清单）**

```bash
rsync -a --exclude .git --exclude logs --exclude .gocache \
  --exclude worktrees --exclude web/node_modules --exclude bin \
  --exclude .env --exclude .DS_Store --exclude .superpowers --exclude .hallmark \
  /Users/a1/项目/ai-gateway/ /Users/a1/项目/my-market/ai-gateway/
```

- [ ] **Step 2: 验证目录齐全**

Run: `ls ai-gateway/cmd | sort`
Expected: agent, autoroute...（与旧仓 `ls /Users/a1/项目/ai-gateway/cmd | sort` 输出一致，且 gateway/billing/router/reconciler/event-consumer/mock-provider 在列）；`ls ai-gateway/{internal,web,docs,scripts,deploy,eval}` 均非空；`test -f ai-gateway/web/dist/index.html || ls ai-gateway/web/dist`（dist 在）。

- [ ] **Step 3: 验证排除生效（体积断言）**

Run: `du -sh ai-gateway && test ! -d ai-gateway/.git && test ! -d ai-gateway/web/node_modules && test ! -f ai-gateway/.env`
Expected: 体积约 200-400MB（eval 220M + web/dist + 源码），无 GB 级；三个 test 均成功（退出码 0）。

- [ ] **Step 4: 初始化 git 与 .gitignore，首提交并验证无密钥**

根目录写 `.gitignore`（上列排除项 + `deploy/compose/.env`）后：

```bash
git init && git add -A
git log --all --diff-filter=A --name-only -- '.env*' '*secret*' | grep -v example || true
```
Expected: 除 `.env.example` 外无输出。然后 `git commit -m "chore: migrate ai-gateway into monorepo (history stays in archived repo)"`。

---

### Task 2: 根模块与 go.work

**Files:**
- Create: `go.mod`（根模块）、`go.work`
- Create: `services/mall-bff/README.md`、`services/mall-goods/README.md`、`services/mall-order/README.md`、`services/seckill/README.md`、`deploy/compose/README.md`（占位，各一行说明归属阶段；v0.5 注：原列出的 app/harmony/README.md 已随鸿蒙作业取消移除）

**Interfaces:**
- Produces: 根模块 `github.com/frostloom/my-market`；go.work 含 `.` 与 `./ai-gateway`（R1 的 mall 服务代码全部位于根模块）

- [ ] **Step 1: 初始化根模块**

Run: `go mod init github.com/frostloom/my-market`（Go 版本对齐 ai-gateway：1.25）

- [ ] **Step 2: 写 go.work**

内容：`go 1.25` + `use (./ai-gateway 与 .)`。不 `go work sync`（根模块暂无依赖）。

- [ ] **Step 3: 建骨架目录与占位 README**（上列 Files）

- [ ] **Step 4: 双模块构建验证**

Run: `go build ./... && (cd ai-gateway && go build ./... && go vet ./...)`
Expected: 均零错误（证明模块路径未变、workspace 未破坏原仓）。

- [ ] **Step 5: ai-gateway 原有单测仍绿**

Run: `cd ai-gateway && go test ./internal/... 2>&1 | tail -5`
Expected: 全 PASS（无编译失败；个别依赖环境的用例按旧仓现状记录，不新增失败）。

- [ ] **Step 6: Commit** — `chore: root module skeleton + go.work workspace`

---

### Task 3: compose 分 profile（infra + ai）

**Files:**
- Create: `deploy/compose/docker-compose.yml`、`deploy/compose/.env.example`

**Interfaces:**
- Consumes: ai-gateway 旧 `docker-compose.yml` 的镜像/参数与环境变量约定（X-Control-Token、AGENT_LLM_API_KEY 等，值从 `.env.example` 模板化）
- Produces: `docker compose -f deploy/compose/docker-compose.yml --profile infra|ai up -d`；R1 将追加 `--profile mall`

- [ ] **Step 1: 搬运并改造 compose**

以旧仓 compose 为底：MySQL(3307, utf8mb4)、Redis(6381)、Kafka(`9092:29092` 映射) 归 `profiles: [infra]`；gateway/billing/router/agent/mock-provider 归 `profiles: [ai]`，gateway 环境显式 `GATEWAY_PORT=18080`；控制面令牌等敏感值全部走 `${VAR}` 引 `.env`。`.env.example` 逐项列变量与生成命令注释（如 `openssl rand -base64 32`）。

- [ ] **Step 2: `--profile infra up` 并验证**

Run: `docker compose -f deploy/compose/docker-compose.yml --profile infra up -d && sleep 8 && nc -z 127.0.0.1 3307 && nc -z 127.0.0.1 6381 && nc -z 127.0.0.1 29092`
Expected: 全部连通（注意 Kafka 宿主是 **29092**）。

- [ ] **Step 3: `--profile ai up` 并健康检查**

Run: `docker compose ... --profile ai up -d && for p in 18080 9101 9103 9102 9105; do curl -sf localhost:$p/healthz >/dev/null && echo "$p ok"; done`
Expected: 五个端口全 ok（18080 在列=验证 GATEWAY_PORT 注入成功）。

- [ ] **Step 4: Commit** — `chore: compose profiles infra/ai`

---

### Task 4: R0 冒烟与验收

**Files:**
- Create: `scripts/smoke-r0.sh`

**Interfaces:**
- Consumes: ai-gateway 既有 `scripts/seed*`（造演示租户/余额）与 README 快速开始的 /v1 调用方式

- [ ] **Step 1: 写冒烟脚本**

脚本依次：全 healthz → 运行旧仓 seed（或等价 SQL）造一个有余额的租户+key → `curl -X POST :18080/v1/chat/completions`（Bearer key，mock provider）→ 调 billing admin 查账断言出现 reserve+settle 两行（grep 计数=2）→ 输出 PASS/FAIL 汇总。

- [ ] **Step 2: 运行**

Run: `bash scripts/smoke-r0.sh`
Expected: 末行 `SMOKE R0: PASS`。

- [ ] **Step 3: 对照 acceptance.md R0 逐项勾选**（清单 8 项全绿，含旧仓归档与 README 指向——归档动作需用户在场时执行，此项标记"待用户操作"）

- [ ] **Step 4: Commit** — `test: r0 smoke script`

**R0 完成定义：acceptance R0 可执行项全绿 + 冒烟 PASS。**
