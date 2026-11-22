// mall-order 支付域测试：回调落账事务（payment.go）。
//
// TDD 硬要求：本文件先于实现编写（RED），实现后转 GREEN。
// 断言锚点 = scripts/seed-goods.sql 种子（113/115/116 为 general 在售）。
// 写入的 orders/items/payment_records/fulfillment_tasks 行与扣减的 stock
// 经 cleanupTenant / snapshotStock 在 t.Cleanup 还原，可重复跑。
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
	ID         uint64 `gorm:"column:id"`
	LineNo     int32  `gorm:"column:line_no"`
	TaskType   int8   `gorm:"column:task_type"`
	Status     int8   `gorm:"column:status"`
	RetryCount int    `gorm:"column:retry_count"`
}

// taskRowsOf 读订单履约任务行。
func taskRowsOf(t *testing.T, s *Store, orderNo string) []taskState {
	t.Helper()
	var rows []taskState
	if err := s.db.Raw(
		"SELECT id, line_no, task_type, status, retry_count FROM fulfillment_tasks WHERE order_no = ? ORDER BY line_no",
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
	snapshotStock(t, s, 113)

	ord := createTestOrder(t, s, ctx, testTenantA, "pay-happy", 113, 1)

	// SignatureVerified=true：模拟已验签的 HTTP 回调入口（流水如实记 1）
	err := s.MarkPaidTx(ctx, Callback{
		OrderNo:           ord.OrderNo,
		Provider:          "mock",
		TradeNo:           "T-pay-happy-1",
		Paid:              true,
		AmountCent:        ord.AmountCent,
		SignatureVerified: true,
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
	snapshotStock(t, s, 115)

	ord := createTestOrder(t, s, ctx, testTenantA, "pay-dup", 115, 1)

	first := Callback{OrderNo: ord.OrderNo, Provider: "mock", TradeNo: "T-dup-1",
		Paid: true, AmountCent: ord.AmountCent}
	if err := s.MarkPaidTx(ctx, first); err != nil {
		t.Fatalf("首次 MarkPaidTx: %v", err)
	}
	// 未置 SignatureVerified（gRPC 内部/mock 通道口径）→ 流水如实记 0
	var sig int8
	if err := s.db.Raw("SELECT signature_verified FROM payment_records WHERE order_no = ?", ord.OrderNo).
		Scan(&sig).Error; err != nil || sig != 0 {
		t.Fatalf("signature_verified = %d（err=%v），want 0（内部通道不虚报验签）", sig, err)
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
	snapshotStock(t, s, 116)

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

// ---------------------------------------------------------------------------
// 4. TestMarkPaidTx_AmountMismatchFailsPayment（终审 I-1 / sequences §1 三分支）
// 首 paid 回调金额与订单金额不一致 → 拒付：受理不报错（返回 nil），落 failed
// 流水（amount_cent=回调金额、审计载荷含订单金额），订单停留 created、不投履约；
// 随后同 trade_no 以正确金额重发 → failed 升级 paid（恢复路径不被污染）。
func TestMarkPaidTx_AmountMismatchFailsPayment(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 113)

	ord := createTestOrder(t, s, ctx, testTenantA, "pay-mismatch", 113, 1)

	// 首回调金额错误（订单金额 +1 分）→ 受理但拒付（nil，非错误）
	bad := Callback{OrderNo: ord.OrderNo, Provider: "mock", TradeNo: "T-mismatch-1",
		Paid: true, AmountCent: ord.AmountCent + 1}
	if err := s.MarkPaidTx(ctx, bad); err != nil {
		t.Fatalf("错额回调应受理不报错（返回 nil），got: %v", err)
	}

	status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo)
	if status != "created" {
		t.Fatalf("订单 status = %s，want 停留 created（错额拒付）", status)
	}
	if got := paymentRowCount(t, s, ord.OrderNo); got != 1 {
		t.Fatalf("payment_records = %d 条，want 1", got)
	}
	recStatus, recAmount := paymentRecord(t, s, ord.OrderNo)
	if recStatus != 3 || recAmount != ord.AmountCent+1 {
		t.Fatalf("流水 = (status=%d, amount=%d)，want failed(3) 且记回调金额 %d",
			recStatus, recAmount, ord.AmountCent+1)
	}
	// 审计载荷：回调金额与订单金额均在案（amount_cent 单列记回调金额，订单金额入 payload；
	// MySQL JSON 列回读带空格规范化，先去空格再比对键值）
	var payload string
	if err := s.db.Raw("SELECT callback_payload FROM payment_records WHERE order_no = ?", ord.OrderNo).
		Scan(&payload).Error; err != nil || payload == "" {
		t.Fatalf("read callback_payload = (%q, %v)，要求非空", payload, err)
	}
	compact := strings.ReplaceAll(payload, " ", "")
	if !strings.Contains(compact, fmt.Sprintf(`"AmountCent":%d`, ord.AmountCent+1)) ||
		!strings.Contains(compact, fmt.Sprintf(`"order_amount_cent":%d`, ord.AmountCent)) {
		t.Fatalf("错额审计载荷缺回调/订单金额：%s", payload)
	}
	if got := len(taskRowsOf(t, s, ord.OrderNo)); got != 0 {
		t.Fatalf("fulfillment_tasks = %d 行，want 0（拒付不投递履约）", got)
	}

	// 恢复：同 trade_no 正确金额重发 → failed 升级 paid，订单置 paid 并投递履约
	if err := s.MarkPaidTx(ctx, Callback{
		OrderNo: ord.OrderNo, Provider: "mock", TradeNo: "T-mismatch-1",
		Paid: true, AmountCent: ord.AmountCent,
	}); err != nil {
		t.Fatalf("正确金额恢复回调: %v", err)
	}
	status, _ = orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo)
	if status != "paid" {
		t.Fatalf("恢复回调后订单 status = %s，want paid", status)
	}
	if got := paymentRowCount(t, s, ord.OrderNo); got != 1 {
		t.Fatalf("payment_records = %d 条，want 1（同号升级不新增流水）", got)
	}
	recStatus, recAmount = paymentRecord(t, s, ord.OrderNo)
	if recStatus != 2 || recAmount != ord.AmountCent {
		t.Fatalf("恢复后流水 = (status=%d, amount=%d)，want paid(2) 且金额=%d",
			recStatus, recAmount, ord.AmountCent)
	}
	tasks := taskRowsOf(t, s, ord.OrderNo)
	if len(tasks) != len(ord.Items) {
		t.Fatalf("恢复后 fulfillment_tasks = %d 行，want %d（正常投递）", len(tasks), len(ord.Items))
	}
	for i, task := range tasks {
		if task.Status != 1 {
			t.Fatalf("任务行 %d status = %d，want pending(1)", i, task.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// 6. TestBeginPaymentTx_CreatesAndReusesInFlight（PRD §5.2 规格对齐）
// 首次建单落 created 流水；未回调前二次建单复用同号；失败回调后在途单失效，
// 再建单分配新号。
func TestBeginPaymentTx_CreatesAndReusesInFlight(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 113)

	ord := createTestOrder(t, s, ctx, testTenantA, "begin-reuse", 113, 1)

	// 1) 首次建单：created 流水 1 条、金额=订单金额
	no1, reused, err := s.BeginPaymentTx(ctx, testTenantA, ord.OrderNo, "mock", ord.AmountCent)
	if err != nil || reused || no1 == "" {
		t.Fatalf("首次 BeginPaymentTx = (no=%q, reused=%v, err=%v)", no1, reused, err)
	}
	if got := paymentRowCount(t, s, ord.OrderNo); got != 1 {
		t.Fatalf("payment_records = %d 条，want 1", got)
	}
	recStatus, recAmount := paymentRecord(t, s, ord.OrderNo)
	if recStatus != 1 || recAmount != ord.AmountCent {
		t.Fatalf("在途流水 = (status=%d, amount=%d)，want (1 created, %d)", recStatus, recAmount, ord.AmountCent)
	}

	// 2) 未回调再建单：复用同号，不新增流水
	no2, reused, err := s.BeginPaymentTx(ctx, testTenantA, ord.OrderNo, "mock", ord.AmountCent)
	if err != nil || !reused || no2 != no1 {
		t.Fatalf("二次 BeginPaymentTx = (no=%q, reused=%v, err=%v)，want 复用 %q", no2, reused, err, no1)
	}
	if got := paymentRowCount(t, s, ord.OrderNo); got != 1 {
		t.Fatalf("复用后 payment_records = %d 条，want 仍 1", got)
	}

	// 3) 失败回调：在途单 created→failed；订单停留 created
	if err := s.MarkPaidTx(ctx, Callback{
		OrderNo: ord.OrderNo, Provider: "mock", TradeNo: no1,
		Paid: false, AmountCent: ord.AmountCent,
	}); err != nil {
		t.Fatalf("失败回调 MarkPaidTx: %v", err)
	}
	if recStatus, _ = paymentRecord(t, s, ord.OrderNo); recStatus != 3 {
		t.Fatalf("失败回调后流水 status = %d，want 3 failed", recStatus)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "created" {
		t.Fatalf("失败回调后订单 = %s，want created", status)
	}

	// 4) 死单不再占用在途位：再建单分配新号
	no3, reused, err := s.BeginPaymentTx(ctx, testTenantA, ord.OrderNo, "mock", ord.AmountCent)
	if err != nil || reused || no3 == "" || no3 == no1 {
		t.Fatalf("失效后 BeginPaymentTx = (no=%q, reused=%v, err=%v)，want 新单号", no3, reused, err)
	}
}

// ---------------------------------------------------------------------------
// 7. TestBeginPaymentTx_PromotesInFlightToPaid
// BeginPayment 建在途单后 paid 回调：在途单升级 paid（不新增流水），
// 订单 created→paid，履约任务投递。
func TestBeginPaymentTx_PromotesInFlightToPaid(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 115)

	ord := createTestOrder(t, s, ctx, testTenantA, "begin-promote", 115, 1)
	no, _, err := s.BeginPaymentTx(ctx, testTenantA, ord.OrderNo, "mock", ord.AmountCent)
	if err != nil {
		t.Fatalf("BeginPaymentTx: %v", err)
	}

	if err := s.MarkPaidTx(ctx, Callback{
		OrderNo: ord.OrderNo, Provider: "mock", TradeNo: no,
		Paid: true, AmountCent: ord.AmountCent, SignatureVerified: true,
	}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}

	if got := paymentRowCount(t, s, ord.OrderNo); got != 1 {
		t.Fatalf("payment_records = %d 条，want 1（在途单升级，不新增）", got)
	}
	if recStatus, _ := paymentRecord(t, s, ord.OrderNo); recStatus != 2 {
		t.Fatalf("流水 status = %d，want 2 paid", recStatus)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "paid" {
		t.Fatalf("订单 = %s，want paid", status)
	}
	if tasks := taskRowsOf(t, s, ord.OrderNo); len(tasks) != len(ord.Items) {
		t.Fatalf("fulfillment_tasks = %d 行，want %d", len(tasks), len(ord.Items))
	}
}

// ---------------------------------------------------------------------------
// 8. TestBeginPaymentTx_RejectsNonCreatedOrWrongTenant
// 订单非 created（已 paid）→ MALL_30002；非本租户 → MALL_30001（不泄漏存在性）；
// 金额 ≤0 → MALL_10005（防御校验，接替已删除的旧 bff CreatePayment 同款防线）。
func TestBeginPaymentTx_RejectsNonCreatedOrWrongTenant(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA, testTenantB)
	snapshotStock(t, s, 116)

	ord := createTestOrder(t, s, ctx, testTenantA, "begin-reject", 116, 1)

	if _, _, err := s.BeginPaymentTx(ctx, testTenantA, ord.OrderNo, "mock", 0); err == nil {
		t.Fatalf("金额 0 建单成功：want MALL_10005")
	} else {
		assertMallCode(t, err, mallerr.CodeInvalidParam)
	}

	if _, _, err := s.BeginPaymentTx(ctx, testTenantB, ord.OrderNo, "mock", ord.AmountCent); err == nil {
		t.Fatalf("跨租户 BeginPaymentTx 成功：want MALL_30001")
	} else {
		assertMallCode(t, err, mallerr.CodeOrderNotFound)
	}

	if err := s.MarkPaidTx(ctx, Callback{
		OrderNo: ord.OrderNo, Provider: "mock", TradeNo: "T-begin-reject-1",
		Paid: true, AmountCent: ord.AmountCent,
	}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	if _, _, err := s.BeginPaymentTx(ctx, testTenantA, ord.OrderNo, "mock", ord.AmountCent); err == nil {
		t.Fatalf("已支付 BeginPaymentTx 成功：want MALL_30002")
	} else {
		assertMallCode(t, err, mallerr.CodeOrderStateInvalid)
	}
}

// ---------------------------------------------------------------------------
// 9. TestBeginPaymentTx_ConcurrentSameOrderSingleInFlight
// 并发两次建单（双击 /pay 竞态）：都成功、同号、在途流水仍恰 1 条
// ——PRD §5.2 在途单即双支付防线。
func TestBeginPaymentTx_ConcurrentSameOrderSingleInFlight(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 113)

	ord := createTestOrder(t, s, ctx, testTenantA, "begin-conc", 113, 1)

	const n = 8
	trades := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			trades[i], _, errs[i] = s.BeginPaymentTx(ctx, testTenantA, ord.OrderNo, "mock", ord.AmountCent)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发建单 %d 失败: %v（快路径 500 即缺陷）", i, errs[i])
		}
		if trades[i] != trades[0] {
			t.Fatalf("并发建单 %d 单号 %q != 首个 %q（必须复用在途单）", i, trades[i], trades[0])
		}
	}
	if got := paymentRowCount(t, s, ord.OrderNo); got != 1 {
		t.Fatalf("payment_records = %d 条，want 1", got)
	}
}

// ---------------------------------------------------------------------------
// 10. TestPaymentRecords_UniqueKeyDBGuard
// uk_order_provider_trade 组合唯一键在 DB 层兜底：同组合重复插入报错
// （幂等不再只依赖「写入前锁订单行」的代码纪律）。
func TestPaymentRecords_UniqueKeyDBGuard(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 115)

	ord := createTestOrder(t, s, ctx, testTenantA, "begin-uk", 115, 1)

	rec := paymentRecordRow{
		OrderNo: ord.OrderNo, Provider: "mock", ProviderTradeNo: "T-uk-dup",
		Status: paymentStatusCreated, AmountCent: ord.AmountCent,
	}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		t.Fatalf("首条流水插入: %v", err)
	}
	rec2 := paymentRecordRow{
		OrderNo: ord.OrderNo, Provider: "mock", ProviderTradeNo: "T-uk-dup",
		Status: paymentStatusCreated, AmountCent: ord.AmountCent,
	}
	if err := s.db.WithContext(ctx).Create(&rec2).Error; err == nil {
		t.Fatalf("同组合重复插入成功：uk_order_provider_trade 未生效")
	} else if !strings.Contains(err.Error(), "1062") {
		t.Fatalf("重复插入错误非 1062 撞键: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 11. TestMarkPaidTx_AmountMismatchVoidsInFlight（评审修复回归）
// 错额 paid 回调落在在途单（created）上：降级 failed 并留错额审计
// （amount_cent 记回调金额），订单停留 created；在途位释放——再建单分配新号。
func TestMarkPaidTx_AmountMismatchVoidsInFlight(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 113)

	ord := createTestOrder(t, s, ctx, testTenantA, "begin-mismatch", 113, 1)
	no, _, err := s.BeginPaymentTx(ctx, testTenantA, ord.OrderNo, "mock", ord.AmountCent)
	if err != nil {
		t.Fatalf("BeginPaymentTx: %v", err)
	}

	if err := s.MarkPaidTx(ctx, Callback{
		OrderNo: ord.OrderNo, Provider: "mock", TradeNo: no,
		Paid: true, AmountCent: ord.AmountCent + 999, SignatureVerified: true,
	}); err != nil {
		t.Fatalf("错额回调 MarkPaidTx: %v", err)
	}

	// 流水降级 failed、金额记回调金额（错额审计口径）
	if got := paymentRowCount(t, s, ord.OrderNo); got != 1 {
		t.Fatalf("payment_records = %d 条，want 1", got)
	}
	recStatus, recAmount := paymentRecord(t, s, ord.OrderNo)
	if recStatus != 3 || recAmount != ord.AmountCent+999 {
		t.Fatalf("流水 = (status=%d, amount=%d)，want (3 failed, %d)", recStatus, recAmount, ord.AmountCent+999)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "created" {
		t.Fatalf("订单 = %s，want created", status)
	}

	// 在途位已释放：再建单拿到新单号（旧实现会永远复用死单）
	no2, reused, err := s.BeginPaymentTx(ctx, testTenantA, ord.OrderNo, "mock", ord.AmountCent)
	if err != nil || reused || no2 == "" || no2 == no {
		t.Fatalf("释放后再建单 = (no=%q, reused=%v, err=%v)，want 新单号", no2, reused, err)
	}
}

// ---------------------------------------------------------------------------
//  12. TestMarkPaidTx_PaidReplayAfterRefundStaysRefunded（二轮评审 c3 回归，
//     预存在缺陷收口）：退款后迟到的 paid 重放不得把流水翻回 paid。
func TestMarkPaidTx_PaidReplayAfterRefundStaysRefunded(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 115)

	ord := createTestOrder(t, s, ctx, testTenantA, "pay-refund-replay", 115, 1)
	cb := Callback{OrderNo: ord.OrderNo, Provider: "mock", TradeNo: "T-refund-replay-1",
		Paid: true, AmountCent: ord.AmountCent, SignatureVerified: true}
	if err := s.MarkPaidTx(ctx, cb); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	if err := s.RefundPaid(ctx, testTenantA, ord.OrderNo); err != nil {
		t.Fatalf("RefundPaid: %v", err)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "refunded" {
		t.Fatalf("退款后订单 = %s，want refunded", status)
	}

	// 迟到的 paid 重放：流水保持 refunded、订单保持 refunded
	if err := s.MarkPaidTx(ctx, cb); err != nil {
		t.Fatalf("重放 MarkPaidTx: %v", err)
	}
	if recStatus, _ := paymentRecord(t, s, ord.OrderNo); recStatus != 4 {
		t.Fatalf("重放后流水 status = %d，want 4 refunded（终态不得翻回）", recStatus)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "refunded" {
		t.Fatalf("重放后订单 = %s，want refunded", status)
	}
}
