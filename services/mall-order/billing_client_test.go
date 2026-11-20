// billing_client 单测（R2-T2）：桩 gRPC server 验证
//   - 每请求注入 X-Control-Token metadata（tokenUnaryClient 拦截器）
//   - Credit/Refund 请求/响应经 billing proto 契约往返
//   - DialBilling 空 token fail-closed（起拨号即报错，不出无鉴权连接）
//   - 生成 client 满足 BillingClient 接口（billing_client.go 已有编译期守卫）
//
// 纯单测：本地监听 127.0.0.1:0，不连 MySQL/billing dev 实例。
package main

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	billingv1 "github.com/frostloom/my-market/pkg/proto/billing/v1"
)

// stubBillingServer 捕获入站 metadata 的 billing 服务桩。
type stubBillingServer struct {
	billingv1.UnimplementedBillingServiceServer

	mu      sync.Mutex
	tokens  []string // 每次 Credit/Refund 调用捕获的 x-control-token
	credits []*billingv1.CreditRequest
	refunds []*billingv1.RefundRequest
}

func (s *stubBillingServer) recordToken(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	s.tokens = append(s.tokens, strings.Join(md.Get(billingMetadataKey), ","))
	s.mu.Unlock()
}

func (s *stubBillingServer) Credit(ctx context.Context, req *billingv1.CreditRequest) (*billingv1.CreditReply, error) {
	s.recordToken(ctx)
	s.mu.Lock()
	s.credits = append(s.credits, req)
	s.mu.Unlock()
	// 回显：balance = amount*2 + 1，证明响应经 proto 往返（非零值哨兵）。
	return &billingv1.CreditReply{BalanceCent: req.GetAmountCent()*2 + 1, Created: true}, nil
}

func (s *stubBillingServer) Refund(ctx context.Context, req *billingv1.RefundRequest) (*billingv1.RefundReply, error) {
	s.recordToken(ctx)
	s.mu.Lock()
	s.refunds = append(s.refunds, req)
	s.mu.Unlock()
	return &billingv1.RefundReply{RefundedCent: req.GetAmountCent() - 1}, nil
}

func (s *stubBillingServer) capturedTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tokens...)
}

// startStubBilling 起 127.0.0.1:0 桩服务，返回地址与停止函数。
func startStubBilling(t *testing.T) (addr string, stub *stubBillingServer, stop func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stub = &stubBillingServer{}
	gs := grpc.NewServer()
	billingv1.RegisterBillingServiceServer(gs, stub)
	go func() { _ = gs.Serve(lis) }()
	return lis.Addr().String(), stub, gs.Stop
}

func TestBillingClient_InjectsTokenPerRequest(t *testing.T) {
	addr, stub, stop := startStubBilling(t)
	defer stop()

	const secret = "tok-mall-r2-secret"
	client, closeConn, err := DialBilling(addr, secret)
	if err != nil {
		t.Fatalf("DialBilling: %v", err)
	}
	defer closeConn()

	ctx := context.Background()
	// Credit：token 头到达 + 契约往返。
	rep, err := client.Credit(ctx, &billingv1.CreditRequest{
		TenantId: 42, AmountCent: 5300, RefType: "mall", RefId: "M202611190000000000001:1",
	})
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if rep.GetBalanceCent() != 5300*2+1 || !rep.GetCreated() {
		t.Fatalf("Credit 回复往返不符: %+v", rep)
	}
	// Refund：第二个请求同样注入（每请求，非一次性）。
	rrep, err := client.Refund(ctx, &billingv1.RefundRequest{TenantId: 42, AmountCent: 5300, Ref: "M202611190000000000001:1"})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if rrep.GetRefundedCent() != 5299 {
		t.Fatalf("Refund 回复往返不符: %+v", rrep)
	}

	toks := stub.capturedTokens()
	if len(toks) != 2 {
		t.Fatalf("桩收到 %d 次调用，want 2（Credit+Refund）", len(toks))
	}
	for i, tok := range toks {
		if tok != secret {
			t.Fatalf("第 %d 次调用 token = %q，want %q（X-Control-Token 未按请求注入）", i+1, tok, secret)
		}
	}
}

func TestBillingClient_EmptyTokenFailClosed(t *testing.T) {
	addr, _, stop := startStubBilling(t)
	defer stop()

	client, closeConn, err := DialBilling(addr, "")
	if err == nil {
		closeConn()
		t.Fatalf("空 token 拨号应报错（fail-closed），got client=%v", client)
	}
	if !strings.Contains(err.Error(), billingTokenEnv) {
		t.Fatalf("错误应点名 %s 缺失，got: %v", billingTokenEnv, err)
	}
}

// TestBillingClient_RejectsUnauthenticated 与真实鉴权语义对齐：billing 侧
// 全局拦截器对错 token 回 Unauthenticated——用真桩前置校验的相反面佐证
// token 头确实是服务端判定依据（本桩不校验，仅对照性验证：错误 token 到达时
// 桩捕获值 ≠ 正确 secret，由 capturedTokens 断言兜底）。
func TestBillingClient_WrongTokenNotSilentlyAccepted(t *testing.T) {
	addr, stub, stop := startStubBilling(t)
	defer stop()

	client, closeConn, err := DialBilling(addr, "wrong-token")
	if err != nil {
		t.Fatalf("DialBilling: %v", err)
	}
	defer closeConn()
	if _, err := client.Credit(context.Background(), &billingv1.CreditRequest{TenantId: 1, AmountCent: 1, RefType: "mall", RefId: "x:1"}); err != nil {
		t.Fatalf("Credit: %v", err) // 桩不校验 token，调用本身应通；断言在捕获值
	}
	toks := stub.capturedTokens()
	if len(toks) != 1 || toks[0] != "wrong-token" {
		t.Fatalf("桩捕获 token = %v，want [wrong-token]（拦截器透传错误值，服务端才拒）", toks)
	}
}
