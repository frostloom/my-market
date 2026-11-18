// mall-order recharge 直购建单集成测试（R2-T2，连 dev MySQL 3307/market）。
//
// TDD 硬要求：本文件先于实现编写（RED：CreateRechargeOrder 未定义），
// 实现后转 GREEN。断言锚点 = scripts/seed-goods.sql 种子 recharge 三档
// （201 face=1000 bonus=0 / 202 face=5000 bonus=300 / 203 face=10000
// bonus=1000，price=face，limited=0，on_sale=1）。
// 写入的 orders/items 与临时改动的 goods.limited/stock 一律 t.Cleanup 还原，
// 种子 44 商品锚点不被污染，可重复跑。cleanup 复用 order_store_test.go 既有模式。
package main

import (
	"context"
	"testing"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// setGoodsRaw 直写 goods.limited/stock（LimitedStock 测试临时开关 + 还原锚点）。
func setGoodsRaw(t *testing.T, s *Store, id uint64, limited int8, stock int) {
	t.Helper()
	if err := s.db.Exec("UPDATE goods SET limited = ?, stock = ? WHERE id = ?", limited, stock, id).Error; err != nil {
		t.Fatalf("改商品 %d (limited=%d stock=%d): %v", id, limited, stock, err)
	}
}

// snapshotGoods 记录 limited+stock 快照并在测试结束后按快照还原
// （LimitedStock 测试改种子行 limited=1 用，防止锚点污染）。
func snapshotGoods(t *testing.T, s *Store, ids ...uint64) {
	t.Helper()
	type snap struct {
		limited int8
		stock   int
	}
	saved := map[uint64]snap{}
	for _, id := range ids {
		row, err := s.GetGoodsRow(context.Background(), id)
		if err != nil {
			t.Fatalf("快照商品 %d: %v", id, err)
		}
		saved[id] = snap{limited: row.Limited, stock: row.Stock}
	}
	t.Cleanup(func() {
		for id, v := range saved {
			setGoodsRaw(t, s, id, v.limited, v.stock)
		}
	})
}

// ---------------------------------------------------------------------------
// 1. TestCreateRechargeOrder_HappyPath：kind=1 事务、单行 credit 快照、
// limited=0 不动库存、幂等同 key 同 payload 返回原单。
func TestCreateRechargeOrder_HappyPath(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 202)
	before := stockOf(t, s, 202)

	// 种子 202：price=face=5000、bonus=300（选 bonus 非零档验证赠送快照）。
	ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "rech-happy-1")
	if err != nil {
		t.Fatalf("CreateRechargeOrder: %v", err)
	}
	if ord.Kind != 1 || ord.Status != "created" || ord.AmountCent != 5000 {
		t.Fatalf("订单头 = (kind=%d, status=%s, amount=%d)，want (1, created, 5000)",
			ord.Kind, ord.Status, ord.AmountCent)
	}
	if ord.TenantID != testTenantA || ord.IdemKey != "rech-happy-1" {
		t.Fatalf("订单归属错: tenant=%d idem=%q", ord.TenantID, ord.IdemKey)
	}
	if len(ord.Items) != 1 {
		t.Fatalf("订单行 = %d，want 1（recharge 单恰一行）", len(ord.Items))
	}
	it := ord.Items[0]
	if it.LineNo != 1 || it.GoodsID != 202 || it.Qty != 1 ||
		it.Type != 2 /* credit */ || it.GoodsType != 2 ||
		it.PriceCent != 5000 || it.FaceValueCent != 5000 || it.BonusCent != 300 ||
		it.GoodsName == "" {
		t.Fatalf("行快照不符: %+v", it)
	}
	// limited=0：不碰库存。
	if got := stockOf(t, s, 202); got != before {
		t.Fatalf("stock(202) = %d，want %d（limited=0 不得扣库存）", got, before)
	}

	// 落库读回一致（kind=1 / 单行 type=2 持久化）。
	back, err := s.GetOrder(ctx, testTenantA, ord.OrderNo)
	if err != nil {
		t.Fatalf("读回订单: %v", err)
	}
	if back.Kind != 1 || len(back.Items) != 1 || back.Items[0].Type != 2 ||
		back.Items[0].BonusCent != 300 || back.AmountCent != 5000 {
		t.Fatalf("落库读回不符: kind=%d items=%+v amount=%d", back.Kind, back.Items, back.AmountCent)
	}

	// 幂等：同 key 同 payload → 返回原单，不落第二单、不重复动账。
	again, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "rech-happy-1")
	if err != nil {
		t.Fatalf("重放建单: %v（同 key 同 payload 应返回原单）", err)
	}
	if again.OrderNo != ord.OrderNo || again.AmountCent != ord.AmountCent {
		t.Fatalf("重放 = (%s, %d)，want 原单 (%s, %d)",
			again.OrderNo, again.AmountCent, ord.OrderNo, ord.AmountCent)
	}
	if got := countOrders(t, s, testTenantA); got != 1 {
		t.Fatalf("orders = %d，want 1（重放不落新单）", got)
	}
}

// ---------------------------------------------------------------------------
// 2. TestCreateRechargeOrder_RejectsGeneralGoods：type=1 general 不可直购
// → MALL_30007；不落单、不动库存。
func TestCreateRechargeOrder_RejectsGeneralGoods(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 101)
	before := stockOf(t, s, 101)

	_, err := s.CreateRechargeOrder(ctx, testTenantA, 101, "rech-gen-1")
	assertMallCode(t, err, mallerr.CodeGoodsTypeNotAllowed)
	if got := countOrders(t, s, testTenantA); got != 0 {
		t.Fatalf("orders = %d，want 0（拒绝请求不得落单）", got)
	}
	if got := stockOf(t, s, 101); got != before {
		t.Fatalf("stock(101) = %d，want %d（拒绝请求不得动库存）", got, before)
	}
}

// ---------------------------------------------------------------------------
// 3. TestCreateRechargeOrder_LimitedStock：limited=1 时条件扣库存——
// 首单扣 1、售罄后第二单（不同 key）MALL_20003 整体回滚。
// 种子 201-203 均 limited=0：测试内临时 UPDATE limited=1, stock=1，Cleanup 还原。
func TestCreateRechargeOrder_LimitedStock(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotGoods(t, s, 202) // limited+stock 快照还原

	setGoodsRaw(t, s, 202, 1 /*limited*/, 1 /*stock*/)

	ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "rech-lim-1")
	if err != nil {
		t.Fatalf("首单: %v", err)
	}
	if got := stockOf(t, s, 202); got != 0 {
		t.Fatalf("stock(202) = %d，want 0（limited=1 单内条件扣减）", got)
	}

	_, err = s.CreateRechargeOrder(ctx, testTenantA, 202, "rech-lim-2")
	assertMallCode(t, err, mallerr.CodeOutOfStock)
	if got := countOrders(t, s, testTenantA); got != 1 {
		t.Fatalf("orders = %d，want 1（售罄请求必须整体回滚，不留脏单）", got)
	}
	if ord.OrderNo == "" {
		t.Fatalf("首单号缺失")
	}
}

// ---------------------------------------------------------------------------
// 4. TestCreateRechargeOrder_IdemConflict：同 key 不同 payload（goods_id
// 不同）→ MALL_30003；原单保持不变、两次请求都不动库存。
func TestCreateRechargeOrder_IdemConflict(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 201, 202)
	before201, before202 := stockOf(t, s, 201), stockOf(t, s, 202)

	orig, err := s.CreateRechargeOrder(ctx, testTenantA, 201, "rech-conflict-1")
	if err != nil {
		t.Fatalf("首次建单: %v", err)
	}
	_, err = s.CreateRechargeOrder(ctx, testTenantA, 202, "rech-conflict-1")
	assertMallCode(t, err, mallerr.CodeIdempotencyConflict)

	if got := countOrders(t, s, testTenantA); got != 1 {
		t.Fatalf("orders = %d，want 1（冲突请求不得落单）", got)
	}
	back, err := s.GetOrder(ctx, testTenantA, orig.OrderNo)
	if err != nil {
		t.Fatalf("读回原单: %v", err)
	}
	if len(back.Items) != 1 || back.Items[0].GoodsID != 201 || back.AmountCent != 1000 {
		t.Fatalf("原单被冲突请求污染: %+v", back.Items)
	}
	if got := stockOf(t, s, 201); got != before201 {
		t.Fatalf("stock(201) = %d，want %d", got, before201)
	}
	if got := stockOf(t, s, 202); got != before202 {
		t.Fatalf("stock(202) = %d，want %d（冲突请求整体回滚）", got, before202)
	}
}

// ---------------------------------------------------------------------------
// 5. TestCancelRechargeOrder_RestockMatchesDeduction：取消/超时回补必须与
// 建单扣减对称——limited=0（建单未扣库存）取消后不得回补（否则 stock 虚增、
// 污染种子锚点）；limited=1（建单扣了 1）取消后回补 1。
// cart（kind=2）全行回补的既有口径由 timeout_test.go 既有测试锚定，不回归。
func TestCancelRechargeOrder_RestockMatchesDeduction(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotGoods(t, s, 202) // limited+stock 快照还原（本测试临时改 limited）

	// 1) 无限量档（种子 limited=0，stock=0）：建单不动库存，取消也不得动。
	ordU, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "rech-cancel-u")
	if err != nil {
		t.Fatalf("无限量档建单: %v", err)
	}
	before := stockOf(t, s, 202)
	if err := s.CancelOrder(ctx, testTenantA, ordU.OrderNo); err != nil {
		t.Fatalf("取消无限量档订单: %v", err)
	}
	status, _ := orderStatusOf(t, s, ctx, testTenantA, ordU.OrderNo)
	if status != "closed" {
		t.Fatalf("取消后 status = %s，want closed", status)
	}
	if got := stockOf(t, s, 202); got != before {
		t.Fatalf("取消后 stock(202) = %d，want %d（limited=0 建单未扣，取消不得回补）", got, before)
	}

	// 2) 限量档（临时 limited=1, stock=3）：建单扣 1，取消回补 1。
	setGoodsRaw(t, s, 202, 1 /*limited*/, 3 /*stock*/)
	ordL, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "rech-cancel-l")
	if err != nil {
		t.Fatalf("限量档建单: %v", err)
	}
	if got := stockOf(t, s, 202); got != 2 {
		t.Fatalf("限量档建单后 stock(202) = %d，want 2（单内条件扣减）", got)
	}
	if err := s.CancelOrder(ctx, testTenantA, ordL.OrderNo); err != nil {
		t.Fatalf("取消限量档订单: %v", err)
	}
	if got := stockOf(t, s, 202); got != 3 {
		t.Fatalf("取消后 stock(202) = %d，want 3（limited=1 扣减对称回补）", got)
	}
}
