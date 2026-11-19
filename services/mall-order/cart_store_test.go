// mall-order 购物车集成测试：upsert 合并 qty、行数/数量上限、类型与在售校验。
// 写入的 cart_items 行按 tenant 在 cleanupTenant 中清除，可重复跑。
package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// cartRows/viewOf 等断言辅助（视图行数与指定商品 qty）。
func cartQtyOf(t *testing.T, v *CartView, goodsID uint64) (int, bool) {
	t.Helper()
	for _, l := range v.Items {
		if l.GoodsID == goodsID {
			return l.Qty, true
		}
	}
	return 0, false
}

func mustAddCart(t *testing.T, s *Store, tenant, goodsID uint64, qty int) *CartView {
	t.Helper()
	v, err := s.AddCart(context.Background(), tenant, goodsID, qty)
	if err != nil {
		t.Fatalf("AddCart(goods=%d, qty=%d): %v", goodsID, qty, err)
	}
	return v
}

// ---------------------------------------------------------------------------
// 8. TestCart_UpsertMergeAndLimits
// 加购合并（uk_tenant_goods）、PATCH qty=0 删行、DELETE 幂等、
// 20 行上限 MALL_20004 / qty 1..99 MALL_20005 / recharge 不可加购 MALL_30007。
func TestCart_UpsertMergeAndLimits(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantC)

	// -- upsert 合并：同商品两次加购合并为一行 qty 累加
	v := mustAddCart(t, s, testTenantC, 101, 1)
	if len(v.Items) != 1 {
		t.Fatalf("首次加购行数 = %d，want 1", len(v.Items))
	}
	v = mustAddCart(t, s, testTenantC, 101, 2)
	if len(v.Items) != 1 {
		t.Fatalf("同商品重复加购未合并：行数 = %d，want 1", len(v.Items))
	}
	if q, ok := cartQtyOf(t, v, 101); !ok || q != 3 {
		t.Fatalf("合并后 qty = (%d, %v)，want (3, true)", q, ok)
	}
	v = mustAddCart(t, s, testTenantC, 112, 1)
	v = mustAddCart(t, s, testTenantC, 113, 2)
	if len(v.Items) != 3 {
		t.Fatalf("行数 = %d，want 3", len(v.Items))
	}
	// 合计 = Σ 当前价×qty（499900×3 + 59900×1 + 6900×2）
	if want := int64(499900*3 + 59900*1 + 6900*2); v.TotalCent != want {
		t.Fatalf("total_cent = %d，want %d", v.TotalCent, want)
	}

	// -- PATCH：改 qty 生效；qty=0 等价删除
	id101 := v.Items[0].ID // 行序按 id 升序，首行即 101
	v, err := s.PatchCartQty(ctx, testTenantC, id101, 5)
	if err != nil {
		t.Fatalf("PatchCartQty(5): %v", err)
	}
	if q, _ := cartQtyOf(t, v, 101); q != 5 {
		t.Fatalf("PATCH 后 qty = %d，want 5", q)
	}
	v, err = s.PatchCartQty(ctx, testTenantC, id101, 0)
	if err != nil {
		t.Fatalf("PatchCartQty(0): %v（qty=0 等价删除）", err)
	}
	if len(v.Items) != 2 {
		t.Fatalf("qty=0 删行后行数 = %d，want 2", len(v.Items))
	}
	if _, err = s.PatchCartQty(ctx, testTenantC, id101, 3); err == nil {
		t.Fatalf("PATCH 已删行应报错（购物车行不存在）")
	}

	// -- REMOVE：幂等，重复删除不报错
	id112 := v.Items[0].ID
	if _, err = s.RemoveCart(ctx, testTenantC, id112); err != nil {
		t.Fatalf("RemoveCart: %v", err)
	}
	if _, err = s.RemoveCart(ctx, testTenantC, id112); err != nil {
		t.Fatalf("RemoveCart 重复删除: %v（应幂等 200）", err)
	}
	v, _ = s.GetCart(ctx, testTenantC)
	if len(v.Items) != 1 {
		t.Fatalf("删行后行数 = %d，want 1", len(v.Items))
	}

	// -- 数量上限：单行 99 封顶；合并超 99 拒绝；qty 越界拒绝
	v = mustAddCart(t, s, testTenantC, 114, 99)
	if q, _ := cartQtyOf(t, v, 114); q != 99 {
		t.Fatalf("qty=99 应成功，got %d", q)
	}
	_, err = s.AddCart(ctx, testTenantC, 114, 1) // 99+1=100 > 99
	assertMallCode(t, err, mallerr.CodeGoodsQtyExceeded)
	_, err = s.AddCart(ctx, testTenantC, 115, 100)
	assertMallCode(t, err, mallerr.CodeGoodsQtyExceeded)
	_, err = s.AddCart(ctx, testTenantC, 115, 0)
	assertMallCode(t, err, mallerr.CodeGoodsQtyExceeded)

	// -- 类型与在售校验：recharge 拒 30007、未上架拒 20002、不存在拒 20001
	_, err = s.AddCart(ctx, testTenantC, 201, 1)
	assertMallCode(t, err, mallerr.CodeGoodsTypeNotAllowed)
	// goods 301（R2.5 月卡）已上架、subscription 可购：应加购成功（先加后删，
	// 不影响下方行数计数）；未上架锚改用临时下架的 113。
	mustAddCart(t, s, testTenantC, 301, 1)
	v, _ = s.GetCart(ctx, testTenantC)
	id301 := uint64(0)
	for _, it := range v.Items {
		if it.GoodsID == 301 {
			id301 = it.ID
		}
	}
	if id301 == 0 {
		t.Fatalf("上架的 subscription 商品 301 应可加购")
	}
	if err := s.db.Exec("UPDATE goods SET on_sale = 0 WHERE id = 113").Error; err != nil {
		t.Fatalf("临时下架 113: %v", err)
	}
	t.Cleanup(func() {
		_ = s.db.Exec("UPDATE goods SET on_sale = 1 WHERE id = 113").Error
	})
	_, err = s.AddCart(ctx, testTenantC, 113, 1)
	assertMallCode(t, err, mallerr.CodeGoodsOffSale)
	if err := s.db.Exec("UPDATE goods SET on_sale = 1 WHERE id = 113").Error; err != nil {
		t.Fatalf("还原 113 在售: %v", err)
	}
	if _, err = s.RemoveCart(ctx, testTenantC, id301); err != nil {
		t.Fatalf("RemoveCart(301): %v", err)
	}
	_, err = s.AddCart(ctx, testTenantC, 999999, 1)
	assertMallCode(t, err, mallerr.CodeGoodsNotFound)

	// -- 行数上限 20：当前 2 行，补到 20 行后第 21 行拒 MALL_20004
	for _, gid := range []uint64{105, 106, 107, 108, 109, 110, 111, 115,
		116, 117, 118, 119, 120, 121, 122, 123, 124, 125} {
		mustAddCart(t, s, testTenantC, gid, 1)
	}
	v, _ = s.GetCart(ctx, testTenantC)
	if len(v.Items) != 20 {
		t.Fatalf("行数 = %d，want 20", len(v.Items))
	}
	_, err = s.AddCart(ctx, testTenantC, 126, 1)
	assertMallCode(t, err, mallerr.CodeCartRowsExceeded)
}

// ---------------------------------------------------------------------------
// 9. TestAddCart_RejectsSeckillGoods（Fix Round 1）
// 契约 §4「recharge/秒杀商品返回 MALL_30007」：存在 status=2（上线）
// seckill_activities 的商品不可加购；同一商品无活动（或仅草稿 status=1）
// 时加购正常（正反两断言）。活动行在 t.Cleanup 删除。
func TestAddCart_RejectsSeckillGoods(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantC)

	// 正断言（无活动）：同商品加购正常
	v := mustAddCart(t, s, testTenantC, 113, 1)
	if q, _ := cartQtyOf(t, v, 113); q != 1 {
		t.Fatalf("无活动时加购 qty = %d，want 1", q)
	}

	// 反断言（上线活动 status=2）：加购拒 MALL_30007（合并路径同样被拦）
	insertSeckillActivity(t, s, 113, 2) // 行由 t.Cleanup 删除
	if _, err := s.AddCart(ctx, testTenantC, 113, 1); err == nil {
		t.Fatalf("上线秒杀活动商品加购应被拒")
	} else {
		assertMallCode(t, err, mallerr.CodeGoodsTypeNotAllowed)
	}

	// 语义边界（与实现注释口径一致）：草稿活动 status=1 不拒
	_ = insertSeckillActivity(t, s, 114, 1)
	mustAddCart(t, s, testTenantC, 114, 1)
}

// ---------------------------------------------------------------------------
// 10. TestCart_SubscriptionQtyLimitOne（R2.5-T3 必改 1：订阅 qty=1 限购）
// 订阅商品行级履约模型一行只开通一期——qty>1 收双倍货款只开一期（资金缺口），
// 购物车层全部入口限购 qty=1：AddCart 新行 / AddCart 合并 / PATCH 改数量，
// 越界一律 MALL_20005；qty=1 正常放行（正反两断言）。
func TestCart_SubscriptionQtyLimitOne(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantC)
	seedSubscriptionGoods(t, s) // 301 在售且已关联套餐（种子前置校验）

	// 反断言：订阅商品 qty=2 新行加购 → MALL_20005，购物车不落行。
	if _, err := s.AddCart(ctx, testTenantC, 301, 2); err == nil {
		t.Fatalf("订阅商品 qty=2 加购应被拒")
	} else {
		assertMallCode(t, err, mallerr.CodeGoodsQtyExceeded)
	}
	if v := mustViewCart(t, s, testTenantC); len(v.Items) != 0 {
		t.Fatalf("拒绝路径不得落购物车行: rows=%d", len(v.Items))
	}

	// 正断言：qty=1 加购放行。
	v := mustAddCart(t, s, testTenantC, 301, 1)
	if q, ok := cartQtyOf(t, v, 301); !ok || q != 1 {
		t.Fatalf("订阅商品 qty=1 加购 = (ok=%v, qty=%d)，want (true, 1)", ok, q)
	}

	// 反断言：合并路径（已有 qty=1 行再加 1）→ MALL_20005，qty 保持 1。
	if _, err := s.AddCart(ctx, testTenantC, 301, 1); err == nil {
		t.Fatalf("订阅商品合并加购（1+1=2）应被拒")
	} else {
		assertMallCode(t, err, mallerr.CodeGoodsQtyExceeded)
	}
	v = mustViewCart(t, s, testTenantC)
	if q, _ := cartQtyOf(t, v, 301); q != 1 {
		t.Fatalf("合并被拒后 qty = %d，want 保持 1", q)
	}

	// 反断言：PATCH 改数量 qty=3 → MALL_20005（第三条绕过口，同口径拦截）。
	var itemID uint64
	for _, l := range v.Items {
		if l.GoodsID == 301 {
			itemID = l.ID
		}
	}
	if itemID == 0 {
		t.Fatalf("购物车缺 301 行，无法测 PATCH")
	}
	if _, err := s.PatchCartQty(ctx, testTenantC, itemID, 3); err == nil {
		t.Fatalf("订阅商品 PATCH qty=3 应被拒")
	} else {
		assertMallCode(t, err, mallerr.CodeGoodsQtyExceeded)
	}
	// 正断言：PATCH qty=1 幂等放行（qty=0 删除口径不变，本用例不覆盖）。
	if _, err := s.PatchCartQty(ctx, testTenantC, itemID, 1); err != nil {
		t.Fatalf("订阅商品 PATCH qty=1: %v", err)
	}
	if q, _ := cartQtyOf(t, mustViewCart(t, s, testTenantC), 301); q != 1 {
		t.Fatalf("PATCH qty=1 后 qty = %d，want 1", q)
	}
}

// mustViewCart 读购物车视图（失败即 Fatal）。
func mustViewCart(t *testing.T, s *Store, tenant uint64) *CartView {
	t.Helper()
	v, err := s.GetCart(context.Background(), tenant)
	if err != nil {
		t.Fatalf("GetCart: %v", err)
	}
	return v
}

// seckillActivityRow 测试专用最小行（仅用于插桩活动，生产代码只 Count）。
type seckillActivityRow struct {
	ID               uint64    `gorm:"column:id"`
	GoodsID          uint64    `gorm:"column:goods_id"`
	SeckillPriceCent int64     `gorm:"column:seckill_price_cent"`
	TotalStock       int32     `gorm:"column:total_stock"`
	SoldStock        int32     `gorm:"column:sold_stock"`
	RedisKey         string    `gorm:"column:redis_key"`
	StartAt          time.Time `gorm:"column:start_at"`
	EndAt            time.Time `gorm:"column:end_at"`
	Status           int8      `gorm:"column:status"`
}

func (seckillActivityRow) TableName() string { return "seckill_activities" }

// insertSeckillActivity 插入一条秒杀活动行并登记 cleanup 删除，返回其 id。
func insertSeckillActivity(t *testing.T, s *Store, goodsID uint64, status int8) uint64 {
	t.Helper()
	now := time.Now()
	row := seckillActivityRow{
		GoodsID:          goodsID,
		SeckillPriceCent: 9900,
		TotalStock:       10,
		RedisKey:         fmt.Sprintf("t4-test-%d-%d", goodsID, now.UnixNano()),
		StartAt:          now,
		EndAt:            now.Add(24 * time.Hour),
		Status:           status,
	}
	if err := s.db.Create(&row).Error; err != nil {
		t.Fatalf("insert seckill_activity(goods=%d, status=%d): %v", goodsID, status, err)
	}
	id := row.ID
	t.Cleanup(func() {
		if err := s.db.Exec("DELETE FROM seckill_activities WHERE id = ?", id).Error; err != nil {
			t.Fatalf("cleanup seckill_activity id=%d: %v", id, err)
		}
	})
	return id
}
