// mall-goods 服务入口：gRPC（mallv1.GoodsService + 标准健康检查）监听
// GOODS_GRPC_ADDR（默认 :9111）；小 HTTP 监听 GOODS_HTTP_ADDR（默认 :9110）
// 只挂 /healthz（compose 健康检查用）。DSN 必须经 MARKET_MYSQL_DSN 注入
// （凭据不硬编码、不落代码仓）。
package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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

	grpcAddr := envOr("GOODS_GRPC_ADDR", ":9111")
	httpAddr := envOr("GOODS_HTTP_ADDR", ":9110")
	dsn := os.Getenv("MARKET_MYSQL_DSN")
	if dsn == "" {
		log.Fatal("MARKET_MYSQL_DSN 未设置（形如 root:***@tcp(host:3306)/market?parseTime=true）")
	}

	store, err := OpenStore(dsn)
	if err != nil {
		log.Fatalf("打开 market 库失败: %v", err)
	}
	defer store.Close() //nolint:errcheck // 退出路径，连接池释放失败不影响结果

	// gRPC：业务服务 + 标准 grpc_health_v1（探活语义，T7 compose 用 wget HTTP 探活，
	// gRPC 健康检查供服务网格/运维工具使用）。
	gs := grpc.NewServer()
	mallv1.RegisterGoodsServiceServer(gs, newGoodsServer(store))
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
	log.Printf("mall-goods 起服务：gRPC %s，HTTP %s（/healthz）", grpcAddr, httpAddr)

	// 优雅退出：SIGINT/SIGTERM → 摘健康状态 → GracefulStop。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("收到退出信号，开始优雅停机")
		healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
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
