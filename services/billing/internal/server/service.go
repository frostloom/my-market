// Package server billing 计费服务的 gRPC 控制面实现。
package server

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/frostloom/my-market/pkg/controlplane"
	billingv1 "github.com/frostloom/my-market/pkg/proto/billing/v1"
	"github.com/frostloom/my-market/services/billing/store"
)

type Service struct {
	billingv1.UnimplementedBillingServiceServer
	store            *store.Store
	log              *slog.Logger
	requireReserveV2 bool
}

type Option func(*Service)

// RequireReserveV2 rejects legacy reservations before any store mutation.
func RequireReserveV2() Option {
	return func(s *Service) { s.requireReserveV2 = true }
}

func NewService(st *store.Store, log *slog.Logger, options ...Option) *Service {
	s := &Service{store: st, log: log}
	for _, option := range options {
		option(s)
	}
	return s
}

// Reserve 预占额度（幂等）。
func (s *Service) Reserve(ctx context.Context, req *billingv1.ReserveRequest) (*billingv1.ReserveResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	modes := md.Get(controlplane.ReserveModeKey)
	strict := len(modes) == 1 && (modes[0] == controlplane.ReserveReplayV1 || modes[0] == controlplane.ReserveReplayV2)
	if len(modes) > 0 && !strict {
		return nil, status.Error(codes.InvalidArgument, "unsupported reserve contract")
	}
	if s.requireReserveV2 && (!strict || modes[0] != controlplane.ReserveReplayV2) {
		return nil, status.Error(codes.FailedPrecondition, "reserve content contract required")
	}
	if (!strict || modes[0] != controlplane.ReserveReplayV2) && len(md.Get(controlplane.ReserveContentKey)) > 0 {
		return nil, status.Error(codes.InvalidArgument, "unexpected reserve content digest")
	}
	if len(modes) == 1 && modes[0] == controlplane.ReserveReplayV2 {
		values := md.Get(controlplane.ReserveContentKey)
		if len(values) != 1 || len(values[0]) != 64 {
			return nil, status.Error(codes.InvalidArgument, "invalid reserve content digest")
		}
		decoded, err := hex.DecodeString(values[0])
		if err != nil || hex.EncodeToString(decoded) != values[0] {
			return nil, status.Error(codes.InvalidArgument, "invalid reserve content digest")
		}
		ctx = controlplane.WithReserveContent(ctx, values[0])
	}
	var resp *billingv1.ReserveResponse
	var err error
	if strict {
		if err = grpc.SetHeader(ctx, metadata.Pairs(controlplane.ReserveContractKey, modes[0])); err != nil {
			return nil, status.Error(codes.Unavailable, "reserve replay negotiation unavailable")
		}
		resp, err = s.store.ReserveWithReplaySignal(ctx, req)
	} else {
		resp, err = s.store.Reserve(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	s.log.Info("reserve",
		"request_id", req.RequestId,
		"tenant_id", req.TenantId,
		"model", req.Model,
		"code", resp.Code.String(),
		"balance_after", resp.BalanceAfter,
	)
	return resp, nil
}

// GetBalance 查询租户余额。
func (s *Service) GetBalance(ctx context.Context, req *billingv1.GetBalanceRequest) (*billingv1.GetBalanceResponse, error) {
	bal, err := s.store.GetBalance(ctx, uint64(req.TenantId))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "get balance: %v", err)
	}
	return &billingv1.GetBalanceResponse{Balance: bal}, nil
}

// ReportUsage 上报实际用量（持久 marker，幂等）。
func (s *Service) ReportUsage(ctx context.Context, req *billingv1.ReportUsageRequest) (*billingv1.ReportUsageResponse, error) {
	if err := s.store.ReportUsage(ctx, req); err != nil {
		return nil, err
	}
	return &billingv1.ReportUsageResponse{Accepted: true}, nil
}

// Settle 结算（幂等）。网关把实测用量放在 metadata 里，和结算同一次调用。
func (s *Service) Settle(ctx context.Context, req *billingv1.SettleRequest) (*billingv1.SettleResponse, error) {
	usageCtx, err := settleUsageContext(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.store.Settle(usageCtx, req)
	if err != nil {
		return nil, err
	}
	s.log.Info("settle", "request_id", req.RequestId, "settled", resp.Settled, "delta_quota", resp.DeltaQuota)
	return resp, nil
}

// Reverse 冲正/退款（幂等）。
func (s *Service) Reverse(ctx context.Context, req *billingv1.ReverseRequest) (*billingv1.ReverseResponse, error) {
	resp, err := s.store.Reverse(ctx, req)
	if err != nil {
		return nil, err
	}
	s.log.Info("reverse", "request_id", req.RequestId, "reversed", resp.Reversed, "refund_quota", resp.RefundQuota, "reason", req.Reason)
	return resp, nil
}

// RebuildBalance 从账本重建余额投影（M4 对账兜底）。
func (s *Service) RebuildBalance(ctx context.Context, req *billingv1.RebuildBalanceRequest) (*billingv1.RebuildBalanceResponse, error) {
	bal, err := s.store.RebuildBalance(ctx, uint64(req.TenantId))
	if err != nil {
		return nil, err
	}
	s.log.Info("rebuild balance", "tenant_id", req.TenantId, "balance", bal)
	return &billingv1.RebuildBalanceResponse{Balance: bal}, nil
}

// Credit 入账（R2 商城充值闭环，幂等）：store 复用 topups 同步事务路径，
// uk_ref(ref_type, ref_id) 幂等；校验失败（非正金额/ref 非法）由 store 返回 InvalidArgument。
func (s *Service) Credit(ctx context.Context, req *billingv1.CreditRequest) (*billingv1.CreditReply, error) {
	bal, created, err := s.store.Credit(ctx, uint64(req.TenantId), req.AmountCent, req.RefType, req.RefId)
	if err != nil {
		return nil, err
	}
	s.log.Info("credit",
		"tenant_id", req.TenantId,
		"amount_cent", req.AmountCent,
		"ref_type", req.RefType,
		"ref_id", req.RefId,
		"created", created,
		"balance_cent", bal,
	)
	return &billingv1.CreditReply{BalanceCent: bal, Created: created}, nil
}

// Refund 按额退款（R2 商城冲退，幂等）：实际退额 = min(amount_cent, 当前余额)，绝不退负；
// ref 为幂等锚点，同 ref 重放 refunded_cent=0（no-op）。
func (s *Service) Refund(ctx context.Context, req *billingv1.RefundRequest) (*billingv1.RefundReply, error) {
	refunded, err := s.store.RefundCent(ctx, uint64(req.TenantId), req.AmountCent, req.Ref)
	if err != nil {
		return nil, err
	}
	s.log.Info("refund",
		"tenant_id", req.TenantId,
		"amount_cent", req.AmountCent,
		"ref", req.Ref,
		"refunded_cent", refunded,
	)
	return &billingv1.RefundReply{RefundedCent: refunded}, nil
}

// ActivateSubscription 开通/续期月卡（R2.5，幂等）：无本期权益按 plan 开通本期（模拟月费校验
// 不接真钱），已有本期权益自本期期满顺延一个周期；同 ref 重放 no-op 返回当前订阅摘要；
// 校验失败/跨租户 ref 由 store 返回 InvalidArgument/NotFound/FailedPrecondition。
func (s *Service) ActivateSubscription(ctx context.Context, req *billingv1.ActivateSubscriptionRequest) (*billingv1.ActivateSubscriptionReply, error) {
	res, err := s.store.ActivateSubscription(ctx, uint64(req.TenantId), uint64(req.PlanId), req.Ref)
	if err != nil {
		return nil, err
	}
	cycleNum := int64(0)
	if res.Sub != nil {
		cycleNum = int64(res.Sub.CycleNum)
	}
	s.log.Info("activate subscription",
		"tenant_id", req.TenantId,
		"plan_id", req.PlanId,
		"ref", req.Ref,
		"activated", res.Activated,
		"is_renewal", res.IsRenewal,
		"cycle_num", cycleNum,
	)
	return &billingv1.ActivateSubscriptionReply{
		Activated:    res.Activated,
		IsRenewal:    res.IsRenewal,
		Subscription: subscriptionSummary(res.Sub),
	}, nil
}

// CancelSubscription 退订（R2.5「退款=退订」语义，幂等）：置 auto_renew=0、当期权益保留至期满、
// 不退费；已退订重放 cancelled=false（no-op）；无订阅 → NotFound。
func (s *Service) CancelSubscription(ctx context.Context, req *billingv1.CancelSubscriptionRequest) (*billingv1.CancelSubscriptionReply, error) {
	sub, cancelled, err := s.store.CancelSubscriptionRef(ctx, uint64(req.TenantId), req.Ref)
	if err != nil {
		return nil, err
	}
	s.log.Info("cancel subscription",
		"tenant_id", req.TenantId,
		"ref", req.Ref,
		"cancelled", cancelled,
	)
	return &billingv1.CancelSubscriptionReply{
		Cancelled:    cancelled,
		Subscription: subscriptionSummary(sub),
	}, nil
}

// subscriptionSummary 订阅行 → RPC 摘要（nil → 全零值摘要）。
func subscriptionSummary(sub *store.Subscription) *billingv1.SubscriptionSummary {
	if sub == nil {
		return &billingv1.SubscriptionSummary{}
	}
	out := &billingv1.SubscriptionSummary{
		PlanId:    int64(sub.PlanID),
		Status:    sub.Status,
		AutoRenew: sub.AutoRenew,
		CycleNum:  int64(sub.CycleNum),
	}
	if sub.CycleStart != nil {
		out.PeriodStart = sub.CycleStart.Unix()
	}
	if sub.CycleEnd != nil {
		out.PeriodEnd = sub.CycleEnd.Unix()
	}
	return out
}

func settleUsageContext(ctx context.Context) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	prompt := md.Get(controlplane.SettlePromptKey)
	completion := md.Get(controlplane.SettleCompletionKey)
	provider := md.Get(controlplane.SettleProviderKey)
	if len(prompt) == 0 && len(completion) == 0 && len(provider) == 0 {
		return ctx, nil
	}
	if len(prompt) != 1 || len(completion) != 1 || len(provider) != 1 {
		return nil, status.Error(codes.InvalidArgument, "incomplete settle usage")
	}
	values := [3]int64{}
	for i, raw := range []string{prompt[0], completion[0], provider[0]} {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return nil, status.Error(codes.InvalidArgument, "invalid settle usage")
		}
		values[i] = n
	}
	return controlplane.WithSettleUsage(ctx, controlplane.SettleUsage{
		Prompt: values[0], Completion: values[1], Provider: values[2],
	}), nil
}

// ValidateAPIKey 校验 API Key（gateway 不碰 MySQL，密钥验证统一走本服务）。
func (s *Service) ValidateAPIKey(ctx context.Context, req *billingv1.ValidateAPIKeyRequest) (*billingv1.ValidateAPIKeyResponse, error) {
	k, err := s.store.ValidateAPIKey(ctx, req.ApiKeyHash)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "validate key: %v", err)
	}
	if k == nil {
		return &billingv1.ValidateAPIKeyResponse{Valid: false}, nil
	}
	return &billingv1.ValidateAPIKeyResponse{
		Valid:    true,
		ApiKeyId: int64(k.ID),
		TenantId: int64(k.TenantID),
	}, nil
}
