package importguard

import (
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 规则哨兵用例（表驱动）：正向 = 白名单内合法户，反向 = 必须拒绝的越界形态。
// 本表即 R2.7-T3 门禁的规格（TDD）：任何规则改动先改表再改码。
// 裁定来源：r2.7-mapping.md §11 裁定 A（A-i 公开位 + 白名单门禁强化）。
func TestRule_SentinelCases(t *testing.T) {
	const mod = "github.com/frostloom/my-market/"
	cases := []struct {
		name      string
		importer  string // 仓库相对路径（含文件名）
		importPkg string // Go import 路径
		wantOK    bool   // true = 必须放行，false = 必须报违规
	}{
		// —— 正向：白名单合法户（裁定 A-i 三户口径）——
		{"agent 生产码直读 billing store（既有跨域债，白名单内）",
			"services/agent/agent/tools.go", mod + "services/billing/store", true},
		{"agent 域测试直读 billing store（同域测试同样合法）",
			"services/agent/agent/chat_test.go", mod + "services/billing/store", true},
		{"tools 评测基建 import billing admin",
			"tools/internal/taskbench/paired_adapter.go", mod + "services/billing/admin", true},
		{"tools 评测基建 import agent 公开位",
			"tools/internal/localpaired/live.go", mod + "services/agent/agent", true},
		{"channelintegration 集成测试 import billing store",
			"services/billing/internal/channelintegration/channel_auth_test.go", mod + "services/billing/store", true},
		{"channelintegration 集成测试 import router 公开位",
			"services/billing/internal/channelintegration/channel_auth_test.go", mod + "services/router/router", true},
		{"billing 同树 import store（cmd 与 internal 皆同域）",
			"services/billing/cmd/billing/main.go", mod + "services/billing/store", true},
		{"billing 同树 internal 伸手 store",
			"services/billing/internal/server/service.go", mod + "services/billing/store", true},
		{"router 同树 import 公开位",
			"services/router/cmd/router/main.go", mod + "services/router/router", true},
		{"mall 的唯一 AI 契约面 pkg/proto/billing/v1（allowedPrefix 语义保留）",
			"services/mall-order/billing_client.go", mod + "pkg/proto/billing/v1", true},
		{"mall 同域互认（mall-bff→mall-gen 生成物）",
			"services/mall-bff/gclient.go", mod + "services/mall-gen/mallv1", true},
		{"agent 同树 internal 不受公开面约束",
			"services/agent/cmd/agent/main.go", mod + "services/agent/internal/knowledge", true},
		{"非模块 import（stdlib/三方）不受管",
			"services/mall-goods/main.go", "google.golang.org/grpc", true},
		{"前缀形近不误伤：billing/storage 不属 store 面（非 mall 户不受管）",
			"tools/internal/taskbench/x.go", mod + "services/billing/storage", true},

		// —— 反向：越界必须红 ——
		{"mall→billing store 公开位（恶意形态①，门禁测试实证用例）",
			"services/mall-order/malicious.go", mod + "services/billing/store", false},
		{"mall→router 公开位（非白名单树 import router）",
			"services/mall-bff/malicious.go", mod + "services/router/router", false},
		{"mall→agent 公开位",
			"services/mall-order/malicious.go", mod + "services/agent/agent", false},
		{"mall→billing internal（编译器同样拦截，门禁双保险）",
			"services/mall-goods/malicious.go", mod + "services/billing/internal/server", false},
		{"mall→gateway 树",
			"services/mall-order/malicious.go", mod + "services/gateway/internal/handler", false},
		{"mall→mock-provider 树",
			"services/mall-bff/malicious.go", mod + "services/mock-provider/cmd/mock-provider", false},
		{"gateway→router 公开位（非白名单树）",
			"services/gateway/cmd/gateway/malicious.go", mod + "services/router/router", false},
		{"gateway→agent 公开位（非白名单树）",
			"services/gateway/internal/handler/malicious.go", mod + "services/agent/agent", false},
		{"billing 树伸手 agent 公开位（跨且非白名单户）",
			"services/billing/cmd/billing/malicious.go", mod + "services/agent/agent", false},
		{"pkg 公共库伸手服务公开面（分层倒置）",
			"pkg/jev/malicious.go", mod + "services/billing/store", false},
		{"seckill 非白名单树伸手 billing store（其余一律拒绝）",
			"services/seckill/malicious.go", mod + "services/billing/store", false},
		{"退役模块路径（R2.7 合并前旧 import）复活即拒",
			"services/mall-order/malicious.go", "github.com/frostloom/ai-gateway/pkg/proto/billing/v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkImport(tc.importer, tc.importPkg)
			if tc.wantOK && err != nil {
				t.Fatalf("合法 import 被误拒: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("越界 import 未被拦截: %s → %s", tc.importer, tc.importPkg)
			}
		})
	}
}

// 合成哨兵：恶意源码文本经同一「解析→规则」管线必须恰报 1 条违规
// （防静态扫描路径与规则函数脱钩导致守卫空转）。
func TestScanFile_SentinelPipelineDetectsViolation(t *testing.T) {
	const src = `package main

import (
	_ "github.com/frostloom/my-market/pkg/proto/billing/v1"
	_ "github.com/frostloom/my-market/services/billing/store"
)
`
	fset := token.NewFileSet()
	imports, err := parseImports(fset, "services/mall-order/sentinel_probe.go", src)
	if err != nil {
		t.Fatalf("哨兵源码解析失败: %v", err)
	}
	if len(imports) != 2 {
		t.Fatalf("哨兵应解析出 2 条 import，实得 %d: %v", len(imports), imports)
	}
	violations := 0
	for _, imp := range imports {
		if err := checkImport("services/mall-order/sentinel_probe.go", imp); err != nil {
			violations++
			t.Logf("哨兵捕获违规: %v", err)
		}
	}
	if violations != 1 {
		t.Fatalf("哨兵失效：期望恰 1 条违规（store 越界），实得 %d", violations)
	}
}

// 仓库全量扫描：services/ pkg/ tools/ 全部 .go 文件（含 _test.go）当前零越界 → 恒绿；
// 任何未来越界 import 使本测试红。扫描面自检：每根 >0 文件，防根路径漂移守卫空转。
func TestImportBoundary_RepoScanGreen(t *testing.T) {
	// 测试 cwd = 本包目录（pkg/importguard），仓库根在 ../..。
	root := filepath.Join("..", "..")
	roots := []string{"services", "pkg", "tools"}
	counts := make(map[string]int, len(roots))
	fset := token.NewFileSet()

	for _, dir := range roots {
		full := filepath.Join(root, dir)
		err := filepath.WalkDir(full, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "testdata", ".git", "node_modules":
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			imports, err := parseImports(fset, path, nil)
			if err != nil {
				t.Errorf("解析 %s 失败: %v", path, err)
				return nil
			}
			counts[dir]++
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			for _, imp := range imports {
				if impErr := checkImport(rel, imp); impErr != nil {
					t.Errorf("%s: import %q 越界: %v", rel, imp, impErr)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s 失败: %v", full, err)
		}
	}
	// 扫描面自检：每根至少扫到一个源文件（根漂移或目录丢失即 Fatal）。
	total := 0
	for _, dir := range roots {
		if counts[dir] == 0 {
			if _, statErr := os.Stat(filepath.Join(root, dir)); statErr != nil {
				t.Fatalf("扫描根 %s 不可达（测试 cwd 漂移？）: %v", dir, statErr)
			}
			t.Fatalf("扫描根 %s 未扫到任何 .go 文件（守卫空转）", dir)
		}
		total += counts[dir]
	}
	t.Logf("import 边界门禁（白名单版）扫描 %d 个 Go 源文件（services=%d pkg=%d tools=%d），零越界",
		total, counts["services"], counts["pkg"], counts["tools"])
}
