// mall-order：支付域（Provider 抽象 + 回调落账事务）。
//
// Callback 落账事务语义（MarkPaidTx，ADR-004/ADR-007 同库事务口径）：
//
//	BEGIN → 锁订单行 → 幂等判定（同 order_no+provider+provider_trade_no 已
//	paid → no-op，金额不一致也以首回调为准）→ 写 payment_records
//	（新 trade_no 插入；旧 failed 升级 paid）→ cb.Paid 且订单 created 时
//	条件更新 created→paid + INSERT fulfillment_tasks（行级 pending，
//	uk_order_line 幂等）→ COMMIT。
//	paid=false 失败回调：仅落 failed 流水，订单停留 created（R0 台账裁定）。
//	订单已 paid 又来 paid 回调：只补流水、不重复投递履约（幂等 200）。
//
// 验签：HTTP 回调路径经 Verifier（HMAC-SHA256 占位）验签后才进 MarkPaidTx；
// gRPC MarkPaid 为受信内部/mock 回调入口，不走验签但做租户重校验（server.go）。
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// payment_records.status / fulfillment_tasks.status 编码（db-schema 注释）。
const (
	paymentStatusCreated  = 1
	paymentStatusPaid     = 2
	paymentStatusFailed   = 3
	paymentStatusRefunded = 4

	taskStatusPending = 1
	taskStatusDone    = 2
	taskStatusFailed  = 3
	taskStatusVoided  = 4
)

// mall_orders.closed_reason 编码（db-schema 注释）。
const (
	closedReasonTimeout = 1
	closedReasonUser    = 2
)

// paymentRecordRow payment_records 表行（db-schema.sql）。
type paymentRecordRow struct {
	ID                uint64    `gorm:"column:id"`
	OrderNo           string    `gorm:"column:order_no"`
	Provider          string    `gorm:"column:provider"`
	ProviderTradeNo   string    `gorm:"column:provider_trade_no"`
	Status            int8      `gorm:"column:status"`
	AmountCent        int64     `gorm:"column:amount_cent"`
	CallbackPayload   *string   `gorm:"column:callback_payload"` // JSON NULL
	SignatureVerified int8      `gorm:"column:signature_verified"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
}

func (paymentRecordRow) TableName() string { return "payment_records" }

// PaymentProvider 支付渠道抽象（ADR-007 三档接入：mock / stripe / wechat）。
// R1 仅 mock 实现；CreatePayment 返回渠道侧支付单号（trade_no）。
type PaymentProvider interface {
	CreatePayment(ctx context.Context, orderNo string, amountCent int64) (tradeNo string, err error)
}

// Callback 支付回调载荷（Provider 回调 / mock 内部回调的统一形态）。
type Callback struct {
	OrderNo    string
	Provider   string
	TradeNo    string
	Paid       bool
	AmountCent int64
	// SignatureVerified 验签来源标记（按入口如实落 payment_records）：
	// HTTP 回调经 Verifier 验签后为 true；gRPC 内部/mock 受信通道为 false。
	SignatureVerified bool
}

// Verifier 回调验签函数（HTTP 回调路径注入；mock 为 HMAC-SHA256 占位）。
type Verifier func(payload []byte, sig string) (Callback, error)

// MockPaymentProvider 进程内占位 Provider：生成渠道支付单号，
// 不与外部系统交互（真实渠道接入后按 ADR-007 替换实现）。
type MockPaymentProvider struct{}

// CreatePayment 生成 trade_no：MP + yyyyMMddHHmmss + 6 位随机数字。
func (MockPaymentProvider) CreatePayment(_ context.Context, orderNo string, amountCent int64) (string, error) {
	if orderNo == "" {
		return "", mallerr.E(mallerr.CodeInvalidParam, false, "创建支付单缺 order_no")
	}
	if amountCent <= 0 {
		return "", mallerr.E(mallerr.CodeInvalidParam, false, "创建支付单金额非法: %d", amountCent)
	}
	return genTradeNo(), nil
}

// genTradeNo 渠道支付单号（CHAR(128) 上限内；撞号概率由随机段压低，
// payment_records 幂等键为 (order_no, provider, provider_trade_no) 组合）。
func genTradeNo() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		n = big.NewInt(0) // 熵源故障退化：唯一性由组合幂等键兜底
	}
	return fmt.Sprintf("MP%s%06d", time.Now().Format("20060102150405"), n.Int64())
}

// SignCallback HMAC-SHA256 占位签名：payload = Callback JSON 序列化，
// sig = hex(HMAC-SHA256(secret, payload))。HTTP 回调联调占位，
// 真实渠道接入后换渠道验签实现（Verifier 注入点不变）。
func SignCallback(secret []byte, cb Callback) ([]byte, string) {
	payload, err := json.Marshal(cb)
	if err != nil {
		// Callback 全为可序列化标量字段，此处不可达；防御性返回空载荷
		payload = []byte("{}")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return payload, hex.EncodeToString(mac.Sum(nil))
}

// NewHMACVerifier 构造 HMAC-SHA256 占位验签器：先比签（hmac.Equal 恒时比较），
// 再从 payload 还原 Callback。签名不符或载荷非法均返回错误（→ MALL_30005）。
func NewHMACVerifier(secret []byte) Verifier {
	return func(payload []byte, sig string) (Callback, error) {
		mac := hmac.New(sha256.New, secret)
		mac.Write(payload)
		want := make([]byte, hex.EncodedLen(mac.Size()))
		hex.Encode(want, mac.Sum(nil))
		if !hmac.Equal(want, []byte(sig)) {
			return Callback{}, errors.New("signature mismatch")
		}
		var cb Callback
		if err := json.Unmarshal(payload, &cb); err != nil {
			return Callback{}, fmt.Errorf("payload 非回调载荷: %w", err)
		}
		return cb, nil
	}
}

// MarkPaidFromCallback 验签 → 落账的组合入口（HTTP 回调路径）。
// 验签失败 → MALL_30005，订单与流水零写入；验签通过后走 MarkPaidTx
// （SignatureVerified 置 true——流水 signature_verified 按入口如实记录）。
// 返回落账后的订单（读回最新状态）。
func (s *Store) MarkPaidFromCallback(ctx context.Context, payload []byte, sig string, verify Verifier) (*Order, error) {
	cb, err := verify(payload, sig)
	if err != nil {
		return nil, mallerr.E(mallerr.CodeCallbackSignInvalid, false, "回调验签失败: %v", err)
	}
	cb.SignatureVerified = true
	if err := s.MarkPaidTx(ctx, cb); err != nil {
		return nil, err
	}
	return s.getOrderByNo(ctx, cb.OrderNo)
}

// MarkPaidTx 回调落账事务（语义见包注释）。幂等依据：
// payment_records (order_no, provider, provider_trade_no) + 订单状态条件更新。
func (s *Store) MarkPaidTx(ctx context.Context, cb Callback) error {
	if cb.OrderNo == "" || cb.Provider == "" {
		return mallerr.E(mallerr.CodeInvalidParam, false, "回调缺 order_no/provider")
	}
	payloadJSON, err := json.Marshal(cb)
	if err != nil {
		payloadJSON = []byte("{}") // 同 SignCallback：标量载荷不可达此分支
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) 锁订单行（并发回调串行化；不存在 → MALL_30001）。
		var row orderRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_no = ?", cb.OrderNo).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return mallerr.E(mallerr.CodeOrderNotFound, false, "订单不存在: %s", cb.OrderNo)
		}
		if err != nil {
			return fmt.Errorf("mall-order: lock order %s: %w", cb.OrderNo, err)
		}

		// 2) 幂等判定：同 (order_no, provider, trade_no) 已 paid → no-op
		//    （金额不一致也以首回调为准，Review Focus #3）。
		var exist paymentRecordRow
		err = tx.Where("order_no = ? AND provider = ? AND provider_trade_no = ?",
			cb.OrderNo, cb.Provider, cb.TradeNo).First(&exist).Error
		hasExisting := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("mall-order: load payment record %s: %w", cb.OrderNo, err)
		}
		if hasExisting {
			switch {
			case exist.Status == paymentStatusPaid:
				return nil
			case !cb.Paid && exist.Status == paymentStatusFailed:
				return nil // 重复失败回调
			case !cb.Paid:
				return nil // 已 paid 又来失败回调：矛盾回调，以首回调为准
			}
			// cb.Paid 且旧流水 failed：Provider 重试恢复，下方升级为 paid。
		} else if !cb.Paid {
			// 失败回调（新 trade_no）：仅落 failed 流水，订单停留 created（R0 台账）。
			rec := paymentRecordRow{
				OrderNo: cb.OrderNo, Provider: cb.Provider, ProviderTradeNo: cb.TradeNo,
				Status: paymentStatusFailed, AmountCent: cb.AmountCent,
				CallbackPayload:   strPtr(string(payloadJSON)),
				SignatureVerified: sigFlag(cb.SignatureVerified),
			}
			if err := tx.Create(&rec).Error; err != nil {
				return fmt.Errorf("mall-order: insert failed record %s: %w", cb.OrderNo, err)
			}
			return nil
		}

		// 3) 写 paid 流水（新插入 / failed 升级）；signature_verified 按入口如实
		//    记录（HTTP 回调=1，gRPC 内部/mock 通道=0）。
		if hasExisting {
			if err := tx.Model(&paymentRecordRow{}).Where("id = ?", exist.ID).Updates(map[string]any{
				"status":             paymentStatusPaid,
				"amount_cent":        cb.AmountCent,
				"callback_payload":   string(payloadJSON),
				"signature_verified": sigFlag(cb.SignatureVerified),
			}).Error; err != nil {
				return fmt.Errorf("mall-order: promote payment record %s: %w", cb.OrderNo, err)
			}
		} else {
			rec := paymentRecordRow{
				OrderNo: cb.OrderNo, Provider: cb.Provider, ProviderTradeNo: cb.TradeNo,
				Status: paymentStatusPaid, AmountCent: cb.AmountCent,
				CallbackPayload:   strPtr(string(payloadJSON)),
				SignatureVerified: sigFlag(cb.SignatureVerified),
			}
			if err := tx.Create(&rec).Error; err != nil {
				return fmt.Errorf("mall-order: insert payment record %s: %w", cb.OrderNo, err)
			}
		}

		// 4) 订单 created→paid 条件更新（幂等闸门）：仅首次迁移时投递履约任务；
		//    订单已 paid/终态 → 只补流水（上方已写），不重复投递。
		//    paid_at 用 Go 侧时钟（与 created_at 同源，避免 dev 容器 UTC 会话
		//    的 NOW(3) 与 Go 本地时间 8h 视差）。
		res := tx.Exec(
			"UPDATE mall_orders SET status = ?, paid_at = ? WHERE order_no = ? AND status = ?",
			orderStatusPaid, time.Now(), cb.OrderNo, orderStatusCreated)
		if res.Error != nil {
			return fmt.Errorf("mall-order: mark order paid %s: %w", cb.OrderNo, res.Error)
		}
		if res.RowsAffected == 1 {
			// INSERT IGNORE：uk_order_line 兜底，重放不炸唯一键。
			// next_run_at 用 SQL NOW(3)：worker 拉取比较同为 NOW(3)，两侧同钟自洽。
			if err := tx.Exec(
				"INSERT IGNORE INTO fulfillment_tasks (order_no, line_no, task_type, status, retry_count, next_run_at) "+
					"SELECT order_no, line_no, type, ?, 0, NOW(3) FROM mall_order_items WHERE order_no = ?",
				taskStatusPending, cb.OrderNo).Error; err != nil {
				return fmt.Errorf("mall-order: enqueue fulfillment %s: %w", cb.OrderNo, err)
			}
		}
		return nil
	})
}

// getOrderByNo 按单号读订单（无租户过滤；回调路径无租户语境用）。
func (s *Store) getOrderByNo(ctx context.Context, orderNo string) (*Order, error) {
	var row orderRow
	err := s.db.WithContext(ctx).Where("order_no = ?", orderNo).First(&row).Error
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

// strPtr *string 辅助（JSON 载荷落库）。
func strPtr(v string) *string { return &v }

// sigFlag bool → payment_records.signature_verified 编码（1 已验签 / 0 未验签）。
func sigFlag(v bool) int8 {
	if v {
		return 1
	}
	return 0
}
