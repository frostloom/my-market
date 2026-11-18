// mall-order：履约 outbox worker（ADR-004 + R2-T3 credit 分支）。
//
// RunFulfillmentOnce 拉一批到期 pending 任务（status=1 且 next_run_at 到期）
// → 逐行履约（task_type 分发：credit 行调 billing.Credit 真实入账，
// ref_type="mall"/ref_id=order_no:line_no，其余行走 shipLine 接缝/mock）→ 标
// done；订单推进 paid→fulfilled 两条路：本批触达订单的精准 promote +
// 每 tick 一次的全量幂等扫描（自愈"标完 done 后、promote 前崩溃"窗口，
// 含 failed 行的订单不推进——留给对账）。执行失败 → retry_count+1 指数
// 退避（2^n 秒），retry_count 达 5 → failed 终态（对账介入）。
// credit 行断电窗口由 billing uk_ref 幂等兜底：Credit 已提交、done 未标 →
// 重放同 ref no-op → 收敛（恰入账一次，TestWorker_CreditCrashReplay_ExactlyOneTopup）。
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

	"github.com/frostloom/my-market/pkg/mallerr"
	billingv1 "github.com/frostloom/ai-gateway/pkg/proto/billing/v1"
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
	taskTypeSubscribe = 3 // subscription 行：订阅开通（R2.5 前不出现，mock 兜底）
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
// billing.Credit 真实入账通道，其余走 shipLine 接缝/mock）→ 标 done，
// 推进全行 done 的订单至 fulfilled。返回本轮完成的任务行数。
func (s *Store) RunFulfillmentOnce(ctx context.Context) (int, error) {
	var tasks []fulfillmentTaskRow
	if err := s.db.WithContext(ctx).
		Where("status = ? AND next_run_at <= NOW(3)", taskStatusPending).
		Order("id ASC").Limit(fulfillmentBatch).Find(&tasks).Error; err != nil {
		return 0, fmt.Errorf("mall-order: pull fulfillment tasks: %w", err)
	}
	done := 0
	touched := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		shipErr := s.fulfillLine(ctx, t)
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
		// 失败：retry_count+1 指数退避；达上限 → failed 终态。
		// MySQL 单表 UPDATE 的 SET 从左到右求值：next_run_at 与 status 必须排在
		// retry_count 自增之前，引用到的才是旧值——首败延迟 2s，序列 2/4/8/16/32s，
		// 第 5 次失败（旧 retry_count=4）转 failed 且 retry_count 终值 5（brief/
		// db-schema"上限 5"口径；若排在自增后会整体提前一拍，退避 4s 起步）。
		if err := s.db.WithContext(ctx).Exec(
			"UPDATE fulfillment_tasks "+
				"SET next_run_at = DATE_ADD(NOW(3), INTERVAL POWER(2, retry_count + 1) SECOND), "+
				"    status = IF(retry_count + 1 >= ?, ?, status), "+
				"    retry_count = retry_count + 1, "+
				"    last_error = LEFT(?, 512) "+
				"WHERE id = ? AND status = ?",
			fulfillmentMaxRetry, taskStatusFailed, shipErr.Error(), t.ID, taskStatusPending).Error; err != nil {
			return done, fmt.Errorf("mall-order: backoff task %d: %w", t.ID, err)
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

// fulfillLine 单行履约分发（R2-T3）：task_type=credit 的行走 billing.Credit
// 真实入账通道（不经 shipLine 接缝——入账幂等由 billing uk_ref 承担，接缝
// 桩测经 Store.billing 注入）；其余行（ship/subscribe）走既有 shipLine 接缝
// （nil = mock，故障演练/物流渠道注入语义不变）。
func (s *Store) fulfillLine(ctx context.Context, t fulfillmentTaskRow) error {
	if t.TaskType == taskTypeCredit {
		return s.creditLine(ctx, t)
	}
	ship := s.shipLine
	if ship == nil {
		ship = shipLineMock
	}
	return ship(ctx, t)
}

// creditLine recharge 行履约：billing.Credit 入账（R2-T3）。
//
// 请求组装（裁定 1/2）：tenant 取自订单头（mall_orders.tenant_id，任务行只带
// order_no/line_no）；金额 = 行快照 face_value_cent + bonus_cent（赠送随入账）；
// ref_type="mall"、ref_id=order_no:line_no —— billing uk_ref(ref_type,ref_id)
// 是"恰入账一次"的幂等提交点。成功（created=true）或重放 no-op（created=false，
// billing 已入账）都算履约成功；失败返回错误走既有退避（第 5 败 failed 交对账）。
// 崩溃窗口：Credit 已提交、done 未标 → 重放同 ref → no-op → 收敛（不重不漏，
// TestWorker_CreditCrashReplay_ExactlyOneTopup）。
func (s *Store) creditLine(ctx context.Context, t fulfillmentTaskRow) error {
	if s.billing == nil {
		return mallerr.E(mallerr.CodeUpstreamUnavailable, true,
			"billing client 未接线（credit 行履约需要 %s/%s）", billingAddrEnv, billingTokenEnv)
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
		return fmt.Errorf("mall-order: load credit line %s:%d: %w", t.OrderNo, t.LineNo, err)
	}
	if line.TenantID == 0 || line.LineType != typeCredit {
		// 订单行缺失/类型不符：数据不一致，重试也不会好——退避后 failed 交对账。
		return mallerr.E(mallerr.CodeInternal, false,
			"credit 行数据不一致: %s:%d（tenant=%d type=%d）", t.OrderNo, t.LineNo, line.TenantID, line.LineType)
	}
	amount := line.FaceValueCent + line.BonusCent
	if amount <= 0 {
		return mallerr.E(mallerr.CodeInternal, false,
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
		return fmt.Errorf("billing credit %s:%d: %w", t.OrderNo, t.LineNo, err)
	}
	return nil
}

// shipLineMock mock 发货（恒成功）：Store.shipLine 的缺省实现（general 行物流
// 占位）。credit 行不走此路径（真实 billing 入账，见 fulfillLine）；订阅开通
// R2.5 接入前由 mock 兜底（种子 301 on_sale=0，实际不会出现订阅行）。
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
