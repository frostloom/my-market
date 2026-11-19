// mall-order：订单终态分支——超时关单 / 用户取消 / 退款（paid 分支）。
//
// 状态机口径（db-schema mall_orders.status 注释）：
//
//	created(1) --支付--> paid(2) --全行履约 done--> fulfilled(3)
//	created(1) --超时/用户取消--> closed(5, reason=timeout/user) + 回补库存
//	paid(2)    --退款--> refunded(4)：作废 pending 履约行（voided）+ 流水置
//	 refunded；done 的 credit/subscribe 行守卫拒绝（R2/R2.5 窗口防护）
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
// 回补范围 = 建单时实际扣过库存的行，与扣减对称：
//   - kind=2（cart 结算）：建单全行无条件直扣 → 全行回补（既有口径）；
//   - kind=1（recharge 直购，R2-T2）：仅 limited=1 商品建单时条件扣减，
//     无限量档（limited=0）建单不碰库存 → 取消/超时同样不得回补
//     （否则 stock 虚增、污染种子锚点——TestCancelRechargeOrder_RestockMatchesDeduction）；
//   - 其他 kind（如未来的 seckill）：按 limited=1 行保守回补；接入时若扣减
//     口径不同，需同步此处。
// 注意：对称依据是 goods.limited 的**当前值**而非建单时快照；若 recharge
// 商品在单据存续期内翻转 limited，存在少补/多补一枚的理论窗口（已记台账）。
func restockOrder(tx *gorm.DB, orderNo string) error {
	res := tx.Exec(
		"UPDATE goods g "+
			"JOIN mall_order_items i ON i.goods_id = g.id "+
			"JOIN mall_orders o ON o.order_no = i.order_no "+
			"SET g.stock = g.stock + i.qty "+
			"WHERE i.order_no = ? AND (o.kind = 2 OR g.limited = 1)", orderNo)
	if res.Error != nil {
		return fmt.Errorf("mall-order: restock %s: %w", orderNo, res.Error)
	}
	return nil
}

// RefundPaid 退款（paid 分支，契约 §5 规则 D15）：
// 作废 pending 履约行（voided，done 行保留）→ paid→refunded 条件更新
// → paid 流水置 refunded（"退款终态为 refunded，流水另记"口径）。
// 已 refunded 幂等返回 nil；created/closed/fulfilled → MALL_30002。
// 守卫（R2-T3 Fix Round 1；R2.5-T3 扩 subscribe）：存在 done 的 credit/subscribe
// 行 → MALL_30008——已真实入账/订阅已激活（auto_renew=1 将续费）而 promote 未
// 完成，直退会保留余额/自动续费订阅又退款，必须等 fulfilled 后走 RefundFulfilled
// 冲退/退订路径（窗口 R2 及其 subscribe 面，见 refund.go 文件头分析）。
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
			// 窗口 R2 防护（R2-T3 Fix Round 1 裁定；R2.5-T3 扩 subscribe 面）：
			// 存在 done 的 credit/subscribe 行 = billing 已真实履约（入账/订阅
			// 激活且将自动续费）而 promote 未完成（订单停留 paid）。直退会
			// "保留余额/续费订阅又拿退款"（done 行按既有口径不作废，钱/订阅留在
			// billing）→ 拒绝 MALL_30008（可重试），等 promote 推进 fulfilled
			// 后走 RefundFulfilled 冲退/退订路径（credit 走 mall-refund 冲退、
			// subscribe 走 mall-sub-refund 退订）。
			// 前置守卫为快照快速通道；void 后 FOR UPDATE 复查收口 TOCTOU
			// （见下方 1.5，Fix Round 2）。
			var doneFulfill int64
			if err := tx.Raw(
				"SELECT COUNT(*) FROM fulfillment_tasks WHERE order_no = ? AND status = ? AND task_type IN (?, ?)",
				orderNo, taskStatusDone, taskTypeCredit, taskTypeSubscribe).Scan(&doneFulfill).Error; err != nil {
				return fmt.Errorf("mall-order: check done fulfillment line %s: %w", orderNo, err)
			}
			if doneFulfill > 0 {
				return mallerr.E(mallerr.CodeFulfillmentInProgress, false,
					"订单履约收口中，暂不可直接退款（billing 已履约待收口，请稍后重试）: %s", orderNo)
			}
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
		// 1.5) void 后复查（TOCTOU 收口，Fix Round 2；R2.5-T3 扩 subscribe）：
		// 前置守卫 COUNT 是快照读、任务行未锁，"worker done 先于本事务 void
		// 提交"的交错仍可漏过——worker 层一复检通过 → Credit/Activate 提交 →
		// done 先提交 → 本事务守卫读到 pending 放行 → void 匹配 0 行 → 订单翻
		// refunded 而 done credit/subscribe 行在（双得余额/保留续费订阅，且
		// worker done 已成功不触发 mall-void 补偿）。故 void 之后以 FOR UPDATE
		// 锁读复查：锁读是 current read（REPEATABLE READ 下普通 SELECT 仍读
		// 事务快照，看不到刚提交的 done 行；锁读才见最新已提交版本，含本事务
		// 自己 voided 的行）。>0 → 返回 MALL_30008 整体回滚（void/翻转一并
		// 撤销，done 行已可见拒绝成立，等 promote→fulfilled 走冲退/退订）；
		// =0 → 照常继续。两种交错各自收敛：done 落 void 前 → 本复查拒绝；
		// done 落 void 后 → void 匹配到 pending 行、worker done 条件更新 0 行
		// 走 mall-void 补偿（worker.creditLine/subscribeLine 层二重读分诊）。
		var doneFulfillAfterVoid int64
		if err := tx.Raw(
			"SELECT COUNT(*) FROM fulfillment_tasks WHERE order_no = ? AND status = ? AND task_type IN (?, ?) FOR UPDATE",
			orderNo, taskStatusDone, taskTypeCredit, taskTypeSubscribe).Scan(&doneFulfillAfterVoid).Error; err != nil {
			return fmt.Errorf("mall-order: recheck done fulfillment line %s: %w", orderNo, err)
		}
		if doneFulfillAfterVoid > 0 {
			return mallerr.E(mallerr.CodeFulfillmentInProgress, false,
				"订单履约收口中，暂不可直接退款（billing 已履约待收口，请稍后重试）: %s", orderNo)
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
