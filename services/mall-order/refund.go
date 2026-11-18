// mall-order：fulfilled 订单退款（R2-T3，契约 §5 规则 D15）。
//
// 状态机扩展（db-schema mall_orders.status 注释）：
//
//	paid(2)      --退款--> refunded(4)：RefundPaid（R1 既有，作废 pending 履约行）
//	fulfilled(3) --退款--> refunded(4)：RefundFulfilled（本文件，逐行冲退/直退）
//
// D15 逐行语义：
//   - recharge（credit）行：billing.Refund(tenant, face+bonus,
//     ref="mall-refund:{order_no}:{line_no}") 冲退——与入账 ref
//     （ref_type="mall", ref_id=order_no:line_no）独立（ADR-005），实际退额 =
//     min(请求额, 当前余额)（billing 侧已保证），以 returned refunded_cent 记实退额；
//   - general（ship）行：直接退（mock 渠道无真实资金流，按行快照价×qty 记退款额）
//     + 回补库存（restockOrder 既有对称口径：kind=2 全行 / kind=1 仅 limited 行）；
//   - subscription 行：R2.5 前不会实际出现（种子 301 on_sale=0）；守卫拒绝
//     （MALL_30002），TODO(R2.5)：接入退订语义（不退费=退订）。
//
// 提交顺序与崩溃窗口分析（裁定 3："先全部子操作成功再置 refunded"）：
//
//	[1] 预读校验（无锁）→ [2] 逐行 billing.Refund（事务外，每 ref 幂等）
//	→ [3] 终局事务（锁单复查 fulfilled → refunded + 流水置 refunded + 回补库存）
//
// billing.Refund 必须在 mall 事务之外：事务内做外部 RPC 会长时间持锁，且
// RPC 提交后事务回滚无法撤回。由此产生的崩溃窗口逐段分析：
//
//   - 窗口 A（部分行已冲退、后续行失败/进程崩溃）：已冲退行的钱已退（billing
//     已提交），订单停留 fulfilled。重试 RefundFulfilled：全部行以同 ref 重入
//     ——已退行 no-op（refunded_cent=0），未退行继续；最终一次性翻转状态。
//     金额正确性由 billing uk_ref 保证（绝不双重退款），无孤儿态。
//   - 窗口 B（全部行已冲退、mall 事务未提交即崩溃）：钱已全退、订单停留
//     fulfilled——用户可见"已退货未退款"？不：钱已退、状态未翻。重试同一接口
//     即收敛（同 ref 全 no-op + 翻转）；无自动重试器（退款是用户触发的同步
//     RPC），失败响应 MALL_50003/50002 明确可重试，用户重试即愈。
//   - 窗口 C（并发重复退款请求）：两请求对同单同 ref 各自冲退 → billing 侧
//     uk_ref 恰一条流水；终局事务条件更新 WHERE status=fulfilled 只有一个赢家，
//     输家锁下复查到 refunded → 幂等 no-op。
//   - 窗口 R1（退款作废 vs 正在执行的 credit 履约）：worker 批拉 pending 快照
//     → RefundPaid 作废行+翻 refunded → worker 仍按快照执行 creditLine →
//     billing.Credit 真实入账 → done 条件更新 0 行静默跳过 → 租户白得余额。
//     两层防护（worker.creditLine，Fix Round 1）：层一执行前单行复检 pending
//     （缩窗，已 voided 即静默跳过零副作用）；层二入账提交后 done 更新 0 行
//     → 立即以独立冲销 ref `mall-void:{order_no}:{line_no}` 调 billing.Refund
//     补偿 + WARN 日志。残留：复检与 Credit 之间的亚毫秒窗口由补偿兜底；
//     补偿自身失败不重试（行已 voided 不可重放），ERROR 日志交对账检出
//     （billing 侧 mall:* 与 mall-refund:*/mall-void:* 流水交叉核对，R4 对账 job）。
//   - 窗口 R2（done 已标 vs promote 前）：任务行 done（已真实入账）、订单仍
//     paid → RefundPaid 保留 done 行照常直退 → 租户既留余额又拿退款。
//     防护（RefundPaid，Fix Round 1）：直退事务内检出 done credit 行 →
//     MALL_30008 拒绝（可重试），等 promote 推进 fulfilled 后走冲退路径。
//     TOCTOU 收口（Fix Round 2）：前置守卫 COUNT 是快照读、任务行未锁——
//     "worker done 先于本事务 void 提交"的交错仍可漏过（守卫读到 pending 放行
//     → void 匹配 0 行 → 翻 refunded 而 done 行在，且 worker done 已成功不触发
//     mall-void 补偿）。故 void UPDATE 之后同一事务以 FOR UPDATE 锁读重跑
//     done-credit COUNT（锁读 = current read，才见刚提交的 done 行；普通 SELECT
//     在 REPEATABLE READ 下仍读事务快照）→ >0 整体回滚并 MALL_30008。
//     两种交错各自收敛：done 落 void 前 → 复查拒绝；done 落 void 后 → void
//     匹配 pending 行、worker done 0 行走 mall-void 补偿。残留：MALL_30008 到
//     promote 完成之间存在短暂"退款暂不可用"，属预期语义（履约收口中）。
//
// 上述所有窗口里 billing 都是幂等提交点，不需要 mall 侧补偿动作（无"先退钱
// 再失败且无法收敛"的路径——R1 的冲销补偿是唯一主动补偿，其失败路径显式留
// 对账）；唯一的代价是窗口 B 期间"账实分离"（billing 已退、mall 未翻），收敛
// 依赖用户重试——R2 接受此取舍（对账 job 属后续里程碑）。
//
// 幂等：已 refunded 订单重复退款 → no-op（errOrderAlreadyRefunded 哨兵，
// server 回 200 当前状态）。错误透传（裁定 3）：billing Unavailable →
// MALL_50003、DeadlineExceeded → MALL_50002，其余 → MALL_50001；整单退款失败
// 不得部分提交（mall 侧状态/流水/库存只在终局事务一次性迁移）。
package main

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	billingv1 "github.com/frostloom/ai-gateway/pkg/proto/billing/v1"
	"github.com/frostloom/my-market/pkg/mallerr"
)

// errOrderAlreadyRefunded 幂等哨兵：订单已 refunded（重复退款 no-op）。
// server 层捕获后按"返回当前状态 + 订单金额"应答（R1 重复退款同口径）。
var errOrderAlreadyRefunded = errors.New("mall-order: order already refunded")

// RefundFulfilled fulfilled 订单退款（D15）：逐行冲退（credit）/直退+回补
// （general），全部子操作成功后终局事务一次性迁移状态。返回本次实退金额
// （Σ billing refunded_cent + general 行快照金额；重放全 no-op 时为 0）。
// 提交顺序与崩溃窗口分析见文件头注释。
func (s *Store) RefundFulfilled(ctx context.Context, tenantID uint64, orderNo string) (int64, error) {
	// [1] 预读：归属校验（不匹配视为不存在，与 GetOrder 同口径）+ 状态守卫 +
	// 行集快照。无锁预读（billing RPC 不该持行锁）；状态竞态由 [4] 锁下复查兜底。
	// 本地守卫（归属/状态/行类型）先于 billing 接线检查：不依赖外部通道即可判定。
	var row orderRow
	err := s.db.WithContext(ctx).
		Where("order_no = ? AND tenant_id = ?", orderNo, tenantID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, mallerr.E(mallerr.CodeOrderNotFound, false, "订单不存在: %s", orderNo)
	}
	if err != nil {
		return 0, fmt.Errorf("mall-order: load order %s: %w", orderNo, err)
	}
	switch row.Status {
	case orderStatusRefunded:
		return 0, errOrderAlreadyRefunded // 幂等：已退款
	case orderStatusFulfilled:
		// 唯一合法前置态，继续。
	default:
		return 0, mallerr.E(mallerr.CodeOrderStateInvalid, false,
			"订单状态不允许退款: %s（当前 %s）", orderNo, statusName(row.Status))
	}
	items, err := s.loadOrderItems(ctx, []uint64{row.ID})
	if err != nil {
		return 0, err
	}

	// [2] 全行预检（先全查后执行）：subscription 行在动任何钱之前拒绝，
	// 避免"冲退到一半撞守卫"的必然失败路径。
	hasCredit := false
	for _, it := range items {
		if it.Type == typeSubscription { // 行侧编码 3=subscribe（与 goods.type=3 同值、语义分属两表）
			// TODO(R2.5)：接入订阅退订语义（不退费=退订）后移除本守卫。
			return 0, mallerr.E(mallerr.CodeOrderStateInvalid, false,
				"订阅行退款未实现（R2.5 退订语义）: %s:%d", orderNo, it.LineNo)
		}
		if it.Type == typeCredit {
			hasCredit = true
		}
	}
	// 充值行需要 billing 通道；纯 general 单（无资金通道）不依赖 billing。
	if hasCredit && s.billing == nil {
		return 0, mallerr.E(mallerr.CodeUpstreamUnavailable, true,
			"billing client 未接线（充值单冲退需要 %s/%s）", billingAddrEnv, billingTokenEnv)
	}

	// [3] 逐行冲退/直退（事务外；每行幂等锚点独立，重试收敛——见文件头）。
	refunded := int64(0)
	for _, it := range items {
		switch it.Type {
		case typeCredit:
			// recharge 行：billing.Refund 冲退（ref 与入账 ref 独立，ADR-005）。
			amount := it.FaceValueCent + it.BonusCent
			if amount <= 0 {
				return 0, mallerr.E(mallerr.CodeInternal, false,
					"credit 行金额非法: %s:%d（face=%d bonus=%d）", orderNo, it.LineNo, it.FaceValueCent, it.BonusCent)
			}
			cctx, cancel := context.WithTimeout(ctx, billingRPCTimeout)
			rep, err := s.billing.Refund(cctx, &billingv1.RefundRequest{
				TenantId:   int64(row.TenantID),
				AmountCent: amount,
				Ref:        fmt.Sprintf("mall-refund:%s:%d", orderNo, it.LineNo),
			})
			cancel()
			if err != nil {
				return 0, mapBillingError(err, fmt.Sprintf("refund %s:%d", orderNo, it.LineNo))
			}
			// refunded_cent 记实退额：足额=amount；不足=billing min 钳制；
			// 同 ref 重放=0（no-op）。三种返回都合法，累加即本次实退。
			refunded += rep.GetRefundedCent()
		case typeGeneral:
			// general 行：直接退（mock 渠道，无真实资金流），按行快照记账。
			refunded += it.PriceSnapshotCent * int64(it.Qty)
		default:
			return 0, mallerr.E(mallerr.CodeInternal, false,
				"订单行类型非法: %s:%d（type=%d）", orderNo, it.LineNo, it.Type)
		}
	}

	// [4] 终局事务：锁单复查（并发退款/预读后状态漂移在此收敛）→
	// fulfilled→refunded + paid 流水置 refunded + 回补库存（restockOrder
	// 既有对称口径）。任何一步失败整体回滚（不部分提交）。
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur orderRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_no = ?", orderNo).First(&cur).Error; err != nil {
			return fmt.Errorf("mall-order: lock order %s: %w", orderNo, err)
		}
		switch cur.Status {
		case orderStatusRefunded:
			return errOrderAlreadyRefunded // 并发退款赢了：幂等 no-op（钱侧同 ref 已收敛）
		case orderStatusFulfilled:
			// 继续。
		default:
			return mallerr.E(mallerr.CodeOrderStateInvalid, false,
				"订单状态不允许退款: %s（当前 %s）", orderNo, statusName(cur.Status))
		}
		if err := tx.Exec(
			"UPDATE mall_orders SET status = ? WHERE order_no = ? AND status = ?",
			orderStatusRefunded, orderNo, orderStatusFulfilled).Error; err != nil {
			return fmt.Errorf("mall-order: refund order %s: %w", orderNo, err)
		}
		// paid 流水置 refunded（流水另记；created/failed 流水不动，与 RefundPaid 同口径）。
		if err := tx.Exec(
			"UPDATE payment_records SET status = ? WHERE order_no = ? AND status = ?",
			paymentStatusRefunded, orderNo, paymentStatusPaid).Error; err != nil {
			return fmt.Errorf("mall-order: refund records %s: %w", orderNo, err)
		}
		// 回补库存（建单时实际扣过的行；general 行直退的"回补"侧）。
		return restockOrder(tx, orderNo)
	}); err != nil {
		if errors.Is(err, errOrderAlreadyRefunded) {
			return 0, err
		}
		return 0, fmt.Errorf("mall-order: fulfill refund tx %s: %w", orderNo, err)
	}
	return refunded, nil
}

// mapBillingError billing gRPC 错误 → 契约码（裁定 3 透传口径）：
// Unavailable → MALL_50003（下游不可用）、DeadlineExceeded（含 ctx 超时）→
// MALL_50002（下游超时）、其余 → MALL_50001（含 billing 业务拒绝，如幂等键
// 冲突/无可退额度——原文随错误体透出供对账）。三者均标记可重试。
func mapBillingError(err error, what string) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.Unavailable:
		return mallerr.E(mallerr.CodeUpstreamUnavailable, true, "billing 不可用: %s: %v", what, err)
	case codes.DeadlineExceeded:
		return mallerr.E(mallerr.CodeUpstreamTimeout, true, "billing 超时: %s: %v", what, err)
	default:
		return mallerr.E(mallerr.CodeInternal, true, "billing 调用失败: %s: %v", what, err)
	}
}
