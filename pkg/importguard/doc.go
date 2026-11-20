// Package importguard 承载跨服务 import 边界门禁（R2.7 裁定 A 改造版，非删除）。
//
// R2.7 服务拆分后全仓单模块 github.com/frostloom/my-market，服务边界分两层强制：
//  1. 编译器：各服务 internal/** 的可见性由 Go internal 规则强制（树外 import
//     直接编译失败）——编译器管 internal；
//  2. 本守卫：编译器管不到的「公开面」——服务树内非 internal 的跨域包与 mall
//     域的越树 import——用白名单约束——门禁管公开面。
//
// 门禁规则（v2，用例规格见 boundary_test.go 的哨兵表）：
//
//   - 规则 0（退役路径）：github.com/frostloom/ai-gateway/** 独立模块已退役
//     （R2.7 合并进根模块），任何 import 一律拒绝；
//   - 规则 1（mall 域隔离）：services/mall-*（mall-bff/mall-goods/mall-order/mall-gen）
//     禁止 import services/{billing,gateway,router,agent,mock-provider}/**——mall 对
//     AI 域的唯一合法契约面 = pkg/proto/billing/v1（R2 以来 allowedPrefix 语义保留）；
//   - 规则 2（公开面白名单）：裁定 A-i 的树内非 internal 公开位按名单治理——
//     services/billing/{store,admin} 与 services/router/router 的跨树合法户 =
//     {services/agent/** 生产码, tools/** 评测基建, services/billing/internal/channelintegration}；
//     services/agent/agent 同法（合法户 = tools/**）；同树 import 恒合法；其余（尤其
//     mall）一律拒绝。pkg/** 伸手服务公开面同样拒绝（公共库不倒挂服务树）。
//
// 实现：go/parser（ImportsOnly）静态扫 services/ pkg/ tools/ 全部 .go 文件（含
// _test.go——测试同样受边界约束）。守卫自检：扫描面每根 >0 文件（防根漂移空转）+
// 合成哨兵用例（同一「解析→规则」管线对恶意源码文本必须报违规，防扫描与规则脱钩）。
// 当前仓库无越界 import → 恒绿；价值在防未来越界。
//
// 沿革：R2 双模块 replace 时代的守卫只管「mall→ai-gateway 仅许 pkg/proto/billing/v1」；
// 原 R2.7 计划「删除 importguard、编译器全接管」被裁定 A 修订——store/admin/agent/router
// 四个公开位留在服务树非 internal 目录（包源码零改动），边界改由本白名单门禁治理。
// 详见 docs/spec/adr/ADR-011。
package importguard
