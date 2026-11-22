// gclient.go：下游 gRPC 客户端接口化。
//
// 设计取舍（报告已注明）：选择"generated client 接口化"而非"generated server 桩"——
// 接口方法签名与 mall-gen 生成 client 完全一致，真实 client 直接满足接口，
// 测试用函数字段桩（httptest 直驱 gin 引擎，无需 bufconn/grpc server）。
// bff 业务代码只依赖这两个接口 + 错误经 mallerr.FromGRPC 还原载体。
package main

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

// GoodsClient 商品域下游（mall-goods :9111）。签名与 mallv1.GoodsServiceClient 一致。
type GoodsClient interface {
	ListCategories(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*mallv1.ListCategoriesReply, error)
	ListGoods(ctx context.Context, in *mallv1.ListGoodsReq, opts ...grpc.CallOption) (*mallv1.ListGoodsReply, error)
	GetGoods(ctx context.Context, in *mallv1.GetGoodsReq, opts ...grpc.CallOption) (*mallv1.GoodsItem, error)
}

// OrderClient 订单/购物车/支付落账下游（mall-order :9112）。签名与
// mallv1.OrderServiceClient 一致。
type OrderClient interface {
	CreateCartOrder(ctx context.Context, in *mallv1.CreateCartOrderReq, opts ...grpc.CallOption) (*mallv1.OrderReply, error)
	CreateRechargeOrder(ctx context.Context, in *mallv1.CreateRechargeOrderReq, opts ...grpc.CallOption) (*mallv1.OrderReply, error)
	GetOrder(ctx context.Context, in *mallv1.GetOrderReq, opts ...grpc.CallOption) (*mallv1.OrderReply, error)
	ListOrders(ctx context.Context, in *mallv1.ListOrdersReq, opts ...grpc.CallOption) (*mallv1.ListOrdersReply, error)
	MarkPaid(ctx context.Context, in *mallv1.MarkPaidReq, opts ...grpc.CallOption) (*mallv1.OrderReply, error)
	BeginPayment(ctx context.Context, in *mallv1.BeginPaymentReq, opts ...grpc.CallOption) (*mallv1.BeginPaymentReply, error)
	CancelOrder(ctx context.Context, in *mallv1.CancelOrderReq, opts ...grpc.CallOption) (*mallv1.OrderReply, error)
	RefundOrder(ctx context.Context, in *mallv1.RefundOrderReq, opts ...grpc.CallOption) (*mallv1.RefundReply, error)
	GetCart(ctx context.Context, in *mallv1.GetCartReq, opts ...grpc.CallOption) (*mallv1.CartReply, error)
	AddCart(ctx context.Context, in *mallv1.AddCartReq, opts ...grpc.CallOption) (*mallv1.CartReply, error)
	PatchCartQty(ctx context.Context, in *mallv1.PatchCartQtyReq, opts ...grpc.CallOption) (*mallv1.CartReply, error)
	RemoveCart(ctx context.Context, in *mallv1.RemoveCartReq, opts ...grpc.CallOption) (*mallv1.CartReply, error)
}

// 编译期守卫：生成 client 满足接口（签名漂移在编译期暴露）。
var (
	_ GoodsClient = mallv1.NewGoodsServiceClient(nil)
	_ OrderClient = mallv1.NewOrderServiceClient(nil)
)

// dialGoods / dialOrder 建立 gRPC 连接（进程内复用单连接；非阻塞握手，
// 首个 RPC 时才真正建连——下游暂不可达不阻塞 bff 起服）。
func dialGoods(addr string) (GoodsClient, func(), error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return mallv1.NewGoodsServiceClient(conn), func() { _ = conn.Close() }, nil
}

func dialOrder(addr string) (OrderClient, func(), error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return mallv1.NewOrderServiceClient(conn), func() { _ = conn.Close() }, nil
}

// rpcTimeout 下游调用统一超时（契约 5xxx：超时→MALL_50002、不可达→MALL_50003，
// 由 mw_errors 按 gRPC 码映射；这里只给 ctx 上界）。
const rpcTimeout = 5 * time.Second
