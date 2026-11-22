// mall-order：购物车存储层（cart_items 表）。
// upsert 合并 qty（uk_tenant_goods）、20 行上限 MALL_20004、qty 1..99 MALL_20005；
// recharge（type=2）与未上架商品不可加购（MALL_30007 / MALL_20002）；
// 订阅商品（type=3）限购 qty=1（MALL_20005，R2.5 必改 1：AddCart/合并/PATCH
// 三入口同口径，下单路径另有同款守卫防直连 RPC 绕过）。
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// 购物车上限（api-contract §4）。
const (
	cartMaxRows = 20
	cartQtyMin  = 1
	cartQtyMax  = 99
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
// 类型可购 type∈{1,3} 且非秒杀活动商品（recharge/秒杀拒 MALL_30007）、
// 行数上限 20（MALL_20004）。
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
	// 订阅商品限购 qty=1（R2.5 必改 1）：履约/任务模型是行级的——一行订阅商品
	// 只开通一期，qty>1 会收双倍货款只开一期（资金缺口）。购物车、PATCH、下单
	// 三处入口同口径拦截（防绕过）；真钱接入（P5）前必须闭合。
	if g.Type == typeSubscription && qty != 1 {
		return nil, mallerr.E(mallerr.CodeGoodsQtyExceeded, false,
			"订阅商品限购 1 件: %d（qty=%d）", goodsID, qty)
	}
	// 秒杀商品不可加购（契约 §4「recharge/秒杀商品返回 MALL_30007」）：
	// 存在 status=2（上线）活动的商品一律拒绝，不限时间窗——上线即拒，
	// 避免加购后结算时点落入活动窗口造成双通道成交；草稿(1)/结束(3)不拒。
	var seckillN int64
	if err := s.db.WithContext(ctx).Table("seckill_activities").
		Where("goods_id = ? AND status = 2", goodsID).
		Count(&seckillN).Error; err != nil {
		return nil, fmt.Errorf("mall-order: check seckill activity: %w", err)
	}
	if seckillN > 0 {
		return nil, mallerr.E(mallerr.CodeGoodsTypeNotAllowed, false,
			"商品 %d 为秒杀商品，不可加入购物车", goodsID)
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		var exist cartRow
		err := tx.Where("tenant_id = ? AND goods_id = ?", tenantID, goodsID).
			First(&exist).Error
		switch {
		case err == nil:
			return mergeCartRow(tx, &exist, g.Type == typeSubscription, goodsID, qty, now)
		case errors.Is(err, gorm.ErrRecordNotFound):
			// 新行：20 行上限（快照口径；并发加不同商品的理论竞窗只影响
			// 产品上限、不涉资金不变量，由上限语义兜底）。
			var n int64
			if err := tx.Model(&cartRow{}).Where("tenant_id = ?", tenantID).Count(&n).Error; err != nil {
				return fmt.Errorf("mall-order: count cart rows: %w", err)
			}
			if n >= cartMaxRows {
				return mallerr.E(mallerr.CodeCartRowsExceeded, false,
					"购物车行数超上限（%d）", cartMaxRows)
			}
			if g.Type == typeSubscription {
				// 订阅商品限购 1：并发同商品加购必须显式拒绝（合并即违反
				// 限购不变量），故用裸 INSERT + 撞键锁下重读分流，不走
				// 会静默合并的 upsert。
				if err := tx.Create(&cartRow{
					TenantID:  tenantID,
					GoodsID:   goodsID,
					Qty:       int32(qty),
					CreatedAt: now,
					UpdatedAt: now,
				}).Error; err != nil {
					var me *mysqldrv.MySQLError
					if errors.As(err, &me) && me.Number == mysqlErrDupEntry {
						return mallerr.E(mallerr.CodeGoodsQtyExceeded, false,
							"订阅商品限购 1 件: %d（并发加购已存在）", goodsID)
					}
					return fmt.Errorf("mall-order: insert cart row: %w", err)
				}
				return nil
			}
			// 普通商品：单语句 upsert——并发首加同商品时由 MySQL 在一条
			// 语句内原子合并（qty 累加、LEAST 封顶 99 保 chk_qty 不变式），
			// 不用「INSERT + 1062 重读」两步（多路并发下会互相等锁触发 1213），
			// 也不会把 1062 原文透给客户端（契约 §0.2 人话口径）。
			// VALUES() 在 MySQL 8.0.20+ 有弃用告警但全 8.0.x 可用；AS new
			// 别名语法在 <8.0.20 硬失败——栈为 mysql:8.0 浮动 tag，取兼容优先。
			if err := tx.Exec(
				"INSERT INTO cart_items (tenant_id, goods_id, qty, created_at, updated_at) "+
					"VALUES (?, ?, ?, ?, ?) "+
					"ON DUPLICATE KEY UPDATE qty = LEAST(qty + VALUES(qty), ?), updated_at = VALUES(updated_at)",
				tenantID, goodsID, qty, now, now, cartQtyMax).Error; err != nil {
				return fmt.Errorf("mall-order: upsert cart row: %w", err)
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

// mergeCartRow 加购合并：条件原子累加 qty = qty + ?（带 ≤99 守卫）。
// 读改写式 Updates(快照值+qty) 在并发合并下会丢一次累加，故以当前行值做
// 条件更新——RowsAffected=0 即合并越限（或行被并发删除，客户端重试即走
// 新行路径，同码可接受）。订阅商品行 qty 恒 1（三入口同口径保证），既有行
// 存在时任何累加都越限购，确定性拒绝。
func mergeCartRow(tx *gorm.DB, exist *cartRow, isSubscription bool, goodsID uint64, qty int, now time.Time) error {
	if isSubscription {
		return mallerr.E(mallerr.CodeGoodsQtyExceeded, false,
			"订阅商品限购 1 件: %d（合并后 %d）", goodsID, int(exist.Qty)+qty)
	}
	if int(exist.Qty)+qty > cartQtyMax {
		return mallerr.E(mallerr.CodeGoodsQtyExceeded, false,
			"商品数量超限: 合并后 %d（≤99）", int(exist.Qty)+qty)
	}
	res := tx.Exec(
		"UPDATE cart_items SET qty = qty + ?, updated_at = ? "+
			"WHERE id = ? AND qty + ? <= ?",
		qty, now, exist.ID, qty, cartQtyMax)
	if res.Error != nil {
		return fmt.Errorf("mall-order: merge cart row: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return mallerr.E(mallerr.CodeGoodsQtyExceeded, false,
			"商品数量超限: 合并后超过上限（≤%d）", cartQtyMax)
	}
	return nil
}

// PatchCartQty 改数量（契约 PATCH /cart/items/{id}）；qty=0 等价删除。
// 目标行不存在或不属于该租户 → MALL_10005（参数错误，且避免跨租户探测）；
// 订阅商品行 qty 限购 1（R2.5 必改 1，与 AddCart 同口径 MALL_20005）。
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
	// 订阅商品限购 qty=1（与 AddCart 同口径，第三条绕过口）：目标行商品为
	// 订阅类型时 PATCH 到 qty≠1 拒绝 MALL_20005（qty=1 本身是幂等放行——
	// 统一口径"订阅行 qty 必须 =1"；删除仍走 qty=0）。
	var gType int8
	if err := s.db.WithContext(ctx).Raw(
		"SELECT g.type FROM cart_items c JOIN goods g ON g.id = c.goods_id "+
			"WHERE c.id = ? AND c.tenant_id = ?", itemID, tenantID).
		Scan(&gType).Error; err != nil {
		return nil, fmt.Errorf("mall-order: load cart goods type %d: %w", itemID, err)
	}
	if gType == typeSubscription && qty != 1 {
		return nil, mallerr.E(mallerr.CodeGoodsQtyExceeded, false,
			"订阅商品限购 1 件（不可改数量，删除请用 qty=0）: 行 %d", itemID)
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
