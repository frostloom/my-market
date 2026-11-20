// mall-order credit 履约测试（R2-T3）：worker 对 type=credit 的 pending 履约行
// 经 billing.Credit 入账（ref_type="mall"、ref_id=order_no:line_no、金额=face+bonus）。
//
// 两组：
//  1. 桩测试（TestWorker_CreditLineFulsillsViaBilling 系列）：注入 BillingClient
//     函数桩，断言 ref 组装 / 金额 / 成功后行 done 与订单 fulfilled，以及
//     created=false 重放（no-op）也按履约成功收口；
//  2. 真 billing 集成（TestWorker_CreditCrashReplay_ExactlyOneTopup）：spawn
//     services/billing/cmd/billing 真实子进程（dev MySQL ai_gateway + dev Redis 6381，
//     X-Control-Token 鉴权），跑两轮 RunFulfillmentOnce 模拟重启重放，
//     断言 billing topups 该 ref 恰 1 条（uk_ref 幂等提交点）、第二轮
//     CreditReply.created=false 仍收敛、订单 fulfilled。
//
// mall 侧 import 边界（R2.7 白名单门禁 pkg/importguard）：mall 对 AI 域只允许
// import billing proto 契约面，因此"真 billing"以子进程方式拉起（越树 import
// services/billing/** 被门禁拒绝，服务 internal 另被 Go 编译器拒绝）。
// 隔离与还原：mall 行按 cleanupTenant 还原；billing 侧每测试专属租户
// （显式高段 id，与 mall 测试租户段约定一致），t.Cleanup 删 topups/tenants +
// Redis 余额/锁栅栏键。禁止并发跑基础设施套件（T1 警示）。
package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	billingv1 "github.com/frostloom/my-market/pkg/proto/billing/v1"
)

// ---------------------------------------------------------------------------
// BillingClient 桩（纯内存，不连网）

// stubBilling BillingClient 桩：捕获 Credit/Refund/ActivateSubscription/
// CancelSubscription 请求，按预设序列回放应答。
type stubBilling struct {
	mu sync.Mutex
	// credits/refunds 捕获的入站请求（调用序）。
	credits []*billingv1.CreditRequest
	refunds []*billingv1.RefundRequest
	// activates/cancels 捕获的订阅开通/退订请求（调用序，R2.5-T2）。
	activates []*billingv1.ActivateSubscriptionRequest
	cancels   []*billingv1.CancelSubscriptionRequest
	// nextCreditReplies 依次回放的 Credit 应答；耗尽后缺省 created=true。
	nextCreditReplies []*billingv1.CreditReply
	// nextActivateReplies 依次回放的 ActivateSubscription 应答；耗尽后缺省
	// activated=true（首次开通）。
	nextActivateReplies []*billingv1.ActivateSubscriptionReply
	// creditErr / activateErr 非空时对应方法每次返回该错误（故障注入，
	// 走 worker 退避）。
	creditErr   error
	activateErr error
	// onCredit / onActivate 对应调用时的副作用钩子（竞态注入：模拟并发退款
	// 在 billing 提交后作废任务行等现场）；在锁外调用需自行保证幂等。
	onCredit   func(*billingv1.CreditRequest)
	onActivate func(*billingv1.ActivateSubscriptionRequest)
}

func (b *stubBilling) Credit(ctx context.Context, req *billingv1.CreditRequest, _ ...grpc.CallOption) (*billingv1.CreditReply, error) {
	b.mu.Lock()
	b.credits = append(b.credits, req)
	creditErr, hook := b.creditErr, b.onCredit
	b.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	if creditErr != nil {
		return nil, creditErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.nextCreditReplies) > 0 {
		rep := b.nextCreditReplies[0]
		b.nextCreditReplies = b.nextCreditReplies[1:]
		return rep, nil
	}
	return &billingv1.CreditReply{BalanceCent: req.GetAmountCent(), Created: true}, nil
}

func (b *stubBilling) Refund(_ context.Context, req *billingv1.RefundRequest, _ ...grpc.CallOption) (*billingv1.RefundReply, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refunds = append(b.refunds, req)
	return &billingv1.RefundReply{RefundedCent: req.GetAmountCent()}, nil
}

func (b *stubBilling) ActivateSubscription(ctx context.Context, req *billingv1.ActivateSubscriptionRequest, _ ...grpc.CallOption) (*billingv1.ActivateSubscriptionReply, error) {
	b.mu.Lock()
	b.activates = append(b.activates, req)
	activateErr, hook := b.activateErr, b.onActivate
	b.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	if activateErr != nil {
		return nil, activateErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.nextActivateReplies) > 0 {
		rep := b.nextActivateReplies[0]
		b.nextActivateReplies = b.nextActivateReplies[1:]
		return rep, nil
	}
	return &billingv1.ActivateSubscriptionReply{Activated: true, IsRenewal: false,
		Subscription: &billingv1.SubscriptionSummary{PlanId: req.GetPlanId(), Status: "active"}}, nil
}

func (b *stubBilling) CancelSubscription(_ context.Context, req *billingv1.CancelSubscriptionRequest, _ ...grpc.CallOption) (*billingv1.CancelSubscriptionReply, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancels = append(b.cancels, req)
	return &billingv1.CancelSubscriptionReply{Cancelled: true}, nil
}

func (b *stubBilling) creditCalls() []*billingv1.CreditRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*billingv1.CreditRequest(nil), b.credits...)
}

func (b *stubBilling) refundCalls() []*billingv1.RefundRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*billingv1.RefundRequest(nil), b.refunds...)
}

func (b *stubBilling) activateCalls() []*billingv1.ActivateSubscriptionRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*billingv1.ActivateSubscriptionRequest(nil), b.activates...)
}

func (b *stubBilling) cancelCalls() []*billingv1.CancelSubscriptionRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*billingv1.CancelSubscriptionRequest(nil), b.cancels...)
}

// recordingBilling 真 billing client 的记录包装：透传每次调用并捕获
// CreditReply / ActivateSubscriptionReply（断言重放轮 created/activated=false 用）。
type recordingBilling struct {
	inner BillingClient
	mu    sync.Mutex
	// replies / activateReplies 捕获的成功应答（调用序）。
	replies         []*billingv1.CreditReply
	activateReplies []*billingv1.ActivateSubscriptionReply
}

func (r *recordingBilling) Credit(ctx context.Context, in *billingv1.CreditRequest, opts ...grpc.CallOption) (*billingv1.CreditReply, error) {
	rep, err := r.inner.Credit(ctx, in, opts...)
	if err == nil {
		r.mu.Lock()
		r.replies = append(r.replies, rep)
		r.mu.Unlock()
	}
	return rep, err
}

func (r *recordingBilling) Refund(ctx context.Context, in *billingv1.RefundRequest, opts ...grpc.CallOption) (*billingv1.RefundReply, error) {
	return r.inner.Refund(ctx, in, opts...)
}

func (r *recordingBilling) ActivateSubscription(ctx context.Context, in *billingv1.ActivateSubscriptionRequest, opts ...grpc.CallOption) (*billingv1.ActivateSubscriptionReply, error) {
	rep, err := r.inner.ActivateSubscription(ctx, in, opts...)
	if err == nil {
		r.mu.Lock()
		r.activateReplies = append(r.activateReplies, rep)
		r.mu.Unlock()
	}
	return rep, err
}

func (r *recordingBilling) CancelSubscription(ctx context.Context, in *billingv1.CancelSubscriptionRequest, opts ...grpc.CallOption) (*billingv1.CancelSubscriptionReply, error) {
	return r.inner.CancelSubscription(ctx, in, opts...)
}

func (r *recordingBilling) creditReplies() []*billingv1.CreditReply {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*billingv1.CreditReply(nil), r.replies...)
}

func (r *recordingBilling) activateReplyLog() []*billingv1.ActivateSubscriptionReply {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*billingv1.ActivateSubscriptionReply(nil), r.activateReplies...)
}

// ---------------------------------------------------------------------------
// 1. TestWorker_CreditLineFulfillsViaBilling（桩）：credit 行经 billing.Credit
// 履约 → done + 订单 fulfilled；ref 组装（ref_type=mall / ref_id=order:line /
// 金额=face+bonus / tenant 取自订单）；created=false 重放按 no-op 成功收口。
func TestWorker_CreditLineFulfillsViaBilling(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "cred-stub-1")
	if err != nil {
		t.Fatalf("CreateRechargeOrder: %v", err)
	}
	if len(ord.Items) != 1 || ord.Items[0].Type != typeCredit {
		t.Fatalf("recharge 单应含恰 1 条 credit 行，got %+v", ord.Items)
	}
	wantAmount := ord.Items[0].FaceValueCent + ord.Items[0].BonusCent
	if wantAmount <= 0 {
		t.Fatalf("种子 202 面值+赠送异常: %d", wantAmount)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-cred-stub", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	tasks := taskRowsOf(t, s, ord.OrderNo)
	if len(tasks) != 1 || tasks[0].TaskType != typeCredit || tasks[0].Status != taskStatusPending {
		t.Fatalf("待履约任务行异常: %+v（want 1 行 type=2 pending）", tasks)
	}

	stub := &stubBilling{}
	s.billing = stub

	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce: %v", err)
	}
	if done != 1 {
		t.Fatalf("done = %d，want 1（credit 行入账后标 done）", done)
	}

	// ref 组装断言：ref_type="mall"、ref_id=order_no:line_no、金额=face+bonus、
	// tenant 取自订单头（mall_orders.tenant_id）。
	calls := stub.creditCalls()
	if len(calls) != 1 {
		t.Fatalf("billing.Credit 调用 %d 次，want 1", len(calls))
	}
	req := calls[0]
	if req.GetTenantId() != int64(testTenantA) {
		t.Fatalf("Credit.TenantId = %d，want %d（取自订单）", req.GetTenantId(), testTenantA)
	}
	if req.GetAmountCent() != wantAmount {
		t.Fatalf("Credit.AmountCent = %d，want %d（face+bonus 快照）", req.GetAmountCent(), wantAmount)
	}
	if req.GetRefType() != "mall" {
		t.Fatalf("Credit.RefType = %q，want \"mall\"", req.GetRefType())
	}
	if wantRef := fmt.Sprintf("%s:%d", ord.OrderNo, ord.Items[0].LineNo); req.GetRefId() != wantRef {
		t.Fatalf("Credit.RefId = %q，want %q（order_no:line_no）", req.GetRefId(), wantRef)
	}

	// 履约收口：任务行 done、订单 paid→fulfilled。
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusDone {
			t.Fatalf("任务行 line=%d status=%d，want done=2", task.LineNo, task.Status)
		}
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("订单 status = %s，want fulfilled", st)
	}

	// created=false 重放（billing 已入账、done 标记丢失的崩溃窗口）：
	// 同 ref 重入 → billing no-op → 仍按履约成功收口（行回 done、订单不变）。
	if err := s.db.Exec(
		"UPDATE fulfillment_tasks SET status = ?, retry_count = 0, next_run_at = NOW(3) WHERE order_no = ?",
		taskStatusPending, ord.OrderNo).Error; err != nil {
		t.Fatalf("打回 pending: %v", err)
	}
	stub2 := &stubBilling{nextCreditReplies: []*billingv1.CreditReply{{BalanceCent: wantAmount, Created: false}}}
	s.billing = stub2
	done, err = s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("重放 RunFulfillmentOnce: %v", err)
	}
	if done != 1 {
		t.Fatalf("重放 done = %d，want 1（created=false no-op 仍算履约成功）", done)
	}
	replay := stub2.creditCalls()
	if len(replay) != 1 || replay[0].GetRefId() != req.GetRefId() {
		t.Fatalf("重放 Credit 请求异常: %+v（want 同 ref 重入）", replay)
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusDone {
			t.Fatalf("重放后任务行 status=%d，want done=2", task.Status)
		}
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("重放后订单 status = %s，want 仍 fulfilled", st)
	}
}

// ---------------------------------------------------------------------------
// 1b. TestWorker_CreditFailureBackoffs：billing.Credit 失败 → 走既有退避
// （不标 done、不推进订单），last_error 留痕。
func TestWorker_CreditFailureBackoffs(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "cred-fail-1")
	if err != nil {
		t.Fatalf("CreateRechargeOrder: %v", err)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-cred-fail", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	id := taskID(t, s, ord.OrderNo)

	stub := &stubBilling{creditErr: status.Error(codes.Unavailable, "billing down（注入）")}
	s.billing = stub

	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce: %v", err)
	}
	if done != 0 {
		t.Fatalf("done = %d，want 0（credit 失败不得标 done）", done)
	}
	if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "paid" {
		t.Fatalf("订单 status = %s，want 仍 paid（不推进）", st)
	}
	statusV, retry := taskStatusRetry(t, s, id)
	if statusV != taskStatusPending || retry != 1 {
		t.Fatalf("任务行 = (status=%d, retry=%d)，want (pending=1, 1)（首败退避）", statusV, retry)
	}
	if d := taskDelaySeconds(t, s, id); d < 0 || d > 4 {
		t.Fatalf("退避延迟 = %ds，want 约 2s（2^n 序列起步）", d)
	}
}

// ---------------------------------------------------------------------------
// 真 billing 集成环境：spawn services/billing/cmd/billing 子进程（dev MySQL ai_gateway
// + dev Redis 6381，X-Control-Token 鉴权；BILLING_LEDGER_BATCH_SECONDS=0 保证
// Credit/Refund 同步落 MySQL，断言立即可见）。R2.7 起单模块：服务 internal 包
// 仍不可跨树 import（Go internal 规则），故走子进程 + 直连 ai_gateway 库做播种/断言/清理。

const (
	billingTestRedisAddr = "127.0.0.1:6381"
	billingTestMySQLAddr = "127.0.0.1:3307"
	// billingTestTenantBase 真 billing 集成租户显式 id 段（与 mall 测试租户
	// 段约定一致：远离真实自增段；mall 单与 billing 租户必须同 id 才能闭环）。
	billingTestTenantBase = uint64(912000)
)

// billingTestEnv 真 billing 子进程 + ai_gateway 库/Redis 直连句柄。
type billingTestEnv struct {
	client BillingClient // 真 gRPC client（带 token；注入 s.billing 用）
	grpc   billingv1.BillingServiceClient
	aidb   *gorm.DB
	rdb    *goredis.Client
	tenant uint64 // seedTenant 后有效
	runID  string
}

// envFileValue 从 deploy/compose/.env 读键值（凭据不硬编码、不打日志）。
func envFileValue(t *testing.T, key string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", ".env"))
	if err != nil {
		t.Fatalf("读 deploy/compose/.env: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if v, ok := strings.CutPrefix(line, key+"="); ok && v != "" {
			return v
		}
	}
	t.Fatalf("deploy/compose/.env 缺 %s", key)
	return ""
}

// freePort 临时占用 127.0.0.1:0 取空闲端口（释放后子进程绑定，竞态窗口可忽略）。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen :0: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// tailStderr 取子进程 stderr 尾部（诊断用；不含凭据——token 只经 env 传递）。
func tailStderr(buf *bytes.Buffer) string {
	s := buf.String()
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s
}

// buildBillingBinary 构建 services/billing/cmd/billing（go build 缓存热身后 <1s）。
func buildBillingBinary(t *testing.T) string {
	t.Helper()
	billingDir, err := filepath.Abs(filepath.Join("..", "..", "services", "billing"))
	if err != nil {
		t.Fatalf("resolve billing dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(billingDir, "cmd", "billing")); err != nil {
		t.Skipf("billing 服务树不在位（%s），跳过真 billing 集成", billingDir)
	}
	bin := filepath.Join(t.TempDir(), "billing")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/billing")
	cmd.Dir = billingDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 billing 失败: %v\n%s", err, out)
	}
	return bin
}

// startRealBilling 拉起真 billing 子进程并等待就绪（GetBalance 探活）。
func startRealBilling(t *testing.T) *billingTestEnv {
	t.Helper()
	ctx := context.Background()

	rdb := goredis.NewClient(&goredis.Options{Addr: billingTestRedisAddr, DB: 0})
	closed := false
	t.Cleanup(func() {
		if !closed {
			closed = true
			_ = rdb.Close()
		}
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("dev redis not available at %s: %v", billingTestRedisAddr, err)
	}

	password := envFileValue(t, "MYSQL_ROOT_PASSWORD")
	token := envFileValue(t, "CONTROL_PLANE_TOKEN")
	dsn := fmt.Sprintf("root:%s@tcp(%s)/ai_gateway?charset=utf8mb4&parseTime=True&loc=Local",
		password, billingTestMySQLAddr)
	aidb, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open dev ai_gateway db: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := aidb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	bin := buildBillingBinary(t)
	grpcPort, adminPort := freePort(t), freePort(t)
	var stderr bytes.Buffer
	cmd := exec.Command(bin)
	// 账本批量关闭（Credit/Refund 同步落 MySQL，断言立即可见）；Kafka 显式关空；
	// token 只经 env 传给子进程，不落日志。本地 dev 库/Redis 直连（T4 容器重建另做）。
	cmd.Env = append(os.Environ(),
		"MYSQL_DSN="+dsn,
		"REDIS_ADDR="+billingTestRedisAddr,
		"REDIS_PASSWORD=",
		"CONTROL_PLANE_TOKEN="+token,
		"BILLING_PORT="+strconv.Itoa(grpcPort),
		"BILLING_ADMIN_PORT="+strconv.Itoa(adminPort),
		"BILLING_LEDGER_BATCH_SECONDS=0",
		"BILLING_AUTH_CACHE_TTL_SECONDS=0",
		"BILLING_REQUIRE_RESERVE_V2=0",
		"BILLING_LEDGER_VIA_KAFKA=0",
		"KAFKA_BROKERS=",
		"SUB_RENEW_INTERVAL_SEC=3600",
		"LOG_LEVEL=warn",
	)
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 billing 子进程: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	addr := fmt.Sprintf("127.0.0.1:%d", grpcPort)
	raw, closeConn, err := DialBilling(addr, token)
	if err != nil {
		t.Fatalf("DialBilling(%s): %v\nbilling stderr 尾部:\n%s", addr, err, tailStderr(&stderr))
	}
	t.Cleanup(closeConn)
	svc, ok := raw.(billingv1.BillingServiceClient)
	if !ok {
		t.Fatalf("DialBilling 返回值不是 billingv1 生成 client（类型 %T）", raw)
	}

	// 就绪探测：最多 20s；Unavailable/Deadline 视为未起好，其余 gRPC 状态
	// （含业务码）说明鉴权与路由已通。
	deadline := time.Now().Add(20 * time.Second)
	for {
		var last error
		_, last = svc.GetBalance(ctx, &billingv1.GetBalanceRequest{TenantId: 1})
		if last == nil || (status.Code(last) != codes.Unavailable && status.Code(last) != codes.DeadlineExceeded) {
			break
		}
		select {
		case e := <-exited:
			t.Fatalf("billing 子进程提前退出: %v\nstderr 尾部:\n%s", e, tailStderr(&stderr))
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("billing 子进程未就绪（%s）\nstderr 尾部:\n%s", addr, tailStderr(&stderr))
		}
	}
	return &billingTestEnv{
		client: raw,
		grpc:   svc,
		aidb:   aidb,
		rdb:    rdb,
		runID:  fmt.Sprint(time.Now().UnixNano()),
	}
}

// seedTenant 播种专属 billing 租户（显式高段 id = mall 单租户 id，闭环两侧同一
// 身份；余额三处一致：initial_quota / ledger_balance 列 / Redis 投影）。
// t.Cleanup 删 topups + tenants + Redis 余额/锁栅栏键（栅栏无 TTL，测试负责清）。
func (e *billingTestEnv) seedTenant(t *testing.T, initialCent int64) {
	t.Helper()
	ctx := context.Background()
	e.tenant = billingTestTenantBase + uint64(time.Now().UnixNano()%300_000)
	if err := e.aidb.Exec(
		"INSERT INTO tenants (id, name, initial_quota, ledger_balance, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 0, NOW(3), NOW(3))",
		e.tenant, fmt.Sprintf("r2t3-%s", e.runID), initialCent, initialCent).Error; err != nil {
		t.Fatalf("seed billing tenant: %v", err)
	}
	if err := e.rdb.Set(ctx, fmt.Sprintf("billing:balance:%d", e.tenant), initialCent, 0).Err(); err != nil {
		t.Fatalf("seed redis balance: %v", err)
	}
	t.Cleanup(func() {
		if err := e.aidb.Exec("DELETE FROM topups WHERE tenant_id = ?", e.tenant).Error; err != nil {
			t.Errorf("cleanup topups tenant=%d: %v", e.tenant, err)
		}
		if err := e.aidb.Exec("DELETE FROM tenants WHERE id = ?", e.tenant).Error; err != nil {
			t.Errorf("cleanup tenant=%d: %v", e.tenant, err)
		}
		if err := e.rdb.Del(ctx,
			fmt.Sprintf("billing:balance:%d", e.tenant),
			fmt.Sprintf("billing:cash-owner:%d", e.tenant)).Err(); err != nil {
			t.Errorf("cleanup redis keys tenant=%d: %v", e.tenant, err)
		}
	})
}

// e.topupCount / e.topupAmount / e.balances 为断言辅助（t 由参数传入）。

func (e *billingTestEnv) topupCount(t *testing.T, refType, refID string) int64 {
	t.Helper()
	var n int64
	if err := e.aidb.Raw(
		"SELECT COUNT(*) FROM topups WHERE tenant_id = ? AND ref_type = ? AND ref_id = ?",
		e.tenant, refType, refID).Scan(&n).Error; err != nil {
		t.Fatalf("count topups: %v", err)
	}
	return n
}

// topupAmount 读指定 ref 首行 (amount, requested_cent)；requested NULL → 0。
func (e *billingTestEnv) topupAmount(t *testing.T, refType, refID string) (int64, int64) {
	t.Helper()
	var row struct {
		Amount        int64  `gorm:"column:amount"`
		RequestedCent *int64 `gorm:"column:requested_cent"`
	}
	if err := e.aidb.Raw(
		"SELECT amount, requested_cent FROM topups WHERE tenant_id = ? AND ref_type = ? AND ref_id = ? ORDER BY id LIMIT 1",
		e.tenant, refType, refID).Scan(&row).Error; err != nil {
		t.Fatalf("read topup: %v", err)
	}
	var requested int64
	if row.RequestedCent != nil {
		requested = *row.RequestedCent
	}
	return row.Amount, requested
}

// balances 断言余额三处一致（账本公式 = initial + Σtopups；本套件无 bills）。
func (e *billingTestEnv) balances(t *testing.T) (ledger int64, column int64, redisV int64) {
	t.Helper()
	ctx := context.Background()
	if err := e.aidb.Raw(
		"SELECT t.initial_quota + COALESCE((SELECT SUM(amount) FROM topups WHERE tenant_id = t.id), 0) "+
			"FROM tenants t WHERE t.id = ?", e.tenant).Scan(&ledger).Error; err != nil {
		t.Fatalf("ledger balance: %v", err)
	}
	if err := e.aidb.Raw(
		"SELECT ledger_balance FROM tenants WHERE id = ?", e.tenant).Scan(&column).Error; err != nil {
		t.Fatalf("column balance: %v", err)
	}
	v, err := e.rdb.Get(ctx, fmt.Sprintf("billing:balance:%d", e.tenant)).Int64()
	if err != nil {
		t.Fatalf("redis balance: %v", err)
	}
	return ledger, column, v
}

// ---------------------------------------------------------------------------
// 1c. TestWorker_CreditVoidCompensation（Fix Round 1，窗口 R1 两层防护）：
//   - 层一：执行前复检——任务行已被 voided（并发退款先行）→ 静默跳过，
//     billing 零触达（不 Credit 也不补偿，钱没入过账无需冲销）；
//   - 层二：复检通过、billing.Credit 已提交、done 更新 0 行（复检与 done 之间
//     被并发退款 voided，桩钩子在该窗口注入作废）→ 立即以 mall-void ref 冲销
//     补偿（金额=face+bonus、tenant 正确）。
func TestWorker_CreditVoidCompensation(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	t.Run("recheck_skips_voided_before_credit", func(t *testing.T) {
		ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "cred-void-a")
		if err != nil {
			t.Fatalf("CreateRechargeOrder: %v", err)
		}
		if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
			TradeNo: "T-cred-void-a", Paid: true, AmountCent: ord.AmountCent}); err != nil {
			t.Fatalf("MarkPaidTx: %v", err)
		}
		// 造竞态结果：RefundPaid 已作废任务行并翻转订单（真实事务同款终态）。
		if err := s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
			taskStatusVoided, ord.OrderNo).Error; err != nil {
			t.Fatalf("造 voided 行: %v", err)
		}
		if err := s.db.Exec("UPDATE mall_orders SET status = ? WHERE order_no = ?",
			orderStatusRefunded, ord.OrderNo).Error; err != nil {
			t.Fatalf("造 refunded 单: %v", err)
		}

		stub := &stubBilling{}
		s.billing = stub
		done, err := s.RunFulfillmentOnce(ctx)
		if err != nil {
			t.Fatalf("RunFulfillmentOnce: %v", err)
		}
		if done != 0 {
			t.Fatalf("done = %d，want 0（voided 行静默跳过）", done)
		}
		if n := len(stub.creditCalls()) + len(stub.refundCalls()); n != 0 {
			t.Fatalf("billing 触达 %d 次，want 0（层一复检：未入账不补偿）", n)
		}
		for _, task := range taskRowsOf(t, s, ord.OrderNo) {
			if task.Status != taskStatusVoided {
				t.Fatalf("任务行 status=%d，want 仍 voided=4", task.Status)
			}
		}
	})

	t.Run("no_compensation_when_done_by_other_executor", func(t *testing.T) {
		// R2.5-T3 必改 2：done 更新 0 行在多执行器下歧义——0 行也可能是另一执行器
		// 已把行标 done（幂等条件更新的输家）。补偿前重读任务行：仅 voided 才冲销；
		// done（另一执行器已收口，余额归属正确）→ info 跳过，绝不双重冲销。
		ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "cred-void-c")
		if err != nil {
			t.Fatalf("CreateRechargeOrder: %v", err)
		}
		if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
			TradeNo: "T-cred-void-c", Paid: true, AmountCent: ord.AmountCent}); err != nil {
			t.Fatalf("MarkPaidTx: %v", err)
		}
		// 竞态注入：Credit 提交后、done 标记前，另一执行器已完成收口（行 done，
		// 订单照常 paid——promote 由该执行器负责）——本方 done 更新必 0 行。
		stub := &stubBilling{onCredit: func(*billingv1.CreditRequest) {
			_ = s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
				taskStatusDone, ord.OrderNo).Error
		}}
		s.billing = stub

		done, err := s.RunFulfillmentOnce(ctx)
		if err != nil {
			t.Fatalf("RunFulfillmentOnce: %v", err)
		}
		if done != 0 {
			t.Fatalf("done = %d，want 0（本方未收口，另一执行器已标 done）", done)
		}
		if n := len(stub.creditCalls()); n != 1 {
			t.Fatalf("Credit 调用 %d 次，want 1（uk_ref 幂等，重入无害）", n)
		}
		if n := len(stub.refundCalls()); n != 0 {
			t.Fatalf("Refund（冲销补偿）调用 %d 次，want 0（行已 done=另一执行器收口，不得冲销）", n)
		}
		for _, task := range taskRowsOf(t, s, ord.OrderNo) {
			if task.Status != taskStatusDone {
				t.Fatalf("任务行 status=%d，want 保持 done=2", task.Status)
			}
		}
	})

	t.Run("compensates_when_voided_after_credit", func(t *testing.T) {
		ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "cred-void-b")
		if err != nil {
			t.Fatalf("CreateRechargeOrder: %v", err)
		}
		wantAmount := ord.Items[0].FaceValueCent + ord.Items[0].BonusCent
		if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
			TradeNo: "T-cred-void-b", Paid: true, AmountCent: ord.AmountCent}); err != nil {
			t.Fatalf("MarkPaidTx: %v", err)
		}
		// 竞态注入：Credit 提交后、done 标记前，RefundPaid 并发完成（作废行 +
		// 翻转订单，与真实事务同款终态）——复检已通过、done 更新必 0 行。
		stub := &stubBilling{onCredit: func(*billingv1.CreditRequest) {
			_ = s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
				taskStatusVoided, ord.OrderNo).Error
			_ = s.db.Exec("UPDATE mall_orders SET status = ? WHERE order_no = ?",
				orderStatusRefunded, ord.OrderNo).Error
		}}
		s.billing = stub

		done, err := s.RunFulfillmentOnce(ctx)
		if err != nil {
			t.Fatalf("RunFulfillmentOnce: %v", err)
		}
		if done != 0 {
			t.Fatalf("done = %d，want 0（行被 voided，未完成履约收口）", done)
		}
		// 入账恰 1 次（billing 已提交），冲销补偿恰 1 次：mall-void ref +
		// 金额=face+bonus + tenant 取自订单。
		if n := len(stub.creditCalls()); n != 1 {
			t.Fatalf("Credit 调用 %d 次，want 1", n)
		}
		refunds := stub.refundCalls()
		if len(refunds) != 1 {
			t.Fatalf("Refund（冲销补偿）调用 %d 次，want 1", len(refunds))
		}
		wantRef := fmt.Sprintf("mall-void:%s:%d", ord.OrderNo, ord.Items[0].LineNo)
		rep := refunds[0]
		if rep.GetRef() != wantRef || rep.GetAmountCent() != wantAmount || rep.GetTenantId() != int64(testTenantA) {
			t.Fatalf("冲销请求 = (ref=%s, amount=%d, tenant=%d)，want (%s, %d, %d)",
				rep.GetRef(), rep.GetAmountCent(), rep.GetTenantId(), wantRef, wantAmount, testTenantA)
		}
		// 任务行保持 voided（不可重放）；订单保持 refunded（冲销已撤回入账）。
		for _, task := range taskRowsOf(t, s, ord.OrderNo) {
			if task.Status != taskStatusVoided {
				t.Fatalf("任务行 status=%d，want 仍 voided=4（不可重放）", task.Status)
			}
		}
		if st, _ := orderStatusOf(t, s, ctx, testTenantA, ord.OrderNo); st != "refunded" {
			t.Fatalf("订单 status = %s，want refunded", st)
		}
	})
}

// TestCreditLine_RecheckSkipsNonPending（Fix Round 2）：直调 creditLine 的
// 层一复检分支——任务行已被并发退款 voided 时返回 (false, nil)、billing
// Credit/Refund 双零触达（未入账不补偿）、任务行保持 voided。
func TestCreditLine_RecheckSkipsNonPending(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	ord, err := s.CreateRechargeOrder(ctx, testTenantA, 202, "cred-recheck-1")
	if err != nil {
		t.Fatalf("CreateRechargeOrder: %v", err)
	}
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-cred-recheck", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}
	// 竞态先行结果：任务行已 voided（RefundPaid 已作废并翻转订单）。
	if err := s.db.Exec("UPDATE fulfillment_tasks SET status = ? WHERE order_no = ?",
		taskStatusVoided, ord.OrderNo).Error; err != nil {
		t.Fatalf("造 voided 行: %v", err)
	}
	var trow fulfillmentTaskRow
	if err := s.db.Where("order_no = ?", ord.OrderNo).First(&trow).Error; err != nil {
		t.Fatalf("load task row: %v", err)
	}

	stub := &stubBilling{}
	s.billing = stub
	marked, err := s.creditLine(ctx, trow)
	if err != nil {
		t.Fatalf("creditLine: %v（voided 行应静默跳过）", err)
	}
	if marked {
		t.Fatalf("marked = true，want false（行非 pending，未履约）")
	}
	if n := len(stub.creditCalls()) + len(stub.refundCalls()); n != 0 {
		t.Fatalf("billing 触达 %d 次，want 0（层一复检：零触达）", n)
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusVoided {
			t.Fatalf("任务行 status=%d，want 仍 voided=4", task.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. TestWorker_CreditCrashReplay_ExactlyOneTopup（真 billing）：paid+pending
// credit 行 → RunOnce（入账+done）→ 把任务行打回 pending（模拟"billing 已入账、
// done 未落"的断电窗口）→ 再 RunOnce（重启重放）→ billing topups 该 ref 恰 1 条
// （uk_ref 幂等提交点）、第二轮 CreditReply.created=false、订单 fulfilled。
func TestWorker_CreditCrashReplay_ExactlyOneTopup(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	env := startRealBilling(t)
	env.seedTenant(t, 0)
	tenantID := env.tenant
	cleanupTenant(t, s, tenantID)

	ord, err := s.CreateRechargeOrder(ctx, tenantID, 202, "cred-replay-"+env.runID)
	if err != nil {
		t.Fatalf("CreateRechargeOrder: %v", err)
	}
	wantAmount := ord.Items[0].FaceValueCent + ord.Items[0].BonusCent
	wantRef := fmt.Sprintf("%s:%d", ord.OrderNo, ord.Items[0].LineNo)
	if err := s.MarkPaidTx(ctx, Callback{OrderNo: ord.OrderNo, Provider: "mock",
		TradeNo: "T-cred-replay", Paid: true, AmountCent: ord.AmountCent}); err != nil {
		t.Fatalf("MarkPaidTx: %v", err)
	}

	rec := &recordingBilling{inner: env.client}
	s.billing = rec

	// 第一轮：入账 + done + 订单 fulfilled。
	done, err := s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#1: %v", err)
	}
	if done != 1 {
		t.Fatalf("第一轮 done = %d，want 1", done)
	}
	if st, _ := orderStatusOf(t, s, ctx, tenantID, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("第一轮订单 status = %s，want fulfilled", st)
	}
	if n := env.topupCount(t, "mall", wantRef); n != 1 {
		t.Fatalf("第一轮后 topups(%s) = %d 条，want 恰 1", wantRef, n)
	}
	if amount, _ := env.topupAmount(t, "mall", wantRef); amount != wantAmount {
		t.Fatalf("topup amount = %d，want %d", amount, wantAmount)
	}
	ledger, column, redisV := env.balances(t)
	if ledger != wantAmount || column != wantAmount || redisV != wantAmount {
		t.Fatalf("第一轮余额 = (ledger=%d, column=%d, redis=%d)，want 全等于 %d",
			ledger, column, redisV, wantAmount)
	}

	// 断电窗口：billing 已入账（topups 已提交）、done 标记丢失 → 任务行回 pending。
	if err := s.db.Exec(
		"UPDATE fulfillment_tasks SET status = ?, retry_count = 0, next_run_at = NOW(3) WHERE order_no = ?",
		taskStatusPending, ord.OrderNo).Error; err != nil {
		t.Fatalf("打回 pending: %v", err)
	}

	// 第二轮（重启重放）：同 ref 重入 → billing no-op → 收口，不重复入账。
	done, err = s.RunFulfillmentOnce(ctx)
	if err != nil {
		t.Fatalf("RunFulfillmentOnce#2: %v", err)
	}
	if done != 1 {
		t.Fatalf("重放 done = %d，want 1", done)
	}
	if n := env.topupCount(t, "mall", wantRef); n != 1 {
		t.Fatalf("重放后 topups(%s) = %d 条，want 恰 1（uk_ref 幂等）", wantRef, n)
	}
	replies := rec.creditReplies()
	if len(replies) != 2 {
		t.Fatalf("Credit 调用 %d 次，want 2（首轮 + 重放轮）", len(replies))
	}
	if !replies[0].GetCreated() {
		t.Fatalf("首轮 CreditReply.created = false，want true")
	}
	if replies[1].GetCreated() {
		t.Fatalf("重放轮 CreditReply.created = true，want false（同 ref no-op）")
	}
	for _, task := range taskRowsOf(t, s, ord.OrderNo) {
		if task.Status != taskStatusDone {
			t.Fatalf("重放后任务行 status=%d，want done=2", task.Status)
		}
	}
	if st, _ := orderStatusOf(t, s, ctx, tenantID, ord.OrderNo); st != "fulfilled" {
		t.Fatalf("重放后订单 status = %s，want fulfilled", st)
	}
	ledger, column, redisV = env.balances(t)
	if ledger != wantAmount || column != wantAmount || redisV != wantAmount {
		t.Fatalf("重放后余额 = (ledger=%d, column=%d, redis=%d)，want 全等于 %d（不重不漏）",
			ledger, column, redisV, wantAmount)
	}
}
