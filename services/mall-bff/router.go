// router.go：mall-bff 应用装配与路由表（api-contract §2-§7 一一对应）。
//
// 中间件顺序（外→内）：recoverShape（panic 统一形状）→ errShape（统一错误出口）
// → 路由组。限流挂点：注册/登录在鉴权前按 IP；鉴权路由组按租户（auth 之后）。
// R6 前不做静态托管（go:embed 前端属 R6）；健康口沿用服务模式：/healthz 同端口。
package main

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// hashSHA256 hmac 构造用（签名/验签共用）。
var hashSHA256 = sha256.New

// Config 应用配置（全部来自环境变量；见 main.go env 映射）。
type Config struct {
	Addr              string        // 监听地址（默认 :18090）
	GatewayBase       string        // gateway 基址（默认 http://127.0.0.1:18080）
	CallbackSelfURL   string        // mock provider 回调基址（默认 http://127.0.0.1:18090）
	PayCallbackSecret []byte        // 回调 HMAC 密钥（必填，凭据不落码）
	RateLimitPerMin   int           // 默认 120（契约 §8）
	AuthCacheTTL      time.Duration // 会话缓存 TTL（默认 30s）
	CallbackDelay     time.Duration // mock 异步回调延迟（默认 200ms；测试可覆写）
}

// App mall-bff 应用。
type App struct {
	cfg      Config
	engine   *gin.Engine
	sessions *sessionCache
	goods    GoodsClient
	orders   OrderClient
	billing  BillingClient // billing gRPC（/me/overview 余额；handler_me.go）
	topups   TopupsSource  // billing admin 流水（/me/overview 最近 10 条）
	provider PayProvider
	rdb      *redis.Client
	closeFns []func()
}

// Handler 暴露 http.Handler（测试/托管复用）。
func (a *App) Handler() http.Handler { return a.engine }

// Close 释放下游连接与 Redis。
func (a *App) Close() error {
	for _, fn := range a.closeFns {
		fn()
	}
	return nil
}

// NewApp 装配路由。goods/orders/billing 为接口（gclient.go / handler_me.go）：
// 生产传 gRPC client / admin HTTP client，测试传桩。rdb 为 nil 时限流
// fail-open 全放行（无 Redis 的最小运行）。
func NewApp(cfg Config, goods GoodsClient, orders OrderClient, billing BillingClient, topups TopupsSource, rdb *redis.Client) *App {
	log := slog.Default()
	if cfg.RateLimitPerMin <= 0 {
		cfg.RateLimitPerMin = 120
	}
	if cfg.AuthCacheTTL <= 0 {
		cfg.AuthCacheTTL = 30 * time.Second
	}
	if cfg.CallbackDelay <= 0 {
		cfg.CallbackDelay = 200 * time.Millisecond
	}
	gin.SetMode(gin.ReleaseMode)

	a := &App{
		cfg:      cfg,
		engine:   gin.New(),
		sessions: newSessionCache(cfg.AuthCacheTTL),
		goods:    goods,
		orders:   orders,
		billing:  billing,
		topups:   topups,
		rdb:      rdb,
	}
	// 不信任任何代理头：gin 默认信任所有代理，X-Forwarded-For 可伪造——
	// IP 限流键（ClientIP）轮换 XFF 即每请求新桶，限流形同虚设。置 nil 后
	// ClientIP 回落 RemoteAddr（直连地址，不可伪造）。R3 进 K8s/Ingress 时
	// 改为信任集群网段（engine.SetTrustedProxies([]string{"10.0.0.0/8"})）。
	if err := a.engine.SetTrustedProxies(nil); err != nil {
		log.Warn("SetTrustedProxies failed", "err", err)
	}
	// mock provider 自持（回调基址经闭包动态解析——测试可先起 httptest
	// 服再改 a.cfg.CallbackSelfURL，与装配顺序解耦）。
	a.provider = newMockProvider(func() string { return a.cfg.CallbackSelfURL },
		cfg.PayCallbackSecret, cfg.CallbackDelay, log)

	gw := newGatewayClient(cfg.GatewayBase)
	a.engine.Use(recoverShape(log), errShape(log))

	// 健康口：同端口 /healthz（沿用各服务模式；R7 compose 用 HTTP 探活）。
	a.engine.GET("/healthz", func(c *gin.Context) {
		c.String(http.StatusOK, "ok\n")
	})

	api := a.engine.Group("/api/v1")

	// —— 认证（§2，无需鉴权；IP 维度限流为 billing 槽位之外的第二层）——
	ipRL := rateLimitMiddleware(rdb, "ip", cfg.RateLimitPerMin, ipRateLimitKey, log)
	api.POST("/auth/register", ipRL, proxyRegister(gw))
	api.POST("/auth/login", ipRL, proxyLogin(gw))

	// —— 支付回调（§5，公开；信任边界=验签）——
	api.POST("/payments/callback", handlePaymentCallback(orders, cfg.PayCallbackSecret))

	// —— 鉴权路由组（§0.1：除上三端点外全部需要会话）——
	authed := api.Group("")
	authed.Use(authMiddleware(log, gw, a.sessions))
	authed.Use(rateLimitMiddleware(rdb, "tenant", cfg.RateLimitPerMin, tenantRateLimitKey, log))

	// logout 在鉴权组内（撤销当前会话；同时逐出本地会话缓存使登出即时生效）。
	authed.POST("/auth/logout", handleLogout(gw, a.sessions))

	// 商品（§3）
	authed.GET("/categories", handleCategories(goods))
	authed.GET("/goods", handleListGoods(goods))
	authed.GET("/goods/:id", handleGoodsDetail(goods))

	// 购物车（§4）
	authed.GET("/cart", handleGetCart(orders))
	authed.POST("/cart/items", handleAddCart(orders))
	authed.PATCH("/cart/items/:id", handlePatchCartQty(orders))
	authed.DELETE("/cart/items/:id", handleRemoveCart(orders))

	// 订单与支付（§5）
	authed.POST("/orders", handleCreateOrder(orders))
	authed.POST("/orders/:no/pay", handlePay(orders, a.provider))
	authed.GET("/orders", handleListOrders(orders))
	authed.GET("/orders/:no", handleGetOrder(orders))
	authed.POST("/orders/:no/cancel", handleCancelOrder(orders))
	authed.POST("/orders/:no/refund", handleRefundOrder(orders))

	// 我的信息（§7）：余额 + 最近 10 条流水；subscription R2.5 前恒 null。
	authed.GET("/me/overview", handleMeOverview(billing, topups))

	// 未注册路径（含 §6 秒杀 R4——后续轮交付）：
	// 保持统一错误形状（gin 默认 404 空体会破坏契约 §0.2）。
	a.engine.NoRoute(func(c *gin.Context) {
		// 契约错误表无"路由不存在"码；取参数校验码 + 404（前端不应打到此处）。
		respondErr(c, http.StatusNotFound, mallerr.CodeInvalidParam, false, "接口不存在")
	})
	return a
}

// ---------------------------------------------------------------------------
// 共享小工具（分页写出 / 参数解析 / 随机数）

// writePage 游标分页响应（契约 §0.3）：next_cursor 空 → null 即到底。
func writePage(c *gin.Context, items any, nextCursor string, hasMore bool) {
	var next any
	if nextCursor != "" {
		next = nextCursor
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "next_cursor": next, "has_more": hasMore})
}

// parseUintStr 正整数字符串解析。
func parseUintStr(s string) (uint64, error) {
	return strconv.ParseUint(s, 10, 64)
}

// jsonDecode 宽松解码（上游/下游 JSON 响应容错）。
func jsonDecode(r io.Reader, into any) error {
	return json.NewDecoder(r).Decode(into)
}
