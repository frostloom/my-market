// mall-order 履约 outbox worker 测试（worker.go）。
//
// TDD 硬要求：本文件先于实现编写（RED），实现后转 GREEN。
// 锚点：种子 general 商品 113/114（两行单）；
// 写入行经 cleanupTenant 在 t.Cleanup 还原，可重复跑。
package main

import (
	"context"
	"testing"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// taskID 订单首条履约任务行 id。
func taskID(t *testing.T, s *Store, orderNo string) uint64 {
	t.Helper()
	var id uint64
	if err := s.db.Raw("SELECT id FROM fulfillment_tasks WHERE order_no = ? ORDER BY id LIMIT 1", orderNo).
		Scan(&id).Error; err != nil {
		t.Fatalf("read task id %s: %v", orderNo, err)
	}
	return id
}

// taskDelaySeconds 任务 next_run_at 相对 DB 会话 NOW(3) 的秒数（写入与读取
// 两侧同为 SQL 时钟，不受 dev 容器 UTC 会话影响）。
func taskDelaySeconds(t *testing.T, s *Store, id uint64) int64 {
	t.Helper()
	var d int64
	if err := s.db.Raw("SELECT TIMESTAMPDIFF(SECOND, NOW(3), next_run_at) FROM fulfillment_tasks WHERE id = ?", id).
		Scan(&d).Error; err != nil {
		t.Fatalf("read task delay %d: %v", id, err)
	}
	return d
}

// forceTaskDue 强制任务到期（模拟退避等待流逝；SQL 时钟口径）。
func forceTaskDue(t *testing.T, s *Store, id uint64) {
	t.Helper()
	if err := s.db.Exec("UPDATE fulfillment_tasks SET next_run_at = NOW(3) WHERE id = ?", id).Error; err != nil {
		t.Fatalf("force due task %d: %v", id, err)
	}
}

// taskStatusRetry 读任务 (status, retry_count)。
func taskStatusRetry(t *testing.T, s *Store, id uint64) (int8, int) {
	t.Helper()
	var row struct {
		Status     int8 `gorm:"column:status"`
		RetryCount int  `gorm:"column:retry_count"`
	}
	if err := s.db.Raw("SELECT status, retry_count FROM fulfillment_tasks WHERE id = ?", id).
		Scan(&row).Error; err != nil {
		t.Fatalf("read task state %d: %v", id, err)
	}
	return row.Status, row.RetryCount
}

// ---------------------------------------------------------------------------
// 4. TestRunFulfillmentOnce_ShipLinesToDone_ThenOrderFulfilled
// 两行任务 → 跑一轮全部 ship→done 且订单 fulfilled；再跑一轮幂等
// （done=0、状态不变）。
func TestRunFulfillmentOnce_ShipLinesToDone_ThenOrderFulfilled(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord, err := s.CreateCartOrder(ctx, testTenantA, "ful-fill", []Line{
		{GoodsID: 113, Qty: 1}, {GoodsID: 114, Qty: 1},
	})
	if err != nil {
		t.Fatalf("建单: %v", err)
	}
	if len(ord.Items) != 2 {
		t.Fatalf("订单行 = %d，want 2（两行任务场景）", len(ord.Items))
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-ful-1", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}

	// 第一轮：两行全部 ship→done，订单 paid→fulfilled
	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#1: %v", err)
	}
	if done != 2 {
		t.Fatalf("第一轮 done = %d，want 2", done)
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != 2 {
			t.Fatalf("任务行 line=%d status=%d，want done=2", task.LineNo, task.Status)
		}
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "fulfilled" {
		t.Fatalf("订单 status = %s，want fulfilled（首轮全行 done 即推进）", status)
	}

	// 第二轮：无到期 pending → done=0，状态与任务行不再变化
	done, err = s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#2: %v", err)
	}
	if done != 0 {
		t.Fatalf("第二轮 done = %d，want 0（幂等）", done)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "fulfilled" {
		t.Fatalf("第二轮订单 status = %s，want 仍为 fulfilled", status)
	}
}

// ---------------------------------------------------------------------------
// 5. TestRunFulfillmentOnce_CrashReplay
// 手动造 paid 订单 + pending 任务行（模拟"回调已落账、worker 崩溃未跑"），
// RunFulfillmentOnce 重放收敛到 fulfilled；重复执行幂等。
func TestRunFulfillmentOnce_CrashReplay(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord, err := s.CreateCartOrder(ctx, testTenantA, "ful-crash", []Line{{GoodsID: 113, Qty: 2}})
	if err != nil {
		t.Fatalf("建单: %v", err)
	}

	// 手动造崩溃现场：订单已置 paid（跳过 MarkPaidTx 的事务），
	// 履约任务行以 pending 落库（等价 outbox 半程后进程崩溃）。
	if err := s.db.Exec(
		"UPDATE mall_orders SET status = 2, paid_at = NOW(3) WHERE order_no = ?", ord.OrderNo).Error; err != nil {
		t.Fatalf("造 paid 订单: %v", err)
	}
	if err := s.db.Exec(
		"INSERT INTO fulfillment_tasks (order_no, line_no, task_type, status, retry_count, next_run_at) "+
			"SELECT order_no, line_no, type, 1, 0, NOW(3) FROM mall_order_items WHERE order_no = ?",
		ord.OrderNo).Error; err != nil {
		t.Fatalf("造 pending 任务行: %v", err)
	}

	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#1（崩溃重放）: %v", err)
	}
	if done != len(ord.Items) {
		t.Fatalf("重放 done = %d，want %d", done, len(ord.Items))
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "fulfilled" {
		t.Fatalf("订单 status = %s，want fulfilled（重放收敛）", status)
	}

	// 重复执行：无 pending 可拉 → done=0，不报错、状态不变
	done, err = s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#2（幂等）: %v", err)
	}
	if done != 0 {
		t.Fatalf("重复执行 done = %d，want 0", done)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "fulfilled" {
		t.Fatalf("重复执行后订单 status = %s，want 仍为 fulfilled", status)
	}
}

// ---------------------------------------------------------------------------
// 5b. TestWorker_BackoffRetriesAndFails
// 注入恒失败 ship（Store.shipLine 接缝）→ 退避延迟序列 2/4/8/16/32s
// （每轮手动强制到期模拟时间流逝），第 5 次失败转 failed 且 retry_count=5
// （上限 5 口径）；终态不再拉取、订单停留 paid（含 failed 行留给对账）。
func TestWorker_BackoffRetriesAndFails(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	// 注入故障：替换履约接缝（真实渠道接入前的故障演练口径）
	s.shipLine = func(context.Context, fulfillmentTaskRow) error {
		return errInjectedShipFailure
	}

	ord := createTestOrder(t, s, ctx, testTenantA, "worker-backoff", 113, 1)
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-backoff", Paid: true, AmountCent: ord.AmountCent,
		SignatureVerified: true}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	id := taskID(t, s, ord.OrderNo)
	if id == 0 {
		t.Fatal("无履约任务行")
	}

	// 5 轮失败：退避 2^(旧retry_count+1) 秒 → 2/4/8/16/32
	wantDelays := []int64{2, 4, 8, 16, 32}
	for i, want := range wantDelays {
		if i > 0 {
			forceTaskDue(t, s, id)
		}
		done, err := s.RunFulfillmentOnce(ctx)
		if err != nil {
			t.Fatalf("第 %d 轮 RunFulfillmentOnce: %v", i+1, err)
		}
		if done != 0 {
			t.Fatalf("第 %d 轮 done = %d，want 0（ship 恒失败）", i+1, done)
		}
		if got := taskDelaySeconds(t, s, id); got < want-2 || got > want+2 {
			t.Fatalf("第 %d 轮退避延迟 = %ds，want %d±2s（序列 2/4/8/16/32）", i+1, got, want)
		}
		status, retry := taskStatusRetry(t, s, id)
		if i < 4 {
			if status != taskStatusPending || retry != i+1 {
				t.Fatalf("第 %d 轮后 = (status=%d, retry=%d)，want (pending=1, %d)", i+1, status, retry, i+1)
			}
			continue
		}
		// 第 5 次失败：达上限转 failed，retry_count 终值 5
		if status != taskStatusFailed || retry != 5 {
			t.Fatalf("第 5 轮失败后 = (status=%d, retry=%d)，want (failed=3, 5)", status, retry)
		}
	}

	// 终态：failed 不再被拉取；订单含 failed 行不推进 fulfilled（对账介入）
	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("终态复跑: %v", err)
	}
	if done != 0 {
		t.Fatalf("终态复跑 done = %d，want 0（failed 终态不重跑）", done)
	}
	if status, retry := taskStatusRetry(t, s, id); status != taskStatusFailed || retry != 5 {
		t.Fatalf("终态 = (status=%d, retry=%d)，want (failed=3, 5)", status, retry)
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "paid" {
		t.Fatalf("含 failed 行的订单被推进: status = %s，want 仍 paid（留给对账）", st)
	}
}

// errInjectedShipFailure 注入型 ship 故障（测试用哨兵错误）。
var errInjectedShipFailure = mallerr.E(mallerr.CodeUpstreamUnavailable, true, "ship 渠道不可用（注入故障）")

// ---------------------------------------------------------------------------
// 5c. TestWorker_PromoteSweepHealsCrashWindow
// promote 崩溃窗口自愈：履约行已全 done 但订单停留 paid（模拟上一 tick
// 标完 done 后、promote 前崩溃），下一 tick 的全量幂等扫描应推进 fulfilled。
func TestWorker_PromoteSweepHealsCrashWindow(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord := createTestOrder(t, s, ctx, testTenantA, "worker-sweep", 113, 1)
	// 手动造崩溃窗口现场：订单 paid + 履约行全部 done（跳过 promote）
	if err := s.db.Exec(
		"UPDATE mall_orders SET status = 2, paid_at = NOW(3) WHERE order_no = ?", ord.OrderNo).Error; err != nil {
		t.Fatalf("造 paid 订单: %v", err)
	}
	if err := s.db.Exec(
		"INSERT INTO fulfillment_tasks (order_no, line_no, task_type, status, retry_count, next_run_at) "+
			"SELECT order_no, line_no, type, 1, 0, NOW(3) FROM mall_order_items WHERE order_no = ?",
		ord.OrderNo).Error; err != nil {
		t.Fatalf("造任务行: %v", err)
	}
	if err := s.db.Exec(
		"UPDATE fulfillment_tasks SET status = 2 WHERE order_no = ?", ord.OrderNo).Error; err != nil {
		t.Fatalf("造 done 行: %v", err)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "paid" {
		t.Fatalf("前置态 status = %s，want paid（崩溃窗口）", status)
	}

	// 本 tick 无 pending 可拉（done=0），全量扫描应自愈推进
	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce: %v", err)
	}
	if done != 0 {
		t.Fatalf("done = %d，want 0（无 pending 任务）", done)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "fulfilled" {
		t.Fatalf("扫描后 status = %s，want fulfilled（崩溃窗口自愈）", status)
	}

	// 再跑一轮：幂等（无重复推进副作用）
	if _, err := s.RunFulfillmentOnce(ctx); err != nil {
		t.Fatalf("复跑: %v", err)
	}
	if status, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); status != "fulfilled" {
		t.Fatalf("复跑后 status = %s，want 仍为 fulfilled", status)
	}
}
