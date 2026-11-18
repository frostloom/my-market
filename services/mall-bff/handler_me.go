// handler_me.go：我的信息（api-contract §7）：GET /api/v1/me/overview。
//
// 组装（D14）：会话中间件派生 tenant → 并查 billing 两侧——
//  1. billing gRPC GetBalance（Redis 投影，账本兜底自愈）——薄拨号
//     DialBilling（BILLING_ADDR 默认 127.0.0.1:9101），每请求出站注入
//     X-Control-Token metadata（模式与 mall-order/billing_client.go 同源；
//     ai-gateway internal/pkg/controlplane 不可跨服务 import，键名以本文件
//     常量落契约，compose 双侧注入同名 CONTROL_PLANE_TOKEN）；
//  2. billing admin HTTP GET /admin/topups?tenant_id=（BILLING_ADMIN_ADDR
//     默认 http://127.0.0.1:9103 + X-Control-Token 头）——该端点固定回 50 条
//     （limit 参数未支持），客户端截断到最近 10 条。
//
// 响应形状（契约 §7）：{"balance_cent", "recent_topups"(≤10，空数组非 null),
// "subscription"(R2.5 月卡接入前恒 null)}。
//
// 凭据零泄漏：token 只经两条出站通道流向 billing；不进响应体、不进日志
// （handler_me_test.go 端到端断言）。故障映射：任一下游失败 → MALL_50003
// （可重试）/MALL_50002，经 mw_errors 统一出口。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	billingv1 "github.com/frostloom/ai-gateway/pkg/proto/billing/v1"
	"github.com/frostloom/my-market/pkg/mallerr"
)

// 控制面契约常量（与 mall-order/billing_client.go、ai-gateway controlplane 同值）。
const (
	// billingMetadataKey X-Control-Token 的 gRPC metadata 键（小写）。
	billingMetadataKey = "x-control-token"
	// billingAddrEnv billing gRPC 地址环境变量（compose 注入 billing:9101）。
	billingAddrEnv = "BILLING_ADDR"
	// billingAdminAddrEnv billing admin HTTP 基址环境变量（compose 注入 http://billing:9103）。
	billingAdminAddrEnv = "BILLING_ADMIN_ADDR"
	// billingTokenEnv 控制面令牌环境变量（双侧同名共享）。
	billingTokenEnv = "CONTROL_PLANE_TOKEN"
	// billingDefaultAddr / billingAdminDefaultAddr dev 缺省（compose 宿主映射口）。
	billingDefaultAddr      = "127.0.0.1:9101"
	billingAdminDefaultAddr = "http://127.0.0.1:9103"
	overviewTopupsLimit     = 10 // 契约 §7「最近 10 条」
	topupsHTTPTimeout       = 5 * time.Second
)

// ---------------------------------------------------------------------------
// billing gRPC 客户端（bff 侧薄拨号）

// BillingClient billing 控制面客户端接口（overview 只读 GetBalance）。签名与
// billingv1.BillingServiceClient 一致，生成 client 直接满足接口。
type BillingClient interface {
	GetBalance(ctx context.Context, in *billingv1.GetBalanceRequest, opts ...grpc.CallOption) (*billingv1.GetBalanceResponse, error)
}

// 编译期守卫：生成 client 满足接口（billing 契约漂移在编译期暴露）。
var _ BillingClient = billingv1.NewBillingServiceClient(nil)

// tokenUnaryClient 出站一元拦截器：每请求注入 X-Control-Token metadata
// （ai-gateway controlplane.UnaryClient 同款语义；mall 侧自持实现）。
func tokenUnaryClient(secret string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, billingMetadataKey, secret)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// DialBilling 拨 billing gRPC（insecure + 令牌出站拦截器）。token 为空直接报错
// （fail-closed，与 billing 侧「空令牌一律拒绝」对齐：不把无鉴权连接装成可用客户端）。
func DialBilling(addr, secret string) (BillingClient, func(), error) {
	if secret == "" {
		return nil, nil, fmt.Errorf("mall-bff: %s 未设置（billing gRPC 全方法要求 X-Control-Token）", billingTokenEnv)
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(tokenUnaryClient(secret)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("mall-bff: dial billing %s: %w", addr, err)
	}
	return billingv1.NewBillingServiceClient(conn), func() { _ = conn.Close() }, nil
}

// dialBillingFromEnv 按环境变量组装（BILLING_ADDR 缺省 127.0.0.1:9101、
// CONTROL_PLANE_TOKEN 必填）。main.go 起服用。
func dialBillingFromEnv() (BillingClient, func(), error) {
	return DialBilling(envOr(billingAddrEnv, billingDefaultAddr), os.Getenv(billingTokenEnv))
}

// ---------------------------------------------------------------------------
// billing admin topups HTTP 客户端

// billingTopup billing admin /admin/topups 行 wire 形状：billing 侧 Topup 结构
// 无 json tag，writeJSON 用 encoding/json 默认序列化 → 键 = Go 字段名
// （ID/TenantID/Source/Amount/RequestedCent/RefType/RefID/CreatedAt）。
// bff 只解不回传原形状——出口统一转 snake_case 的 overviewTopupJSON。
type billingTopup struct {
	ID            uint64 `json:"ID"`
	TenantID      uint64 `json:"TenantID"`
	Source        string `json:"Source"`
	Amount        int64  `json:"Amount"`
	RequestedCent *int64 `json:"RequestedCent"`
	RefType       string `json:"RefType"`
	RefID         string `json:"RefID"`
	CreatedAt     string `json:"CreatedAt"` // RFC3339 原样透传
}

// TopupsSource 最近入账流水来源（接口化便于桩测；生产 = adminTopupsClient）。
type TopupsSource interface {
	// RecentTopups 返回该租户最近 ≤limit 条流水（倒序）；失败返回 mallerr 载体。
	RecentTopups(ctx context.Context, tenantID uint64, limit int) ([]billingTopup, error)
}

// adminTopupsClient billing admin HTTP（:9103，X-Control-Token 头门禁）。
type adminTopupsClient struct {
	base  string
	token string
	hc    *http.Client
}

// newAdminTopups 组装客户端。token 为空 fail-closed（与 DialBilling 同口径）。
func newAdminTopups(base, token string) (*adminTopupsClient, error) {
	if token == "" {
		return nil, fmt.Errorf("mall-bff: %s 未设置（billing admin HTTP 除 /healthz /metrics 外要求 X-Control-Token）", billingTokenEnv)
	}
	return &adminTopupsClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		hc:    &http.Client{Timeout: topupsHTTPTimeout},
	}, nil
}

// newAdminTopupsFromEnv 按环境变量组装（BILLING_ADMIN_ADDR 缺省 127.0.0.1:9103）。
func newAdminTopupsFromEnv() (*adminTopupsClient, error) {
	return newAdminTopups(envOr(billingAdminAddrEnv, billingAdminDefaultAddr), os.Getenv(billingTokenEnv))
}

// RecentTopups GET /admin/topups?tenant_id=&limit=：端点现状固定回 50 条
// （billing admin.go 硬编码；limit 参数透传为前向兼容，不作为正确性依据），
// 客户端以 limit 截断兜底。错误统一 MALL_50003（可重试），消息不含 token。
func (t *adminTopupsClient) RecentTopups(ctx context.Context, tenantID uint64, limit int) ([]billingTopup, error) {
	if limit <= 0 {
		limit = overviewTopupsLimit
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/admin/topups?tenant_id=%d&limit=%d", t.base, tenantID, limit), nil)
	if err != nil {
		return nil, mallerr.E(mallerr.CodeUpstreamUnavailable, true, "构造流水查询失败")
	}
	req.Header.Set("X-Control-Token", t.token)
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, mallerr.E(mallerr.CodeUpstreamUnavailable, true, "billing 流水查询不可达，请稍后重试")
	}
	defer resp.Body.Close() //nolint:errcheck // 读侧丢弃
	if resp.StatusCode != http.StatusOK {
		return nil, mallerr.E(mallerr.CodeUpstreamUnavailable, true, "billing 流水查询异常(%d)，请稍后重试", resp.StatusCode)
	}
	var out struct {
		Items []billingTopup `json:"items"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || json.Unmarshal(raw, &out) != nil {
		return nil, mallerr.E(mallerr.CodeUpstreamUnavailable, true, "billing 流水响应异常")
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
	}
	return out.Items, nil
}

// ---------------------------------------------------------------------------
// GET /me/overview 端点

// overviewTopupJSON 契约 §7 recent_topups 行（snake_case 出口形状）。
type overviewTopupJSON struct {
	ID        uint64 `json:"id"`
	Source    string `json:"source"`
	Amount    int64  `json:"amount"` // 正=入账，负=退款
	RefType   string `json:"ref_type"`
	RefID     string `json:"ref_id"`
	CreatedAt string `json:"created_at"`
}

// handleMeOverview GET /me/overview：会话租户 → GetBalance + 最近 10 条流水。
// subscription 恒 null（R2.5 月卡：billing ActivateSubscription 落地后接入
// 订阅查询，契约 §7 允许对象形状 {"active", "plan_name", "tier_remain"}）。
func handleMeOverview(billing BillingClient, topups TopupsSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenant := tenantID(c)
		// 与其余下游调用同口径：统一吃 rpcTimeout（下游卡死 5s 内 5xxx 出错）。
		ctx, cancel := context.WithTimeout(c.Request.Context(), rpcTimeout)
		defer cancel()
		bal, err := billing.GetBalance(ctx, &billingv1.GetBalanceRequest{TenantId: int64(tenant)})
		if err != nil {
			abortWith(c, err) // 无载体 gRPC 错误 → mw_errors 粗映射（Unavailable→503 MALL_50003）
			return
		}
		rows, err := topups.RecentTopups(ctx, tenant, overviewTopupsLimit)
		if err != nil {
			abortWith(c, err)
			return
		}
		// 契约「最近 10 条」上限在出口处兜底（客户端已截断，防御桩/上游溢出）。
		if len(rows) > overviewTopupsLimit {
			rows = rows[:overviewTopupsLimit]
		}
		items := make([]overviewTopupJSON, 0, len(rows)) // make 空 slice → 序列化 [] 非 null
		for _, r := range rows {
			items = append(items, overviewTopupJSON{
				ID:        r.ID,
				Source:    r.Source,
				Amount:    r.Amount,
				RefType:   r.RefType,
				RefID:     r.RefID,
				CreatedAt: r.CreatedAt,
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"balance_cent":  bal.GetBalance(),
			"recent_topups": items,
			// R2.5 月卡：接入 billing 订阅查询前恒 null（契约 §7）。
			"subscription": nil,
		})
	}
}
