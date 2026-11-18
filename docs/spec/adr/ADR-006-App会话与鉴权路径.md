# ADR-006 App 会话：gateway 代理 + 内部凭证数据面

- 状态：已接受（对应 PRD D14；N9 扩展）
- 日期：2026-11-18

## 背景

App 需要账密登录（billing `/auth/*` 已有，但挂在 9103 admin 端口且**全端口要求 X-Control-Token**，会话以 HttpOnly cookie 传递）。App 不适合 cookie。

## 决策

- 会话验证：mall-bff 收 `Authorization: Bearer <会话token>`，经 **gateway :18080 `/auth/*` 代理**验证（gateway 已实现注入 X-Control-Token + cookie 透传），mall-bff 不直连 9103——billing 零改动；
- 数据查询（余额/流水/订阅）：mall-bff 以内部服务身份持 X-Control-Token 调 billing gRPC `GetBalance` 与 admin 查询；
- API key 定位不变：仅外部程序调 gateway /v1，App 内后置提供管理页（P4）。

## 理由

- 复用现成代理链路，避免 mall-bff 重新实现控制令牌与 cookie 语义；
- 内部服务持有控制令牌与 gateway 自身行为一致，边界清楚；
- App 无状态 token 模型与移动端习惯、gateway 数据面同构。

## 后果

- mall-bff 对 gateway 有一次轻依赖（会话验证转发，每请求或短缓存）；
- 部署配置多一项：mall-bff 需可达 gateway:18080 与 billing:9101/9103（后者持令牌）。
