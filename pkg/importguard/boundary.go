// 包主体：跨服务 import 边界规则与静态扫描（R2.7 裁定 A 白名单门禁改造版）。
// 规则口径与守卫自检的完整叙述见 doc.go 与 docs/spec/adr/ADR-011。
package importguard

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"strings"
)

const (
	// modulePath 当前唯一 Go 模块（R2.7 起全仓单模块）。
	modulePath = "github.com/frostloom/my-market"
	// legacyModulePath R2.7 合并前 ai-gateway 独立模块路径；整体退役，import 即拒。
	legacyModulePath = "github.com/frostloom/ai-gateway"
	// contractFace mall 对 AI 域的唯一合法契约面（R2 以来 allowedPrefix 语义保留）。
	contractFace = "pkg/proto/billing/v1"
)

// publicFace 裁定 A-i 的「树内非 internal 公开位」：包源码零改动放服务树里
// 非 internal 目录，边界靠本表白名单治理（同树 import 恒合法；跨树仅列名户）。
type publicFace struct {
	prefix    string   // Go import 路径的仓库相对前缀（含子包）
	owner     string   // 所属服务树（仓库相对目录前缀，带尾斜杠）
	crossTree []string // 允许的跨树 import 户（仓库相对目录前缀，带尾斜杠）
}

var publicFaces = []publicFace{
	// billing 数据层与 admin 观测面：agent 生产码直读属既有跨域债（ADR-011 入档），
	// tools 评测基建与 channelintegration 通道鉴权测试为裁定 A 白名单三户。
	{prefix: "services/billing/store", owner: "services/billing/",
		crossTree: []string{"services/agent/", "tools/", "services/billing/internal/channelintegration/"}},
	{prefix: "services/billing/admin", owner: "services/billing/",
		crossTree: []string{"services/agent/", "tools/", "services/billing/internal/channelintegration/"}},
	// router 公开位同法（mapping 裁定 D1：channelintegration 并入白名单机制）。
	{prefix: "services/router/router", owner: "services/router/",
		crossTree: []string{"services/agent/", "tools/", "services/billing/internal/channelintegration/"}},
	// agent 运行时公开位同法；现存跨树户只有 tools 评测基建。
	{prefix: "services/agent/agent", owner: "services/agent/",
		crossTree: []string{"tools/"}},
}

// mallForbiddenTrees mall 域禁越的 AI 服务树（mall 对 AI 域唯一契约面 = contractFace）。
var mallForbiddenTrees = []string{
	"services/billing/", "services/gateway/", "services/router/",
	"services/agent/", "services/mock-provider/",
}

// checkImport 判定单条 import 是否越界；返回 nil = 合法。
// importerRel 为仓库相对路径（含文件名），importPath 为完整 Go import 路径。
func checkImport(importerRel, importPath string) error {
	// 规则 0：退役模块路径（R2.7 单模块化后旧 import 路径整体死亡，复活即拒）。
	if importPath == legacyModulePath || strings.HasPrefix(importPath, legacyModulePath+"/") {
		return fmt.Errorf("退役模块路径 %s（R2.7 起全仓单模块 %s，请改用 %s/… 路径）",
			legacyModulePath, modulePath, modulePath)
	}
	repoPath, ok := repoRelOfImport(importPath)
	if !ok {
		return nil // 非本模块 import（stdlib/三方）不在管辖区。
	}
	// 规则 1：mall 域隔离——mall 禁越五个 AI 服务树；唯一合法契约面 pkg/proto/billing/v1。
	if isMallTree(importerRel) {
		for _, tree := range mallForbiddenTrees {
			if hasDirPrefix(repoPath, tree) {
				return fmt.Errorf("mall 域禁止 import AI 服务树 %s（唯一契约面 %s；跨树需求走 billing gRPC 契约或先入 ADR 白名单）",
					tree, contractFace)
			}
		}
	}
	// 规则 2：公开面白名单——裁定 A-i 四个非 internal 公开位按名单治理。
	for _, face := range publicFaces {
		if repoPath != face.prefix && !strings.HasPrefix(repoPath, face.prefix+"/") {
			continue
		}
		if hasDirPrefix(importerRel, face.owner) || hasAnyDirPrefix(importerRel, face.crossTree) {
			return nil
		}
		return fmt.Errorf("公开面 %s 跨树 import 非法：合法户 = 同树 %s + 白名单 %s",
			face.prefix, face.owner, strings.Join(face.crossTree, "、"))
	}
	return nil
}

// repoRelOfImport 把 Go import 路径折算成仓库相对路径；非本模块 import 返回 ok=false。
func repoRelOfImport(importPath string) (string, bool) {
	if importPath == modulePath {
		return "", true
	}
	if p, found := strings.CutPrefix(importPath, modulePath+"/"); found {
		return p, true
	}
	return "", false
}

// isMallTree importer 是否属于 mall 域服务树（services/mall-*）。
func isMallTree(importerRel string) bool {
	return strings.HasPrefix(importerRel, "services/mall-")
}

// hasDirPrefix path 是否位于 dir 目录下（dir 带尾斜杠语义）。
func hasDirPrefix(path, dir string) bool {
	return path == strings.TrimSuffix(dir, "/") || strings.HasPrefix(path, dir)
}

func hasAnyDirPrefix(path string, dirs []string) bool {
	for _, dir := range dirs {
		if hasDirPrefix(path, dir) {
			return true
		}
	}
	return false
}

// parseImports 解析单个 Go 文件的 import 列表（ImportsOnly，含 _test.go）。
// src 为 nil 时读 filename 磁盘文件（仓库全量扫描路径）；传源码文本则用于哨兵用例。
// 返回已去引号的完整 import 路径。
func parseImports(fset *token.FileSet, filename string, src any) ([]string, error) {
	f, err := parser.ParseFile(fset, filename, src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errors.New("空解析结果")
	}
	paths := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		paths = append(paths, strings.Trim(imp.Path.Value, `"`))
	}
	return paths, nil
}
