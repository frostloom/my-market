// mall-goods：商品域服务（品类 / 商品游标分页 / 详情）。
// store.go 是 market 库（categories/goods 表）的 GORM 访问层；
// 错误一律携带契约码（pkg/mallerr），由 server.go 映射 gRPC status。
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// goods.type 编码（db-schema goods.type 注释）。
const typeRecharge = 2

// tokenEstimateTokensPerCent 轻量档模型单价的换算常数（营销展示口径）：
// 1 分钱 ≈ 1000 token（¥10 面值 ≈ 约 100 万 token）。仅用于 App 端
// "约可得 N token" 文案展示，与 billing 实际扣费无关——非入账依据。
const tokenEstimateTokensPerCent = 1000

// tokenEstimate 把充值金额（分）按轻量档单价换算成展示用 token 数。
func tokenEstimate(cent int64) int64 { return cent * tokenEstimateTokensPerCent }

// categoryRow categories 表行（db-schema.sql）。
type categoryRow struct {
	ID        uint64    `gorm:"column:id"`
	Name      string    `gorm:"column:name"`
	Sort      int32     `gorm:"column:sort"`
	Status    int8      `gorm:"column:status"` // 1 上架 0 下架
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (categoryRow) TableName() string { return "categories" }

// goodsRow goods 表行（db-schema.sql）。
type goodsRow struct {
	ID            uint64    `gorm:"column:id"`
	Type          int8      `gorm:"column:type"`
	CategoryID    uint64    `gorm:"column:category_id"`
	Name          string    `gorm:"column:name"`
	Subtitle      string    `gorm:"column:subtitle"`
	PriceCent     int64     `gorm:"column:price_cent"`
	OrigPriceCent int64     `gorm:"column:orig_price_cent"`
	FaceValueCent int64     `gorm:"column:face_value_cent"`
	BonusCent     int64     `gorm:"column:bonus_cent"`
	ImageURL      string    `gorm:"column:image_url"`
	Detail        *string   `gorm:"column:detail"` // MEDIUMTEXT NULL
	Stock         int32     `gorm:"column:stock"`
	Limited       int8      `gorm:"column:limited"`
	RefPlanID     uint64    `gorm:"column:ref_plan_id"`
	OnSale        int8      `gorm:"column:on_sale"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

func (goodsRow) TableName() string { return "goods" }

// Store market 库只读访问（本服务不写商品/品类）。
type Store struct {
	db *gorm.DB
}

// OpenStore 按 DSN 打开 MySQL 连接并 ping 通。
func OpenStore(dsn string) (*Store, error) {
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("mall-goods: open mysql: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("mall-goods: unwrap sql.DB: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("mall-goods: ping mysql: %w", err)
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

// ListCategories 上架品类，按 sort 升序（契约 GET /categories，六个）。
func (s *Store) ListCategories(ctx context.Context) ([]categoryRow, error) {
	var rows []categoryRow
	err := s.db.WithContext(ctx).
		Where("status = 1").
		Order("sort ASC, id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("mall-goods: list categories: %w", err)
	}
	return rows, nil
}

// ListGoods 品类内键集分页取**在售**商品（契约 GET /goods）。
// category_id=0 表示不限品类；品类不存在 → MALL_20006；非法游标 → MALL_10005。
// SQL 语义：WHERE on_sale=1 [AND category_id=?] [AND (created_at,id) < (?,?)]
// ORDER BY created_at DESC, id DESC LIMIT size+1——多取 1 判 has_more，
// 多出的那行只作判定、不返回。返回的 next_cursor 指向本页最后一行。
func (s *Store) ListGoods(ctx context.Context, categoryID uint64, cursor string, pageSize int32) ([]goodsRow, string, bool, error) {
	size := clampPageSize(pageSize)

	if categoryID > 0 {
		var n int64
		err := s.db.WithContext(ctx).Model(&categoryRow{}).
			Where("id = ?", categoryID).
			Count(&n).Error
		if err != nil {
			return nil, "", false, fmt.Errorf("mall-goods: check category %d: %w", categoryID, err)
		}
		if n == 0 {
			return nil, "", false, mallerr.E(mallerr.CodeCategoryNotFound, false, "品类不存在: %d", categoryID)
		}
	}

	q := s.db.WithContext(ctx).Model(&goodsRow{}).Where("on_sale = 1")
	if categoryID > 0 {
		q = q.Where("category_id = ?", categoryID)
	}
	if cursor != "" {
		at, id, err := mallpage.DecodeCursor(cursor)
		if err != nil {
			return nil, "", false, mallerr.E(mallerr.CodeInvalidParam, false, "游标非法: %v", err)
		}
		// 行值比较推进键集：created_at 同毫秒的多行靠 id 区分（不丢行不重行）。
		q = q.Where("(created_at, id) < (?, ?)", at, id)
	}

	var rows []goodsRow
	if err := q.Order("created_at DESC, id DESC").Limit(size + 1).Find(&rows).Error; err != nil {
		return nil, "", false, fmt.Errorf("mall-goods: list goods: %w", err)
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
	return rows, next, hasMore, nil
}

// GetGoods 商品详情（契约 GET /goods/{id}）。不存在 → MALL_20001；
// 未上架商品**原样返回**（on_sale=false），是否 409 由 bff 决定。
func (s *Store) GetGoods(ctx context.Context, id uint64) (*goodsRow, error) {
	var row goodsRow
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, mallerr.E(mallerr.CodeGoodsNotFound, false, "商品不存在: %d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("mall-goods: get goods %d: %w", id, err)
	}
	return &row, nil
}

// clampPageSize 钳 1..50，缺省 20（契约 §0.3，BFF 侧也做同样裁剪）。
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
