// mall-order：下单域存储层（订单 + 幂等 + 库存直扣事务）。
// store.go 是 market 库（mall_orders/mall_order_items/goods 表）的 GORM 访问层；
// 错误一律携带契约码（pkg/mallerr），由 server.go 映射 gRPC status。
//
// CreateCartOrder 事务语义（ADR-009 同库直扣）：
//
//	BEGIN → 校验商品（on_sale=1 且 type∈{1,3}）→ 逐行条件扣库存
//	（UPDATE goods SET stock=stock-? WHERE id=? AND stock>=?，0 行 → MALL_20003
//	整体回滚）→ INSERT mall_orders(kind=2) + mall_order_items（行号 1..n、
//	价格/面值/赠送快照）→ COMMIT；amount = Σ(快照×qty)。
//
// 幂等实现（不新增判定列）：撞 uk_tenant_idem 时读回原单，行集
// （goods_id,qty 排序归并后）相同 → 返回原单（200 语义）；不同 → MALL_30003。
//
// CreateRechargeOrder（R2 recharge 直购）同构：kind=1 单行 type=credit，
// 校验 type=2 + on_sale、limited=1 才条件扣库存，payload 指纹 = goods_id。
package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/frostloom/my-market/pkg/mallerr"
	"github.com/frostloom/my-market/pkg/mallpage"
)

// 分页钳制口径（api-contract §0.3：page_size 默认 20、上限 50）。
const (
	defaultPageSize = 20
	maxPageSize     = 50
)

// goods.type / mall_order_items.type 编码（db-schema 注释）。
// 两表编码数值恰好一致但语义分属两表：goods.type 2=recharge 商品；
// mall_order_items.type 2=credit 履约行（recharge 行的履约=入账）。
const (
	typeGeneral      = 1 // goods: general  → 行履约类型 1 ship
	typeRecharge     = 2 // goods: recharge → 仅直购（CreateRechargeOrder），不可进 cart 单（MALL_30007）
	typeSubscription = 3 // goods: subscription → 行履约类型 3 subscribe

	typeCredit = 2 // mall_order_items: credit（recharge 直购行，worker 经 billing.Credit 入账）
)

// mall_orders.status / closed_reason 编码（db-schema 注释）。
const (
	orderStatusCreated   = 1
	orderStatusPaid      = 2
	orderStatusFulfilled = 3
	orderStatusRefunded  = 4
	orderStatusClosed    = 5
)

// MySQL 1062：重复键（uk_order_no / uk_tenant_idem 分流依据）。
const mysqlErrDupEntry = 1062

// orderRow mall_orders 表行（db-schema.sql）。
type orderRow struct {
	ID           uint64     `gorm:"column:id"`
	OrderNo      string     `gorm:"column:order_no"`
	TenantID     uint64     `gorm:"column:tenant_id"`
	Status       int8       `gorm:"column:status"` // 1 created 2 paid 3 fulfilled 4 refunded 5 closed
	AmountCent   int64      `gorm:"column:amount_cent"`
	IdemKey      string     `gorm:"column:idem_key"`
	Kind         int8       `gorm:"column:kind"` // 1 recharge直购 2 cart混合 3 seckill
	ClosedReason int8       `gorm:"column:closed_reason"`
	PaidAt       *time.Time `gorm:"column:paid_at"`
	FulfilledAt  *time.Time `gorm:"column:fulfilled_at"`
	ClosedAt     *time.Time `gorm:"column:closed_at"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (orderRow) TableName() string { return "mall_orders" }

// goodsRow goods 表行（db-schema.sql；本服务只在事务内条件更新 stock）。
// 字段全集对齐 mall-goods 侧同名行（cart 视图 goods 保真——api-contract §4
// 引用 §3 全形状），本服务不消费 detail 富文本故不载（*string NULL 列）。
type goodsRow struct {
	ID            uint64 `gorm:"column:id"`
	Type          int8   `gorm:"column:type"`
	CategoryID    uint64 `gorm:"column:category_id"`
	Name          string `gorm:"column:name"`
	Subtitle      string `gorm:"column:subtitle"`
	PriceCent     int64  `gorm:"column:price_cent"`
	OrigPriceCent int64  `gorm:"column:orig_price_cent"`
	FaceValueCent int64  `gorm:"column:face_value_cent"`
	BonusCent     int64  `gorm:"column:bonus_cent"`
	ImageURL      string `gorm:"column:image_url"`
	Stock         int    `gorm:"column:stock"`
	Limited       int8   `gorm:"column:limited"`
	RefPlanID     uint64 `gorm:"column:ref_plan_id"`
	OnSale        int8   `gorm:"column:on_sale"`
}

func (goodsRow) TableName() string { return "goods" }

// orderItemRow mall_order_items 表行（写入用）。
type orderItemRow struct {
	ID                uint64    `gorm:"column:id"`
	OrderID           uint64    `gorm:"column:order_id"`
	OrderNo           string    `gorm:"column:order_no"`
	LineNo            int32     `gorm:"column:line_no"`
	Type              int8      `gorm:"column:type"` // 1 ship(general) 2 credit(recharge) 3 subscribe
	GoodsID           uint64    `gorm:"column:goods_id"`
	Qty               int32     `gorm:"column:qty"`
	PriceSnapshotCent int64     `gorm:"column:price_snapshot_cent"`
	FaceValueCent     int64     `gorm:"column:face_value_cent"`
	BonusCent         int64     `gorm:"column:bonus_cent"`
	CreatedAt         time.Time `gorm:"column:created_at"`
}

func (orderItemRow) TableName() string { return "mall_order_items" }

// orderItemFull 订单行读出（联 goods 补名称与商品类型——行表只存快照金额）。
type orderItemFull struct {
	ID                uint64    `gorm:"column:id"`
	OrderID           uint64    `gorm:"column:order_id"`
	OrderNo           string    `gorm:"column:order_no"`
	LineNo            int32     `gorm:"column:line_no"`
	Type              int8      `gorm:"column:type"`
	GoodsID           uint64    `gorm:"column:goods_id"`
	Qty               int32     `gorm:"column:qty"`
	PriceSnapshotCent int64     `gorm:"column:price_snapshot_cent"`
	FaceValueCent     int64     `gorm:"column:face_value_cent"`
	BonusCent         int64     `gorm:"column:bonus_cent"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	GoodsName         string    `gorm:"column:goods_name"`
	GoodsType         int8      `gorm:"column:goods_type"`
}

// Line 下单行（goods_id + 购买数量）。
type Line struct {
	GoodsID uint64
	Qty     int
}

// OrderItem 订单行聚合（快照 + 商品名）。
type OrderItem struct {
	LineNo        int32 // 行号从 1；ref_id = order_no:line_no
	Type          int8  // 1 ship 2 credit 3 subscribe
	GoodsID       uint64
	GoodsName     string
	GoodsType     int8 // 1 general 2 recharge 3 subscription
	Qty           int
	PriceCent     int64 // 下单时行价格快照
	FaceValueCent int64
	BonusCent     int64
}

// Order 订单聚合。
type Order struct {
	ID           uint64
	OrderNo      string
	TenantID     uint64
	Status       string // created|paid|fulfilled|refunded|closed
	AmountCent   int64
	IdemKey      string
	Kind         int8
	ClosedReason string     // ""|timeout|user
	PaidAt       *time.Time // 支付成功时间（nil=未支付）
	FulfilledAt  *time.Time // 全行履约完成时间（nil=未履约）
	CreatedAt    time.Time
	Items        []OrderItem
}

// PageReq 键集游标分页请求（api-contract §0.3，与 mall-goods 同口径）。
type PageReq struct {
	Cursor   string
	PageSize int32
}

// OrderPage 订单列表页。
type OrderPage struct {
	Orders     []*Order
	NextCursor string
	HasMore    bool
}

// Store market 库访问（订单域：写 orders/items、条件扣 stock、购物车）。
type Store struct {
	db *gorm.DB
	// shipLine 单行履约执行接缝（worker 注入点）：nil = 进程内 mock（恒成功）。
	// 真实渠道接入后赋实现；测试注入桩/故障（见 worker_test.go）。
	shipLine func(ctx context.Context, t fulfillmentTaskRow) error
}

// OpenStore 按 DSN 打开 MySQL 连接并 ping 通。
func OpenStore(dsn string) (*Store, error) {
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("mall-order: open mysql: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("mall-order: unwrap sql.DB: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("mall-order: ping mysql: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 释放底层连接池。
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// GetGoodsRow 原样读商品行（无 on_sale 语义；详情展示与测试锚点用）。
func (s *Store) GetGoodsRow(ctx context.Context, id uint64) (*goodsRow, error) {
	var row goodsRow
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, mallerr.E(mallerr.CodeGoodsNotFound, false, "商品不存在: %d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("mall-order: get goods %d: %w", id, err)
	}
	return &row, nil
}

// CreateCartOrder 购物车结算建单（kind=2，混合 general+subscription）。
// 事务内校验 + 直扣库存 + 落单；幂等键作用域 (tenant_id, key)。
func (s *Store) CreateCartOrder(ctx context.Context, tenantID uint64, idemKey string, lines []Line) (*Order, error) {
	if idemKey == "" || len(idemKey) > 64 {
		return nil, mallerr.E(mallerr.CodeInvalidParam, false, "幂等键必填且不超过 64 字符")
	}
	norm, err := normalizeLines(lines)
	if err != nil {
		return nil, err
	}

	// uk_order_no 撞键（同秒随机数碰撞，概率极低）→ 换单号重试，≤3 次后 MALL_50001。
	const maxNoRetries = 3
	for attempt := 0; attempt < maxNoRetries; attempt++ {
		ord, err := s.createCartOrderOnce(ctx, tenantID, idemKey, norm)
		if err == nil {
			return ord, nil
		}
		var me *mysqldrv.MySQLError
		if !errors.As(err, &me) || me.Number != mysqlErrDupEntry {
			return nil, err
		}
		// 键名分流耦合 MySQL 1062 报文措辞（"for key '<table>.<索引名>'"）：
		// 索引改名/报文格式变更会静默落入 default 分支（MALL_50001），需同步此处。
		switch {
		case strings.Contains(me.Message, "uk_tenant_idem"):
			// 幂等：读回原单比较 payload（同 → 返回原单；异 → MALL_30003）。
			return s.resolveIdempotent(ctx, tenantID, idemKey, norm)
		case strings.Contains(me.Message, "uk_order_no"):
			continue
		default:
			return nil, fmt.Errorf("mall-order: unexpected duplicate key: %v", me)
		}
	}
	return nil, mallerr.E(mallerr.CodeInternal, true, "订单号生成重试耗尽（uk_order_no 连续冲突）")
}

// createCartOrderOnce 单次建单事务（见包注释的事务语义）。
func (s *Store) createCartOrderOnce(ctx context.Context, tenantID uint64, idemKey string, norm []Line) (*Order, error) {
	var out *Order
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) 校验商品并取价格快照（普通 SELECT 不加锁：竞态由第 2 步条件更新兜底）。
		ids := make([]uint64, len(norm))
		for i, l := range norm {
			ids[i] = l.GoodsID
		}
		var gs []goodsRow
		if err := tx.Where("id IN ?", ids).Find(&gs).Error; err != nil {
			return fmt.Errorf("mall-order: load goods: %w", err)
		}
		byID := make(map[uint64]goodsRow, len(gs))
		for _, g := range gs {
			byID[g.ID] = g
		}
		for _, l := range norm {
			g, ok := byID[l.GoodsID]
			switch {
			case !ok:
				return mallerr.E(mallerr.CodeGoodsNotFound, false, "商品不存在: %d", l.GoodsID)
			case g.OnSale != 1:
				return mallerr.E(mallerr.CodeGoodsOffSale, false, "商品已下架: %d", l.GoodsID)
			case g.Type != typeGeneral && g.Type != typeSubscription:
				return mallerr.E(mallerr.CodeGoodsTypeNotAllowed, false,
					"商品 %d 类型不可进购物车单（recharge 仅直购）", l.GoodsID)
			}
		}

		// 2) 逐行条件扣库存：影响行数 0 = 库存不足或并发被抢 → MALL_20003 整体回滚。
		for _, l := range norm {
			res := tx.Exec("UPDATE goods SET stock = stock - ? WHERE id = ? AND stock >= ?",
				l.Qty, l.GoodsID, l.Qty)
			if res.Error != nil {
				return fmt.Errorf("mall-order: deduct stock goods %d: %w", l.GoodsID, res.Error)
			}
			if res.RowsAffected == 0 {
				return mallerr.E(mallerr.CodeOutOfStock, false, "库存不足: 商品 %d", l.GoodsID)
			}
		}

		// 3) 金额 = Σ(价格快照×qty)。
		var amount int64
		for _, l := range norm {
			amount += byID[l.GoodsID].PriceCent * int64(l.Qty)
		}

		// 4) 订单头（uk_order_no / uk_tenant_idem 撞键由上层按键名分流）。
		now := time.Now()
		row := orderRow{
			OrderNo:    genOrderNo(),
			TenantID:   tenantID,
			Status:     orderStatusCreated,
			AmountCent: amount,
			IdemKey:    idemKey,
			Kind:       2,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err // 原样上抛，保留 *mysql.MySQLError 供分流
		}

		// 5) 行快照：行号 1..n，履约类型按商品类型映射。
		items := make([]orderItemRow, 0, len(norm))
		for i, l := range norm {
			g := byID[l.GoodsID]
			items = append(items, orderItemRow{
				OrderID:           row.ID,
				OrderNo:           row.OrderNo,
				LineNo:            int32(i + 1),
				Type:              fulfillmentType(g.Type),
				GoodsID:           l.GoodsID,
				Qty:               int32(l.Qty),
				PriceSnapshotCent: g.PriceCent,
				FaceValueCent:     g.FaceValueCent,
				BonusCent:         g.BonusCent,
				CreatedAt:         now,
			})
		}
		if err := tx.Create(&items).Error; err != nil {
			return fmt.Errorf("mall-order: insert order items: %w", err)
		}

		out = assembleOrder(&row, itemRowsToFull(items, byID))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateRechargeOrder recharge 直购建单（kind=1，单行 type=credit，qty 恒 1）。
// 事务语义（与 CreateCartOrder 同构，ADR-009）：
//
//	BEGIN → 校验商品（type=2 recharge 才可直购，其余 MALL_30007；on_sale=1；
//	limited=1 时条件扣库存 stock-1，0 行 → MALL_20003 整体回滚；limited=0
//	不碰库存——recharge 三档种子均为无限量）→ INSERT mall_orders(kind=1,
//	amount=price_cent) + 单行 mall_order_items(type=2, line_no=1, qty=1,
//	价格/面值/赠送快照) → COMMIT。
//
// 幂等键语义与 CreateCartOrder 一致：作用域 (tenant_id, key)，payload 指纹 =
// goods_id（复用 resolveIdempotent 行集比较，读回行集 {goods_id,1} 相同 →
// 返回原单；不同 → MALL_30003）。
func (s *Store) CreateRechargeOrder(ctx context.Context, tenantID uint64, goodsID uint64, idemKey string) (*Order, error) {
	if idemKey == "" || len(idemKey) > 64 {
		return nil, mallerr.E(mallerr.CodeInvalidParam, false, "幂等键必填且不超过 64 字符")
	}
	if goodsID == 0 {
		return nil, mallerr.E(mallerr.CodeInvalidParam, false, "recharge 下单须带 goods_id")
	}

	// uk_order_no 撞键换单号重试（与 CreateCartOrder 同口径，≤3 次）。
	const maxNoRetries = 3
	for attempt := 0; attempt < maxNoRetries; attempt++ {
		ord, err := s.createRechargeOrderOnce(ctx, tenantID, goodsID, idemKey)
		if err == nil {
			return ord, nil
		}
		var me *mysqldrv.MySQLError
		if !errors.As(err, &me) || me.Number != mysqlErrDupEntry {
			return nil, err
		}
		switch {
		case strings.Contains(me.Message, "uk_tenant_idem"):
			// 幂等读回比较（payload = {goods_id, 1}）。
			return s.resolveIdempotent(ctx, tenantID, idemKey, []Line{{GoodsID: goodsID, Qty: 1}})
		case strings.Contains(me.Message, "uk_order_no"):
			continue
		default:
			return nil, fmt.Errorf("mall-order: unexpected duplicate key: %v", me)
		}
	}
	return nil, mallerr.E(mallerr.CodeInternal, true, "订单号生成重试耗尽（uk_order_no 连续冲突）")
}

// createRechargeOrderOnce 单次 recharge 建单事务（见 CreateRechargeOrder 注释）。
func (s *Store) createRechargeOrderOnce(ctx context.Context, tenantID uint64, goodsID uint64, idemKey string) (*Order, error) {
	var out *Order
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) 校验商品（普通 SELECT 不加锁：limited 竞态由第 2 步条件更新兜底）。
		var g goodsRow
		err := tx.Where("id = ?", goodsID).First(&g).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return mallerr.E(mallerr.CodeGoodsNotFound, false, "商品不存在: %d", goodsID)
		}
		if err != nil {
			return fmt.Errorf("mall-order: load goods %d: %w", goodsID, err)
		}
		switch {
		case g.Type != typeRecharge:
			return mallerr.E(mallerr.CodeGoodsTypeNotAllowed, false,
				"商品 %d 类型不可直购（仅 recharge 支持直购）", goodsID)
		case g.OnSale != 1:
			return mallerr.E(mallerr.CodeGoodsOffSale, false, "商品已下架: %d", goodsID)
		}

		// 2) limited=1 条件扣库存（qty 恒 1）：影响行数 0 = 售罄或并发被抢 →
		//    MALL_20003 整体回滚；limited=0 不执行（无限量档不碰库存）。
		if g.Limited == 1 {
			res := tx.Exec("UPDATE goods SET stock = stock - 1 WHERE id = ? AND stock >= 1", goodsID)
			if res.Error != nil {
				return fmt.Errorf("mall-order: deduct stock goods %d: %w", goodsID, res.Error)
			}
			if res.RowsAffected == 0 {
				return mallerr.E(mallerr.CodeOutOfStock, false, "库存不足: 商品 %d", goodsID)
			}
		}

		// 3) 订单头（kind=1，amount = 价格快照）+ 单行（type=credit，行号 1）。
		now := time.Now()
		row := orderRow{
			OrderNo:    genOrderNo(),
			TenantID:   tenantID,
			Status:     orderStatusCreated,
			AmountCent: g.PriceCent,
			IdemKey:    idemKey,
			Kind:       1,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err // 原样上抛，保留 *mysql.MySQLError 供分流
		}
		item := orderItemRow{
			OrderID:           row.ID,
			OrderNo:           row.OrderNo,
			LineNo:            1,
			Type:              typeCredit,
			GoodsID:           goodsID,
			Qty:               1,
			PriceSnapshotCent: g.PriceCent,
			FaceValueCent:     g.FaceValueCent,
			BonusCent:         g.BonusCent,
			CreatedAt:         now,
		}
		if err := tx.Create(&item).Error; err != nil {
			return fmt.Errorf("mall-order: insert order items: %w", err)
		}

		out = assembleOrder(&row, itemRowsToFull([]orderItemRow{item}, map[uint64]goodsRow{g.ID: g}))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resolveIdempotent 撞 uk_tenant_idem 后的读回判定：
// 读原单行集（goods_id,qty 排序归并）与本次 payload 相同 → 返回原单；
// 不同 → MALL_30003。不新增判定列（以行集比较为 payload 指纹）。
func (s *Store) resolveIdempotent(ctx context.Context, tenantID uint64, idemKey string, norm []Line) (*Order, error) {
	var row orderRow
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND idem_key = ?", tenantID, idemKey).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 插入撞键但原单读不到（极端：原事务回滚后键已释放）→ 按冲突处理，
		// 客户端换 key 重试即可；不静默重建（避免掩盖并发竞态）。
		return nil, mallerr.E(mallerr.CodeIdempotencyConflict, false,
			"幂等键冲突且原单不可见，请更换幂等键重试: %s", idemKey)
	}
	if err != nil {
		return nil, fmt.Errorf("mall-order: load order by idem: %w", err)
	}
	items, err := s.loadOrderItems(ctx, []uint64{row.ID})
	if err != nil {
		return nil, err
	}
	orig := make([]Line, 0, len(items))
	for _, it := range items {
		orig = append(orig, Line{GoodsID: it.GoodsID, Qty: int(it.Qty)})
	}
	sortLines(orig)
	if sameLines(norm, orig) {
		return assembleOrder(&row, items), nil
	}
	return nil, mallerr.E(mallerr.CodeIdempotencyConflict, false,
		"幂等键已绑定不同内容的订单: %s", idemKey)
}

// GetOrder 单笔订单（契约 GET /orders/{no}）。tenant 不匹配视为不存在（MALL_30001）。
func (s *Store) GetOrder(ctx context.Context, tenantID uint64, orderNo string) (*Order, error) {
	var row orderRow
	err := s.db.WithContext(ctx).
		Where("order_no = ? AND tenant_id = ?", orderNo, tenantID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, mallerr.E(mallerr.CodeOrderNotFound, false, "订单不存在: %s", orderNo)
	}
	if err != nil {
		return nil, fmt.Errorf("mall-order: get order %s: %w", orderNo, err)
	}
	items, err := s.loadOrderItems(ctx, []uint64{row.ID})
	if err != nil {
		return nil, err
	}
	return assembleOrder(&row, items), nil
}

// ListOrders 租户内订单键集分页（(created_at,id) 降序，复用 mallpage 游标）。
func (s *Store) ListOrders(ctx context.Context, tenantID uint64, page PageReq) (*OrderPage, error) {
	size := clampPageSize(page.PageSize)

	q := s.db.WithContext(ctx).Model(&orderRow{}).Where("tenant_id = ?", tenantID)
	if page.Cursor != "" {
		at, id, err := mallpage.DecodeCursor(page.Cursor)
		if err != nil {
			return nil, mallerr.E(mallerr.CodeInvalidParam, false, "游标非法: %v", err)
		}
		q = q.Where("(created_at, id) < (?, ?)", at, id)
	}

	var rows []orderRow
	if err := q.Order("created_at DESC, id DESC").Limit(size + 1).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("mall-order: list orders: %w", err)
	}
	hasMore := len(rows) > size
	if hasMore {
		rows = rows[:size]
	}
	next := ""
	if hasMore {
		last := rows[len(rows)-1]
		next = mallpage.EncodeCursor(last.CreatedAt, last.ID)
	}

	ids := make([]uint64, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	items, err := s.loadOrderItems(ctx, ids)
	if err != nil {
		return nil, err
	}
	orders := make([]*Order, 0, len(rows))
	for i := range rows {
		orders = append(orders, assembleOrder(&rows[i], items))
	}
	return &OrderPage{Orders: orders, NextCursor: next, HasMore: hasMore}, nil
}

// ---------------------------------------------------------------------------
// 内部辅助

// normalizeLines 归并同商品行、校验 qty ≥1（订单域口径：越界属参数错误
// MALL_10005；购物车侧才用 MALL_20005），按 goods_id 排序（幂等比较与
// 扣库存顺序稳定，避免死锁）。
func normalizeLines(lines []Line) ([]Line, error) {
	if len(lines) == 0 {
		return nil, mallerr.E(mallerr.CodeInvalidParam, false, "下单行不能为空")
	}
	merged := make(map[uint64]int, len(lines))
	for _, l := range lines {
		if l.Qty < 1 || l.Qty > 99 {
			return nil, mallerr.E(mallerr.CodeInvalidParam, false,
				"下单数量越界: goods=%d qty=%d（1..99）", l.GoodsID, l.Qty)
		}
		if l.GoodsID == 0 {
			return nil, mallerr.E(mallerr.CodeInvalidParam, false, "下单行缺 goods_id")
		}
		merged[l.GoodsID] += l.Qty
	}
	out := make([]Line, 0, len(merged))
	for id, qty := range merged {
		out = append(out, Line{GoodsID: id, Qty: qty})
	}
	sortLines(out)
	return out, nil
}

func sortLines(ls []Line) {
	sort.Slice(ls, func(i, j int) bool { return ls[i].GoodsID < ls[j].GoodsID })
}

func sameLines(a, b []Line) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].GoodsID != b[i].GoodsID || a[i].Qty != b[i].Qty {
			return false
		}
	}
	return true
}

// loadOrderItems 订单行读出（联 goods 补名称），按 (order_id,line_no) 排序。
func (s *Store) loadOrderItems(ctx context.Context, orderIDs []uint64) ([]orderItemFull, error) {
	if len(orderIDs) == 0 {
		return nil, nil
	}
	var rows []orderItemFull
	err := s.db.WithContext(ctx).
		Table("mall_order_items").
		Select("mall_order_items.*, goods.name AS goods_name, goods.type AS goods_type").
		Joins("LEFT JOIN goods ON goods.id = mall_order_items.goods_id").
		Where("mall_order_items.order_id IN ?", orderIDs).
		Order("order_id ASC, line_no ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("mall-order: load order items: %w", err)
	}
	return rows, nil
}

// itemRowsToFull 写入行 → 读出行形态（建单后同事务组装返回值用）。
func itemRowsToFull(rows []orderItemRow, byID map[uint64]goodsRow) []orderItemFull {
	out := make([]orderItemFull, 0, len(rows))
	for _, r := range rows {
		g := byID[r.GoodsID]
		out = append(out, orderItemFull{
			ID:                r.ID,
			OrderID:           r.OrderID,
			OrderNo:           r.OrderNo,
			LineNo:            r.LineNo,
			Type:              r.Type,
			GoodsID:           r.GoodsID,
			Qty:               r.Qty,
			PriceSnapshotCent: r.PriceSnapshotCent,
			FaceValueCent:     r.FaceValueCent,
			BonusCent:         r.BonusCent,
			CreatedAt:         r.CreatedAt,
			GoodsName:         g.Name,
			GoodsType:         g.Type,
		})
	}
	return out
}

// assembleOrder 订单行 + 行集 → 聚合（契约字符串编码）。
func assembleOrder(row *orderRow, items []orderItemFull) *Order {
	ord := &Order{
		ID:           row.ID,
		OrderNo:      row.OrderNo,
		TenantID:     row.TenantID,
		Status:       statusName(row.Status),
		AmountCent:   row.AmountCent,
		IdemKey:      row.IdemKey,
		Kind:         row.Kind,
		ClosedReason: closedReasonName(row.ClosedReason),
		PaidAt:       row.PaidAt,
		FulfilledAt:  row.FulfilledAt,
		CreatedAt:    row.CreatedAt,
		Items:        make([]OrderItem, 0, len(items)),
	}
	for _, it := range items {
		if it.OrderID != row.ID {
			continue
		}
		ord.Items = append(ord.Items, OrderItem{
			LineNo:        it.LineNo,
			Type:          it.Type,
			GoodsID:       it.GoodsID,
			GoodsName:     it.GoodsName,
			GoodsType:     it.GoodsType,
			Qty:           int(it.Qty),
			PriceCent:     it.PriceSnapshotCent,
			FaceValueCent: it.FaceValueCent,
			BonusCent:     it.BonusCent,
		})
	}
	return ord
}

func statusName(v int8) string {
	switch v {
	case orderStatusCreated:
		return "created"
	case orderStatusPaid:
		return "paid"
	case orderStatusFulfilled:
		return "fulfilled"
	case orderStatusRefunded:
		return "refunded"
	case orderStatusClosed:
		return "closed"
	default:
		return "unknown"
	}
}

func closedReasonName(v int8) string {
	switch v {
	case 1:
		return "timeout"
	case 2:
		return "user"
	default:
		return ""
	}
}

// fulfillmentType goods.type → mall_order_items.type（1 ship / 3 subscribe；
// recharge 不进 cart 单，2 credit 不在此产生）。
func fulfillmentType(goodsType int8) int8 {
	if goodsType == typeSubscription {
		return 3
	}
	return 1
}

// genOrderNo 订单号：M + yyyyMMddHHmmss + 6 位随机数字（CHAR(21)）。
// 撞 uk_order_no 由上层换单号重试（≤3 次）。
func genOrderNo() string {
	n, err := crand.Int(crand.Reader, big.NewInt(1_000_000))
	if err != nil {
		// crypto/rand 失败极罕见（系统熵源故障）；退化为固定值让唯一键拦下，
		// 上层重试后耗尽即 MALL_50001，不产生错误订单。
		n = big.NewInt(0)
	}
	return fmt.Sprintf("M%s%06d", time.Now().Format("20060102150405"), n.Int64())
}

// clampPageSize 钳 1..50，缺省 20（契约 §0.3）。
func clampPageSize(n int32) int {
	switch {
	case n <= 0:
		return defaultPageSize
	case n > maxPageSize:
		return maxPageSize
	default:
		return int(n)
	}
}
