package localpaired

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// prepareEvalAssetLayout 建立与真实仓库同构的布局：root 对应仓库根且即工具 cwd
// （R2.7 起单模块，工具以仓库根运行），评测资产在 root/eval/ai-gateway。
func prepareEvalAssetLayout(t *testing.T) (root, work string) {
	t.Helper()
	root = t.TempDir()
	work = root
	return root, work
}

func TestCleanRejectsProtectedRoots(t *testing.T) {
	root, work := prepareEvalAssetLayout(t)
	t.Chdir(work)

	ledger := filepath.Join(root, "billing", "ledger")
	frozen := filepath.Join(root, "eval", "ai-gateway", "task-definitions", "2026-10-12-full-dev-v1")
	langfuse := filepath.Join(root, "deploy", "langfuse")
	allowed := filepath.Join(root, "old-output")
	nested := filepath.Join(allowed, "nested")
	for _, dir := range []string{ledger, frozen, langfuse, nested} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	allow := []string{ledger, frozen, langfuse, allowed}

	if err := Clean(allow, allowed); err == nil {
		t.Fatal("expected error when the new question bank file is absent")
	}
	assertKept(t, nested)

	bank := filepath.Join(root, "eval", "ai-gateway", "campaigns", "2026-11-10-local-paired", "cases.json")
	if err := os.MkdirAll(filepath.Dir(bank), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bank, []byte("local-read-history-001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Clean(allow, allowed); err == nil {
		t.Fatal("expected error when the four task 2 cases are absent")
	}
	assertKept(t, nested)

	const cases = "local-read-history-001\nlocal-offtopic-history-001\nlocal-read-pending-001\nlocal-offtopic-pending-001\n"
	if err := os.WriteFile(bank, []byte(cases), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{ledger, frozen, langfuse} {
		if err := Clean(allow, dir); err == nil {
			t.Fatalf("Clean(%q) succeeded, want error", dir)
		}
		assertKept(t, dir)
	}
	if err := Clean(allow, nested); err == nil {
		t.Fatal("expected error for a path that is not an exact allowlisted entry")
	}
	assertKept(t, nested)

	if err := Clean(allow, allowed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(allowed); !os.IsNotExist(err) {
		t.Fatalf("allowlisted path = %v, want removed", err)
	}
	for _, dir := range []string{ledger, frozen, langfuse} {
		assertKept(t, dir)
	}
}

func assertKept(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatalf("path %q was deleted: %v", dir, err)
	}
}

func TestCleanRejectsProtectedVariants(t *testing.T) {
	root, work := prepareEvalAssetLayout(t)
	t.Chdir(work)
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("billing", "ledger", "keep.txt"), "keep")
	write(filepath.Join("eval", "ai-gateway", "task-definitions", "2026-10-12-full-dev-v1", "keep.txt"), "keep")
	write(filepath.Join("deploy", "langfuse", "keep.txt"), "keep")
	write(filepath.Join("old-output", "keep.txt"), "keep")
	bank := filepath.Join(root, "eval", "ai-gateway", "campaigns", "2026-11-10-local-paired", "cases.json")
	if err := os.MkdirAll(filepath.Dir(bank), 0o755); err != nil {
		t.Fatal(err)
	}
	const cases = "local-read-history-001\nlocal-offtopic-history-001\nlocal-read-pending-001\nlocal-offtopic-pending-001\n"
	if err := os.WriteFile(bank, []byte(cases), 0o644); err != nil {
		t.Fatal(err)
	}

	variants := []struct {
		name   string
		allow  string
		target string
	}{
		{name: "dotdot cleans away langfuse", target: "deploy/langfuse/.."},
		{name: "allow list cleans to langfuse parent", allow: "deploy", target: "deploy/langfuse/.."},
		{name: "dotdot cleans away frozen definitions", target: "eval/ai-gateway/task-definitions/2026-10-12-full-dev-v1/.."},
		{name: "dotdot cleans to current directory", target: "deploy/langfuse/../.."},
		{name: "dotdot cleans allowlisted output to dot", target: "old-output/.."},
		{name: "dotdot returns to allowlisted output", target: "old-output/../old-output"},
		{name: "parent of langfuse", target: "deploy"},
		{name: "parent of frozen definitions", target: "eval/ai-gateway/task-definitions"},
		{name: "ancestor of frozen definitions", target: "eval"},
		{name: "langfuse case variant", target: "deploy/Langfuse"},
		{name: "frozen definitions case variant", target: "Eval/AI-Gateway/Task-Definitions/2026-10-12-full-dev-v1"},
		{name: "billing ledger case variant", target: "Billing/Ledger"},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			before := keepFiles(t, root)
			allow := variant.allow
			if allow == "" {
				allow = variant.target
			}
			err := Clean([]string{allow}, variant.target)
			if err == nil || !strings.Contains(err.Error(), "protected") {
				t.Fatalf("err = %v, want a protected-path refusal", err)
			}
			after := keepFiles(t, root)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("keep files = %v, want %v", after, before)
			}
		})
	}
}

func keepFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "keep.txt" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCleanRejectsAbsoluteWorkdirAndRoot(t *testing.T) {
	root, work := prepareEvalAssetLayout(t)
	t.Chdir(work)
	for _, dir := range []string{
		filepath.Join(root, "billing", "ledger"),
		filepath.Join(root, "eval", "ai-gateway", "task-definitions", "2026-10-12-full-dev-v1"),
		filepath.Join(root, "deploy", "langfuse"),
		filepath.Join(root, "old-output"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bank := filepath.Join(root, "eval", "ai-gateway", "campaigns", "2026-11-10-local-paired", "cases.json")
	if err := os.MkdirAll(filepath.Dir(bank), 0o755); err != nil {
		t.Fatal(err)
	}
	const cases = "local-read-history-001\nlocal-offtopic-history-001\nlocal-read-pending-001\nlocal-offtopic-pending-001\n"
	if err := os.WriteFile(bank, []byte(cases), 0o644); err != nil {
		t.Fatal(err)
	}
	targets := []string{
		work,
		string(filepath.Separator),
		filepath.Join(work, "deploy"),
		filepath.Join(work, "eval"),
		filepath.Join(work, "billing", "ledger"),
		filepath.Join(work, "deploy", "Langfuse"),
		filepath.Join(root, "eval", "ai-gateway", "task-definitions", "2026-10-12-full-dev-v1"),
	}
	before := keepFiles(t, root)
	for _, target := range targets {
		err := Clean([]string{target}, target)
		if err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("Clean(%q) err = %v, want a protected-path refusal", target, err)
		}
	}
	after := keepFiles(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("keep files = %v, want %v", after, before)
	}
}

func TestCleanAllowsUnrelatedDeployAndEval(t *testing.T) {
	root, work := prepareEvalAssetLayout(t)
	t.Chdir(work)
	for _, dir := range []string{
		filepath.Join(root, "billing", "ledger"),
		filepath.Join(root, "eval", "ai-gateway", "task-definitions", "2026-10-12-full-dev-v1"),
		filepath.Join(root, "deploy", "langfuse"),
		filepath.Join(root, "old-output"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bank := filepath.Join(root, "eval", "ai-gateway", "campaigns", "2026-11-10-local-paired", "cases.json")
	if err := os.MkdirAll(filepath.Dir(bank), 0o755); err != nil {
		t.Fatal(err)
	}
	const cases = "local-read-history-001\nlocal-offtopic-history-001\nlocal-read-pending-001\nlocal-offtopic-pending-001\n"
	if err := os.WriteFile(bank, []byte(cases), 0o644); err != nil {
		t.Fatal(err)
	}
	targets := []string{
		filepath.Join("side", "deploy"),
		filepath.Join("side", "eval"),
	}
	for _, dir := range targets {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range targets {
		abs := filepath.Join(work, dir)
		if err := Clean([]string{abs}, abs); err != nil {
			t.Fatalf("Clean(%q) = %v, want removal of an unrelated directory", abs, err)
		}
		if _, err := os.Stat(abs); !os.IsNotExist(err) {
			t.Fatalf("unrelated path %q = %v, want removed", abs, err)
		}
	}
	for _, dir := range []string{
		filepath.Join(root, "billing", "ledger"),
		filepath.Join(root, "eval", "ai-gateway", "task-definitions", "2026-10-12-full-dev-v1"),
		filepath.Join(root, "deploy", "langfuse"),
		filepath.Join(root, "old-output"),
	} {
		assertKept(t, dir)
	}
}
