// mall-order fulfilled 退款测试（R2-T3）：fulfilled 订单逐行退款——
// recharge（credit）行经 billing.Refund 冲退（ref="mall-refund:{order_no}:{line_no}"，
// 与入账 ref 独立；实际退额 = min(请求额, 当前余额)，以 refunded_cent 记实退额），
// general 行直退 + 回补库存（既有口径），subscription 行守卫拒绝（R2.5 前不出现）。
//
// TestRefund_FulfilledRecharge_MinRule 连真 billing（spawn ai-gateway cmd/billing
// 子进程，见 worker_credit_test.go 的 harness）三态：
//  1. 余额充足 → 全退（refunded = face+bonus）；
//  2. 余额不足 → 部分退（refunded = min < 请求额）；
//  3. 同 refund ref 重放（崩溃窗口重试）→ billing no-op 不重复退，订单收敛 refunded。
//
// 冲退发生在终局事务之外（崩溃窗口分析见 refund.go 文件头），billing 侧 uk_ref
// 幂等保证重试收敛、绝不双重退款。禁止并发跑基础设施套件（T1 警示）。
package main

import (
	"context"
	"fmt"
	"testing"

	billingv1 "github.com/frostloom/ai-gateway/pkg/proto/billing/v1"
	"github.com/frostloom/my-market/pkg/mallerr"
)

func TestRefund_FulfilledRecharge_MinRule(t *testing.T) {
	t.Run("sufficient_full_refund", func(t *testing.T) {
		s := openOrderTestStore(t)
		ctx := context.Background()
		env := startRealBilling(t)
		env.seedTenant(t, 0)
		tenantID := env.tenant
		cleanupTenant(t, s, tenantID)

		ord := fulfillRechargeOrder(t, s, ctx, env, "ref-min-a")
		wantAmount := ord.Items[0].FaceValueCent + ord.Items[0].BonusCent
		wantRef := refundRefOf(ord.OrderNo, ord.Items[0].LineNo)

		// 余额充足（刚入账未消费）→ 按请求额全退，refund_cent = 实退额。
		refunded, err := s.RefundFulfilled(ctx, tenantID, ord.OrderNo)
		if err != nil {
			t.Fatalf("RefundFulfilled: %v", err)
		}
		if refunded != wantAmount {
			t.Fatalf("refunded = %d，want %d（余额充足全退）", refunded, wantAmount)
		}
		assertFulfilledRefunded(t, s, ctx, env, tenantID, ord.OrderNo, wantRef, -wantAmount, wantAmount)
		if ledger, column, redisV := env.balances(t); ledger != 0 || column != 0 || redisV != 0 {
			t.Fatalf("退款后余额 = (ledger=%d, column=%d, redis=%d)，want 全 0", ledger, column, redisV)
		}
	})

	t.Run("insufficient_partial_refund", func(t *testing.T) {
		s := openOrderTestStore(t)
		ctx := context.Background()
		env := startRealBilling(t)
		env.seedTenant(t, 0)
		tenantID := env.tenant
		cleanupTenant(t, s, tenantID)

		ord := fulfillRechargeOrder(t, s, ctx, env, "ref-min-b")
		wantAmount := ord.Items[0].FaceValueCent + ord.Items[0].BonusCent

		// 预消费：真 billing Refund RPC 把余额压到请求额以下（模拟已花掉一部分）。
		spend := int64(2000)
		spendRef := "r2t3-spend-" + env.runID
		spendRep, err := env.client.Refund(ctx, refundReq(tenantID, spend, spendRef))
		if err != nil {
			t.Fatalf("预消费 Refund: %v", err)
		}
		if spendRep.GetRefundedCent() != spend {
			t.Fatalf("预消费实退 = %d，want %d", spendRep.GetRefundedCent(), spend)
		}
		if ledger, _, _ := env.balances(t); ledger != wantAmount-spend {
			t.Fatalf("预消费后余额 = %d，want %d", ledger, wantAmount-spend)
		}

		// 余额不足 → billing min 钳制：实退 = 余额（< 请求额），以 refunded_cent 记实退额。
		refunded, err := s.RefundFulfilled(ctx, tenantID, ord.OrderNo)
		if err != nil {
			t.Fatalf("RefundFulfilled: %v", err)
		}
		if refunded != wantAmount-spend {
			t.Fatalf("refunded = %d，want %d（min(请求 %d, 余额 %d)）",
				refunded, wantAmount-spend, wantAmount, wantAmount-spend)
		}
		assertFulfilledRefunded(t, s, ctx, env, tenantID, ord.OrderNo,
			refundRefOf(ord.OrderNo, ord.Items[0].LineNo), -(wantAmount - spend), wantAmount)
		if ledger, column, redisV := env.balances(t); ledger != 0 || column != 0 || redisV != 0 {
			t.Fatalf("退款后余额 = (ledger=%d, column=%d, redis=%d)，want 全 0", ledger, column, redisV)
		}
	})

	t.Run("replay_idempotent_no_double_refund", func(t *testing.T) {
		s := openOrderTestStore(t)
		ctx := context.Background()
		env := startRealBilling(t)
		env.seedTenant(t, 0)
		tenantID := env.tenant
		cleanupTenant(t, s, tenantID)

		ord := fulfillRechargeOrder(t, s, ctx, env, "ref-min-c")
		wantAmount := ord.Items[0].FaceValueCent + ord.Items[0].BonusCent
		wantRef := refundRefOf(ord.OrderNo, ord.Items[0].LineNo)

		// 首次退款成功。
		refunded, err := s.RefundFulfilled(ctx, tenantID, ord.OrderNo)
		if err != nil {
			t.Fatalf("RefundFulfilled#1: %v", err)
		}
		if refunded != wantAmount {
			t.Fatalf("首次 refunded = %d，want %d", refunded, wantAmount)
		}

		// 崩溃窗口重放：billing 已冲退、mall 状态翻转丢失 → 订单被打回 fulfilled，
		// 再次 RefundFulfilled → 同 refund ref 重放 billing no-op（refunded_cent=0）、
		// 不重复退款，订单收敛 refunded。
		if err := s.db.Exec(
			"UPDATE mall_orders SET status = ? WHERE order_no = ? AND tenant_id = ?",
			orderStatusFulfilled, ord.OrderNo, tenantID).Error; err != nil {
			t.Fatalf("打回 fulfilled: %v", err)
		}
		refunded, err = s.RefundFulfilled(ctx, tenantID, ord.OrderNo)
		if err != nil {
			t.Fatalf("RefundFulfilled#2（重放）: %v", err)
		}
		if refunded != 0 {
			t.Fatalf("重放 refunded = %d，want 0（同 ref no-op 不重复退）", refunded)
		}
		if n := env.topupCount(t, "refund", wantRef); n != 1 {
			t.Fatalf("重放后冲退流水 = %d 条，want 恰 1（uk_ref 幂等）", n)
		}
		if st, _ := orderStatusOf(t, s, ctx, tenantID, ord.OrderNo); st != "refunded" {
			t.Fatalf("重放后订单 status = %s，want refunded（收敛）", st)
		}
		if ledger, column, redisV := env.balances(t); ledger != 0 || column != 0 || redisV != 0 {
			t.Fatalf("重放后余额 = (ledger=%d, column=%d, redis=%d)，want 全 0（只退一次）",
				ledger, column, redisV)
		}
	})

	t.Run("fulfilled_recharge_state_guards", func(t *testing.T) {
		s := openOrderTestStore(t)
		ctx := context.Background()
		cleanupTenant(t, s, testTenantA)

		// created 订单走 fulfilled 退款分支 → MALL_30002（状态机守卫，不触 billing）。
		ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "ref-guard-1")
		if err != nil {
			t.Fatalf("CreateRechargeOrder: %v", err)
		}
		_, err = s.RefundFulfilled(ctx, testTenantA, ord.OrderNo)
		assertMallCode(t, err, mallerr.CodeOrderStateInvalid)
		if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "created" {
			t.Fatalf("created 订单被退款污染：status = %s，want created", st)
		}

		// 不存在的订单 → MALL_30001。
		_, err = s.RefundFulfilled(ctx, testTenantA, "M20261119000000NOPE1")
		assertMallCode(t, err, mallerr.CodeOrderNotFound)

		// 跨租户 → MALL_30001（与 GetOrder 同口径，不泄漏存在性）。
		_, err = s.RefundFulfilled(ctx, testTenantB, ord.OrderNo)
		assertMallCode(t, err, mallerr.CodeOrderNotFound)
	})
}

// TestRefundPaid_RejectsWhenDoneCreditLineExists（Fix Round 1，窗口 R2 防护）：
// paid 订单存在 done 的 credit 行（已真实入账、promote 未完成）→ 直退拒绝
// MALL_30008（可重试，等 fulfilled 走冲退路径）；billing 零触达、零副作用。
// 对照：pending credit 行（未入账）→ 既有直退口径不变（void + refunded）。
func TestRefundPaid_RejectsWhenDoneCreditLineExists(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "refpaid-guard-1")
	if err != nil {
		t.Fatalf("CreateRechargeOrder: %v", err)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-refpaid-guard", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	// 造"已入账、promote 未完成"现场：任务行 done（真实入账在 billing 侧，
	// 本桩测只关心 mall 侧守卫）。
	if err := s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
		taskStatusDone, ord.OrderNo).Error; err != nil {
		t.Fatalf("造 done 行: %v", err)
	}

	stub := &stubBilling{}
	s.billing = stub
	err = s.RefundPaid(ctx, testTenantA, ord.OrderNo)
	assertMallCode(t, err, mallerr.CodeFulfillmentInProgress)
	// billing 零触达（直退不冲退入账，守卫在动任何钱之前拒绝）。
	if n := len(stub.creditCalls()) + len(stub.refundCalls()); n != 0 {
		t.Fatalf("billing 触达 %d 次，want 0（守卫拒绝，零副作用）", n)
	}
	// 零副作用：订单仍 paid、流水仍 paid、任务行仍 done。
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "paid" {
		t.Fatalf("订单 status = %s，want 仍 paid（守卫拒绝不迁移）", st)
	}
	if recStatus, _ := paymentRecord(t, s, ord.OrderNo); recStatus != paymentStatusPaid {
		t.Fatalf("支付流水 status = %d，want 仍 paid=2", recStatus)
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusDone {
			t.Fatalf("任务行 status=%d，want 仍 done=2", task.Status)
		}
	}

	// 对照：pending credit 行（未入账）→ 既有直退口径不变。
	ord2, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "refpaid-guard-2")
	if err != nil {
		t.Fatalf("CreateRechargeOrder: %v", err)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord2.OrderNo, Provider: "mock",
		TradeNo: "T-refpaid-guard-2", Paid: true, AmountCent: ord2.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	if err := s.RefundPaid(ctx, testTenantA, ord2.OrderNo); err != nil {
		t.Fatalf("RefundPaid（pending 行）: %v", err)
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord2.OrderNo); st != "refunded" {
		t.Fatalf("pending 行直退后 status = %s，want refunded（R1 口径不变）", st)
	}
	for _, task := range taskRowsOf(t, s, ord2.OrderNo) {
		if task.Status != taskStatusVoided {
			t.Fatalf("pending 任务行 status=%d，want voided=4", task.Status)
		}
	}
}

// TestRefundPaid_VoidThenDoneRace_ClosedByRecheck（Fix Round 2，TOCTOU 收口）：
// "worker done 与 RefundPaid void 交错"构型——paid 单 + credit 行，事务外模拟
// worker 抢先提交 done（直接 UPDATE 行 done、订单仍 paid，等价于守卫读到
// pending 后、void 前该行已 done）→ RefundPaid 必须 MALL_30008 且订单停留
// paid、流水/行零污染、billing 零触达（守卫拒绝成立：done 行已可见，等
// promote→fulfilled 走冲退；不产生"翻 refunded 而 done 行在"的双得余额态）。
// 注：事务内"守卫读到 pending、void 前才提交 done"的真并发交错无法单线程
// 直构；复查用 FOR UPDATE 锁读（current read）保证任何已提交的 done 在 void
// 后必可见——两种交错的收敛推理见 timeout.go 1.5 与 refund.go 文件头。
func TestRefundPaid_VoidThenDoneRace_ClosedByRecheck(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "refpaid-race-1")
	if err != nil {
		t.Fatalf("CreateRechargeOrder: %v", err)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-refpaid-race", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	// 交错现场：worker done 已提交（行 done）、订单仍 paid（promote 未跑）。
	if err := s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
		taskStatusDone, ord.OrderNo).Error; err != nil {
		t.Fatalf("造 done 行: %v", err)
	}

	stub := &stubBilling{}
	s.billing = stub
	err = s.RefundPaid(ctx, testTenantA, ord.OrderNo)
	assertMallCode(t, err, mallerr.CodeFulfillmentInProgress)
	if n := len(stub.creditCalls()) + len(stub.refundCalls()); n != 0 {
		t.Fatalf("billing 触达 %d 次，want 0（守卫/复查拒绝，零副作用）", n)
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "paid" {
		t.Fatalf("订单 status = %s，want 仍 paid（拒绝回滚，不翻 refunded）", st)
	}
	if recStatus, _ := paymentRecord(t, s, ord.OrderNo); recStatus != paymentStatusPaid {
		t.Fatalf("支付流水 status = %d，want 仍 paid=2", recStatus)
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusDone {
			t.Fatalf("任务行 status=%d，want 仍 done=2（未被 void 污染）", task.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// 辅助

// refundRefOf fulfilled 冲退幂等锚点（ADR-005：与入账 ref 独立）。
func refundRefOf(orderNo string, lineNo int32) string {
	return fmt.Sprintf("mall-refund:%s:%d", orderNo, lineNo)
}

// refundReq 组 billing Refund 请求（测试用）。
func refundReq(tenantID uint64, amountCent int64, ref string) *billingv1.RefundRequest {
	return &billingv1.RefundRequest{TenantId: int64(tenantID), AmountCent: amountCent, Ref: ref}
}

// fulfillRechargeOrder 造"已履约"充值单：直购 202 → 支付 → worker 经真 billing
// 入账一轮 → fulfilled，余额 = face+bonus。返回订单聚合。
func fulfillRechargeOrder(t *testing.T, s *Store, ctx context.Context, env *billingTestEnv, idem string) *Order {
	t.Helper()
	ord, err := s.CreateRechargeOrder(ctx, env.tenant, 202, idem+"-"+env.runID)
	if err != nil {
		t.Fatalf("CreateRechargeOrder: %v", err)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-" + idem, Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	s.billing = env.client
	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce: %v", err)
	}
	if done != 1 {
		t.Fatalf("履约 done = %d，want 1", done)
	}
	if st, _ := orderStatusOf(t, s, ctx, env.tenant, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("订单 status = %s，want fulfilled（履约前置态）", st)
	}
	want := ord.Items[0].FaceValueCent + ord.Items[0].BonusCent
	if ledger, _, _ := env.balances(t); ledger != want {
		t.Fatalf("履约后余额 = %d，want %d", ledger, want)
	}
	return ord
}

// assertFulfilledRefunded 断言 fulfilled 退款落账：订单 refunded、paid 流水置
// refunded、billing 冲退流水恰 1 条（负额 + requested 记请求额）。
func assertFulfilledRefunded(t *testing.T, s *Store, ctx context.Context, env *billingTestEnv, tenantID uint64,
	orderNo, wantRef string, wantAmount, wantRequested int64) {
	t.Helper()
	if st, _ := orderStatusOf(t, s, ctx, tenantID, orderNo); st != "refunded" {
		t.Fatalf("订单 status = %s，want refunded", st)
	}
	recStatus, _ := paymentRecord(t, s, orderNo)
	if recStatus != paymentStatusRefunded {
		t.Fatalf("支付流水 status = %d，want refunded=4", recStatus)
	}
	if n := env.topupCount(t, "refund", wantRef); n != 1 {
		t.Fatalf("冲退流水(%s) = %d 条，want 恰 1", wantRef, n)
	}
	amount, requested := env.topupAmount(t, "refund", wantRef)
	if amount != wantAmount || requested != wantRequested {
		t.Fatalf("冲退流水 = (amount=%d, requested=%d)，want (%d, %d)", amount, requested, wantAmount, wantRequested)
	}
}
