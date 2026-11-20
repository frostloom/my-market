// mall-order subscribe 履约测试（R2.5-T2）：worker 对 type=subscribe（行 type=3）
// 的 pending 履约行经 billing.ActivateSubscription 开通/续期月卡
// （ref="order_no:line_no"、plan_id=行快照 ref_plan_id、tenant 取自订单头）。
//
// 幂等与竞态语义对齐 credit 分支（worker.go）：
//   - 重放（同 ref，billing 已激活）→ no-op 摘要 → 仍按履约成功收口；
//   - 失败 → 既有指数退避（retry_count+1，5 败 failed 交对账）；
//   - 层一复检：行被并发退款 voided → 静默跳过（billing 零触达）；
//   - 层二补偿：激活已提交、done 更新 0 行（voided 竞态残留）→ 立即以
//     CancelSubscription（ref="mall-void:order:line"）退订补偿（R2.5 语义：
//     退款=退订，阻止退款后继续续费；当期权益保留，与 T3 退款路径同口径）。
//
// 四组：
//  1. 桩测试（TestWorker_SubscribeLineFulfillsViaBilling 系列）：注入函数桩
//     断言 ref 组装 / plan_id 透传（行快照）/ tenant 取自订单 / activated=false
//     重放收口 / 失败退避 / general+subscription 混合单互不干扰；
//  2. 下单守卫（TestCreateCartOrder_SubscriptionWithoutPlanRejected）：
//     订阅商品未关联 plan（ref_plan_id=0）→ 建单拒绝（MALL_30007）；
//  3. 真 billing 集成（TestWorker_SubscribeRealBilling_MixedOrderFulfills）：
//     spawn services/billing/cmd/billing 子进程（dev MySQL ai_gateway + dev Redis），
//     混合单 general+subscription 各自履约 done、订单 fulfilled、订阅记录
//     active、0 元锚（ref_type="mall-sub"）恰 1 条、余额三口径不动
//     （激活不接真钱）、断电重放 no-op 不重复变更。
//
// 隔离与还原：mall 行按 cleanupTenant 还原 + 301/113 库存快照还原；billing 侧
// 订阅行/topups/tenants/Redis 键在 t.Cleanup 删除。种子前置：goods 301 已上架
// 且 ref_plan_id 关联在售定期套餐（scripts/seed-plans.sql + seed-goods.sql）。
// 禁止并发跑基础设施套件（T1 警示）。
package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frostloom/my-market/pkg/mallerr"
	billingv1 "github.com/frostloom/my-market/pkg/proto/billing/v1"
)

// seedSubscriptionGoods 校验商品 301 种子就绪（上架 + 关联在售定期套餐），
// 返回商品行（测试断言 plan_id 透传用）。种子未应用 → Fatal 并提示种子脚本。
func seedSubscriptionGoods(t *testing.T, s *Store) *goodsRow {
	t.Helper()
	g, err := s.GetGoodsRow(context.Background(), 301)
	if err != nil {
		t.Fatalf("种子商品 301 不在位（先跑 scripts/seed-goods.sql）: %v", err)
	}
	if g.Type != typeSubscription || g.OnSale != 1 || g.RefPlanID == 0 {
		t.Fatalf("商品 301 种子状态异常（type=%d on_sale=%d ref_plan_id=%d），want type=3 on_sale=1 ref_plan_id>0；先跑 scripts/seed-plans.sql + scripts/seed-goods.sql",
			g.Type, g.OnSale, g.RefPlanID)
	}
	return g
}

// itemSnapOf 读订单行快照（ref_plan_id 断言用）。
func itemSnapOf(t *testing.T, s *Store, orderNo string, lineNo int32) (itemType int8, refPlanID uint64) {
	t.Helper()
	var row struct {
		Type      int8   `gorm:"column:type"`
		RefPlanID uint64 `gorm:"column:ref_plan_id"`
	}
	if err := s.db.Raw(
		"SELECT type, ref_plan_id FROM mall_order_items WHERE order_no = ? AND line_no = ?",
		orderNo, lineNo).Scan(&row).Error; err != nil {
		t.Fatalf("read item snapshot %s:%d: %v", orderNo, lineNo, err)
	}
	return row.Type, row.RefPlanID
}

// billingSubOf 读 billing 库该租户订阅行（ORDER BY id LIMIT 1；无行 → nil）。
func billingSubOf(t *testing.T, env *billingTestEnv) *struct {
	ID        uint64    `gorm:"column:id"`
	PlanID    uint64    `gorm:"column:plan_id"`
	Status    string    `gorm:"column:status"`
	AutoRenew int8      `gorm:"column:auto_renew"`
	CycleNum  int       `gorm:"column:cycle_num"`
	CycleEnd  time.Time `gorm:"column:cycle_end"`
} {
	t.Helper()
	var row struct {
		ID        uint64    `gorm:"column:id"`
		PlanID    uint64    `gorm:"column:plan_id"`
		Status    string    `gorm:"column:status"`
		AutoRenew int8      `gorm:"column:auto_renew"`
		CycleNum  int       `gorm:"column:cycle_num"`
		CycleEnd  time.Time `gorm:"column:cycle_end"`
	}
	if err := env.aidb.Raw(
		"SELECT id, plan_id, status, auto_renew, cycle_num, cycle_end FROM subscriptions "+
			"WHERE tenant_id = ? ORDER BY id LIMIT 1", env.tenant).Scan(&row).Error; err != nil {
		t.Fatalf("read billing subscription tenant=%d: %v", env.tenant, err)
	}
	if row.ID == 0 {
		return nil
	}
	return &row
}

// ---------------------------------------------------------------------------
// 1a. 桩：subscribe 行经 billing.ActivateSubscription 履约 → done + 订单
// fulfilled；ref="order_no:line_no"、plan_id=行快照、tenant 取自订单头；
// activated=false 重放（billing 已激活的断电窗口）按 no-op 成功收口。
func TestWorker_SubscribeLineFulfillsViaBilling(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 301)
	g301 := seedSubscriptionGoods(t, s)

	ord, err := s.CreateCartOrder(ctx, testTenantA, "sub-stub-1", []Line{{GoodsID: 301, Qty: 1}})
	if err != nil {
		t.Fatalf("CreateCartOrder: %v", err)
	}
	if len(ord.Items) != 1 || ord.Items[0].Type != typeSubscription {
		t.Fatalf("订单应含恰 1 条 subscribe 行，got %+v", ord.Items)
	}
	// 下单时快照裁定：subscribe 行 ref_plan_id = goods.ref_plan_id。
	if itType, snap := itemSnapOf(t, s, ord.OrderNo, 1); itType != typeSubscription || snap != g301.RefPlanID {
		t.Fatalf("行快照 = (type=%d, ref_plan_id=%d)，want (3, %d)（下单时 plan 快照）", itType, snap, g301.RefPlanID)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-sub-stub", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	tasks := taskRowsOf(t, s, ord.OrderNo)
	if len(tasks) != 1 || tasks[0].TaskType != taskTypeSubscribe || tasks[0].Status != taskStatusPending {
		t.Fatalf("待履约任务行异常: %+v（want 1 行 task_type=3 pending）", tasks)
	}

	stub := &stubBilling{}
	s.billing = stub
	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce: %v", err)
	}
	if done != 1 {
		t.Fatalf("done = %d，want 1（subscribe 行开通后标 done）", done)
	}

	// ref 组装断言：tenant 取自订单头、plan_id=行快照、ref="order_no:line_no"
	//（billing 侧记 ref_type="mall-sub" + ref_id=ref 作 uk 锚）。
	calls := stub.activateCalls()
	if len(calls) != 1 {
		t.Fatalf("billing.ActivateSubscription 调用 %d 次，want 1", len(calls))
	}
	req := calls[0]
	if req.GetTenantId() != int64(testTenantA) {
		t.Fatalf("ActivateSubscription.TenantId = %d，want %d（取自订单）", req.GetTenantId(), testTenantA)
	}
	if req.GetPlanId() != int64(g301.RefPlanID) {
		t.Fatalf("ActivateSubscription.PlanId = %d，want %d（行快照 ref_plan_id）", req.GetPlanId(), g301.RefPlanID)
	}
	if wantRef := fmt.Sprintf("%s:%d", ord.OrderNo, ord.Items[0].LineNo); req.GetRef() != wantRef {
		t.Fatalf("ActivateSubscription.Ref = %q，want %q（order_no:line_no）", req.GetRef(), wantRef)
	}
	if n := len(stub.cancelCalls()); n != 0 {
		t.Fatalf("CancelSubscription 调用 %d 次，want 0（正常履约不退订）", n)
	}

	// 收口：任务行 done、订单 paid→fulfilled。
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusDone {
			t.Fatalf("任务行 line=%d status=%d，want done=2", task.LineNo, task.Status)
		}
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("订单 status = %s，want fulfilled", st)
	}

	// 重放（激活已提交、done 标记丢失的断电窗口）：同 ref 重入 → billing
	// activated=false no-op → 仍按履约成功收口。
	if err := s.db.Exec(
		"UPDATE fulfillment_tasks SET status = ?, retry_count = 0, next_run_at = NOW(3) WHERE order_no = ?",
		taskStatusPending, ord.OrderNo).Error; err != nil {
		t.Fatalf("打回 pending: %v", err)
	}
	stub2 := &stubBilling{nextActivateReplies: []*billingv1.ActivateSubscriptionReply{
		{Activated: false, IsRenewal: false,
			Subscription: &billingv1.SubscriptionSummary{PlanId: int64(g301.RefPlanID), Status: "active"}},
	}}
	s.billing = stub2
	done, err = s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("重放 RunFulfillmentOnce: %v", err)
	}
	if done != 1 {
		t.Fatalf("重放 done = %d，want 1（activated=false no-op 仍算履约成功）", done)
	}
	replay := stub2.activateCalls()
	if len(replay) != 1 || replay[0].GetRef() != req.GetRef() {
		t.Fatalf("重放请求异常: %+v（want 同 ref 重入）", replay)
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusDone {
			t.Fatalf("重放后任务行 status=%d，want done=2", task.Status)
		}
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("重放后订单 status = %s，want 仍 fulfilled", st)
	}
}

// ---------------------------------------------------------------------------
// 1b. 桩：ActivateSubscription 失败 → 走既有退避（不标 done、不推进订单），
// last_error 留痕。
func TestWorker_SubscribeFailureBackoffs(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 301)
	seedSubscriptionGoods(t, s)

	ord, err := s.CreateCartOrder(ctx, testTenantA, "sub-fail-1", []Line{{GoodsID: 301, Qty: 1}})
	if err != nil {
		t.Fatalf("CreateCartOrder: %v", err)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-sub-fail", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	id := taskID(t, s, ord.OrderNo)

	stub := &stubBilling{activateErr: status.Error(codes.Unavailable, "billing down（注入）")}
	s.billing = stub

	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce: %v", err)
	}
	if done != 0 {
		t.Fatalf("done = %d，want 0（subscribe 失败不得标 done）", done)
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "paid" {
		t.Fatalf("订单 status = %s，want 仍 paid（不推进）", st)
	}
	statusV, retry := taskStatusRetry(t, s, id)
	if statusV != taskStatusPending || retry != 1 {
		t.Fatalf("任务行 = (status=%d, retry=%d)，want (pending=1, 1)（首败退避）", statusV, retry)
	}
	if d := taskDelaySeconds(t, s, id); d < 0 || d > 4 {
		t.Fatalf("退避延迟 = %ds，want 约 2s（2^n 序列起步）", d)
	}
}

// ---------------------------------------------------------------------------
// 1c. 桩：general+subscription 混合单互不干扰——subscribe 行独立履约 done；
// ship 行失败时订单不推进（有 pending 行不 promote）、subscribe 的 done 不回滚；
// ship 恢复后下一轮订单 fulfilled。行级快照：general 行 ref_plan_id=0。
func TestWorker_MixedOrder_IndependentFulfillment(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 113, 301)
	g301 := seedSubscriptionGoods(t, s)

	ord, err := s.CreateCartOrder(ctx, testTenantA, "sub-mix-1", []Line{
		{GoodsID: 113, Qty: 1}, {GoodsID: 301, Qty: 1},
	})
	if err != nil {
		t.Fatalf("CreateCartOrder: %v", err)
	}
	if len(ord.Items) != 2 {
		t.Fatalf("订单行 = %d，want 2（混合单场景）", len(ord.Items))
	}
	// 行级快照断言：general 行 ref_plan_id=0、subscribe 行=套餐 id。
	if itType, snap := itemSnapOf(t, s, ord.OrderNo, 1); itType != taskTypeShip || snap != 0 {
		t.Fatalf("general 行快照 = (type=%d, ref_plan_id=%d)，want (1, 0)", itType, snap)
	}
	if itType, snap := itemSnapOf(t, s, ord.OrderNo, 2); itType != typeSubscription || snap != g301.RefPlanID {
		t.Fatalf("subscribe 行快照 = (type=%d, ref_plan_id=%d)，want (3, %d)", itType, snap, g301.RefPlanID)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-sub-mix", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}

	shipCalls := 0
	s.shipLine = func(context.Context, fulfillmentTaskRow) error {
		shipCalls++
		return errors.New("ship 注入失败（第一轮）")
	}
	stub := &stubBilling{}
	s.billing = stub

	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#1: %v", err)
	}
	if done != 1 {
		t.Fatalf("第一轮 done = %d，want 1（仅 subscribe 行履约成功）", done)
	}
	if shipCalls != 1 {
		t.Fatalf("shipLine 调用 %d 次，want 1", shipCalls)
	}
	if n := len(stub.activateCalls()); n != 1 {
		t.Fatalf("ActivateSubscription 调用 %d 次，want 1", n)
	}
	// subscribe 行 done、ship 行退避 pending、订单停留 paid（有 pending 不推进）。
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		switch task.TaskType {
		case taskTypeSubscribe:
			if task.Status != taskStatusDone {
				t.Fatalf("subscribe 任务行 status=%d，want done=2（ship 失败不影响 subscribe 收口）", task.Status)
			}
		case taskTypeShip:
			if task.Status != taskStatusPending {
				t.Fatalf("ship 任务行 status=%d，want pending=1（退避重试）", task.Status)
			}
		default:
			t.Fatalf("混合单出现非法任务类型 %d", task.TaskType)
		}
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "paid" {
		t.Fatalf("订单 status = %s，want 仍 paid（ship 未完成不推进）", st)
	}

	// 第二轮：ship 恢复（mock 恒成功）→ 补齐 done → 订单 fulfilled。
	s.shipLine = nil
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.TaskType == taskTypeShip {
			forceTaskDue(t, s, task.ID)
		}
	}
	done, err = s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#2: %v", err)
	}
	if done != 1 {
		t.Fatalf("第二轮 done = %d，want 1（ship 行补齐）", done)
	}
	if n := len(stub.activateCalls()); n != 1 {
		t.Fatalf("第二轮 ActivateSubscription 调用 %d 次，want 仍 1（subscribe 不重放）", n)
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusDone {
			t.Fatalf("第二轮任务行 line=%d status=%d，want 全部 done", task.LineNo, task.Status)
		}
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("第二轮订单 status = %s，want fulfilled", st)
	}
}

// ---------------------------------------------------------------------------
// 1d. 桩：退款竞态两层防护（与 creditLine 同构）。
//   - 层一：执行前复检——行已被 voided（并发退款先行）→ 静默跳过，billing
//     零触达（不激活也不退订）；
//   - 层二：激活已提交、done 更新 0 行（复检与 done 之间被 voided）→ 立即
//     CancelSubscription（ref="mall-void:order:line"）退订补偿。
func TestWorker_SubscribeVoidCompensation(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 301)
	seedSubscriptionGoods(t, s)

	t.Run("recheck_skips_voided_before_activate", func(t *testing.T) {
		ord, err := s.CreateCartOrder(ctx, testTenantA, "sub-void-a", []Line{{GoodsID: 301, Qty: 1}})
		if err != nil {
			t.Fatalf("CreateCartOrder: %v", err)
		}
		if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
			TradeNo: "T-sub-void-a", Paid: true, AmountCent: ord.AmountCent}); err != nil {
			t.Fatalf("MarkPaidTx: %v", err)
		}
		// 造竞态结果：RefundPaid 已作废任务行并翻转订单（真实事务同款终态）。
		if err := s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
			taskStatusVoided, ord.OrderNo).Error; err != nil {
			t.Fatalf("造 voided 行: %v", err)
		}
		if err := s.db.Exec("UPDATE mall_orders SET status = ? WHERE order_no = ?",
			orderStatusRefunded, ord.OrderNo).Error; err != nil {
			t.Fatalf("造 refunded 单: %v", err)
		}

		stub := &stubBilling{}
		s.billing = stub
		done, err := s.RunFulfillmentOnce(ctx)
		if err != nil {
			t.Fatalf("RunFulfillmentOnce: %v", err)
		}
		if done != 0 {
			t.Fatalf("done = %d，want 0（voided 行静默跳过）", done)
		}
		if n := len(stub.activateCalls()) + len(stub.cancelCalls()); n != 0 {
			t.Fatalf("billing 触达 %d 次，want 0（层一复检：未激活不退订）", n)
		}
		for _, task := range taskRowsOf(t, s, ord.OrderNo) {
			if task.Status != taskStatusVoided {
				t.Fatalf("任务行 status=%d，want 仍 voided=4", task.Status)
			}
		}
	})

	t.Run("no_cancel_when_done_by_other_executor", func(t *testing.T) {
		// R2.5-T3 必改 2：激活提交后 done 更新 0 行在多执行器下歧义——0 行也可能是
		// 另一执行器已把行标 done。补偿前重读任务行：仅 voided 才退订；done（另一
		// 执行器已收口，订阅归属正确）→ info 跳过，绝不误退订在生效的订阅。
		ord, err := s.CreateCartOrder(ctx, testTenantA, "sub-void-c", []Line{{GoodsID: 301, Qty: 1}})
		if err != nil {
			t.Fatalf("CreateCartOrder: %v", err)
		}
		if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
			TradeNo: "T-sub-void-c", Paid: true, AmountCent: ord.AmountCent}); err != nil {
			t.Fatalf("MarkPaidTx: %v", err)
		}
		// 竞态注入：ActivateSubscription 提交后、done 标记前，另一执行器已完成收口
		//（行 done，订单照常 paid——promote 由该执行器负责）——本方 done 更新必 0 行。
		stub := &stubBilling{onActivate: func(*billingv1.ActivateSubscriptionRequest) {
			_ = s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
				taskStatusDone, ord.OrderNo).Error
		}}
		s.billing = stub

		done, err := s.RunFulfillmentOnce(ctx)
		if err != nil {
			t.Fatalf("RunFulfillmentOnce: %v", err)
		}
		if done != 0 {
			t.Fatalf("done = %d，want 0（本方未收口，另一执行器已标 done）", done)
		}
		if n := len(stub.activateCalls()); n != 1 {
			t.Fatalf("ActivateSubscription 调用 %d 次，want 1（同 ref 幂等，重入无害）", n)
		}
		if n := len(stub.cancelCalls()); n != 0 {
			t.Fatalf("CancelSubscription 调用 %d 次，want 0（行已 done=另一执行器收口，不得退订）", n)
		}
		for _, task := range taskRowsOf(t, s, ord.OrderNo) {
			if task.Status != taskStatusDone {
				t.Fatalf("任务行 status=%d，want 保持 done=2", task.Status)
			}
		}
		// 订单随 promote 全量扫描收敛 fulfilled（行已 done = 收口完成，推送
		// 属正常推进；关键断言是不发生退订、不误伤在生效订阅）。
		if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "fulfilled" {
			t.Fatalf("订单 status = %s，want fulfilled（全行 done 由 promote 扫描推进）", st)
		}
	})

	t.Run("cancels_when_voided_after_activate", func(t *testing.T) {
		ord, err := s.CreateCartOrder(ctx, testTenantA, "sub-void-b", []Line{{GoodsID: 301, Qty: 1}})
		if err != nil {
			t.Fatalf("CreateCartOrder: %v", err)
		}
		if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
			TradeNo: "T-sub-void-b", Paid: true, AmountCent: ord.AmountCent}); err != nil {
			t.Fatalf("MarkPaidTx: %v", err)
		}
		// 竞态注入：ActivateSubscription 提交后、done 标记前，RefundPaid 并发
		// 完成（作废行 + 翻转订单）——复检已通过、done 更新必 0 行。
		stub := &stubBilling{onActivate: func(*billingv1.ActivateSubscriptionRequest) {
			_ = s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
				taskStatusVoided, ord.OrderNo).Error
			_ = s.db.Exec("UPDATE mall_orders SET status = ? WHERE order_no = ?",
				orderStatusRefunded, ord.OrderNo).Error
		}}
		s.billing = stub

		done, err := s.RunFulfillmentOnce(ctx)
		if err != nil {
			t.Fatalf("RunFulfillmentOnce: %v", err)
		}
		if done != 0 {
			t.Fatalf("done = %d，want 0（行被 voided，未完成履约收口）", done)
		}
		// 激活恰 1 次（billing 已提交），退订补偿恰 1 次：mall-void ref + tenant 正确。
		if n := len(stub.activateCalls()); n != 1 {
			t.Fatalf("ActivateSubscription 调用 %d 次，want 1", n)
		}
		cancels := stub.cancelCalls()
		if len(cancels) != 1 {
			t.Fatalf("CancelSubscription（退订补偿）调用 %d 次，want 1", len(cancels))
		}
		wantRef := fmt.Sprintf("mall-void:%s:%d", ord.OrderNo, ord.Items[0].LineNo)
		rep := cancels[0]
		if rep.GetRef() != wantRef || rep.GetTenantId() != int64(testTenantA) {
			t.Fatalf("退订请求 = (ref=%s, tenant=%d)，want (%s, %d)",
				rep.GetRef(), rep.GetTenantId(), wantRef, testTenantA)
		}
		// 任务行保持 voided（不可重放）；订单保持 refunded。
		for _, task := range taskRowsOf(t, s, ord.OrderNo) {
			if task.Status != taskStatusVoided {
				t.Fatalf("任务行 status=%d，want 仍 voided=4（不可重放）", task.Status)
			}
		}
		if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "refunded" {
			t.Fatalf("订单 status = %s，want refunded", st)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. 下单守卫：订阅商品未关联 plan（ref_plan_id=0）→ 建单拒绝 MALL_30007
// （fail-closed：不带 plan 快照的订阅行必将在履约侧失败，宁可下单时拒绝）。
func TestCreateCartOrder_SubscriptionWithoutPlanRejected(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	// 临时造无 plan 的上架订阅商品（独立 id，cleanup 删除，不碰种子 301）。
	if err := s.db.Exec(
		"INSERT INTO goods (id, type, category_id, name, subtitle, price_cent, orig_price_cent, " +
			"face_value_cent, bonus_cent, image_url, stock, limited, ref_plan_id, on_sale) " +
			"VALUES (90302, 3, 0, '订阅测试商品（无 plan）', '临时', 100, 0, 0, 0, '', 10, 0, 0, 1)",
	).Error; err != nil {
		t.Fatalf("造临时订阅商品: %v", err)
	}
	t.Cleanup(func() {
		if err := s.db.Exec("DELETE FROM goods WHERE id = 90302").Error; err != nil {
			t.Errorf("cleanup 临时商品 90302: %v", err)
		}
	})

	_, err := s.CreateCartOrder(ctx, testTenantA, "sub-noplan-1", []Line{{GoodsID: 90302, Qty: 1}})
	assertMallCode(t, err, mallerr.CodeGoodsTypeNotAllowed)
	if n := countOrders(t, s, testTenantA); n != 0 {
		t.Fatalf("拒绝路径不得落单: orders=%d", n)
	}
}

// ---------------------------------------------------------------------------
// 3. 真 billing 集成：混合单 general+subscription 各自履约 done、订单
// fulfilled、订阅记录 active、0 元锚恰 1 条、余额三口径不动（激活不接真钱）、
// 断电重放（同 ref）no-op 不重复变更订阅/锚。
func TestWorker_SubscribeRealBilling_MixedOrderFulfills(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	env := startRealBilling(t)
	env.seedTenant(t, 0)
	tenantID := env.tenant
	cleanupTenant(t, s, tenantID)
	// 订阅行清理由本测试自持（seedTenant 只清 topups/tenants/Redis）。
	t.Cleanup(func() {
		if err := env.aidb.Exec("DELETE FROM subscriptions WHERE tenant_id = ?", tenantID).Error; err != nil {
			t.Errorf("cleanup subscriptions tenant=%d: %v", tenantID, err)
		}
	})
	snapshotStock(t, s, 113, 301)
	g301 := seedSubscriptionGoods(t, s)

	ord, err := s.CreateCartOrder(ctx, tenantID, "sub-real-"+env.runID, []Line{
		{GoodsID: 113, Qty: 1}, {GoodsID: 301, Qty: 1},
	})
	if err != nil {
		t.Fatalf("CreateCartOrder: %v", err)
	}
	wantSubRef := fmt.Sprintf("%s:%d", ord.OrderNo, ord.Items[1].LineNo) // 行 2=subscribe（goods_id 升序）
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-sub-real", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}

	rec := &recordingBilling{inner: env.client}
	s.billing = rec

	// 第一轮：ship mock + 真 ActivateSubscription → 双行 done、订单 fulfilled。
	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#1: %v", err)
	}
	if done != 2 {
		t.Fatalf("第一轮 done = %d，want 2（ship + subscribe 各自收口）", done)
	}
	if st, _ := orderStatusOf(t, s, ctx, tenantID, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("第一轮订单 status = %s，want fulfilled", st)
	}

	// 订阅记录：active、auto_renew、cycle_num=1、plan=goods 301 关联套餐、
	// 周期终点 ≈ now+30d（套餐 validity_days）。
	sub := billingSubOf(t, env)
	if sub == nil {
		t.Fatalf("billing 无订阅记录（tenant=%d）", tenantID)
	}
	if sub.Status != "active" || sub.AutoRenew != 1 || sub.CycleNum != 1 {
		t.Fatalf("订阅行 = (status=%s, auto_renew=%d, cycle_num=%d)，want (active, 1, 1)",
			sub.Status, sub.AutoRenew, sub.CycleNum)
	}
	if sub.PlanID != g301.RefPlanID {
		t.Fatalf("订阅 plan_id = %d，want %d（goods 301 关联套餐）", sub.PlanID, g301.RefPlanID)
	}
	if d := time.Until(sub.CycleEnd); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("订阅 cycle_end 距今 %v，want 约 30 天（月度套餐 validity_days）", d)
	}

	// 0 元锚恰 1 条（ref_type="mall-sub"、amount=0）；余额三口径不动。
	if n := env.topupCount(t, "mall-sub", wantSubRef); n != 1 {
		t.Fatalf("topups(%s) = %d 条，want 恰 1", wantSubRef, n)
	}
	if amount, _ := env.topupAmount(t, "mall-sub", wantSubRef); amount != 0 {
		t.Fatalf("激活锚 amount = %d，want 0（0 元流水，余额不动）", amount)
	}
	ledger, column, redisV := env.balances(t)
	if ledger != 0 || column != 0 || redisV != 0 {
		t.Fatalf("激活后余额 = (ledger=%d, column=%d, redis=%d)，want 全 0（激活不接真钱）",
			ledger, column, redisV)
	}

	// 断电窗口：billing 已激活、done 标记丢失 → subscribe 任务行回 pending
	//（ship 行不动）。重放同 ref → activated=false no-op → 收口不重复变更。
	if err := s.db.Exec(
		"UPDATE fulfillment_tasks SET status = ?, retry_count = 0, next_run_at = NOW(3) "+
			"WHERE order_no = ? AND task_type = ?",
		taskStatusPending, ord.OrderNo, taskTypeSubscribe).Error; err != nil {
		t.Fatalf("打回 pending: %v", err)
	}
	done, err = s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#2: %v", err)
	}
	if done != 1 {
		t.Fatalf("重放 done = %d，want 1", done)
	}
	replies := rec.activateReplyLog()
	if len(replies) != 2 {
		t.Fatalf("ActivateSubscription 调用 %d 次，want 2（首轮 + 重放轮）", len(replies))
	}
	if !replies[0].GetActivated() {
		t.Fatalf("首轮 activated = false，want true")
	}
	if replies[1].GetActivated() {
		t.Fatalf("重放轮 activated = true，want false（同 ref no-op）")
	}
	sub2 := billingSubOf(t, env)
	if sub2 == nil || sub2.ID != sub.ID || sub2.CycleNum != sub.CycleNum {
		t.Fatalf("重放后订阅行变更: %+v → %+v（want 同行同期，no-op）", sub, sub2)
	}
	if n := env.topupCount(t, "mall-sub", wantSubRef); n != 1 {
		t.Fatalf("重放后 topups(%s) = %d 条，want 恰 1（uk_ref 幂等）", wantSubRef, n)
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusDone {
			t.Fatalf("重放后任务行 line=%d status=%d，want done=2", task.LineNo, task.Status)
		}
	}
	if st, _ := orderStatusOf(t, s, ctx, tenantID, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("重放后订单 status = %s，want fulfilled", st)
	}
	ledger, column, redisV = env.balances(t)
	if ledger != 0 || column != 0 || redisV != 0 {
		t.Fatalf("重放后余额 = (ledger=%d, column=%d, redis=%d)，want 全 0（不重不漏）",
			ledger, column, redisV)
	}
}
