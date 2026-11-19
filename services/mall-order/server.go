// mall-order：server.go 把 Store 结果映到 mallv1 契约消息，
// 并把 mallerr 契约码映射为 gRPC status（bff 侧再映回 HTTP）。
// 覆盖下单域（CreateCartOrder/GetOrder/ListOrders + 购物车四操作）与
// 支付域（MarkPaid/CancelOrder/RefundOrder）；履约/超时为进程内 worker
// （worker.go/main.go），回调验签路径见 payment.go。
package main

import (
	"context"
	"errors"
	"time"

	"github.com/frostloom/my-market/pkg/mallerr"
	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

// orderServer mallv1.OrderService 实现（无状态，全部走 Store）。
type orderServer struct {
	mallv1.UnimplementedOrderServiceServer
	store *Store
}

func newOrderServer(store *Store) *orderServer {
	return &orderServer{store: store}
}

// CreateCartOrder 建单（契约 POST /orders，cart 结算分支）。
func (s *orderServer) CreateCartOrder(ctx context.Context, req *mallv1.CreateCartOrderReq) (*mallv1.OrderReply, error) {
	var tenantID uint64
	var idemKey string
	var lines []Line
	if req != nil {
		tenantID = req.GetTenantId()
		idemKey = req.GetIdemKey()
		lines = make([]Line, 0, len(req.GetLines()))
		for _, l := range req.GetLines() {
			lines = append(lines, Line{GoodsID: l.GetGoodsId(), Qty: int(l.GetQty())})
		}
	}
	ord, err := s.store.CreateCartOrder(ctx, tenantID, idemKey, lines)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return toProtoOrder(ord), nil
}

// CreateRechargeOrder recharge 直购建单（契约 POST /orders kind=recharge 分支）。
// goods 类型校验（type=2 才可直购）在本服务端，bff 只透传 goods_id。
func (s *orderServer) CreateRechargeOrder(ctx context.Context, req *mallv1.CreateRechargeOrderReq) (*mallv1.OrderReply, error) {
	var tenantID, goodsID uint64
	var idemKey string
	if req != nil {
		tenantID, goodsID, idemKey = req.GetTenantId(), req.GetGoodsId(), req.GetIdemKey()
	}
	ord, err := s.store.CreateRechargeOrder(ctx, tenantID, goodsID, idemKey)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return toProtoOrder(ord), nil
}

// GetOrder 单笔订单（契约 GET /orders/{no}）。
func (s *orderServer) GetOrder(ctx context.Context, req *mallv1.GetOrderReq) (*mallv1.OrderReply, error) {
	var tenantID uint64
	var orderNo string
	if req != nil {
		tenantID = req.GetTenantId()
		orderNo = req.GetOrderNo()
	}
	ord, err := s.store.GetOrder(ctx, tenantID, orderNo)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return toProtoOrder(ord), nil
}

// ListOrders 订单列表（契约 GET /orders?cursor=，键集游标分页）。
func (s *orderServer) ListOrders(ctx context.Context, req *mallv1.ListOrdersReq) (*mallv1.ListOrdersReply, error) {
	var tenantID uint64
	var page PageReq
	if req != nil {
		tenantID = req.GetTenantId()
		page = PageReq{Cursor: req.GetPage().GetCursor(), PageSize: req.GetPage().GetPageSize()}
	}
	p, err := s.store.ListOrders(ctx, tenantID, page)
	if err != nil {
		return nil, toGRPCError(err)
	}
	items := make([]*mallv1.OrderReply, 0, len(p.Orders))
	for _, o := range p.Orders {
		items = append(items, toProtoOrder(o))
	}
	return &mallv1.ListOrdersReply{Items: items, NextCursor: p.NextCursor, HasMore: p.HasMore}, nil
}

// MarkPaid 支付结果落账（契约 Provider 回调 / mock 内部回调幂等入口）。
// paid=true 置 paid；paid=false 失败回调：payment_records 置 failed，
// 订单停留 created（R0 台账裁定）。tenant_id 服务端重校验：订单不存在
// 或不归属该租户 → MALL_30001（与 GetOrder 同口径，不泄漏存在性）。
// signature_verified 由调用方如实上报（req 字段 7）：BFF 回调入口验签后
// 置 true（流水记 1）；内部/mock 受信通道缺省 false（流水记 0）——payment_records
// signature_verified 按入口如实落库，不虚报验签。
func (s *orderServer) MarkPaid(ctx context.Context, req *mallv1.MarkPaidReq) (*mallv1.OrderReply, error) {
	var tenantID uint64
	var cb Callback
	if req != nil {
		tenantID = req.GetTenantId()
		cb = Callback{
			OrderNo:    req.GetOrderNo(),
			Provider:   req.GetProvider(),
			TradeNo:    req.GetProviderTradeNo(),
			Paid:       req.GetPaid(),
			AmountCent: req.GetAmountCent(),
			// 调用方如实上报：BFF 回调入口验签通过后置 true（落流水 1），
			// 内部/mock 受信通道缺省 false（落流水 0）——审计 Minor-4 同口径。
			SignatureVerified: req.GetSignatureVerified(),
		}
	}
	if _, err := s.store.GetOrder(ctx, tenantID, cb.OrderNo); err != nil {
		return nil, toGRPCError(err)
	}
	if err := s.store.MarkPaidTx(ctx, cb); err != nil {
		return nil, toGRPCError(err)
	}
	ord, err := s.store.GetOrder(ctx, tenantID, cb.OrderNo)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return toProtoOrder(ord), nil
}

// CancelOrder 取消订单（契约 POST /orders/{no}/cancel）：仅 created 可取消
// → closed(user) + 回补库存；幂等（已 closed 返回当前状态 200）。
func (s *orderServer) CancelOrder(ctx context.Context, req *mallv1.CancelOrderReq) (*mallv1.OrderReply, error) {
	var tenantID uint64
	var orderNo string
	if req != nil {
		tenantID, orderNo = req.GetTenantId(), req.GetOrderNo()
	}
	if err := s.store.CancelOrder(ctx, tenantID, orderNo); err != nil {
		return nil, toGRPCError(err)
	}
	ord, err := s.store.GetOrder(ctx, tenantID, orderNo)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return toProtoOrder(ord), nil
}

// RefundOrder 退款（契约 §5 规则 D15，按订单状态分流）：
//   - paid → RefundPaid（R1 既有：作废 pending 履约行 + 状态迁移；R2.5 起
//     done 的 credit/subscribe 行同被守卫拒绝 MALL_30008）；
//   - fulfilled → RefundFulfilled（R2-T3/R2.5：recharge 行 billing.Refund 冲退
//     min 口径、general 行直退+回补、subscription 行退订不退费）；
//   - refunded → 幂等 no-op；created/closed → MALL_30002。
//
// RefundReply.refund_cent 口径：fulfilled 冲退 = 本次实退额（billing
// refunded_cent 累加，min 钳制如实透传）；paid 直退 / 已 refunded 幂等回放 =
// 订单金额（R1 口径：mall 侧无实退额落点，见 refund.go 崩溃窗口分析）。
func (s *orderServer) RefundOrder(ctx context.Context, req *mallv1.RefundOrderReq) (*mallv1.RefundReply, error) {
	var tenantID uint64
	var orderNo string
	if req != nil {
		tenantID, orderNo = req.GetTenantId(), req.GetOrderNo()
	}
	ord, err := s.store.GetOrder(ctx, tenantID, orderNo)
	if err != nil {
		return nil, toGRPCError(err)
	}
	// actualRefund < 0 = 未走实退额路径（paid 直退/幂等回放按订单金额应答）。
	actualRefund := int64(-1)
	switch ord.Status {
	case "paid":
		// RefundPaid 自身幂等（已 refunded → nil），状态竞态锁下复查。
		if err := s.store.RefundPaid(ctx, tenantID, orderNo); err != nil {
			return nil, toGRPCError(err)
		}
	case "fulfilled":
		// 预读 fulfilled、并发退款已翻 refunded → 幂等哨兵（按已退应答）。
		c, err := s.store.RefundFulfilled(ctx, tenantID, orderNo)
		if err != nil {
			if !errors.Is(err, errOrderAlreadyRefunded) {
				return nil, toGRPCError(err)
			}
		} else {
			actualRefund = c
		}
	case "refunded":
		// 幂等：已退款，直接应答当前状态。
	default:
		return nil, toGRPCError(mallerr.E(mallerr.CodeOrderStateInvalid, false,
			"订单状态不允许退款: %s（当前 %s）", orderNo, ord.Status))
	}
	ord, err = s.store.GetOrder(ctx, tenantID, orderNo)
	if err != nil {
		return nil, toGRPCError(err)
	}
	refundCent := ord.AmountCent
	if actualRefund >= 0 {
		refundCent = actualRefund
	}
	return &mallv1.RefundReply{Status: ord.Status, RefundCent: refundCent}, nil
}

// GetCart 购物车视图（契约 GET /cart）。
func (s *orderServer) GetCart(ctx context.Context, req *mallv1.GetCartReq) (*mallv1.CartReply, error) {
	var tenantID uint64
	if req != nil {
		tenantID = req.GetTenantId()
	}
	v, err := s.store.GetCart(ctx, tenantID)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return cartReply(v), nil
}

// AddCart 加购（契约 POST /cart/items，upsert 合并 qty）。
func (s *orderServer) AddCart(ctx context.Context, req *mallv1.AddCartReq) (*mallv1.CartReply, error) {
	var tenantID, goodsID uint64
	var qty int32
	if req != nil {
		tenantID, goodsID, qty = req.GetTenantId(), req.GetGoodsId(), req.GetQty()
	}
	v, err := s.store.AddCart(ctx, tenantID, goodsID, int(qty))
	if err != nil {
		return nil, toGRPCError(err)
	}
	return cartReply(v), nil
}

// PatchCartQty 改数量（契约 PATCH /cart/items/{id}，qty=0 等价删除）。
func (s *orderServer) PatchCartQty(ctx context.Context, req *mallv1.PatchCartQtyReq) (*mallv1.CartReply, error) {
	var tenantID, itemID uint64
	var qty int32
	if req != nil {
		tenantID, itemID, qty = req.GetTenantId(), req.GetItemId(), req.GetQty()
	}
	v, err := s.store.PatchCartQty(ctx, tenantID, itemID, int(qty))
	if err != nil {
		return nil, toGRPCError(err)
	}
	return cartReply(v), nil
}

// RemoveCart 删购物车行（契约 DELETE /cart/items/{id}，幂等）。
func (s *orderServer) RemoveCart(ctx context.Context, req *mallv1.RemoveCartReq) (*mallv1.CartReply, error) {
	var tenantID, itemID uint64
	if req != nil {
		tenantID, itemID = req.GetTenantId(), req.GetItemId()
	}
	v, err := s.store.RemoveCart(ctx, tenantID, itemID)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return cartReply(v), nil
}

// ---------------------------------------------------------------------------
// 映射辅助

// toProtoOrder 订单聚合 → 契约消息（时间 RFC3339，空时间留空串）。
func toProtoOrder(o *Order) *mallv1.OrderReply {
	items := make([]*mallv1.OrderItem, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, &mallv1.OrderItem{
			GoodsId:   it.GoodsID,
			GoodsName: it.GoodsName,
			GoodsType: int32(it.GoodsType),
			PriceCent: it.PriceCent,
			Qty:       int32(it.Qty),
		})
	}
	return &mallv1.OrderReply{
		OrderNo:      o.OrderNo,
		Status:       o.Status,
		AmountCent:   o.AmountCent,
		Items:        items,
		CreatedAt:    o.CreatedAt.Format(timeRFC3339),
		ClosedReason: o.ClosedReason,
		PaidAt:       formatRFC3339(o.PaidAt),
		FulfilledAt:  formatRFC3339(o.FulfilledAt),
	}
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"

// formatRFC3339 时间指针 → 契约字符串（nil=未发生 → 空串，契约 Order 字段口径）。
func formatRFC3339(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(timeRFC3339)
}

// cartReply 购物车视图 → 契约消息。
func cartReply(v *CartView) *mallv1.CartReply {
	items := make([]*mallv1.CartItem, 0, len(v.Items))
	for _, l := range v.Items {
		items = append(items, &mallv1.CartItem{
			Id:    l.ID,
			Goods: toProtoGoods(l.Goods),
			Qty:   int32(l.Qty),
		})
	}
	return &mallv1.CartReply{Items: items, TotalCent: v.TotalCent}
}

// toProtoGoods goods 行 → GoodsItem（契约 §3 全形状；cart 场景不含 detail，
// recharge 附 token_estimate 营销展示，与 mall-goods 侧同一口径）。
func toProtoGoods(g goodsRow) *mallv1.GoodsItem {
	item := &mallv1.GoodsItem{
		Id:            g.ID,
		Type:          int32(g.Type),
		Name:          g.Name,
		Subtitle:      g.Subtitle,
		PriceCent:     g.PriceCent,
		OrigPriceCent: g.OrigPriceCent,
		FaceValueCent: g.FaceValueCent,
		BonusCent:     g.BonusCent,
		ImageUrl:      g.ImageURL,
		Stock:         int32(g.Stock),
		OnSale:        g.OnSale != 0,
		RefPlanId:     g.RefPlanID,
		CategoryId:    g.CategoryID,
		Limited:       g.Limited != 0,
	}
	if g.Type == typeRecharge {
		item.TokenEstimate = (g.FaceValueCent + g.BonusCent) * tokenEstimateTokensPerCent
	}
	return item
}

// tokenEstimateTokensPerCent 营销展示口径：1 分 ≈ 1000 token（非入账依据）。
const tokenEstimateTokensPerCent = 1000

// toGRPCError mallerr → gRPC status（统一走 pkg/mallerr.ToGRPC 载体：
// gRPC 码按契约码 HTTP 语义行映射（400→InvalidArgument、404→NotFound、
// 409→FailedPrecondition、5xxx 三态各归其位），ErrorInfo.Reason 携带
// MALL 码 + metadata retryable，bff 侧 FromGRPC 还原；无码错误回落 Internal）。
func toGRPCError(err error) error {
	return mallerr.ToGRPC(err)
}
