// Credit/Refund RPC 集成测试（R2 充值闭环 TDD）。
//
// TDD 硬要求：本文件先于实现编写（RED），实现后转 GREEN。
// 连 dev MySQL（deploy/compose，宿主 3307，ai_gateway 库）+ dev Redis（宿主 6381）。
// 隔离与还原：测试租户用专属命名（credit-rpc-test-*，ID 远离真实自增段），
// t.Cleanup 精确还原本测试写入的 topups / tenants.ledger_balance / Redis 余额键，
// 不触碰其他租户与种子锚点，可重复跑。
//
// DSN 来源：BILLING_TEST_MYSQL_DSN 优先；未设置则从 deploy/compose/.env 读
// MYSQL_ROOT_PASSWORD 拼 DSN（凭据不硬编码、不提交、不打日志）。
package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	billingv1 "github.com/frostloom/my-market/pkg/proto/billing/v1"
	"github.com/frostloom/my-market/services/billing/store"
)

const (
	testRedisAddr = "127.0.0.1:6381"
	testMySQLAddr = "127.0.0.1:3307"
	testMySQLDB   = "ai_gateway"
)

// creditTestEnv 测试环境：真实 dev MySQL + Redis 上的 Store 与 gRPC Service。
type creditTestEnv struct {
	t      *testing.T
	svc    *Service
	st     *store.Store
	db     *gorm.DB
	rdb    *goredis.Client
	ctx    context.Context
	tenant uint64
	runID  string // 每次运行唯一：ref 带上它，防止跨轮次撞 uk_ref（残留行不致命）
}

func testDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("BILLING_TEST_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	pw := "root"
	if raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "deploy", "compose", ".env")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimRight(line, "\r")
			if v, ok := strings.CutPrefix(line, "MYSQL_ROOT_PASSWORD="); ok && v != "" {
				pw = v
				break
			}
		}
	}
	return fmt.Sprintf("root:%s@tcp(%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", pw, testMySQLAddr, testMySQLDB)
}

func newCreditTestEnv(t *testing.T, initialCent int64) *creditTestEnv {
	t.Helper()
	ctx := context.Background()

	rdb := goredis.NewClient(&goredis.Options{Addr: testRedisAddr, DB: 0})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("dev redis not available at %s: %v", testRedisAddr, err)
	}
	db, err := gorm.Open(mysql.Open(testDSN(t)), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open dev mysql: %v（dev MySQL 未就绪？docker compose --profile infra up -d）", err)
	}
	st := store.NewStore(db, rdb)
	if err := st.AutoMigrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := NewService(st, slog.New(slog.DiscardHandler))

	e := &creditTestEnv{t: t, svc: svc, st: st, db: db, rdb: rdb, ctx: ctx, runID: fmt.Sprint(time.Now().UnixNano())}

	// 专属测试租户：余额三处一致（initial_quota / ledger_balance / Redis 投影）。
	tenant := store.Tenant{
		Name:          fmt.Sprintf("credit-rpc-test-%d", time.Now().UnixNano()),
		InitialQuota:  initialCent,
		LedgerBalance: initialCent,
		Status:        0,
	}
	if err := db.Create(&tenant).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	e.tenant = tenant.ID
	if err := rdb.Set(ctx, store.BalanceKey(tenant.ID), initialCent, 0).Err(); err != nil {
		t.Fatalf("seed redis balance: %v", err)
	}

	// 还原（t.Cleanup LIFO：先注册的关连接，后注册的先跑——数据还原必须在连接关闭前执行）。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = rdb.Close()
	})
	t.Cleanup(func() {
		if err := e.db.Exec("DELETE FROM topups WHERE tenant_id = ?", e.tenant).Error; err != nil {
			t.Errorf("cleanup topups: %v", err)
		}
		if err := e.db.Exec("DELETE FROM tenants WHERE id = ?", e.tenant).Error; err != nil {
			t.Errorf("cleanup tenants: %v", err)
		}
		if err := e.rdb.Del(context.Background(), store.BalanceKey(e.tenant), cashFenceKey(e.tenant)).Err(); err != nil {
			t.Errorf("cleanup redis keys: %v", err)
		}
	})
	return e
}

// cashFenceKey 与 store 内部 billing:cash-owner:<tenant> 对齐（锁栅栏，无 TTL，测试负责清）。
func cashFenceKey(tenant uint64) string { return fmt.Sprintf("billing:cash-owner:%d", tenant) }

// ref 幂等键加运行唯一后缀：uk_ref 全库唯一（不按租户隔离），
// 上一轮残留行会让本轮撞幂等（正是实现语义），测试用唯一 ref 保证可重复跑。
func (e *creditTestEnv) ref(base string) string { return base + ":" + e.runID }

func (e *creditTestEnv) ledgerBalance() int64 {
	e.t.Helper()
	bal, err := store.LedgerBalance(e.ctx, e.db, e.tenant)
	if err != nil {
		e.t.Fatalf("LedgerBalance: %v", err)
	}
	return bal
}

func (e *creditTestEnv) columnBalance() int64 {
	e.t.Helper()
	var bal int64
	if err := e.db.Model(&store.Tenant{}).Where("id = ?", e.tenant).
		Select("ledger_balance").Scan(&bal).Error; err != nil {
		e.t.Fatalf("read ledger_balance: %v", err)
	}
	return bal
}

func (e *creditTestEnv) redisBalance() int64 {
	e.t.Helper()
	v, err := e.rdb.Get(e.ctx, store.BalanceKey(e.tenant)).Int64()
	if err != nil {
		e.t.Fatalf("read redis balance: %v", err)
	}
	return v
}

func (e *creditTestEnv) topupCount(refType, refID string) int64 {
	e.t.Helper()
	var n int64
	if err := e.db.Model(&store.Topup{}).Where("tenant_id = ? AND ref_type = ? AND ref_id = ?", e.tenant, refType, refID).
		Count(&n).Error; err != nil {
		e.t.Fatalf("count topups: %v", err)
	}
	return n
}

// TestCredit_HappyPath_TopupAndBalanceConsistent 正常入账：topup 行 + 余额三处一致。
func TestCredit_HappyPath_TopupAndBalanceConsistent(t *testing.T) {
	e := newCreditTestEnv(t, 10_000)

	reply, err := e.svc.Credit(e.ctx, &billingv1.CreditRequest{
		TenantId:   int64(e.tenant),
		AmountCent: 5_000,
		RefType:    "mall",
		RefId:      e.ref("ord_credit_happy:1"),
	})
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if !reply.Created {
		t.Fatalf("Created=false, want true（新 ref 必须入账）")
	}
	if reply.BalanceCent != 15_000 {
		t.Fatalf("BalanceCent=%d, want 15000", reply.BalanceCent)
	}
	// topup 行恰好一条，金额与 ref 正确
	if n := e.topupCount("mall", e.ref("ord_credit_happy:1")); n != 1 {
		t.Fatalf("topup rows=%d, want 1", n)
	}
	var row store.Topup
	if err := e.db.Where("tenant_id = ? AND ref_type = ? AND ref_id = ?", e.tenant, "mall", e.ref("ord_credit_happy:1")).
		First(&row).Error; err != nil {
		t.Fatalf("load topup: %v", err)
	}
	if row.Amount != 5_000 {
		t.Fatalf("topup amount=%d, want 5000", row.Amount)
	}
	// 余额三处一致：账本公式（权威）/ tenants.ledger_balance / Redis 投影
	if bal := e.ledgerBalance(); bal != 15_000 {
		t.Fatalf("ledger balance=%d, want 15000", bal)
	}
	if bal := e.columnBalance(); bal != 15_000 {
		t.Fatalf("tenants.ledger_balance=%d, want 15000", bal)
	}
	if bal := e.redisBalance(); bal != 15_000 {
		t.Fatalf("redis balance=%d, want 15000", bal)
	}
}

// TestCredit_DuplicateRef_NoOp 同 ref 重放：不再入账，返回当前余额与 created=false。
func TestCredit_DuplicateRef_NoOp(t *testing.T) {
	e := newCreditTestEnv(t, 10_000)

	first, err := e.svc.Credit(e.ctx, &billingv1.CreditRequest{
		TenantId: int64(e.tenant), AmountCent: 5_000, RefType: "mall", RefId: e.ref("ord_credit_dup:1"),
	})
	if err != nil {
		t.Fatalf("first Credit: %v", err)
	}
	if !first.Created || first.BalanceCent != 15_000 {
		t.Fatalf("first={created:%v bal:%d}, want {true 15000}", first.Created, first.BalanceCent)
	}
	// 同 ref、不同金额重放：仍必须 no-op（金额不一致不能改账）
	replay, err := e.svc.Credit(e.ctx, &billingv1.CreditRequest{
		TenantId: int64(e.tenant), AmountCent: 7_000, RefType: "mall", RefId: e.ref("ord_credit_dup:1"),
	})
	if err != nil {
		t.Fatalf("replay Credit: %v", err)
	}
	if replay.Created {
		t.Fatalf("replay Created=true, want false（uk_ref 幂等）")
	}
	if replay.BalanceCent != 15_000 {
		t.Fatalf("replay BalanceCent=%d, want 15000（no-op 不改余额）", replay.BalanceCent)
	}
	if n := e.topupCount("mall", e.ref("ord_credit_dup:1")); n != 1 {
		t.Fatalf("topup rows=%d, want 1（重放不得新增流水）", n)
	}
	if bal := e.ledgerBalance(); bal != 15_000 {
		t.Fatalf("ledger balance=%d, want 15000", bal)
	}
	if bal := e.redisBalance(); bal != 15_000 {
		t.Fatalf("redis balance=%d, want 15000", bal)
	}
}

// TestCredit_CrossTenantRef_FailsConsistently 跨租户同 ref 必须 fail-closed（不允许静默 no-op）：
// uk_ref 全库唯一（不按租户隔离）。租户 B 重放租户 A 已占用的 ref → FailedPrecondition
// （预读路径）；租户 A 自身重放 → 仍 no-op。口径锚定两条冲突路径（预读 / 事务内撞 uk_ref）
// 一致：实现侧事务内 !inserted 分支同样复核获胜行租户（并发竞态无法直构，以路径同构覆盖）。
func TestCredit_CrossTenantRef_FailsConsistently(t *testing.T) {
	eA := newCreditTestEnv(t, 10_000)
	eB := newCreditTestEnv(t, 10_000)

	ref := eA.ref("ord_cross_tenant:1")
	if _, err := eA.svc.Credit(eA.ctx, &billingv1.CreditRequest{
		TenantId: int64(eA.tenant), AmountCent: 5_000, RefType: "mall", RefId: ref,
	}); err != nil {
		t.Fatalf("tenant A first Credit: %v", err)
	}

	// 租户 B 携带他人 ref → FailedPrecondition，而非 no-op 成功
	_, err := eB.svc.Credit(eB.ctx, &billingv1.CreditRequest{
		TenantId: int64(eB.tenant), AmountCent: 5_000, RefType: "mall", RefId: ref,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("cross-tenant ref: code=%v, want FailedPrecondition（err=%v）", status.Code(err), err)
	}
	// B 余额与流水零副作用
	if bal := eB.ledgerBalance(); bal != 10_000 {
		t.Fatalf("tenant B ledger balance=%d, want 10000（fail-closed 零副作用）", bal)
	}

	// 租户 A 自身重放同 ref → 仍然 no-op（幂等不受影响）
	replay, err := eA.svc.Credit(eA.ctx, &billingv1.CreditRequest{
		TenantId: int64(eA.tenant), AmountCent: 5_000, RefType: "mall", RefId: ref,
	})
	if err != nil {
		t.Fatalf("tenant A replay Credit: %v", err)
	}
	if replay.Created {
		t.Fatalf("tenant A replay Created=true, want false")
	}
	if replay.BalanceCent != 15_000 {
		t.Fatalf("tenant A replay BalanceCent=%d, want 15000", replay.BalanceCent)
	}
	if n := eA.topupCount("mall", ref); n != 1 {
		t.Fatalf("topup rows=%d, want 1", n)
	}
}

// TestCredit_NonPositiveAmount_Rejected 非正金额拒绝：InvalidArgument，零副作用。
func TestCredit_NonPositiveAmount_Rejected(t *testing.T) {
	e := newCreditTestEnv(t, 10_000)

	for name, amount := range map[string]int64{"zero": 0, "negative": -100} {
		_, err := e.svc.Credit(e.ctx, &billingv1.CreditRequest{
			TenantId: int64(e.tenant), AmountCent: amount, RefType: "mall", RefId: "ord_credit_bad:" + name,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: code=%v, want InvalidArgument（err=%v）", name, status.Code(err), err)
		}
	}
	if e.topupCount("mall", "ord_credit_bad:zero") != 0 || e.topupCount("mall", "ord_credit_bad:negative") != 0 {
		t.Fatalf("rejected credit must not write topups")
	}
	if bal := e.ledgerBalance(); bal != 10_000 {
		t.Fatalf("ledger balance=%d, want 10000（零副作用）", bal)
	}
}

// TestRefund_MinRuleAndIdempotent 退款三态：余额足额全退 / 不足退 min / 同 ref 重放 no-op。
func TestRefund_MinRuleAndIdempotent(t *testing.T) {
	t.Run("sufficient_refunds_requested", func(t *testing.T) {
		e := newCreditTestEnv(t, 10_000)
		reply, err := e.svc.Refund(e.ctx, &billingv1.RefundRequest{
			TenantId: int64(e.tenant), AmountCent: 4_000, Ref: e.ref("refund_min_r1"),
		})
		if err != nil {
			t.Fatalf("Refund: %v", err)
		}
		if reply.RefundedCent != 4_000 {
			t.Fatalf("RefundedCent=%d, want 4000（余额充足按请求额全退）", reply.RefundedCent)
		}
		if bal := e.ledgerBalance(); bal != 6_000 {
			t.Fatalf("ledger balance=%d, want 6000", bal)
		}
		if bal := e.redisBalance(); bal != 6_000 {
			t.Fatalf("redis balance=%d, want 6000（投影对齐）", bal)
		}
		if n := e.topupCount("refund", e.ref("refund_min_r1")); n != 1 {
			t.Fatalf("topup rows=%d, want 1", n)
		}
	})
	t.Run("insufficient_clamps_to_balance", func(t *testing.T) {
		e := newCreditTestEnv(t, 1_000)
		reply, err := e.svc.Refund(e.ctx, &billingv1.RefundRequest{
			TenantId: int64(e.tenant), AmountCent: 5_000, Ref: e.ref("refund_min_r2"),
		})
		if err != nil {
			t.Fatalf("Refund: %v", err)
		}
		if reply.RefundedCent != 1_000 {
			t.Fatalf("RefundedCent=%d, want 1000（min(请求,余额) 口径）", reply.RefundedCent)
		}
		if bal := e.ledgerBalance(); bal != 0 {
			t.Fatalf("ledger balance=%d, want 0（绝不退负）", bal)
		}
	})
	t.Run("duplicate_ref_noop", func(t *testing.T) {
		e := newCreditTestEnv(t, 10_000)
		if _, err := e.svc.Refund(e.ctx, &billingv1.RefundRequest{
			TenantId: int64(e.tenant), AmountCent: 4_000, Ref: e.ref("refund_min_r3"),
		}); err != nil {
			t.Fatalf("first Refund: %v", err)
		}
		replay, err := e.svc.Refund(e.ctx, &billingv1.RefundRequest{
			TenantId: int64(e.tenant), AmountCent: 4_000, Ref: e.ref("refund_min_r3"),
		})
		if err != nil {
			t.Fatalf("replay Refund: %v", err)
		}
		if replay.RefundedCent != 0 {
			t.Fatalf("replay RefundedCent=%d, want 0（同 ref no-op）", replay.RefundedCent)
		}
		if bal := e.ledgerBalance(); bal != 6_000 {
			t.Fatalf("ledger balance=%d, want 6000（重放不再扣）", bal)
		}
		if n := e.topupCount("refund", e.ref("refund_min_r3")); n != 1 {
			t.Fatalf("topup rows=%d, want 1（重放不得新增流水）", n)
		}
	})
}
