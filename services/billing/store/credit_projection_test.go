package store

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
)

// 回归：批量落账模式下 Credit 只写 MySQL 账本，不更新 Redis 投影。
//
// 缺陷路径（修复前）：
//  1. creditCash 在 MySQL 事务里 insertTopup + addLedgerBalance，全程不碰 Redis；
//  2. 随后的 requireCashMatch 在批量模式下遇到「key 存在但与账本不等」直接放行
//     （Redis 是权威，不能拿旧列盖回去）；
//  3. GetBalance → balanceOrEnsure 只在 Redis key **缺失**时重建，key 存在就原样返回。
//
// 结果是充值成功、账本正确，但余额查询恒为旧值：用户看到 0 元，Reserve 读到 0
// 直接 RESERVE_INSUFFICIENT——钱在账本上，却花不出去。
//
// 非批量模式不受影响（requireCashMatch 会走 rebuildCash 自愈），所以本测试必须
// 显式开启 EnableLedgerBatch 才能复现。
func TestCreditRefreshesProjectionInBatchMode(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()

	// 长间隔：本测试只验证投影刷新，不让后台 flush 插手。
	s.EnableLedgerBatch(time.Hour)

	tenant := seedTenant(t, s, 1000)

	if _, created, err := s.Credit(ctx, tenant, 500, "mall", "order-1:1"); err != nil {
		t.Fatalf("credit: %v", err)
	} else if !created {
		t.Fatal("credit reported replay for a fresh ref")
	}

	ledger, err := LedgerBalance(ctx, s.db, tenant)
	if err != nil {
		t.Fatalf("ledger balance: %v", err)
	}
	if ledger != 1500 {
		t.Fatalf("ledger=%d, want 1500 (1000 initial + 500 credit)", ledger)
	}

	// 核心断言：入账后投影必须同步 +500，否则用户看到旧余额且无法消费。
	bal, err := s.GetBalance(ctx, tenant)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal != ledger {
		t.Fatalf("GetBalance=%d, ledger=%d: credit did not refresh the Redis projection", bal, ledger)
	}
}

// 回归：Refund 同样必须更新投影（同一缺陷的退款侧）。退款后若投影不动，
// 用户会看到比真实更多的可花余额，可以继续消费已经退掉的钱。
func TestRefundRefreshesProjectionInBatchMode(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()

	s.EnableLedgerBatch(time.Hour)

	tenant := seedTenant(t, s, 1000)

	refunded, err := s.RefundCent(ctx, tenant, 300, "mall-refund:order-2:1")
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if refunded != 300 {
		t.Fatalf("refunded=%d, want 300", refunded)
	}

	ledger, err := LedgerBalance(ctx, s.db, tenant)
	if err != nil {
		t.Fatalf("ledger balance: %v", err)
	}
	if ledger != 700 {
		t.Fatalf("ledger=%d, want 700 (1000 - 300 refund)", ledger)
	}

	bal, err := s.GetBalance(ctx, tenant)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal != ledger {
		t.Fatalf("GetBalance=%d, ledger=%d: refund did not refresh the Redis projection", bal, ledger)
	}
}

// 回归：该租户有在途预扣（账单还在内存批次里，pending != 0）时入账。
//
// 这是有推理流量时的常态，也是最容易被「用账本列覆盖投影」写法搞坏的状态：
// 账本列缺了在途扣款，覆盖会把已扣的钱复活。增量语义必须在此状态下仍然正确
// ——入账只加自己那 500，不碰在途预扣。
func TestCreditAppliesWhileDebitsPending(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()
	s.EnableLedgerBatch(time.Hour)

	tenant := seedTenant(t, s, 1000)
	seedMockModel(t, s)

	// 一笔在途预扣：Redis 已扣、账单还在内存批次里 → pending != 0。
	if _, err := s.Reserve(ctx, reserveReq("pending-reserve-1", tenant, 84)); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if !s.batch.behindColumn(tenant) {
		t.Fatal("setup: expected pending debits")
	}
	before, err := s.GetBalance(ctx, tenant)
	if err != nil {
		t.Fatalf("balance before: %v", err)
	}

	if _, created, err := s.Credit(ctx, tenant, 500, "mall", "pending-order:1"); err != nil {
		t.Fatalf("credit: %v", err)
	} else if !created {
		t.Fatal("credit reported replay")
	}

	after, err := s.GetBalance(ctx, tenant)
	if err != nil {
		t.Fatalf("balance after: %v", err)
	}
	if after != before+500 {
		t.Fatalf("GetBalance=%d, want %d (in-flight debit must survive the credit)", after, before+500)
	}
}

// 回归：同一 ref 重放时必须补应用投影。
//
// 首次入账可能因 Redis 故障没刷成投影，错误信息正是让调用方「用同一 ref 重试」。
// 若重放走幂等预读分支却不补投影，承诺的自愈永远不发生，调用方会无限重试。
func TestCreditReplayHealsProjection(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()
	s.EnableLedgerBatch(time.Hour)

	tenant := seedTenant(t, s, 1000)

	// 模拟「账本已入账、投影没跟上」：手工写 topup 行 + 改账本列，但不碰 Redis。
	// 等价于首次 Credit 在事务提交后、刷投影前崩溃。
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := insertTopup(tx, &Topup{TenantID: tenant, Source: "mall", Amount: 500, RefType: "mall", RefID: "replay-order:1", CreatedAt: time.Now()}); err != nil {
			return err
		}
		return addLedgerBalance(tx, tenant, 500)
	}); err != nil {
		t.Fatalf("seed committed topup: %v", err)
	}

	// 重放同一 ref。
	bal, created, err := s.Credit(ctx, tenant, 500, "mall", "replay-order:1")
	if err != nil {
		t.Fatalf("credit replay: %v", err)
	}
	if created {
		t.Fatal("replay unexpectedly created a new topup")
	}

	ledger, err := LedgerBalance(ctx, s.db, tenant)
	if err != nil {
		t.Fatalf("ledger balance: %v", err)
	}
	if bal != ledger {
		t.Fatalf("replay returned %d, ledger=%d: replay did not heal the projection", bal, ledger)
	}
	if live, err := s.GetBalance(ctx, tenant); err != nil {
		t.Fatalf("get balance: %v", err)
	} else if live != ledger {
		t.Fatalf("GetBalance=%d, ledger=%d: replay did not heal the projection", live, ledger)
	}
}

// 回归：入账在同一 ref 上重复调用只能加一次钱（Redis 侧也要幂等）。
func TestCreditProjectionAppliesOnce(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()
	s.EnableLedgerBatch(time.Hour)

	tenant := seedTenant(t, s, 1000)

	for i := 0; i < 3; i++ {
		if _, _, err := s.Credit(ctx, tenant, 500, "mall", "once-order:1"); err != nil {
			t.Fatalf("credit #%d: %v", i, err)
		}
	}

	bal, err := s.GetBalance(ctx, tenant)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal != 1500 {
		t.Fatalf("GetBalance=%d, want 1500 (500 applied exactly once)", bal)
	}
}

// 回归：PayRecharge（管理端充值）同样必须更新投影。这条路径与 Credit 是同一个
// 缺陷，只是入口不同：批量模式下订单转 paid、账本已加钱，但 Redis 投影不动。
func TestPayRechargeRefreshesProjectionInBatchMode(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()
	s.EnableLedgerBatch(time.Hour)

	tenant := seedTenant(t, s, 1000)
	o, err := s.CreateRechargeOrder(ctx, tenant, 10, "")
	if err != nil {
		t.Fatalf("create recharge order: %v", err)
	}
	if _, err := s.PayRecharge(ctx, o.OrderNo); err != nil {
		t.Fatalf("pay recharge: %v", err)
	}

	ledger, err := LedgerBalance(ctx, s.db, tenant)
	if err != nil {
		t.Fatalf("ledger balance: %v", err)
	}
	bal, err := s.GetBalance(ctx, tenant)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal != ledger {
		t.Fatalf("GetBalance=%d, ledger=%d: PayRecharge did not refresh the projection", bal, ledger)
	}
}

// 回归：RefundRecharge（充值退款）在批量模式下同样必须更新投影。
func TestRefundRechargeRefreshesProjectionInBatchMode(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()
	s.EnableLedgerBatch(time.Hour)

	tenant := seedTenant(t, s, 1000)
	o, err := s.CreateRechargeOrder(ctx, tenant, 10, "")
	if err != nil {
		t.Fatalf("create recharge order: %v", err)
	}
	// 支付直接落库到 paid，绕开 PayRecharge，让本测试只考察退款路径的投影刷新
	// （否则 PayRecharge 自己的缺陷会先污染基线，退款侧即使不修也能「碰巧对齐」）。
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := insertTopup(tx, &Topup{TenantID: tenant, Source: TopupRecharge, Amount: o.AmountCent, RefType: TopupRecharge, RefID: o.OrderNo, CreatedAt: time.Now()}); err != nil {
			return err
		}
		if err := addLedgerBalance(tx, tenant, o.AmountCent); err != nil {
			return err
		}
		return tx.Model(&RechargeOrder{}).Where("id = ?", o.ID).Update("status", RechargePaid).Error
	}); err != nil {
		t.Fatalf("seed paid order: %v", err)
	}
	// 同步投影，模拟支付侧已正确刷新的状态。
	if err := s.rdb.Set(ctx, BalanceKey(tenant), 1000+o.AmountCent, 0).Err(); err != nil {
		t.Fatalf("seed projection: %v", err)
	}

	if _, err := s.RefundRecharge(ctx, tenant, o.OrderNo); err != nil {
		t.Fatalf("refund recharge: %v", err)
	}

	ledger, err := LedgerBalance(ctx, s.db, tenant)
	if err != nil {
		t.Fatalf("ledger balance: %v", err)
	}
	bal, err := s.GetBalance(ctx, tenant)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal != ledger {
		t.Fatalf("GetBalance=%d, ledger=%d: RefundRecharge did not refresh the projection", bal, ledger)
	}
}

// 回归：RefundAmount（按金额退款）在批量模式下同样必须更新投影。
func TestRefundAmountRefreshesProjectionInBatchMode(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()
	s.EnableLedgerBatch(time.Hour)

	tenant := seedTenant(t, s, 1000)
	if _, err := s.RefundAmount(ctx, tenant, 3, "amount-refund-1"); err != nil {
		t.Fatalf("refund amount: %v", err)
	}

	ledger, err := LedgerBalance(ctx, s.db, tenant)
	if err != nil {
		t.Fatalf("ledger balance: %v", err)
	}
	bal, err := s.GetBalance(ctx, tenant)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal != ledger {
		t.Fatalf("GetBalance=%d, ledger=%d: RefundAmount did not refresh the projection", bal, ledger)
	}
}
