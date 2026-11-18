// mall-order 服务入口：gRPC（mallv1.OrderService + 标准健康检查）监听
// ORDER_GRPC_ADDR（默认 :9112）；小 HTTP 监听 ORDER_HTTP_ADDR（默认 :9120）
// 只挂 /healthz（compose 健康检查用）。DSN 必须经 MARKET_MYSQL_DSN 注入
// （凭据不硬编码、不落代码仓）。支付域 worker（履约 10s / 超时扫描 60s）
// 为进程内 goroutine，随退出信号优雅停止。billing 控制面客户端（credit 行
// 履约 / 充值单冲退）从 BILLING_ADDR/CONTROL_PLANE_TOKEN 拨号注入（T4
// compose 注入；本地起服先 export 同名 env——值取 deploy/compose/.env），
// 缺 token 起服即失败（fail-closed，与 billing 侧空令牌全拒对齐）。
package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	grpcAddr := envOr("ORDER_GRPC_ADDR", ":9112")
	httpAddr := envOr("ORDER_HTTP_ADDR", ":9120")
	dsn := os.Getenv("MARKET_MYSQL_DSN")
	if dsn == "" {
		log.Fatal("MARKET_MYSQL_DSN 未设置（形如 root:***@tcp(host:3307)/market?parseTime=true）")
	}

	store, err := OpenStore(dsn)
	if err != nil {
		log.Fatalf("打开 market 库失败: %v", err)
	}
	defer store.Close() //nolint:errcheck // 退出路径，连接池释放失败不影响结果

	// billing 控制面客户端（R2-T3）：credit 行履约 Credit / fulfilled 冲退 Refund。
	// 空令牌/坏地址在起服时即暴露（fail-closed：不把无鉴权客户端装成可用）。
	billingClient, closeBilling, err := dialBillingFromEnv()
	if err != nil {
		log.Fatalf("billing client 拨号失败: %v", err)
	}
	defer closeBilling() //nolint:errcheck // 退出路径，连接释放失败不影响结果
	store.billing = billingClient

	// gRPC：下单域服务 + 标准 grpc_health_v1（探活语义，compose 用 HTTP 探活，
	// gRPC 健康检查供服务网格/运维工具使用）。
	gs := grpc.NewServer()
	mallv1.RegisterOrderServiceServer(gs, newOrderServer(store))
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(gs, healthSrv)
	// gRPC reflection：供 grpcurl 等开发工具按契约自省（生产网关侧可关）。
	reflection.Register(gs)

	// HTTP：只挂 /healthz。
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	httpLn, err := net.Listen("tcp", httpAddr)
	if err != nil {
		log.Fatalf("HTTP 监听 %s 失败: %v", httpAddr, err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(httpLn); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP serve 退出: %v", err)
		}
	}()

	grpcLn, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("gRPC 监听 %s 失败: %v", grpcAddr, err)
	}
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	log.Printf("mall-order 起服务：gRPC %s，HTTP %s（/healthz）", grpcAddr, httpAddr)

	// 支付域 worker：履约 outbox（10s 一轮）+ 超时关单扫描（60s 一轮）。
	// 退出信号 → cancelWorker + Wait，保证在途一轮跑完再停（worker.go）。
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	var workerWG sync.WaitGroup
	workerWG.Add(2)
	go func() {
		defer workerWG.Done()
		RunFulfillmentLoop(workerCtx, store)
	}()
	go func() {
		defer workerWG.Done()
		RunTimeoutLoop(workerCtx, store)
	}()

	// 优雅退出：SIGINT/SIGTERM → 摘健康状态 → 停 worker → GracefulStop。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("收到退出信号，开始优雅停机")
		healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
		cancelWorker()
		workerWG.Wait()
		gs.GracefulStop()
	}()

	if err := gs.Serve(grpcLn); err != nil {
		log.Fatalf("gRPC serve 退出: %v", err)
	}
	_ = httpSrv.Shutdown(context.Background())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
