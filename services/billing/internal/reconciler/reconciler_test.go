package reconciler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	billingv1 "github.com/frostloom/my-market/pkg/proto/billing/v1"
	"github.com/frostloom/my-market/services/billing/store"
)

// 集成测试（真 MySQL，skip-if-unreachable；与 consumer 测试同模式）。
// 独立测试库，避免与 store 包测试互踩。billing 侧写操作用桩客户端记录调用，
// 只验证 reconciler 的「发现 + 触发 + 计数」——幂等语义由 billing 自身测试覆盖。
const testMySQLHost = "root:root@tcp(127.0.0.1:3307)/"
const testMySQLDB = "ai_gateway_reconciler_test"

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	admin, err := sql.Open("mysql", testMySQLHost)
	if err != nil {
		t.Skipf("mysql not reachable: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE IF NOT EXISTS " + testMySQLDB + " CHARACTER SET utf8mb4"); err != nil {
		t.Skipf("create test db: %v", err)
	}
	admin.Close()
	dsn := testMySQLHost + testMySQLDB + "?charset=utf8mb4&parseTime=True&loc=Local"
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Skipf("open mysql: %v", err)
	}
	st := store.NewStore(db, nil)
	if err := st.AutoMigrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, tbl := range []string{"bills", "tenants"} {
		if err := db.Exec("DELETE FROM " + tbl).Error; err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
	return db
}

// fakeBilling 记录 Reverse/Settle/RebuildBalance 调用；未覆盖的方法若被调用
// 会因嵌入 nil 接口而 panic——正是测试想捕捉的越界行为。
type fakeBilling struct {
	billingv1.BillingServiceClient
	mu           sync.Mutex
	reverseCalls []string
	settleCalls  []string
	rebuildCalls []uint64

	reverseErr error
	reverseNil bool
	settleErr  error
	settleNil  bool
	rebuildErr error
}

func (f *fakeBilling) Reverse(ctx context.Context, req *billingv1.ReverseRequest, opts ...grpc.CallOption) (*billingv1.ReverseResponse, error) {
	f.mu.Lock()
	f.reverseCalls = append(f.reverseCalls, req.GetRequestId())
	f.mu.Unlock()
	if f.reverseErr != nil {
		return nil, f.reverseErr
	}
	if f.reverseNil {
		return nil, nil
	}
	return &billingv1.ReverseResponse{Reversed: true, RefundQuota: req.GetBillId()}, nil
}

func (f *fakeBilling) Settle(ctx context.Context, req *billingv1.SettleRequest, opts ...grpc.CallOption) (*billingv1.SettleResponse, error) {
	f.mu.Lock()
	f.settleCalls = append(f.settleCalls, req.GetRequestId())
	f.mu.Unlock()
	if f.settleErr != nil {
		return nil, f.settleErr
	}
	if f.settleNil {
		return nil, nil
	}
	return &billingv1.SettleResponse{Settled: true}, nil
}

func (f *fakeBilling) RebuildBalance(ctx context.Context, req *billingv1.RebuildBalanceRequest, opts ...grpc.CallOption) (*billingv1.RebuildBalanceResponse, error) {
	f.mu.Lock()
	f.rebuildCalls = append(f.rebuildCalls, uint64(req.GetTenantId()))
	f.mu.Unlock()
	if f.rebuildErr != nil {
		return nil, f.rebuildErr
	}
	return &billingv1.RebuildBalanceResponse{Balance: 100}, nil
}

func newTestReconciler(t *testing.T, opts ...Option) (*Reconciler, *fakeBilling, *gorm.DB) {
	t.Helper()
	db := openTestDB(t)
	fake := &fakeBilling{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(db, fake, log, time.Hour, opts...)
	return r, fake, db
}

func seedBill(t *testing.T, db *gorm.DB, requestID string, tenantID uint64, age time.Duration, usageAt *time.Time) uint64 {
	t.Helper()
	row := store.Bill{
		RequestID:       requestID,
		Phase:           store.PhaseReserve,
		Status:          store.StatusPending,
		TenantID:        tenantID,
		PreQuota:        100,
		CreatedAt:       time.Now().Add(-age),
		UsageReportedAt: usageAt,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed bill %s: %v", requestID, err)
	}
	return row.ID
}

func seedTenant(t *testing.T, db *gorm.DB, id uint64, status int) {
	t.Helper()
	if err := db.Exec(
		"INSERT INTO tenants (id, name, status, initial_quota, ledger_balance) "+
			"VALUES (?, ?, ?, 1000, 1000) ON DUPLICATE KEY UPDATE status = VALUES(status)",
		id, fmt.Sprintf("recon-tenant-%d", id), status).Error; err != nil {
		t.Fatalf("seed tenant %d: %v", id, err)
	}
}

// 1. 默认姿态（D16）：过期 pending 无 usage → 保留待核查，零 RPC。
func TestSweepOnce_HoldsNoUsageByDefault(t *testing.T) {
	r, fake, _ := newTestReconciler(t)
	seedBill(t, r.db, "recon-hold-1", 1, 2*time.Hour, nil)

	stats, err := r.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if stats.Scanned != 1 || stats.Held != 1 || stats.Reversed != 0 || stats.Settled != 0 {
		t.Fatalf("stats = %+v, want {Scanned:1 Held:1}", stats)
	}
	if len(fake.reverseCalls) != 0 || len(fake.settleCalls) != 0 {
		t.Fatalf("default posture must issue no RPCs: reverse=%v settle=%v", fake.reverseCalls, fake.settleCalls)
	}
	if stats.HeldOldestCreatedAt.IsZero() {
		t.Fatalf("HeldOldestCreatedAt should be populated for observability")
	}
}

// 2. 有 usage 标记的过期 pending → 补 Settle。
func TestSweepOnce_SettlesUsageReported(t *testing.T) {
	r, fake, _ := newTestReconciler(t)
	usage := time.Now().Add(-time.Hour)
	seedBill(t, r.db, "recon-settle-1", 1, 2*time.Hour, &usage)

	stats, err := r.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if stats.Settled != 1 || stats.SettleErrors != 0 {
		t.Fatalf("stats = %+v, want Settled:1", stats)
	}
	if len(fake.settleCalls) != 1 || fake.settleCalls[0] != "recon-settle-1" {
		t.Fatalf("settle calls = %v, want [recon-settle-1]", fake.settleCalls)
	}
}

// 3. 显式 WithAutoReverse（legacy）：无 usage 过期 pending → 冲正。
func TestSweepOnce_AutoReverseWhenEnabled(t *testing.T) {
	r, fake, _ := newTestReconciler(t, WithAutoReverse())
	seedBill(t, r.db, "recon-rev-1", 1, 2*time.Hour, nil)

	stats, err := r.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if stats.Reversed != 1 || len(fake.reverseCalls) != 1 || fake.reverseCalls[0] != "recon-rev-1" {
		t.Fatalf("stats = %+v reverse = %v, want Reversed:1", stats, fake.reverseCalls)
	}
}

// 4. 宽限期内与终态行跳过。
func TestSweepOnce_SkipsYoungAndTerminal(t *testing.T) {
	r, _, _ := newTestReconciler(t)
	seedBill(t, r.db, "recon-young-1", 1, time.Minute, nil) // 宽限期内
	usage := time.Now().Add(-time.Hour)
	id := seedBill(t, r.db, "recon-done-1", 1, 2*time.Hour, &usage)
	if err := r.db.Exec("UPDATE bills SET status = ? WHERE id = ?", store.StatusSettled, id).Error; err != nil {
		t.Fatalf("mark settled: %v", err)
	}

	stats, err := r.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if stats.Scanned != 0 {
		t.Fatalf("stats.Scanned = %d, want 0（宽限期内与终态都不扫）", stats.Scanned)
	}
}

// 5. RPC 失败与空响应计入错误计数，不中止整轮。
func TestSweepOnce_ErrorAndNilResponseCounted(t *testing.T) {
	r, fake, _ := newTestReconciler(t)
	fake.settleErr = errors.New("billing down")
	usage := time.Now().Add(-time.Hour)
	seedBill(t, r.db, "recon-err-1", 1, 2*time.Hour, &usage)
	fake.settleNil = false

	stats, err := r.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if stats.SettleErrors != 1 || stats.Settled != 0 {
		t.Fatalf("stats = %+v, want SettleErrors:1", stats)
	}

	// 空响应同样计数（防御分支）。第二轮 sweep 会把仍在 pending 的
	// recon-err-1 一并重扫（失败不收敛正是重扫的意义）：两颗都空响应 → 2。
	fake.mu.Lock()
	fake.settleErr = nil
	fake.settleNil = true
	fake.mu.Unlock()
	seedBill(t, r.db, "recon-nil-1", 1, 2*time.Hour, &usage)
	stats, err = r.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce(nil): %v", err)
	}
	if stats.SettleErrors != 2 {
		t.Fatalf("nil-response stats = %+v, want SettleErrors:2", stats)
	}
}

// 6. RebuildAll：活跃租户逐个重建；失败计数不中止。
func TestRebuildAll_CountsAndToleratesErrors(t *testing.T) {
	r, fake, _ := newTestReconciler(t)
	seedTenant(t, r.db, 1, 0)
	seedTenant(t, r.db, 2, 0)
	seedTenant(t, r.db, 3, 1) // 禁用租户不重建（store 侧拒绝；此处仅不入列）

	stats, err := r.RebuildAll(context.Background())
	if err != nil {
		t.Fatalf("RebuildAll: %v", err)
	}
	if stats.Rebuilt != 2 || len(fake.rebuildCalls) != 2 {
		t.Fatalf("stats = %+v rebuild = %v, want 2 active tenants", stats, fake.rebuildCalls)
	}
}
