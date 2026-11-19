// Package importguard 承载跨服务 import 边界守卫测试。
//
// R2 裁定：mall 侧（services/ + pkg/ 全部 Go 源文件）对 ai-gateway 的唯一
// 允许 import 面 = github.com/frostloom/ai-gateway/pkg/proto/billing/v1
// （T1 把 billing proto 从 internal/proto 移到 pkg/proto 就是为了这个暴露面）。
// ai-gateway 的 internal/** 一律不得被 mall 侧 import（含测试文件）。
//
// 守卫测试 TestImportBoundary_OnlyPkgProto 用 go/parser 静态扫描两个目录的
// 全部 .go 文件 import 列表做断言——当前无越界 import 时恒绿（守卫性质，
// 价值在防未来越界），不依赖任何运行时基建。services/ai-gateway/ 子树
// （R2.6 结构归位后位于 services/ 下的独立 Go 模块）不在扫描面——其自身
// 的 internal 规则由 Go 编译器强制，无需守卫代劳。
package importguard
