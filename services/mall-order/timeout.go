// mall-order：订单终态分支——超时关单 / 用户取消 / 退款（paid 分支）。
//
// 状态机口径（db-schema mall_orders.status 注释）：
//
//	created(1) --支付--> paid(2) --全行履约 done--> fulfilled(3)
//	created(1) --超时/用户取消--> closed(5, reason=timeout/user) + 回补库存
//	paid(2)    --退款--> refunded(4)：作废 pending 履约行（voided）+ 流水置 refunded
//
// 所有迁移都是条件更新（WHERE status=旧态）实现幂等：
//   - 已 closed 的重复取消/扫描 → no-op（契约：重复操作返回当前状态 200）；
//   - 已 refunded 的重复退款 → no-op；
//   - 状态机非法迁移（paid 再取消、created 直接退款）→ MALL_30002。
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// timeoutScanBatch 单轮超时扫描的最大候选订单数（防大表全量）。
const timeoutScanBatch = 100

// CloseTimedOut 关闭超时未支付订单：created 且 created_at 早于 now-olderThan
// → closed(timeout) + 回补库存。返回本次实际关闭的订单数。
// 截止线在 Go 侧计算（created_at 由 Go 侧时钟写入，两边同源；dev MySQL
// 容器为 UTC 会话，若用 NOW(3) 比对会产生 8h 视差）。
func (s *Store) CloseTimedOut(ctx context.Context, olderThan time.Duration) (int, error) {
	// 快照拉候选（无锁）；逐单事务内条件更新兜底并发与重复扫描。
	cutoff := time.Now().Add(-olderThan)
	var rows []orderRow
	if err := s.db.WithContext(ctx).
		Where("status = ? AND created_at <= ?", orderStatusCreated, cutoff).
		Order("id ASC").Limit(timeoutScanBatch).Find(&rows).Error; err != nil {
		return 0, fmt.Errorf("mall-order: scan timed-out orders: %w", err)
	}
	n := 0
	for _, r := range rows {
		_, status, err := s.closeOrder(ctx, r.OrderNo, closedReasonTimeout)
		if err != nil {
			return n, err
		}
		if status == orderStatusClosed {
			n++
		}
	}
	return n, nil
}

// CancelOrder 用户取消（契约 POST /orders/{no}/cancel）：仅 created 可取消
// → closed(user) + 回补库存；已 closed 幂等返回 nil；跨租户 → MALL_30001；
// paid/fulfilled/refunded → MALL_30002。
func (s *Store) CancelOrder(ctx context.Context, tenantID uint64, orderNo string) error {
	// 归属校验：不匹配视为不存在（与 GetOrder 同口径，不泄漏存在性）。
	var row orderRow
	err := s.db.WithContext(ctx).
		Where("order_no = ? AND tenant_id = ?", orderNo, tenantID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return mallerr.E(mallerr.CodeOrderNotFound, false, "订单不存在: %s", orderNo)
	}
	if err != nil {
		return fmt.Errorf("mall-order: load order %s: %w", orderNo, err)
	}
	switch {
	case row.Status == orderStatusClosed:
		return nil // 幂等：已关闭（无论 timeout/user）
	case row.Status != orderStatusCreated:
		return mallerr.E(mallerr.CodeOrderStateInvalid, false,
			"订单状态不允许取消: %s（当前 %s）", orderNo, statusName(row.Status))
	}
	// closeOrder 锁下复查会捕捉预读与关单之间的状态迁移（如并发支付）：
	// 迁移未发生且终态不是 closed → 取消与该状态冲突，如实 MALL_30002
	// （不得 200 回包掩盖 paid 状态）。
	migrated, status, err := s.closeOrder(ctx, orderNo, closedReasonUser)
	if err != nil {
		return err
	}
	if !migrated && status != orderStatusClosed {
		return mallerr.E(mallerr.CodeOrderStateInvalid, false,
			"订单状态不允许取消: %s（当前 %s）", orderNo, statusName(status))
	}
	return nil
}

// closeOrder 单单关单事务：created→closed(reason) 条件更新 + 回补库存。
// 返回是否发生实际迁移，以及锁下终读的订单状态——迁移未发生时调用方按
// 终态分流（closed → 幂等 no-op；其余 → 状态冲突）。
func (s *Store) closeOrder(ctx context.Context, orderNo string, reason int8) (bool, int8, error) {
	migrated := false
	var finalStatus int8
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row orderRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_no = ?", orderNo).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return mallerr.E(mallerr.CodeOrderNotFound, false, "订单不存在: %s", orderNo)
		}
		if err != nil {
			return fmt.Errorf("mall-order: lock order %s: %w", orderNo, err)
		}
		// 锁下复查（快照拉取后有并发迁移的可能）
		if row.Status != orderStatusCreated {
			finalStatus = row.Status // 已非 created：幂等 no-op（不回补库存）
			return nil
		}
		res := tx.Exec(
			"UPDATE mall_orders SET status = ?, closed_reason = ?, closed_at = ? "+
				"WHERE order_no = ? AND status = ?",
			orderStatusClosed, reason, time.Now(), orderNo, orderStatusCreated)
		if res.Error != nil {
			return fmt.Errorf("mall-order: close order %s: %w", orderNo, res.Error)
		}
		if res.RowsAffected == 0 {
			// 持锁下条件更新 0 行（理论上不可达，防御性重读终态）
			var cur orderRow
			if err := tx.Where("order_no = ?", orderNo).First(&cur).Error; err != nil {
				return fmt.Errorf("mall-order: reload order %s: %w", orderNo, err)
			}
			finalStatus = cur.Status
			return nil
		}
		migrated = true
		finalStatus = orderStatusClosed
		return restockOrder(tx, orderNo)
	})
	return migrated, finalStatus, err
}

// restockOrder 回补订单行库存（事务内；行快照 qty 按商品累回）。
func restockOrder(tx *gorm.DB, orderNo string) error {
	res := tx.Exec(
		"UPDATE goods g "+
			"JOIN mall_order_items i ON i.goods_id = g.id "+
			"SET g.stock = g.stock + i.qty "+
			"WHERE i.order_no = ?", orderNo)
	if res.Error != nil {
		return fmt.Errorf("mall-order: restock %s: %w", orderNo, res.Error)
	}
	return nil
}

// RefundPaid 退款（R1 仅 paid 分支，契约 §5）：
// 作废 pending 履约行（voided，done 行保留）→ paid→refunded 条件更新
// → paid 流水置 refunded（"退款终态为 refunded，流水另记"口径）。
// 已 refunded 幂等返回 nil；created/closed/fulfilled → MALL_30002。
func (s *Store) RefundPaid(ctx context.Context, tenantID uint64, orderNo string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 归属校验 + 锁订单行。
		var row orderRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_no = ? AND tenant_id = ?", orderNo, tenantID).
			First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return mallerr.E(mallerr.CodeOrderNotFound, false, "订单不存在: %s", orderNo)
		}
		if err != nil {
			return fmt.Errorf("mall-order: lock order %s: %w", orderNo, err)
		}
		switch row.Status {
		case orderStatusRefunded:
			return nil // 幂等：已退款
		case orderStatusPaid:
			// R1 唯一合法退款前置态，继续。
		default:
			return mallerr.E(mallerr.CodeOrderStateInvalid, false,
				"订单状态不允许退款: %s（当前 %s，R1 仅 paid 分支）", orderNo, statusName(row.Status))
		}

		// 1) 作废 pending 履约行（已 done 行保留；failed 行保留供对账）。
		if err := tx.Exec(
			"UPDATE fulfillment_tasks SET status = ? WHERE order_no = ? AND status = ?",
			taskStatusVoided, orderNo, taskStatusPending).Error; err != nil {
			return fmt.Errorf("mall-order: void fulfillment %s: %w", orderNo, err)
		}
		// 2) paid→refunded 条件更新。
		if err := tx.Exec(
			"UPDATE mall_orders SET status = ? WHERE order_no = ? AND status = ?",
			orderStatusRefunded, orderNo, orderStatusPaid).Error; err != nil {
			return fmt.Errorf("mall-order: refund order %s: %w", orderNo, err)
		}
		// 3) paid 流水置 refunded（流水另记；created/failed 流水不动）。
		if err := tx.Exec(
			"UPDATE payment_records SET status = ? WHERE order_no = ? AND status = ?",
			paymentStatusRefunded, orderNo, paymentStatusPaid).Error; err != nil {
			return fmt.Errorf("mall-order: refund records %s: %w", orderNo, err)
		}
		return nil
	})
}
