// mall-order 支付域测试：回调落账事务（payment.go）。
//
// TDD 硬要求：本文件先于实现编写（RED），实现后转 GREEN。
// 断言锚点 = scripts/seed-goods.sql 种子（113/115/116 为 general 在售）。
// 写入的 orders/items/payment_records/fulfillment_tasks 行与扣减的 stock
// 经 cleanupTenant / snapshotStock 在 t.Cleanup 还原，可重复跑。
package main

import (
	"context"
	"testing"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// createTestOrder 建一笔测试订单（general 商品，kind=2）。
func createTestOrder(t *testing.T, s *Store, ctx context.Context, tenant uint64, idemKey string, goodsID uint64, qty int) *Order {
	t.Helper()
	ord, err := s.CreateCartOrder(ctx, tenant, idemKey, []Line{{GoodsID: goodsID, Qty: qty}})
	if err != nil {
		t.Fatalf("CreateCartOrder(%s): %v", idemKey, err)
	}
	return ord
}

// paymentRowCount 统计订单的支付流水行数。
func paymentRowCount(t *testing.T, s *Store, orderNo string) int64 {
	t.Helper()
	var n int64
	if err := s.db.Raw("SELECT COUNT(*) FROM payment_records WHERE order_no = ?", orderNo).Scan(&n).Error; err != nil {
		t.Fatalf("count payment_records %s: %v", orderNo, err)
	}
	return n
}

// paymentRecord 读订单首条支付流水的状态/金额（status: 1 created 2 paid 3 failed 4 refunded）。
func paymentRecord(t *testing.T, s *Store, orderNo string) (status int8, amountCent int64) {
	t.Helper()
	var row struct {
		Status     int8  `gorm:"column:status"`
		AmountCent int64 `gorm:"column:amount_cent"`
	}
	if err := s.db.Raw(
		"SELECT status, amount_cent FROM payment_records WHERE order_no = ? ORDER BY id LIMIT 1", orderNo).
		Scan(&row).Error; err != nil {
		t.Fatalf("read payment_records %s: %v", orderNo, err)
	}
	return row.Status, row.AmountCent
}

// taskState 履约任务行读出（status: 1 pending 2 done 3 failed 4 voided）。
type taskState struct {
	LineNo     int32 `gorm:"column:line_no"`
	TaskType   int8  `gorm:"column:task_type"`
	Status     int8  `gorm:"column:status"`
	RetryCount int   `gorm:"column:retry_count"`
}

// taskRowsOf 读订单履约任务行。
func taskRowsOf(t *testing.T, s *Store, orderNo string) []taskState {
	t.Helper()
	var rows []taskState
	if err := s.db.Raw(
		"SELECT line_no, task_type, status, retry_count FROM fulfillment_tasks WHERE order_no = ? ORDER BY line_no",
		orderNo).Scan(&rows).Error; err != nil {
		t.Fatalf("read fulfillment_tasks %s: %v", orderNo, err)
	}
	return rows
}

// orderStatusOf 读订单当前状态与 closed_reason（字符串编码）。
func orderStatusOf(t *testing.T, s *Store, ctx context.Context, tenant uint64, orderNo string) (string, string) {
	t.Helper()
	ord, err := s.GetOrder(ctx, tenant, orderNo)
	if err != nil {
		t.Fatalf("GetOrder %s: %v", orderNo, err)
	}
	return ord.Status, ord.ClosedReason
}

// ---------------------------------------------------------------------------
// 1. TestMarkPaidTx_HappyPath_ThreeTablesConsistent
// 支付回调落账：同一事务内 mall_orders.paid + payment_records(paid) +
// fulfillment_tasks(pending 行) 三表一致可见。
func TestMarkPaidTx_HappyPath_ThreeTablesConsistent(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord := createTestOrder(t, s, ctx, testTenantA, "pay-happy", 113, 1)

	err := s.MarkPaidTx(ctx, Callback{
		OrderNo:    ord.OrderNo,
		Provider:   "mock",
		TradeNo:    "T-pay-happy-1",
		Paid:       true,
		AmountCent: ord.AmountCent,
	})
	if err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}

	// 订单：created → paid，paid_at 已落
	status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo)
	if status != "paid" {
		t.Fatalf("订单 status = %s，want paid", status)
	}
	var paidAt int64
	if err := s.db.Raw("SELECT COUNT(*) FROM mall_orders WHERE order_no = ? AND paid_at IS NOT NULL", ord.OrderNo).
		Scan(&paidAt).Error; err != nil || paidAt != 1 {
		t.Fatalf("paid_at 未落库（rows=%d, err=%v）", paidAt, err)
	}

	// 流水：恰 1 条 paid、金额=订单金额、已验签
	if got := paymentRowCount(t, s, ord.OrderNo); got != 1 {
		t.Fatalf("payment_records = %d 条，want 1", got)
	}
	recStatus, recAmount := paymentRecord(t, s, ord.OrderNo)
	if recStatus != 2 || recAmount != ord.AmountCent {
		t.Fatalf("流水 = (status=%d, amount=%d)，want (2, %d)", recStatus, recAmount, ord.AmountCent)
	}
	var verified int64
	if err := s.db.Raw("SELECT signature_verified FROM payment_records WHERE order_no = ?", ord.OrderNo).
		Scan(&verified).Error; err != nil || verified != 1 {
		t.Fatalf("signature_verified = %d（err=%v），want 1", verified, err)
	}

	// 履约：每行一条 pending 任务，task_type 与订单行 type 一致
	tasks := taskRowsOf(t, s, ord.OrderNo)
	if len(tasks) != len(ord.Items) {
		t.Fatalf("fulfillment_tasks = %d 行，want %d", len(tasks), len(ord.Items))
	}
	for i, task := range tasks {
		want := ord.Items[i]
		if task.LineNo != want.LineNo || int(task.TaskType) != int(want.Type) || task.Status != 1 {
			t.Fatalf("任务行 %d = (line=%d type=%d status=%d)，want (line=%d type=%d pending=1)",
				i, task.LineNo, task.TaskType, task.Status, want.LineNo, want.Type)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. TestMarkPaidTx_DuplicateCallbackDifferentAmount（Review Focus #3）
// 同 (order_no, provider, trade_no) 二次回调且金额不同（999）→ no-op：
// 流水仍 1 条、金额以首回调为准、订单停留 paid。
func TestMarkPaidTx_DuplicateCallbackDifferentAmount(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord := createTestOrder(t, s, ctx, testTenantA, "pay-dup", 115, 1)

	first := Callback{OrderNo: ord.OrderNo, Provider: "mock", TradeNo: "T-dup-1",
		Paid: true, AmountCent: ord.AmountCent}
	if err := s.MarkPaidTx(ctx, first); err != nil {
		t.Fatalf("首次 MarkPaidTx: %v", err)
	}

	second := first
	second.AmountCent = 999
	if err := s.MarkPaidTx(ctx, second); err != nil {
		t.Fatalf("重复回调（金额 999）应幂等 no-op，got: %v", err)
	}

	if got := paymentRowCount(t, s, ord.OrderNo); got != 1 {
		t.Fatalf("payment_records = %d 条，want 1（重复回调不新增流水）", got)
	}
	recStatus, recAmount := paymentRecord(t, s, ord.OrderNo)
	if recStatus != 2 || recAmount != ord.AmountCent {
		t.Fatalf("流水 = (status=%d, amount=%d)，want 以首回调为准 (2, %d)", recStatus, recAmount, ord.AmountCent)
	}
	status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo)
	if status != "paid" {
		t.Fatalf("订单 status = %s，want 仍为 paid", status)
	}
	if got := len(taskRowsOf(t, s, ord.OrderNo)); got != len(ord.Items) {
		t.Fatalf("fulfillment_tasks = %d 行，want %d（重复回调不重复投递履约）", got, len(ord.Items))
	}
}

// ---------------------------------------------------------------------------
// 3. TestMarkPaidTx_VerifyFail_StaysCreated
// 验签错误 → MALL_30005，订单停留 created、无流水落库；
// 并以正确签名做对照：verify 往返应还原 Callback（MockProvider 生成 trade_no）。
func TestMarkPaidTx_VerifyFail_StaysCreated(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord := createTestOrder(t, s, ctx, testTenantA, "pay-verify", 116, 1)

	// MockPaymentProvider 生成 trade_no（占位 Provider 的接口面）
	provider := MockPaymentProvider{}
	tradeNo, err := provider.CreatePayment(ctx, ord.OrderNo, ord.AmountCent)
	if err != nil || tradeNo == "" {
		t.Fatalf("MockPaymentProvider.CreatePayment = (%q, %v)，要求非空 trade_no", tradeNo, err)
	}

	secret := []byte("test-callback-secret")
	cb := Callback{OrderNo: ord.OrderNo, Provider: "mock", TradeNo: tradeNo,
		Paid: true, AmountCent: ord.AmountCent}
	payload, _ := SignCallback(secret, cb)

	// 对照：正确签名可验签还原
	gotCB, verr := NewHMACVerifier(secret)(payload, mustSign(secret, cb))
	if verr != nil || gotCB != cb {
		t.Fatalf("正确签名验签 = (%+v, %v)，要求还原原回调", gotCB, verr)
	}

	// 错误签名 → MALL_30005，订单与流水均不动
	_, badErr := s.MarkPaidFromCallback(ctx, payload, "deadbeef", NewHMACVerifier(secret))
	assertMallCode(t, badErr, mallerr.CodeCallbackSignInvalid)

	status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo)
	if status != "created" {
		t.Fatalf("订单 status = %s，want 停留 created", status)
	}
	if got := paymentRowCount(t, s, ord.OrderNo); got != 0 {
		t.Fatalf("payment_records = %d 条，want 0（验签失败不落流水）", got)
	}
	if got := len(taskRowsOf(t, s, ord.OrderNo)); got != 0 {
		t.Fatalf("fulfillment_tasks = %d 行，want 0", got)
	}
}

// mustSign 用 secret 重签回调（测试内对照签名）。
func mustSign(secret []byte, cb Callback) string {
	_, sig := SignCallback(secret, cb)
	return sig
}
