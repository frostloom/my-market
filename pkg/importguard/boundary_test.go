// import 边界守卫（R2 裁定）：mall 侧对 ai-gateway 只允许 import
// github.com/frostloom/ai-gateway/pkg/proto/billing/v1 前缀的包——
// billing 契约面；internal/** 及其余任何 ai-gateway 包一律拒绝。
//
// 实现：go/parser（ImportsOnly）扫 services/ 与 pkg/ 全部 .go 文件
// （含 _test.go——测试同样受边界约束）。当前仓库无越界 import → 恒绿；
// 未来任何越界 import 使本测试红，提示改走 billing proto 契约面。
package importguard

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 允许的 ai-gateway import 前缀（唯一暴露面）。
const allowedPrefix = "github.com/frostloom/ai-gateway/pkg/proto/billing/v1"

func TestImportBoundary_OnlyPkgProto(t *testing.T) {
	// 测试 cwd = 本包目录，仓库根在 ../..。
	root := filepath.Join("..", "..")
	// 只扫 mall 侧根模块代码。services/ai-gateway 子目录是 workspace 另一模块
	// （R2.6 结构归位后位于 services/ 下），自身代码不受此边界约束——其 internal
	// 规则由 Go 编译器强制，故遍历时显式跳过该子树。
	roots := []string{
		filepath.Join(root, "services"),
		filepath.Join(root, "pkg"),
	}
	// 异模块子树根（walk 中按目录路径精确匹配，整目录剪枝）。
	skipDir := filepath.Join(root, "services", "ai-gateway")

	fset := token.NewFileSet()
	scanned := 0
	for _, dir := range roots {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == "testdata" || name == ".git" {
					return fs.SkipDir
				}
				// services/ai-gateway = 独立 Go 模块（workspace 成员），不在守卫管辖区。
				if path == skipDir {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Errorf("解析 %s 失败: %v", path, err)
				return nil
			}
			scanned++
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				if !strings.Contains(p, "frostloom/ai-gateway") {
					continue
				}
				if !strings.HasPrefix(p, allowedPrefix) {
					t.Errorf("%s: import %q 越界——mall 侧仅允许 %q（billing proto 契约面）",
						path, p, allowedPrefix)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s 失败: %v", dir, err)
		}
	}
	// 扫描面自检：目录存在且至少扫到源文件，防止根路径漂移导致守卫空转。
	if scanned == 0 {
		if _, err := os.Stat(roots[0]); err != nil {
			t.Fatalf("扫描根不可达（测试 cwd 漂移？）: %v", err)
		}
		t.Fatalf("未扫描到任何 .go 文件（扫描根配置失效）")
	}
	t.Logf("import 边界守卫扫描 %d 个 Go 源文件，允许前缀 %s", scanned, allowedPrefix)
}
