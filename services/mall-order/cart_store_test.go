// mall-order 购物车集成测试：upsert 合并 qty、行数/数量上限、类型与在售校验。
// 写入的 cart_items 行按 tenant 在 cleanupTenant 中清除，可重复跑。
package main

import (
	"context"
	"testing"

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
	_, err = s.AddCart(ctx, testTenantC, 301, 1) // subscription 占位 on_sale=0
	assertMallCode(t, err, mallerr.CodeGoodsOffSale)
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
