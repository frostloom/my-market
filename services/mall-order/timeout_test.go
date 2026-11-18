// mall-order 超时关单 / 用户取消 / 退款（paid 分支）测试（timeout.go）。
//
// TDD 硬要求：本文件先于实现编写（RED），实现后转 GREEN。
// 锚点：种子 general 商品 117/118/119；订单 created_at 可回拨（超时扫描），
// stock 经 snapshotStock 绝对还原，行经 cleanupTenant 清理，可重复跑。
package main

import (
	"context"
	"testing"
	"time"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// ---------------------------------------------------------------------------
// 6. TestCloseTimedOut_ClosesAndRestocks
// created_at 早于 30 分钟的订单 → closed(timeout) + 库存回补；
// 15 分钟的订单不动。
func TestCloseTimedOut_ClosesAndRestocks(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 117)

	before := stockOf(t, s, 117)
	o31 := createTestOrder(t, s, ctx, testTenantA, "timeout-31", 117, 2)
	o15 := createTestOrder(t, s, ctx, testTenantA, "timeout-15", 117, 1)

	// 回拨 created_at（Go 侧时钟值，与 CloseTimedOut 的截止线同源）：
	// 31 分钟前 / 15 分钟前
	if err := s.db.Exec(
		"UPDATE mall_orders SET created_at = ? WHERE order_no = ?",
		time.Now().Add(-31*time.Minute), o31.OrderNo).Error; err != nil {
		t.Fatalf("回拨 31 分钟订单: %v", err)
	}
	if err := s.db.Exec(
		"UPDATE mall_orders SET created_at = ? WHERE order_no = ?",
		time.Now().Add(-15*time.Minute), o15.OrderNo).Error; err != nil {
		t.Fatalf("回拨 15 分钟订单: %v", err)
	}

	n, err := s.CloseTimedOut(ctx, 30*time.Minute)
	if err != nil {
		t.Fatalf("CloseTimedOut: %v", err)
	}
	if n != 1 {
		t.Fatalf("CloseTimedOut 关单数 = %d，want 1（仅 31 分钟那单）", n)
	}

	status, reason := orderStatusOf(t, s, ctx, testTenantA, o31.OrderNo)
	if status != "closed" || reason != "timeout" {
		t.Fatalf("超时订单 = (status=%s, reason=%s)，want (closed, timeout)", status, reason)
	}
	// 回补口径：o31 的 qty 2 累回，o15（未关闭）的扣减 1 仍在 → before-1
	if got := stockOf(t, s, 117); got != before-1 {
		t.Fatalf("stock(117) = %d，want %d（仅超时单 o31 回补 qty 2，o15 扣减保留）", got, before-1)
	}

	status, reason = orderStatusOf(t, s, ctx, testTenantA, o15.OrderNo)
	if status != "created" || reason != "" {
		t.Fatalf("15 分钟订单 = (status=%s, reason=%s)，want (created, 空)", status, reason)
	}

	// 再扫一轮：无新超时 → n=0
	if n, err := s.CloseTimedOut(ctx, 30*time.Minute); err != nil || n != 0 {
		t.Fatalf("重复扫描 = (%d, %v)，want (0, nil)", n, err)
	}
}

// ---------------------------------------------------------------------------
// 7. TestCancelOrder_UserPath
// 用户取消：created → closed(user) + 回补库存；重复取消幂等；
// 跨租户取消 → MALL_30001；paid 订单取消 → MALL_30002。
func TestCancelOrder_UserPath(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA, testTenantB)
	snapshotStock(t, s, 118)

	before := stockOf(t, s, 118)
	ord := createTestOrder(t, s, ctx, testTenantA, "cancel-1", 118, 2)
	if got := stockOf(t, s, 118); got != before-2 {
		t.Fatalf("建单后 stock(118) = %d，want %d", got, before-2)
	}

	// 用户取消 → closed(user) + 回补
	if err := s.CancelOrder(ctx, testTenantA, ord.OrderNo); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	status, reason := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo)
	if status != "closed" || reason != "user" {
		t.Fatalf("取消后 = (status=%s, reason=%s)，want (closed, user)", status, reason)
	}
	if got := stockOf(t, s, 118); got != before {
		t.Fatalf("stock(118) = %d，want 回补后 %d", got, before)
	}

	// 重复取消：幂等 200 语义（nil），状态/库存不再变化
	if err := s.CancelOrder(ctx, testTenantA, ord.OrderNo); err != nil {
		t.Fatalf("重复取消应幂等，got: %v", err)
	}
	if got := stockOf(t, s, 118); got != before {
		t.Fatalf("重复取消重复回补：stock(118) = %d，want %d", got, before)
	}

	// 跨租户取消：视为不存在 → MALL_30001
	assertMallCode(t, s.CancelOrder(ctx, testTenantB, ord.OrderNo), mallerr.CodeOrderNotFound)

	// paid 订单不可取消 → MALL_30002
	ordPaid := createTestOrder(t, s, ctx, testTenantA, "cancel-2", 118, 1)
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ordPaid.OrderNo, Provider: "mock",
		TradeNo: "T-cancel-2", Paid: true, AmountCent: ordPaid.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	assertMallCode(t, s.CancelOrder(ctx, testTenantA, ordPaid.OrderNo), mallerr.CodeOrderStateInvalid)
}

// ---------------------------------------------------------------------------
// 8. TestRefundPaid_VoidsPendingThenRefunds
// 退款（R1 仅 paid 分支）：pending 履约行作废 → paid→refunded →
// paid 流水置 refunded；重复退款幂等；created 订单退款 → MALL_30002；
// 作废后的任务不被 worker 执行。
func TestRefundPaid_VoidsPendingThenRefunds(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 119)

	ord := createTestOrder(t, s, ctx, testTenantA, "refund-1", 119, 1)
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-refund-1", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}

	// paid → refunded：作废 pending 行
	if err := s.RefundPaid(ctx, testTenantA, ord.OrderNo); err != nil {
		t.Fatalf("RefundPaid: %v", err)
	}
	status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo)
	if status != "refunded" {
		t.Fatalf("订单 status = %s，want refunded", status)
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != 4 {
			t.Fatalf("任务行 line=%d status=%d，want voided=4", task.LineNo, task.Status)
		}
	}
	recStatus, _ := paymentRecord(t, s, ord.OrderNo)
	if recStatus != 4 {
		t.Fatalf("支付流水 status = %d，want refunded=4", recStatus)
	}

	// 重复退款：幂等（nil），状态不变
	if err := s.RefundPaid(ctx, testTenantA, ord.OrderNo); err != nil {
		t.Fatalf("重复退款应幂等，got: %v", err)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "refunded" {
		t.Fatalf("重复退款后 status = %s，want 仍为 refunded", status)
	}

	// created 订单退款 → MALL_30002（R1 仅 paid 分支）
	ordCreated := createTestOrder(t, s, ctx, testTenantA, "refund-2", 119, 1)
	assertMallCode(t, s.RefundPaid(ctx, testTenantA, ordCreated.OrderNo), mallerr.CodeOrderStateInvalid)
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ordCreated.OrderNo); status != "created" {
		t.Fatalf("created 订单被退款污染：status = %s，want created", status)
	}

	// 作废后的任务不被 worker 执行，订单停留在 refunded
	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce: %v", err)
	}
	if done != 0 {
		t.Fatalf("作废行被执行 done = %d，want 0", done)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "refunded" {
		t.Fatalf("退款单被 worker 推进：status = %s，want 仍为 refunded", status)
	}
}
