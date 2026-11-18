// billing_client.go：mall-order → billing 控制面的薄 gRPC 客户端（R2-T2）。
//
// import 边界（守卫测试 TestImportBoundary_OnlyPkgProto）：mall 侧唯一允许的
// ai-gateway import 面 = github.com/frostloom/ai-gateway/pkg/proto/billing/v1。
// 鉴权：billing gRPC 全方法经 ai-gateway 侧全局 UnaryServer 拦截器校验
// X-Control-Token metadata（键名 controlplane.MetadataKey = "x-control-token"，
// CONTROL_PLANE_TOKEN 同值共享）。ai-gateway 的 internal/pkg/controlplane 不可跨
// 服务 import，键名以本文件常量落契约（T4 compose 注入同名 env 双侧对齐）。
//
// 调用点（R2-T3 已接线）：worker credit 行履约 Credit（worker.go creditLine）/
// fulfilled 充值单冲退 Refund（refund.go）。接口方法签名与
// billingv1.BillingServiceClient 一致，测试注入函数桩（stubBilling）或真子进程
// client（recordingBilling 包装）即可。
package main

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	billingv1 "github.com/frostloom/ai-gateway/pkg/proto/billing/v1"
)

// 控制面契约常量（与 ai-gateway/internal/pkg/controlplane 同值）。
const (
	// billingMetadataKey X-Control-Token 的 gRPC metadata 键（小写）。
	billingMetadataKey = "x-control-token"
	// billingAddrEnv billing gRPC 地址环境变量（T4 compose 注入 billing:9101）。
	billingAddrEnv = "BILLING_ADDR"
	// billingTokenEnv 控制面令牌环境变量（双侧同名共享，ai-gateway controlplane.Env）。
	billingTokenEnv = "CONTROL_PLANE_TOKEN"
	// billingDefaultAddr dev 缺省（compose 宿主映射口）。
	billingDefaultAddr = "127.0.0.1:9101"
)

// BillingClient billing 控制面客户端接口（T3 worker 注入点：信用卡履约
// Credit / fulfilled 冲退 Refund）。签名与 billingv1.BillingServiceClient
// 对应方法一致，生成 client 直接满足接口。
type BillingClient interface {
	Credit(ctx context.Context, in *billingv1.CreditRequest, opts ...grpc.CallOption) (*billingv1.CreditReply, error)
	Refund(ctx context.Context, in *billingv1.RefundRequest, opts ...grpc.CallOption) (*billingv1.RefundReply, error)
}

// 编译期守卫：生成 client 满足接口（billing 契约漂移在编译期暴露）。
var _ BillingClient = billingv1.NewBillingServiceClient(nil)

// tokenUnaryClient 出站一元拦截器：每请求注入 X-Control-Token metadata
//（ai-gateway controlplane.UnaryClient 同款语义；mall 侧自持实现）。
func tokenUnaryClient(secret string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, billingMetadataKey, secret)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// DialBilling 拨 billing gRPC（insecure + 令牌出站拦截器）。
// token 为空直接报错（fail-closed，与 billing 侧「空令牌一律拒绝」对齐：
// 不把无鉴权连接装成可用客户端，起服时即暴露配置缺失）。
func DialBilling(addr, secret string) (BillingClient, func(), error) {
	if secret == "" {
		return nil, nil, fmt.Errorf("mall-order: %s 未设置（billing gRPC 全方法要求 X-Control-Token）", billingTokenEnv)
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tokenUnaryClient(secret)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("mall-order: dial billing %s: %w", addr, err)
	}
	return billingv1.NewBillingServiceClient(conn), func() { _ = conn.Close() }, nil
}

// dialBillingFromEnv 按环境变量组装（BILLING_ADDR 缺省 127.0.0.1:9101、
// CONTROL_PLANE_TOKEN 必填）。T3 worker 接线 / T4 compose 注入用。
func dialBillingFromEnv() (BillingClient, func(), error) {
	return DialBilling(envOr(billingAddrEnv, billingDefaultAddr), os.Getenv(billingTokenEnv))
}
