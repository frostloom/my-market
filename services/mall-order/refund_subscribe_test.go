// mall-order subscription 行退款测试（R2.5-T3）：D15「退款=退订」——
// fulfilled 订单退款时 subscription 行调 billing.CancelSubscription
// （ref="mall-sub-refund:{order_no}:{line_no}"，审计 ref；billing 侧幂等来自
// 状态定点收敛：已退订重放 cancelled=false no-op），**不退费**（该行金额不进
// 退款额），退订标记落 billing 侧（订阅行 status=cancelled + auto_renew=0，
// 当期权益保留至期满——mall_order_items 无行级退订列，退款证据与 credit 行
// 同口径落 billing，零 schema 改动）。
//
// TestRefund_Subscription_CancelsNotRefunds 连真 billing（spawn ai-gateway
// cmd/billing 子进程，见 worker_credit_test.go harness）：
//  1. 混合单（general+subscription）fulfilled 退款 → 实退额=general 行金额
//     （订阅行排除）、订阅退订（auto_renew=0、当期保留 entitled）、余额三口径
//     不动（不退费）、零 refund 冲退流水；
//  2. 纯订阅单重放幂等（崩溃窗口 B 重试）→ 同状态收敛、cancelled=false
//     no-op、订阅行/锚/余额零变更。
//
// TestRefund_SubscriptionCancelRef_Naming（桩）：ref 组装 =
// mall-sub-refund:{order_no}:{line_no}、tenant 取自订单头、订阅行零 billing.
// Refund 触达（不退费在调用面上的直接证据）。
//
// TestRefundPaid_SubscribeLine（桩）：paid 直退含 subscribe 行——
// pending 行照常 voided+refunded（billing 零触达）；done 行（订阅已激活待
// promote）→ done-credit 守卫扩展覆盖 subscribe → MALL_30008（窗口 R2
// subscribe 面：直退=保留自动续费订阅又退款，须等 fulfilled 走退订路径）。
//
// 隔离与还原：mall 行按 cleanupTenant 还原 + 301/113 库存快照还原；billing 侧
// 订阅行/topups/tenants/Redis 键在 t.Cleanup 删除。种子前置：goods 301 已上架
// 且 ref_plan_id 关联在售定期套餐（scripts/seed-plans.sql + seed-goods.sql）。
// 禁止并发跑基础设施套件（T1 警示）。
package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// subRefundRefOf subscription 行退订幂等/审计锚（T3 定名：与 R2 充值冲退
// mall-refund:*（billing.Refund 通道）、R2.5 补偿 mall-void:*（worker 竞态
// 补偿）三足鼎立——前缀区分通道，后段 order:line 定位行）。
func subRefundRefOf(orderNo string, lineNo int32) string {
	return fmt.Sprintf("mall-sub-refund:%s:%d", orderNo, lineNo)
}

// fulfillCartOrder 造"已履约"cart 单：建单 → 支付 → worker 一轮（ship mock +
// billing 真通道）→ fulfilled。返回订单聚合（行序 goods_id 升序）。
func fulfillCartOrder(t *testing.T, s *Store, ctx context.Context, env *billingTestEnv, idem string, lines []Line) *Order {
	t.Helper()
	ord, err := s.CreateCartOrder(ctx, env.tenant, idem+"-"+env.runID, lines)
	if err != nil {
		t.Fatalf("CreateCartOrder: %v", err)
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
	if done != len(lines) {
		t.Fatalf("履约 done = %d，want %d", done, len(lines))
	}
	if st, _ := orderStatusOf(t, s, ctx, env.tenant, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("订单 status = %s，want fulfilled（履约前置态）", st)
	}
	return ord
}

// assertEntitledCancelled 断言 billing 订阅行 = 已退订且当期权益保留：
// status=cancelled、auto_renew=0、cycle_end 仍在未来（entitled）、cycle_num 不变。
func assertEntitledCancelled(t *testing.T, env *billingTestEnv, wantCycleNum int) {
	t.Helper()
	sub := billingSubOf(t, env)
	if sub == nil {
		t.Fatalf("billing 无订阅记录（tenant=%d）", env.tenant)
	}
	if sub.Status != "cancelled" {
		t.Fatalf("订阅 status = %q，want cancelled（退款=退订）", sub.Status)
	}
	if sub.AutoRenew != 0 {
		t.Fatalf("订阅 auto_renew = %d，want 0（退款后停止续费）", sub.AutoRenew)
	}
	if d := time.Until(sub.CycleEnd); d <= 0 {
		t.Fatalf("订阅 cycle_end 距今 %v，want 仍在未来（当期权益保留至期满）", d)
	}
	if sub.CycleNum != wantCycleNum {
		t.Fatalf("订阅 cycle_num = %d，want %d（退订不开新周期）", sub.CycleNum, wantCycleNum)
	}
}

// refundTopupCount 该租户 refund 通道冲退流水条数（订阅行不退费的账本面证据：
// general 直退同样零 billing 流水，混合单退款后应恒 0）。
func refundTopupCount(t *testing.T, env *billingTestEnv) int64 {
	t.Helper()
	var n int64
	if err := env.aidb.Raw(
		"SELECT COUNT(*) FROM topups WHERE tenant_id = ? AND ref_type = ?",
		env.tenant, "refund").Scan(&n).Error; err != nil {
		t.Fatalf("count refund topups: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// 1. TestRefund_Subscription_CancelsNotRefunds（真 billing）
func TestRefund_Subscription_CancelsNotRefunds(t *testing.T) {
	t.Run("mixed_order_refunds_general_only_cancels_subscription", func(t *testing.T) {
		s := openOrderTestStore(t)
		ctx := context.Background()
		env := startRealBilling(t)
		env.seedTenant(t, 0)
		tenantID := env.tenant
		cleanupTenant(t, s, tenantID)
		t.Cleanup(func() {
			if err := env.aidb.Exec("DELETE FROM subscriptions WHERE tenant_id = ?", tenantID).Error; err != nil {
				t.Errorf("cleanup subscriptions tenant=%d: %v", tenantID, err)
			}
		})
		snapshotStock(t, s, 113, 301)
		g301 := seedSubscriptionGoods(t, s)

		// 混合单：113 general（行 1）+ 301 subscription（行 2）。
		ord := fulfillCartOrder(t, s, ctx, env, "sub-refund-a", []Line{
			{GoodsID: 113, Qty: 1}, {GoodsID: 301, Qty: 1},
		})
		var generalCent int64
		for _, it := range ord.Items {
			if it.GoodsID == 113 {
				generalCent = it.PriceCent * int64(it.Qty)
			}
		}
		if generalCent <= 0 {
			t.Fatalf("general 行金额异常: %+v", ord.Items)
		}
		subLine := int32(0)
		for _, it := range ord.Items {
			if it.GoodsID == 301 {
				subLine = it.LineNo
			}
		}
		// 履约后订阅 active + auto_renew（前置态自检）。
		if sub := billingSubOf(t, env); sub == nil || sub.Status != "active" || sub.AutoRenew != 1 {
			t.Fatalf("履约后订阅前置态异常: %+v", billingSubOf(t, env))
		}
		wantCycleNum := billingSubOf(t, env).CycleNum
		stockBeforeRefund := stockOf(t, s, 113)

		// fulfilled 退款：general 行直退、订阅行退订不退费。
		refunded, err := s.RefundFulfilled(ctx, tenantID, ord.OrderNo)
		if err != nil {
			t.Fatalf("RefundFulfilled: %v", err)
		}
		// general 行回补（acceptance R2.5：restockOrder 对称口径，kind=2 全行）。
		if got := stockOf(t, s, 113); got != stockBeforeRefund+1 {
			t.Fatalf("stock(113) = %d，want %d（退款回补 qty=1）", got, stockBeforeRefund+1)
		}
		if refunded != generalCent {
			t.Fatalf("refunded = %d，want %d（仅 general 行；订阅行金额 %d 不进退款额）",
				refunded, generalCent, g301.PriceCent)
		}
		if st, _ := orderStatusOf(t, s, ctx, tenantID, ord.OrderNo); st != "refunded" {
			t.Fatalf("订单 status = %s，want refunded", st)
		}

		// billing 断言：退订（auto_renew=0）且当期权益保留（entitled：cancelled
		// + cycle_end 未到期）、周期数不变；账本零漂移（不退费）。
		assertEntitledCancelled(t, env, wantCycleNum)
		if ledger, column, redisV := env.balances(t); ledger != 0 || column != 0 || redisV != 0 {
			t.Fatalf("退款后余额 = (ledger=%d, column=%d, redis=%d)，want 全 0（订阅行不退费）",
				ledger, column, redisV)
		}
		// 激活 0 元锚仍在且恰 1 条；零 refund 冲退流水（general 直退/订阅退订
		// 均不走 billing.Refund 通道）。
		wantSubRef := fmt.Sprintf("%s:%d", ord.OrderNo, subLine)
		if n := env.topupCount(t, "mall-sub", wantSubRef); n != 1 {
			t.Fatalf("激活锚(%s) = %d 条，want 恰 1", wantSubRef, n)
		}
		if n := refundTopupCount(t, env); n != 0 {
			t.Fatalf("refund 冲退流水 = %d 条，want 0（混合单直退+退订零 billing 资金流）", n)
		}
	})

	t.Run("replay_idempotent_subscription_only", func(t *testing.T) {
		s := openOrderTestStore(t)
		ctx := context.Background()
		env := startRealBilling(t)
		env.seedTenant(t, 0)
		tenantID := env.tenant
		cleanupTenant(t, s, tenantID)
		t.Cleanup(func() {
			if err := env.aidb.Exec("DELETE FROM subscriptions WHERE tenant_id = ?", tenantID).Error; err != nil {
				t.Errorf("cleanup subscriptions tenant=%d: %v", tenantID, err)
			}
		})
		snapshotStock(t, s, 301)
		seedSubscriptionGoods(t, s)

		ord := fulfillCartOrder(t, s, ctx, env, "sub-refund-b", []Line{{GoodsID: 301, Qty: 1}})
		subBefore := billingSubOf(t, env)
		wantSubRef := fmt.Sprintf("%s:%d", ord.OrderNo, ord.Items[0].LineNo)

		// 首次退款：纯订阅单实退额 = 0（退订不退费）。
		refunded, err := s.RefundFulfilled(ctx, tenantID, ord.OrderNo)
		if err != nil {
			t.Fatalf("RefundFulfilled#1: %v", err)
		}
		if refunded != 0 {
			t.Fatalf("首次 refunded = %d，want 0（订阅行不进退款额）", refunded)
		}
		assertEntitledCancelled(t, env, subBefore.CycleNum)

		// 崩溃窗口重放：mall 状态翻转丢失 → 订单打回 fulfilled，再次退款 →
		// Cancel 状态定点收敛 no-op、订阅行/锚/余额零变更、订单收敛 refunded。
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
			t.Fatalf("重放 refunded = %d，want 0（退订 no-op 不重复记账）", refunded)
		}
		subAfter := billingSubOf(t, env)
		if subAfter == nil || subAfter.ID != subBefore.ID ||
			subAfter.CycleNum != subBefore.CycleNum || subAfter.Status != "cancelled" ||
			subAfter.AutoRenew != 0 {
			t.Fatalf("重放后订阅行变更: %+v → %+v（want 同行同态 no-op）", subBefore, subAfter)
		}
		if n := env.topupCount(t, "mall-sub", wantSubRef); n != 1 {
			t.Fatalf("重放后激活锚 = %d 条，want 恰 1", n)
		}
		if n := refundTopupCount(t, env); n != 0 {
			t.Fatalf("重放后 refund 流水 = %d 条，want 0", n)
		}
		if ledger, column, redisV := env.balances(t); ledger != 0 || column != 0 || redisV != 0 {
			t.Fatalf("重放后余额 = (ledger=%d, column=%d, redis=%d)，want 全 0",
				ledger, column, redisV)
		}
		if st, _ := orderStatusOf(t, s, ctx, tenantID, ord.OrderNo); st != "refunded" {
			t.Fatalf("重放后订单 status = %s，want refunded（收敛）", st)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. TestRefund_SubscriptionCancelRef_Naming（桩）：ref 组装与"不退费"调用面。
func TestRefund_SubscriptionCancelRef_Naming(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 301)
	seedSubscriptionGoods(t, s)

	ord, err := s.CreateCartOrder(ctx, testTenantA, "sub-refname-1", []Line{{GoodsID: 301, Qty: 1}})
	if err != nil {
		t.Fatalf("CreateCartOrder: %v", err)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-sub-refname", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	stub := &stubBilling{}
	s.billing = stub
	if _, err := s.RunFulfillmentOnce(ctx); err != nil {
		t.Fatalf("RunFulfillmentOnce: %v", err)
	}

	refunded, err := s.RefundFulfilled(ctx, testTenantA, ord.OrderNo)
	if err != nil {
		t.Fatalf("RefundFulfilled: %v", err)
	}
	if refunded != 0 {
		t.Fatalf("refunded = %d，want 0（订阅行不退费）", refunded)
	}
	cancels := stub.cancelCalls()
	if len(cancels) != 1 {
		t.Fatalf("CancelSubscription 调用 %d 次，want 1", len(cancels))
	}
	wantRef := subRefundRefOf(ord.OrderNo, ord.Items[0].LineNo)
	if got := cancels[0].GetRef(); got != wantRef {
		t.Fatalf("CancelSubscription.Ref = %q，want %q（mall-sub-refund:order:line 审计锚）", got, wantRef)
	}
	if got := cancels[0].GetTenantId(); got != int64(testTenantA) {
		t.Fatalf("CancelSubscription.TenantId = %d，want %d（取自订单头）", got, testTenantA)
	}
	if n := len(stub.refundCalls()); n != 0 {
		t.Fatalf("billing.Refund 调用 %d 次，want 0（订阅行不退费：不走冲退通道）", n)
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "refunded" {
		t.Fatalf("订单 status = %s，want refunded", st)
	}

	// 重放（崩溃窗口）：同 ref 再次退订（状态定点收敛 cancelled=false），
	// 不重复退费、订单收敛。
	if err := s.db.Exec(
		"UPDATE mall_orders SET status = ? WHERE order_no = ? AND tenant_id = ?",
		orderStatusFulfilled, ord.OrderNo, testTenantA).Error; err != nil {
		t.Fatalf("打回 fulfilled: %v", err)
	}
	refunded, err = s.RefundFulfilled(ctx, testTenantA, ord.OrderNo)
	if err != nil {
		t.Fatalf("重放 RefundFulfilled: %v", err)
	}
	if refunded != 0 {
		t.Fatalf("重放 refunded = %d，want 0", refunded)
	}
	if n := len(stub.cancelCalls()); n != 2 {
		t.Fatalf("重放后 CancelSubscription 调用 %d 次，want 2（同 ref 状态收敛幂等）", n)
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "refunded" {
		t.Fatalf("重放后订单 status = %s，want refunded", st)
	}
}

// ---------------------------------------------------------------------------
// 3. TestRefundPaid_SubscribeLine（桩）：paid 直退含 subscribe 行。
//   - pending subscribe 行 → 既有直退口径（voided + refunded），billing 零触达；
//   - done subscribe 行（订阅已激活、promote 未完成）→ done 守卫扩展覆盖
//     subscribe → MALL_30008（直退=保留自动续费订阅又退款），等 fulfilled
//     走退订路径（窗口 R2 subscribe 面）。
func TestRefundPaid_SubscribeLine(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 301)
	seedSubscriptionGoods(t, s)

	newPaidSubOrder := func(t *testing.T, idem string) *Order {
		t.Helper()
		ord, err := s.CreateCartOrder(ctx, testTenantA, idem, []Line{{GoodsID: 301, Qty: 1}})
		if err != nil {
			t.Fatalf("CreateCartOrder: %v", err)
		}
		if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
			TradeNo: "T-" + idem, Paid: true, AmountCent: ord.AmountCent}); err != nil {
			t.Fatalf("MarkPaidTx: %v", err)
		}
		return ord
	}

	t.Run("pending_line_voids_and_refunds", func(t *testing.T) {
		ord := newPaidSubOrder(t, "sub-refpaid-a")
		stub := &stubBilling{}
		s.billing = stub
		if err := s.RefundPaid(ctx, testTenantA, ord.OrderNo); err != nil {
			t.Fatalf("RefundPaid（pending subscribe 行）: %v", err)
		}
		if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "refunded" {
			t.Fatalf("订单 status = %s，want refunded", st)
		}
		for _, task := range taskRowsOf(t, s, ord.OrderNo) {
			if task.Status != taskStatusVoided {
				t.Fatalf("pending subscribe 任务行 status=%d，want voided=4", task.Status)
			}
		}
		if n := len(stub.activateCalls()) + len(stub.cancelCalls()) + len(stub.refundCalls()); n != 0 {
			t.Fatalf("billing 触达 %d 次，want 0（未激活直退零副作用）", n)
		}
	})

	t.Run("done_line_rejects_until_fulfilled", func(t *testing.T) {
		ord := newPaidSubOrder(t, "sub-refpaid-b")
		// 交错现场：worker 已激活并标 done（订阅 active 且将自动续费），
		// promote 未跑——订单停留 paid。
		if err := s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
			taskStatusDone, ord.OrderNo).Error; err != nil {
			t.Fatalf("造 done 行: %v", err)
		}
		stub := &stubBilling{}
		s.billing = stub
		err := s.RefundPaid(ctx, testTenantA, ord.OrderNo)
		assertMallCode(t, err, mallerr.CodeFulfillmentInProgress)
		if n := len(stub.activateCalls()) + len(stub.cancelCalls()) + len(stub.refundCalls()); n != 0 {
			t.Fatalf("billing 触达 %d 次，want 0（守卫拒绝零副作用）", n)
		}
		if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "paid" {
			t.Fatalf("订单 status = %s，want 仍 paid（拒绝回滚，等 fulfilled 走退订路径）", st)
		}
		for _, task := range taskRowsOf(t, s, ord.OrderNo) {
			if task.Status != taskStatusDone {
				t.Fatalf("任务行 status=%d，want 保持 done=2", task.Status)
			}
		}
	})
}
