// mall-order 履约 outbox worker 测试（worker.go）。
//
// TDD 硬要求：本文件先于实现编写（RED），实现后转 GREEN。
// 锚点：种子 general 商品 113/114（两行单）；
// 写入行经 cleanupTenant 在 t.Cleanup 还原，可重复跑。
package main

import (
	"context"
	"testing"
)

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
