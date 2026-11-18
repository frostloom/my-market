// mall-order：购物车存储层（cart_items 表）。
// upsert 合并 qty（uk_tenant_goods）、20 行上限 MALL_20004、qty 1..99 MALL_20005；
// recharge（type=2）与未上架商品不可加购（MALL_30007 / MALL_20002）。
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// 购物车上限（api-contract §4）。
const (
	cartMaxRows    = 20
	cartQtyMin     = 1
	cartQtyMax     = 99
)

// cartRow cart_items 表行（db-schema.sql）。
type cartRow struct {
	ID        uint64    `gorm:"column:id"`
	TenantID  uint64    `gorm:"column:tenant_id"`
	GoodsID   uint64    `gorm:"column:goods_id"`
	Qty       int32     `gorm:"column:qty"` // 1-99（DB 侧另有 chk_qty CHECK 兜底）
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (cartRow) TableName() string { return "cart_items" }

// CartLine 购物车行视图（契约 {"id","goods","qty"}，goods 附名称/价格）。
type CartLine struct {
	ID        uint64 // cart_items.id（PATCH/DELETE 目标）
	GoodsID   uint64
	GoodsName string
	Goods     goodsRow
	Qty       int
}

// CartView 购物车视图（含合计 = Σ 当前价×qty）。
type CartView struct {
	Items     []CartLine
	TotalCent int64
}

// GetCart 购物车视图（契约 GET /cart；空车返回空 items + total 0）。
func (s *Store) GetCart(ctx context.Context, tenantID uint64) (*CartView, error) {
	return s.loadCart(ctx, tenantID)
}

// AddCart 加购（契约 POST /cart/items）：upsert 同商品合并 qty。
// 校验：qty 1..99（MALL_20005）、商品存在（MALL_20001）、在售（MALL_20002）、
// 类型可购 type∈{1,3}（recharge 等拒 MALL_30007）、行数上限 20（MALL_20004）。
func (s *Store) AddCart(ctx context.Context, tenantID uint64, goodsID uint64, qty int) (*CartView, error) {
	if qty < cartQtyMin || qty > cartQtyMax {
		return nil, mallerr.E(mallerr.CodeGoodsQtyExceeded, false,
			"商品数量越界: %d（1..99）", qty)
	}
	g, err := s.GetGoodsRow(ctx, goodsID)
	if err != nil {
		return nil, err // MALL_20001
	}
	if g.OnSale != 1 {
		return nil, mallerr.E(mallerr.CodeGoodsOffSale, false, "商品已下架: %d", goodsID)
	}
	if g.Type != typeGeneral && g.Type != typeSubscription {
		return nil, mallerr.E(mallerr.CodeGoodsTypeNotAllowed, false,
			"商品 %d 不可加购（recharge 仅直购）", goodsID)
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		var exist cartRow
		err := tx.Where("tenant_id = ? AND goods_id = ?", tenantID, goodsID).
			First(&exist).Error
		switch {
		case err == nil:
			// 合并：同商品累加 qty，封顶 99（撞 chk_qty 前先拒绝，给出契约码）。
			if int(exist.Qty)+qty > cartQtyMax {
				return mallerr.E(mallerr.CodeGoodsQtyExceeded, false,
					"商品数量超限: 合并后 %d（≤99）", int(exist.Qty)+qty)
			}
			if err := tx.Model(&cartRow{}).Where("id = ?", exist.ID).
				Updates(map[string]any{"qty": int(exist.Qty) + qty, "updated_at": now}).Error; err != nil {
				return fmt.Errorf("mall-order: merge cart row: %w", err)
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			// 新行：20 行上限。
			var n int64
			if err := tx.Model(&cartRow{}).Where("tenant_id = ?", tenantID).Count(&n).Error; err != nil {
				return fmt.Errorf("mall-order: count cart rows: %w", err)
			}
			if n >= cartMaxRows {
				return mallerr.E(mallerr.CodeCartRowsExceeded, false,
					"购物车行数超上限（%d）", cartMaxRows)
			}
			if err := tx.Create(&cartRow{
				TenantID:  tenantID,
				GoodsID:   goodsID,
				Qty:       int32(qty),
				CreatedAt: now,
				UpdatedAt: now,
			}).Error; err != nil {
				return fmt.Errorf("mall-order: insert cart row: %w", err)
			}
		default:
			return fmt.Errorf("mall-order: load cart row: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.loadCart(ctx, tenantID)
}

// PatchCartQty 改数量（契约 PATCH /cart/items/{id}）；qty=0 等价删除。
// 目标行不存在或不属于该租户 → MALL_10005（参数错误，且避免跨租户探测）。
func (s *Store) PatchCartQty(ctx context.Context, tenantID uint64, itemID uint64, qty int) (*CartView, error) {
	if qty == 0 {
		if err := s.db.WithContext(ctx).Exec(
			"DELETE FROM cart_items WHERE id = ? AND tenant_id = ?", itemID, tenantID).Error; err != nil {
			return nil, fmt.Errorf("mall-order: delete cart row: %w", err)
		}
		return s.loadCart(ctx, tenantID)
	}
	if qty < cartQtyMin || qty > cartQtyMax {
		return nil, mallerr.E(mallerr.CodeGoodsQtyExceeded, false,
			"商品数量越界: %d（1..99，0 等价删除）", qty)
	}
	res := s.db.WithContext(ctx).Model(&cartRow{}).
		Where("id = ? AND tenant_id = ?", itemID, tenantID).
		Updates(map[string]any{"qty": qty, "updated_at": time.Now()})
	if res.Error != nil {
		return nil, fmt.Errorf("mall-order: patch cart row: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, mallerr.E(mallerr.CodeInvalidParam, false, "购物车行不存在: %d", itemID)
	}
	return s.loadCart(ctx, tenantID)
}

// RemoveCart 删购物车行（契约 DELETE /cart/items/{id}）：幂等，行不存在也 200。
func (s *Store) RemoveCart(ctx context.Context, tenantID uint64, itemID uint64) (*CartView, error) {
	if err := s.db.WithContext(ctx).Exec(
		"DELETE FROM cart_items WHERE id = ? AND tenant_id = ?", itemID, tenantID).Error; err != nil {
		return nil, fmt.Errorf("mall-order: remove cart row: %w", err)
	}
	return s.loadCart(ctx, tenantID)
}

// loadCart 组装购物车视图：行按 id 升序（稳定序），联 goods 取名称/价格。
func (s *Store) loadCart(ctx context.Context, tenantID uint64) (*CartView, error) {
	var rows []cartRow
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("id ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("mall-order: list cart rows: %w", err)
	}
	view := &CartView{Items: make([]CartLine, 0, len(rows))}
	if len(rows) == 0 {
		return view, nil
	}
	gids := make([]uint64, len(rows))
	for i := range rows {
		gids[i] = rows[i].GoodsID
	}
	var gs []goodsRow
	if err := s.db.WithContext(ctx).Where("id IN ?", gids).Find(&gs).Error; err != nil {
		return nil, fmt.Errorf("mall-order: load cart goods: %w", err)
	}
	gByID := make(map[uint64]goodsRow, len(gs))
	for _, g := range gs {
		gByID[g.ID] = g
	}
	for _, r := range rows {
		g := gByID[r.GoodsID] // 行引用的商品被删（无外键）时为零值，展示为空行即可
		view.Items = append(view.Items, CartLine{
			ID:        r.ID,
			GoodsID:   r.GoodsID,
			GoodsName: g.Name,
			Goods:     g,
			Qty:       int(r.Qty),
		})
		view.TotalCent += g.PriceCent * int64(r.Qty)
	}
	return view, nil
}
