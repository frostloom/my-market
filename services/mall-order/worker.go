// mall-order：履约 outbox worker（ADR-004 + R2-T3 credit 分支 + R2.5-T2 subscribe 分支）。
//
// RunFulfillmentOnce 拉一批到期 pending 任务（status=1 且 next_run_at 到期）
// → 逐行履约（task_type 分发：credit 行调 billing.Credit 真实入账
// ref_type="mall"/ref_id=order_no:line_no，subscribe 行调 billing.
// ActivateSubscription 开通月卡（R2.5-T2，行快照 ref_plan_id 透传），
// 其余行走 shipLine 接缝/mock）→ 标 done；订单推进 paid→fulfilled 两条路：
// 本批触达订单的精准 promote + 每 tick 一次的全量幂等扫描（自愈"标完 done
// 后、promote 前崩溃"窗口，含 failed 行的订单不推进——留给对账）。执行失败
// → retry_count+1 指数退避（2^n 秒），retry_count 达 5 → failed 终态（对账介入）。
// credit 行断电窗口由 billing uk_ref 幂等兜底：Credit 已提交、done 未标 →
// 重放同 ref no-op → 收敛（恰入账一次，TestWorker_CreditCrashReplay_ExactlyOneTopup）；
// subscribe 行同构（mall-sub 0 元锚 uk_ref，TestWorker_SubscribeRealBilling_MixedOrderFulfills）。
// 并发退款竞态（窗口 R1：快照拉取后被 RefundPaid 作废）两层防护覆盖
// void-先于提交与 done-已提交两类交错（credit：mall-void 冲销补偿；subscribe：
// mall-void 退订补偿）；Credit-已提交-done-不可达交错
// 见窗口 R1'（未闭合，R4 对账检出）。防护 = 执行前复检 pending（缩窗）+
// 提交后 done 0 行时 mall-void ref 补偿
// （creditLine/compensateVoidedCredit、subscribeLine/cancelVoidedSubscription；
// 窗口分析见 refund.go 文件头）。
//
// 进程内双循环（main.go 起两个 goroutine，退出信号经 ctx 取消优雅停止）：
//   - RunFulfillmentLoop：履约轮询，间隔 10s；
//   - RunTimeoutLoop：超时关单扫描（CloseTimedOut 30 分钟），间隔 60s。
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	billingv1 "github.com/frostloom/ai-gateway/pkg/proto/billing/v1"
	"github.com/frostloom/my-market/pkg/mallerr"
)

// worker 节奏常量（brief：履约 10s / 超时扫描 60s / 订单超时 30 分钟）。
const (
	fulfillmentInterval = 10 * time.Second
	timeoutScanInterval = 60 * time.Second
	orderTimeout        = 30 * time.Minute
	fulfillmentBatch    = 100 // 单轮最大任务行数
	fulfillmentMaxRetry = 5   // retry_count 上限，达限 → failed
)

// fulfillment_tasks.task_type 编码（db-schema 注释；与 mall_order_items.type
// 同值语义——MarkPaidTx 按行 type 投递）。
const (
	taskTypeShip      = 1 // general 行：物流发货（R2 mock）
	taskTypeCredit    = 2 // recharge 行：billing.Credit 入账（R2-T3 真实通道）
	taskTypeSubscribe = 3 // subscription 行：billing.ActivateSubscription 开通（R2.5-T2 真实通道）
)

// billingRPCTimeout billing 单次 RPC 预算（Credit 履约 / Refund 冲退同口径，
// 与 bff 侧下游调用 5s 对齐）：worker 循环 ctx 长寿命，必须给单次外部调用
// 设独立期限，防 billing 挂起拖死整轮履约。
const billingRPCTimeout = 5 * time.Second

// fulfillmentTaskRow fulfillment_tasks 表行（db-schema.sql，读取用）。
type fulfillmentTaskRow struct {
	ID         uint64    `gorm:"column:id"`
	OrderNo    string    `gorm:"column:order_no"`
	LineNo     int32     `gorm:"column:line_no"`
	TaskType   int8      `gorm:"column:task_type"` // 1 ship 2 credit 3 subscribe
	Status     int8      `gorm:"column:status"`
	RetryCount int       `gorm:"column:retry_count"`
	LastError  string    `gorm:"column:last_error"`
	NextRunAt  time.Time `gorm:"column:next_run_at"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

func (fulfillmentTaskRow) TableName() string { return "fulfillment_tasks" }

// RunFulfillmentOnce 执行一轮履约：拉到期 pending → 逐行履约（credit 行走
// billing.Credit 真实入账通道且履约收口自持，其余走 shipLine 接缝/mock）→ 标
// done，推进全行 done 的订单至 fulfilled。返回本轮完成的任务行数。
func (s *Store) RunFulfillmentOnce(ctx context.Context) (int, error) {
	var tasks []fulfillmentTaskRow
	if err := s.db.WithContext(ctx).
		Where("status = ? AND next_run_at <= NOW(3)", taskStatusPending).
		Order("id ASC").Limit(fulfillmentBatch).Find(&tasks).Error; err != nil {
		return 0, fmt.Errorf("mall-order: pull fulfillment tasks: %w", err)
	}
	ship := s.shipLine
	if ship == nil {
		ship = shipLineMock
	}
	done := 0
	touched := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		if t.TaskType == taskTypeCredit {
			// credit 行：履约收口自持（复检 pending → billing.Credit → done
			// 条件更新；行被并发退款 voided 时立即冲销补偿——R1 窗口，见
			// creditLine 注释）。marked=false（跳过/已冲销）不计 done。
			marked, err := s.creditLine(ctx, t)
			if err != nil {
				// 入账或收口失败 → 退避重试（重放由 billing uk_ref 幂等兜底；
				// Credit 已提交而 done 更新报 DB 错的极罕见情形也由重放收敛）。
				if berr := s.backoffTask(ctx, t, err); berr != nil {
					return done, berr
				}
				continue
			}
			if marked {
				done++
				touched[t.OrderNo] = true
			}
			continue
		}
		if t.TaskType == taskTypeSubscribe {
			// subscribe 行（R2.5-T2）：履约收口自持（复检 pending → billing.
			// ActivateSubscription 开通/续期 → done 条件更新；行被并发退款
			// voided 时立即 CancelSubscription 退订补偿，语义同构 creditLine，
			// 见 subscribeLine 注释）。marked=false（跳过/已退订）不计 done。
			marked, err := s.subscribeLine(ctx, t)
			if err != nil {
				// 开通或收口失败 → 退避重试（重放由 billing mall-sub uk_ref
				// 幂等兜底；激活已提交而 done 未标的断电窗口重放 no-op 收敛）。
				if berr := s.backoffTask(ctx, t, err); berr != nil {
					return done, berr
				}
				continue
			}
			if marked {
				done++
				touched[t.OrderNo] = true
			}
			continue
		}
		shipErr := ship(ctx, t)
		if shipErr == nil {
			// 条件更新幂等：并发 worker / 重放时 RowsAffected=0 不计 done。
			res := s.db.WithContext(ctx).Exec(
				"UPDATE fulfillment_tasks SET status = ?, last_error = '' WHERE id = ? AND status = ?",
				taskStatusDone, t.ID, taskStatusPending)
			if res.Error != nil {
				return done, fmt.Errorf("mall-order: complete task %d: %w", t.ID, res.Error)
			}
			if res.RowsAffected == 1 {
				done++
				touched[t.OrderNo] = true
			}
			continue
		}
		if err := s.backoffTask(ctx, t, shipErr); err != nil {
			return done, err
		}
	}

	// 推进订单：本批触达（产生 done 迁移）的订单，全部行 done 且至少一行
	// → paid→fulfilled 条件更新（ refunded/closed 单不会被命中：status 守卫）。
	for orderNo := range touched {
		if err := s.promoteFulfilled(ctx, orderNo); err != nil {
			return done, err
		}
	}
	// 崩溃自愈扫描：上一 tick 标完 done 后、promote 前崩溃的订单（停留 paid）
	// 由全量幂等扫描收口；含 failed 行的订单不推进（对账介入）。
	if _, err := s.promoteAllFulfilled(ctx); err != nil {
		return done, err
	}
	return done, nil
}

// promoteFulfilled 单订单 paid→fulfilled：全部履约行 done（无 pending/failed）。
// fulfilled_at 用 Go 侧时钟（与 created_at 同源口径）。
func (s *Store) promoteFulfilled(ctx context.Context, orderNo string) error {
	res := s.db.WithContext(ctx).Exec(
		"UPDATE mall_orders o "+
			"SET o.status = ?, o.fulfilled_at = ? "+
			"WHERE o.order_no = ? AND o.status = ? "+
			"AND EXISTS (SELECT 1 FROM fulfillment_tasks f WHERE f.order_no = o.order_no) "+
			"AND NOT EXISTS (SELECT 1 FROM fulfillment_tasks f "+
			"  WHERE f.order_no = o.order_no AND f.status IN (?, ?))",
		orderStatusFulfilled, time.Now(), orderNo, orderStatusPaid, taskStatusPending, taskStatusFailed)
	if res.Error != nil {
		return fmt.Errorf("mall-order: promote fulfilled %s: %w", orderNo, res.Error)
	}
	return nil
}

// promoteAllFulfilled 全量幂等扫描：所有"paid 且履约行全 done（至少一行，
// 无 pending/failed，含 failed 的留给对账）"的订单 → fulfilled。
// 单条 UPDATE，与 touched 精准 promote 并存（幂等无害）；返回推进订单数。
func (s *Store) promoteAllFulfilled(ctx context.Context) (int64, error) {
	res := s.db.WithContext(ctx).Exec(
		"UPDATE mall_orders o "+
			"SET o.status = ?, o.fulfilled_at = ? "+
			"WHERE o.status = ? "+
			"AND EXISTS (SELECT 1 FROM fulfillment_tasks f WHERE f.order_no = o.order_no) "+
			"AND NOT EXISTS (SELECT 1 FROM fulfillment_tasks f "+
			"  WHERE f.order_no = o.order_no AND f.status IN (?, ?))",
		orderStatusFulfilled, time.Now(), orderStatusPaid, taskStatusPending, taskStatusFailed)
	if res.Error != nil {
		return 0, fmt.Errorf("mall-order: promote sweep: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// backoffTask 履约失败退避：retry_count+1 指数退避；达上限 → failed 终态。
// MySQL 单表 UPDATE 的 SET 从左到右求值：next_run_at 与 status 必须排在
// retry_count 自增之前，引用到的才是旧值——首败延迟 2s，序列 2/4/8/16/32s，
// 第 5 次失败（旧 retry_count=4）转 failed 且 retry_count 终值 5（brief/
// db-schema"上限 5"口径；若排在自增后会整体提前一拍，退避 4s 起步）。
func (s *Store) backoffTask(ctx context.Context, t fulfillmentTaskRow, shipErr error) error {
	if err := s.db.WithContext(ctx).Exec(
		"UPDATE fulfillment_tasks "+
			"SET next_run_at = DATE_ADD(NOW(3), INTERVAL POWER(2, retry_count + 1) SECOND), "+
			"    status = IF(retry_count + 1 >= ?, ?, status), "+
			"    retry_count = retry_count + 1, "+
			"    last_error = LEFT(?, 512) "+
			"WHERE id = ? AND status = ?",
		fulfillmentMaxRetry, taskStatusFailed, shipErr.Error(), t.ID, taskStatusPending).Error; err != nil {
		return fmt.Errorf("mall-order: backoff task %d: %w", t.ID, err)
	}
	return nil
}

// creditLine recharge 行履约（R2-T3 + Fix Round 1 收口）：billing.Credit 入账
// 且履约收口自持（done 标记与冲销补偿在本函数闭环）。
//
// 请求组装（裁定 1/2）：tenant 取自订单头（mall_orders.tenant_id，任务行只带
// order_no/line_no）；金额 = 行快照 face_value_cent + bonus_cent（赠送随入账）；
// ref_type="mall"、ref_id=order_no:line_no —— billing uk_ref(ref_type,ref_id)
// 是"恰入账一次"的幂等提交点。成功（created=true）或重放 no-op（created=false，
// billing 已入账）都算履约成功；失败返回错误走既有退避（第 5 败 failed 交对账）。
//
// 窗口 R1 两层防护（worker 快照与并发 RefundPaid 的竞态，详见 refund.go 文件头）：
//   - 层一（缩窗）：执行前单行复检任务行仍 pending——快照拉取与执行之间已被
//     voided 的行静默跳过（零副作用，不触 billing）；
//   - 层二（兜底）：入账提交后 done 条件更新影响 0 行（复检与 done 之间的
//     亚毫秒窗口内被 voided）→ billing 已真实入账而订单已被退款 → 立即以独立
//     冲销 ref `mall-void:{order_no}:{line_no}` 调 billing.Refund 补偿并告警。
//
// 返回 marked：任务行已标 done（正常收口）= true；跳过（已 voided）或已冲销
// 补偿 = false（均不计入本轮 done，行未完成）。
func (s *Store) creditLine(ctx context.Context, t fulfillmentTaskRow) (bool, error) {
	if s.billing == nil {
		return false, mallerr.E(mallerr.CodeUpstreamUnavailable, true,
			"billing client 未接线（credit 行履约需要 %s/%s）", billingAddrEnv, billingTokenEnv)
	}

	// 层一：复检任务行仍 pending（单行主键 SELECT）。已 voided/done/failed →
	// 静默跳过（billing 零触达，无补偿必要）。
	var st int8
	if err := s.db.WithContext(ctx).
		Raw("SELECT status FROM fulfillment_tasks WHERE id = ?", t.ID).
		Scan(&st).Error; err != nil {
		return false, fmt.Errorf("mall-order: recheck credit task %d: %w", t.ID, err)
	}
	if st != taskStatusPending {
		return false, nil
	}

	// 行快照 + 租户：JOIN 订单头取 tenant_id，行表取 face/bonus 快照。
	var line struct {
		TenantID      uint64 `gorm:"column:tenant_id"`
		LineType      int8   `gorm:"column:type"`
		FaceValueCent int64  `gorm:"column:face_value_cent"`
		BonusCent     int64  `gorm:"column:bonus_cent"`
	}
	err := s.db.WithContext(ctx).
		Table("mall_order_items").
		Select("mall_orders.tenant_id, mall_order_items.type, mall_order_items.face_value_cent, mall_order_items.bonus_cent").
		Joins("JOIN mall_orders ON mall_orders.order_no = mall_order_items.order_no").
		Where("mall_order_items.order_no = ? AND mall_order_items.line_no = ?", t.OrderNo, t.LineNo).
		Scan(&line).Error
	if err != nil {
		return false, fmt.Errorf("mall-order: load credit line %s:%d: %w", t.OrderNo, t.LineNo, err)
	}
	if line.TenantID == 0 || line.LineType != typeCredit {
		// 订单行缺失/类型不符：数据不一致，重试也不会好——退避后 failed 交对账。
		return false, mallerr.E(mallerr.CodeInternal, false,
			"credit 行数据不一致: %s:%d（tenant=%d type=%d）", t.OrderNo, t.LineNo, line.TenantID, line.LineType)
	}
	amount := line.FaceValueCent + line.BonusCent
	if amount <= 0 {
		return false, mallerr.E(mallerr.CodeInternal, false,
			"credit 行金额非法: %s:%d（face=%d bonus=%d）", t.OrderNo, t.LineNo, line.FaceValueCent, line.BonusCent)
	}
	cctx, cancel := context.WithTimeout(ctx, billingRPCTimeout)
	defer cancel()
	_, err = s.billing.Credit(cctx, &billingv1.CreditRequest{
		TenantId:   int64(line.TenantID),
		AmountCent: amount,
		RefType:    "mall",
		RefId:      fmt.Sprintf("%s:%d", t.OrderNo, t.LineNo),
	})
	if err != nil {
		return false, fmt.Errorf("billing credit %s:%d: %w", t.OrderNo, t.LineNo, err)
	}

	// 层二：入账已提交 → done 条件更新。0 行 = 行在此窗口内被并发退款 voided
	//（复检与 done 之间的亚毫秒残留）——租户对该单既无订单也不该留余额，
	// 立即冲销补偿。done 更新自身报 DB 错则按失败退避（重放：Credit 同 ref
	// no-op → done 更新重试；若行彼时已 voided 则走本补偿分支收口）。
	res := s.db.WithContext(ctx).Exec(
		"UPDATE fulfillment_tasks SET status = ?, last_error = '' WHERE id = ? AND status = ?",
		taskStatusDone, t.ID, taskStatusPending)
	if res.Error != nil {
		return false, fmt.Errorf("mall-order: complete credit task %d: %w", t.ID, res.Error)
	}
	if res.RowsAffected == 1 {
		return true, nil
	}
	s.compensateVoidedCredit(ctx, t, line.TenantID, amount)
	return false, nil
}

// compensateVoidedCredit 窗口 R1 防护层二（creditLine 内调用，billing 必已入账）：
// 以独立冲销 ref `mall-void:{order_no}:{line_no}` 调 billing.Refund 撤回刚入账
// 的金额（min 口径：若并发消费已花掉部分余额，实退可能 < 金额，以 refunded_cent
// 如实记日志）。冲销失败不重试——任务行已 voided 不可重放，补偿失败仅 ERROR
// 日志，交对账检出（billing 侧 mall:* 与 mall-refund:*/mall-void:* 流水交叉
// 核对，R4 对账 job 落地）。
func (s *Store) compensateVoidedCredit(ctx context.Context, t fulfillmentTaskRow, tenantID uint64, amountCent int64) {
	ref := fmt.Sprintf("mall-void:%s:%d", t.OrderNo, t.LineNo)
	cctx, cancel := context.WithTimeout(ctx, billingRPCTimeout)
	defer cancel()
	rep, err := s.billing.Refund(cctx, &billingv1.RefundRequest{
		TenantId:   int64(tenantID),
		AmountCent: amountCent,
		Ref:        ref,
	})
	if err != nil {
		log.Printf("[worker] ERROR 冲销补偿失败（入账后行被退款作废，需对账检出）: order=%s line=%d amount=%d ref=%s: %v",
			t.OrderNo, t.LineNo, amountCent, ref, err)
		return
	}
	log.Printf("[worker] WARN 冲销补偿执行（入账后行被退款作废）: order=%s line=%d amount=%d refunded=%d ref=%s",
		t.OrderNo, t.LineNo, amountCent, rep.GetRefundedCent(), ref)
}

// subscribeLine subscribe 行履约（R2.5-T2）：billing.ActivateSubscription
// 开通/续期月卡，履约收口自持（done 标记与退订补偿在本函数闭环），与
// creditLine 同构（R2-T3 框架）。
//
// 请求组装（裁定 2）：tenant 取自订单头（mall_orders.tenant_id，任务行只带
// order_no/line_no）；plan_id = 行快照 mall_order_items.ref_plan_id（下单时
// 从 goods.ref_plan_id 快照，商品侧改价/换套餐不影响已下单的履约与退款审计，
// 行级证据自持）；ref = order_no:line_no —— billing 以 topups 0 元流水
// （ref_type="mall-sub"、ref_id=ref）的 uk_ref 为幂等提交点，订阅行本身无
// ref 列。成功（activated=true，开通或续一期）或重放 no-op（activated=false，
// billing 已激活，断电窗口重放）都算履约成功；失败返回错误走既有退避
// （第 5 败 failed 交对账）。
//
// 退款竞态两层防护（与 creditLine 同构，窗口分析见 refund.go 文件头）：
//   - 层一（缩窗）：执行前单行复检任务行仍 pending——快照拉取与执行之间已被
//     voided 的行静默跳过（零副作用，不触 billing）；
//   - 层二（兜底）：激活提交后 done 条件更新影响 0 行（复检与 done 之间的
//     亚毫秒窗口内被 voided）→ billing 已开通而订单已被退款 → 立即以独立
//     审计 ref `mall-void:{order_no}:{line_no}` 调 billing.CancelSubscription
//     退订补偿（R2.5「退款=退订」语义：置 auto_renew=0 阻止退款后继续续费，
//     当期权益保留至期满；Cancel 状态定点收敛幂等，与 T3 退款路径同接口）。
//
// 返回 marked：任务行已标 done（正常收口）= true；跳过（已 voided）或已退订
// 补偿 = false（均不计入本轮 done，行未完成）。
func (s *Store) subscribeLine(ctx context.Context, t fulfillmentTaskRow) (bool, error) {
	if s.billing == nil {
		return false, mallerr.E(mallerr.CodeUpstreamUnavailable, true,
			"billing client 未接线（subscribe 行履约需要 %s/%s）", billingAddrEnv, billingTokenEnv)
	}

	// 层一：复检任务行仍 pending（单行主键 SELECT）。已 voided/done/failed →
	// 静默跳过（billing 零触达，无退订必要）。
	var st int8
	if err := s.db.WithContext(ctx).
		Raw("SELECT status FROM fulfillment_tasks WHERE id = ?", t.ID).
		Scan(&st).Error; err != nil {
		return false, fmt.Errorf("mall-order: recheck subscribe task %d: %w", t.ID, err)
	}
	if st != taskStatusPending {
		return false, nil
	}

	// 行快照 + 租户：JOIN 订单头取 tenant_id，行表取 ref_plan_id 快照。
	var line struct {
		TenantID  uint64 `gorm:"column:tenant_id"`
		LineType  int8   `gorm:"column:type"`
		RefPlanID uint64 `gorm:"column:ref_plan_id"`
	}
	err := s.db.WithContext(ctx).
		Table("mall_order_items").
		Select("mall_orders.tenant_id, mall_order_items.type, mall_order_items.ref_plan_id").
		Joins("JOIN mall_orders ON mall_orders.order_no = mall_order_items.order_no").
		Where("mall_order_items.order_no = ? AND mall_order_items.line_no = ?", t.OrderNo, t.LineNo).
		Scan(&line).Error
	if err != nil {
		return false, fmt.Errorf("mall-order: load subscribe line %s:%d: %w", t.OrderNo, t.LineNo, err)
	}
	if line.TenantID == 0 || line.LineType != typeSubscription || line.RefPlanID == 0 {
		// 订单行缺失/类型不符/无 plan 快照：数据不一致，重试也不会好——退避后
		// failed 交对账（下单守卫已拒 ref_plan_id=0 的新单，此分支只兜存量脏数据）。
		return false, mallerr.E(mallerr.CodeInternal, false,
			"subscribe 行数据不一致: %s:%d（tenant=%d type=%d ref_plan_id=%d）",
			t.OrderNo, t.LineNo, line.TenantID, line.LineType, line.RefPlanID)
	}
	ref := fmt.Sprintf("%s:%d", t.OrderNo, t.LineNo)
	cctx, cancel := context.WithTimeout(ctx, billingRPCTimeout)
	defer cancel()
	rep, err := s.billing.ActivateSubscription(cctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(line.TenantID),
		PlanId:   int64(line.RefPlanID),
		Ref:      ref,
	})
	if err != nil {
		return false, fmt.Errorf("billing activate subscription %s:%d: %w", t.OrderNo, t.LineNo, err)
	}
	if !rep.GetActivated() {
		// 重放（同 ref 已激活，断电窗口重入）：no-op 摘要按履约成功收口
		//（与 Credit created=false 同口径），续期信息落日志备查。
		log.Printf("[worker] subscribe 重放 no-op（billing 已激活）: order=%s line=%d ref=%s cycle_num=%d",
			t.OrderNo, t.LineNo, ref, rep.GetSubscription().GetCycleNum())
	} else if rep.GetIsRenewal() {
		log.Printf("[worker] subscribe 续一期: order=%s line=%d plan=%d period_end=%d",
			t.OrderNo, t.LineNo, line.RefPlanID, rep.GetSubscription().GetPeriodEnd())
	}

	// 层二：激活已提交 → done 条件更新。0 行 = 行在此窗口内被并发退款 voided
	//（复检与 done 之间的亚毫秒残留）——订单已退款而订阅仍将自动续费，立即退订
	// 补偿（退款=退订）。done 更新自身报 DB 错则按失败退避（重放：激活同 ref
	// no-op → done 更新重试；若行彼时已 voided 则走本补偿分支收口）。
	res := s.db.WithContext(ctx).Exec(
		"UPDATE fulfillment_tasks SET status = ?, last_error = '' WHERE id = ? AND status = ?",
		taskStatusDone, t.ID, taskStatusPending)
	if res.Error != nil {
		return false, fmt.Errorf("mall-order: complete subscribe task %d: %w", t.ID, res.Error)
	}
	if res.RowsAffected == 1 {
		return true, nil
	}
	s.cancelVoidedSubscription(ctx, t, line.TenantID)
	return false, nil
}

// cancelVoidedSubscription 窗口层二补偿（subscribeLine 内调用，billing 必已
// 激活）：以独立审计 ref `mall-void:{order_no}:{line_no}` 调 billing.
// CancelSubscription 退订（置 auto_renew=0，当期权益保留至期满——R2.5「退款=
// 退订」同口径）。Cancel 状态定点收敛天然幂等，补偿失败不重试——任务行已
// voided 不可重放，失败仅 ERROR 日志交对账检出（billing 侧 mall-sub 激活锚
// 与订阅状态交叉核对，R4 对账 job 落地）。
func (s *Store) cancelVoidedSubscription(ctx context.Context, t fulfillmentTaskRow, tenantID uint64) {
	ref := fmt.Sprintf("mall-void:%s:%d", t.OrderNo, t.LineNo)
	cctx, cancel := context.WithTimeout(ctx, billingRPCTimeout)
	defer cancel()
	rep, err := s.billing.CancelSubscription(cctx, &billingv1.CancelSubscriptionRequest{
		TenantId: int64(tenantID),
		Ref:      ref,
	})
	if err != nil {
		log.Printf("[worker] ERROR 退订补偿失败（激活后行被退款作废，需对账检出）: order=%s line=%d ref=%s: %v",
			t.OrderNo, t.LineNo, ref, err)
		return
	}
	log.Printf("[worker] WARN 退订补偿执行（激活后行被退款作废）: order=%s line=%d cancelled=%v ref=%s",
		t.OrderNo, t.LineNo, rep.GetCancelled(), ref)
}

// shipLineMock mock 发货（恒成功）：Store.shipLine 的缺省实现（general 行物流
// 占位）。credit/subscribe 行不走此路径（billing 真实通道：creditLine /
// subscribeLine，见各自注释）。
func shipLineMock(context.Context, fulfillmentTaskRow) error { return nil }

// RunFulfillmentLoop 履约轮询循环（间隔 fulfillmentInterval），ctx 取消即退出。
func RunFulfillmentLoop(ctx context.Context, s *Store) {
	ticker := time.NewTicker(fulfillmentInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			done, err := s.RunFulfillmentOnce(ctx)
			if err != nil {
				log.Printf("[worker] 履约轮询失败: %v", err)
				continue
			}
			if done > 0 {
				log.Printf("[worker] 履约完成 %d 行", done)
			}
		}
	}
}

// RunTimeoutLoop 超时关单扫描循环（间隔 timeoutScanInterval，阈值 orderTimeout）。
func RunTimeoutLoop(ctx context.Context, s *Store) {
	ticker := time.NewTicker(timeoutScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.CloseTimedOut(ctx, orderTimeout)
			if err != nil {
				log.Printf("[worker] 超时关单扫描失败: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("[worker] 超时关单 %d 笔", n)
			}
		}
	}
}
