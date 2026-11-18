// mall-bff 服务入口：商城对外的唯一 HTTP 入口（REST :18090，R6 将在此端口
// 挂 go:embed 前端，本任务仅 API）。健康口沿用服务模式：/healthz 同端口。
//
// 环境变量（全部可缺省走 dev 默认；凭据类必填不落码）：
//
//	BFF_ADDR            默认 :18090
//	GOODS_ADDR          默认 127.0.0.1:9111
//	ORDER_ADDR          默认 127.0.0.1:9112
//	GATEWAY_ADDR        默认 http://127.0.0.1:18080（会话链路 /auth/*，D14）
//	REDIS_ADDR          默认 127.0.0.1:6381（dev Redis 容器映射口）
//	PAY_CALLBACK_SECRET 必填——/payments/callback 验签与 mock provider 签名
//	                    共用的 HMAC 密钥（openssl rand -hex 32；.env.example 有模板）
//	PAY_CALLBACK_URL    默认 http://127.0.0.1:18090（mock 回调回打自身基址；
//	                    反代/容器部署时置对外可达地址）
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	secret := os.Getenv("PAY_CALLBACK_SECRET")
	if secret == "" {
		log.Fatal("PAY_CALLBACK_SECRET 未设置（回调验签与 mock 签名共用密钥，形如 openssl rand -hex 32 输出）")
	}

	cfg := Config{
		Addr:              envOr("BFF_ADDR", ":18090"),
		GatewayBase:       envOr("GATEWAY_ADDR", "http://127.0.0.1:18080"),
		CallbackSelfURL:   envOr("PAY_CALLBACK_URL", "http://127.0.0.1:18090"),
		PayCallbackSecret: []byte(secret),
		RateLimitPerMin:   envIntOr("RATE_LIMIT_PER_MIN", 120),
		AuthCacheTTL:      30 * time.Second,
		CallbackDelay:     200 * time.Millisecond,
	}

	goods, closeGoods, err := dialGoods(envOr("GOODS_ADDR", "127.0.0.1:9111"))
	if err != nil {
		log.Fatalf("连接 mall-goods 失败: %v", err)
	}
	orders, closeOrder, err := dialOrder(envOr("ORDER_ADDR", "127.0.0.1:9112"))
	if err != nil {
		log.Fatalf("连接 mall-order 失败: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{
		Addr: envOr("REDIS_ADDR", "127.0.0.1:6381"),
	})
	// 连接可用性早失败：限流层起不来只影响限流（fail-open），起服时探一次提示。
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 2*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		log.Printf("警告：Redis 不可达（%v）——限流 fail-open 运行", err)
	}
	cancelPing()

	app := NewApp(cfg, goods, orders, rdb)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	lnErr := make(chan error, 1)
	go func() { lnErr <- srv.ListenAndServe() }()
	log.Printf("mall-bff 起服务：%s（/api/v1 + /healthz）；gateway=%s goods=%s order=%s redis=%s",
		cfg.Addr, cfg.GatewayBase, envOr("GOODS_ADDR", "127.0.0.1:9111"), envOr("ORDER_ADDR", "127.0.0.1:9112"), rdb.Options().Addr)

	// 优雅退出：SIGINT/SIGTERM → 停 HTTP → 关下游连接/Redis。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		log.Printf("收到退出信号，开始优雅停机")
	case err := <-lnErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP serve 退出: %v", err)
		}
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdownCtx)
	_ = app.Close()
	closeGoods()
	closeOrder()
	_ = rdb.Close()
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
	}
	return def
}
