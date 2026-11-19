// ActivateSubscription/CancelSubscription RPC 集成测试（R2.5 月卡订阅 TDD）。
//
// TDD 硬要求：本文件先于实现编写（RED），实现后转 GREEN。
// 连 dev MySQL（deploy/compose，宿主 3307，ai_gateway 库）+ dev Redis（宿主 6381）。
// 隔离与还原：测试租户/套餐用专属命名（sub-rpc-test-*，ID 远离真实数据段），
// t.Cleanup 精确还原本测试写入的 subscriptions / topups(0 元激活锚) / plans /
// tenants / Redis 余额键，不触碰其他租户与种子数据，可重复跑。
//
// 幂等方案（零 schema 改动，报告已裁定）：激活锚复用 topups uk_ref——
// 同事务写一条 0 元流水（source=subscription, ref_type="mall-sub", ref_id=ref），
// 余额公式 Σtopups 不受 0 元行影响；退订不落锚（定点收敛状态迁移天然幂等）。
//
// DSN 来源同 credit_test.go：BILLING_TEST_MYSQL_DSN 优先；未设置则从
// deploy/compose/.env 读 MYSQL_ROOT_PASSWORD 拼 DSN（凭据不硬编码、不提交、不打日志）。
package server

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/frostloom/ai-gateway/internal/billing/store"
	billingv1 "github.com/frostloom/ai-gateway/pkg/proto/billing/v1"
)

// subRpcTestEnv 测试环境：真实 dev MySQL + Redis 上的 Store 与 gRPC Service，
// 外加本环境专属的两个在售定期订阅套餐（唯一命名，Cleanup 删除）。
type subRpcTestEnv struct {
	t       *testing.T
	svc     *Service
	st      *store.Store
	db      *gorm.DB
	rdb     *goredis.Client
	ctx     context.Context
	tenant  uint64
	plan    uint64 // 主套餐（续购主线）
	planAlt uint64 // 备用套餐（跨套餐续购 fail-closed 用）
	runID   string // 每次运行唯一：ref 带上它，防止跨轮次撞 uk_ref（残留行不致命）
}

func newSubRpcTestEnv(t *testing.T, initialCent int64) *subRpcTestEnv {
	t.Helper()
	ctx := context.Background()

	rdb := goredis.NewClient(&goredis.Options{Addr: testRedisAddr, DB: 0})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("dev redis not available at %s: %v", testRedisAddr, err)
	}
	db, err := gorm.Open(mysql.Open(testDSN(t)), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open dev mysql: %v（dev MySQL 未就绪？docker compose --profile infra up -d）", err)
	}
	st := store.NewStore(db, rdb)
	if err := st.AutoMigrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := NewService(st, slog.New(slog.DiscardHandler))

	e := &subRpcTestEnv{t: t, svc: svc, st: st, db: db, rdb: rdb, ctx: ctx, runID: fmt.Sprint(time.Now().UnixNano())}

	// 专属测试租户：余额三处一致（initial_quota / ledger_balance / Redis 投影）。
	tenant := store.Tenant{
		Name:          fmt.Sprintf("sub-rpc-test-%d", time.Now().UnixNano()),
		InitialQuota:  initialCent,
		LedgerBalance: initialCent,
		Status:        0,
	}
	if err := db.Create(&tenant).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	e.tenant = tenant.ID
	if err := rdb.Set(ctx, store.BalanceKey(tenant.ID), initialCent, 0).Err(); err != nil {
		t.Fatalf("seed redis balance: %v", err)
	}

	// 专属测试套餐 ×2（dev plans 表当前无种子，测试自给自足；窗口额度给模拟值）。
	for i, p := range []store.Plan{
		{Name: fmt.Sprintf("sub-rpc-test-月卡A-%d", time.Now().UnixNano()), PlanType: store.PlanTypeRecurring,
			PriceMoney: 80, ValidityDays: 30, RefreshHours: 5, WindowQuotaCent: 1_000_000,
			TierQuota: `{"轻量":2000000}`, Status: 0},
		{Name: fmt.Sprintf("sub-rpc-test-月卡B-%d", time.Now().UnixNano()), PlanType: store.PlanTypeRecurring,
			PriceMoney: 300, ValidityDays: 30, RefreshHours: 5, WindowQuotaCent: 4_000_000,
			TierQuota: `{"旗舰":800000}`, Status: 0},
	} {
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("seed plan %d: %v", i, err)
		}
		if i == 0 {
			e.plan = p.ID
		} else {
			e.planAlt = p.ID
		}
	}

	// 还原（t.Cleanup LIFO：先注册的关连接，后注册的先跑——数据还原必须在连接关闭前执行）。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = rdb.Close()
	})
	t.Cleanup(func() {
		if err := e.db.Exec("DELETE FROM subscriptions WHERE tenant_id = ?", e.tenant).Error; err != nil {
			t.Errorf("cleanup subscriptions: %v", err)
		}
		if err := e.db.Exec("DELETE FROM topups WHERE tenant_id = ?", e.tenant).Error; err != nil {
			t.Errorf("cleanup topups: %v", err)
		}
		if err := e.db.Exec("DELETE FROM plans WHERE id IN (?, ?)", e.plan, e.planAlt).Error; err != nil {
			t.Errorf("cleanup plans: %v", err)
		}
		if err := e.db.Exec("DELETE FROM tenants WHERE id = ?", e.tenant).Error; err != nil {
			t.Errorf("cleanup tenants: %v", err)
		}
		if err := e.rdb.Del(context.Background(), store.BalanceKey(e.tenant), cashFenceKey(e.tenant)).Err(); err != nil {
			t.Errorf("cleanup redis keys: %v", err)
		}
	})
	return e
}

// ref 幂等键加运行唯一后缀：uk_ref 全库唯一（不按租户隔离），上一轮残留行会让本轮撞幂等。
func (e *subRpcTestEnv) ref(base string) string { return base + ":" + e.runID }

func (e *subRpcTestEnv) ledgerBalance() int64 {
	e.t.Helper()
	bal, err := store.LedgerBalance(e.ctx, e.db, e.tenant)
	if err != nil {
		e.t.Fatalf("LedgerBalance: %v", err)
	}
	return bal
}

// subRow 返回本租户最新一条订阅记录（无则 nil）。
func (e *subRpcTestEnv) subRow() *store.Subscription {
	e.t.Helper()
	var sub store.Subscription
	err := e.db.Where("tenant_id = ?", e.tenant).Order("id DESC").First(&sub).Error
	if err == gorm.ErrRecordNotFound {
		return nil
	}
	if err != nil {
		e.t.Fatalf("load subscription: %v", err)
	}
	return &sub
}

// markerCount 本租户 mall-sub 激活锚流水条数。
func (e *subRpcTestEnv) markerCount(refID string) int64 {
	e.t.Helper()
	var n int64
	if err := e.db.Model(&store.Topup{}).
		Where("tenant_id = ? AND ref_type = ? AND ref_id = ?", e.tenant, "mall-sub", refID).
		Count(&n).Error; err != nil {
		e.t.Fatalf("count markers: %v", err)
	}
	return n
}

// within 判断 actual 落在 want±tol 秒内。
func within(t *testing.T, label string, actual, want time.Time, tol time.Duration) {
	t.Helper()
	if diff := actual.Sub(want); diff > tol || diff < -tol {
		t.Fatalf("%s = %v, want within ±%v of %v", label, actual, tol, want)
	}
}

// TestActivateSubscription_FirstActivation 首次开通：参数校验（未知/下架套餐拒绝且零副作用）
// + 正常开通（active、auto_renew=1、本期窗口=30 天、0 元锚恰好一条、余额不动）。
func TestActivateSubscription_FirstActivation(t *testing.T) {
	e := newSubRpcTestEnv(t, 10_000)

	t.Run("unknown_plan_not_found", func(t *testing.T) {
		_, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: 999_999_999, Ref: e.ref("ord_act_unknown"),
		})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("unknown plan: code=%v, want NotFound（err=%v）", status.Code(err), err)
		}
		if e.subRow() != nil {
			t.Fatalf("unknown plan must not create subscription")
		}
	})

	t.Run("disabled_plan_rejected", func(t *testing.T) {
		// 临时下架本环境主套餐，拒绝后恢复（Cleanup 不依赖此值）
		if err := e.db.Model(&store.Plan{}).Where("id = ?", e.plan).Update("status", 1).Error; err != nil {
			t.Fatalf("disable plan: %v", err)
		}
		_, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: e.ref("ord_act_disabled"),
		})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("disabled plan: code=%v, want FailedPrecondition（err=%v）", status.Code(err), err)
		}
		if e.subRow() != nil {
			t.Fatalf("disabled plan must not create subscription")
		}
		// 恢复在售（模拟月费校验通过后再走主线）
		if err := e.db.Model(&store.Plan{}).Where("id = ?", e.plan).Update("status", 0).Error; err != nil {
			t.Fatalf("re-enable plan: %v", err)
		}
	})

	t.Run("activates_current_period", func(t *testing.T) {
		ref := e.ref("ord_act_first")
		reply, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: ref,
		})
		if err != nil {
			t.Fatalf("ActivateSubscription: %v", err)
		}
		if !reply.Activated || reply.IsRenewal {
			t.Fatalf("reply={activated:%v renewal:%v}, want {true false}", reply.Activated, reply.IsRenewal)
		}
		sum := reply.Subscription
		if sum.GetPlanId() != int64(e.plan) || sum.GetStatus() != "active" || !sum.GetAutoRenew() || sum.GetCycleNum() != 1 {
			t.Fatalf("summary={plan:%d status:%q renew:%v num:%d}, want 主套餐/active/true/1",
				sum.GetPlanId(), sum.GetStatus(), sum.GetAutoRenew(), sum.GetCycleNum())
		}
		// 本期窗口 = 30 天（plan.validity_days）
		within(t, "period_end", time.Unix(sum.GetPeriodEnd(), 0), time.Now().Add(30*24*time.Hour), 2*time.Minute)
		// DB 行一致
		sub := e.subRow()
		if sub == nil || sub.Status != store.SubActive || !sub.AutoRenew || sub.CycleNum != 1 {
			t.Fatalf("sub row = %+v, want active/auto_renew/num=1", sub)
		}
		// 0 元激活锚恰好一条（source=subscription, amount=0, ref_type=mall-sub）
		if n := e.markerCount(ref); n != 1 {
			t.Fatalf("marker rows=%d, want 1", n)
		}
		var row store.Topup
		if err := e.db.Where("tenant_id = ? AND ref_type = ? AND ref_id = ?", e.tenant, "mall-sub", ref).
			First(&row).Error; err != nil {
			t.Fatalf("load marker: %v", err)
		}
		if row.Amount != 0 || row.Source != store.TopupSubscription {
			t.Fatalf("marker={amount:%d source:%s}, want {0 subscription}（0 元锚不动余额）", row.Amount, row.Source)
		}
		// 月费模拟不接真钱：余额三口径不动
		if bal := e.ledgerBalance(); bal != 10_000 {
			t.Fatalf("ledger balance=%d, want 10000（模拟月费不入账）", bal)
		}
	})
}

// TestActivateSubscription_ReplayRef_NoOp 同 ref 重放：不再变更订阅（不延期/不加锚），
// 返回当前订阅摘要；重放携带不同 plan_id 也不动（与 Credit「重放不校验载荷」同口径）。
func TestActivateSubscription_ReplayRef_NoOp(t *testing.T) {
	e := newSubRpcTestEnv(t, 10_000)
	ref := e.ref("ord_act_replay")

	first, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: ref,
	})
	if err != nil {
		t.Fatalf("first Activate: %v", err)
	}
	if !first.Activated {
		t.Fatalf("first Activated=false, want true")
	}
	end1 := time.Unix(first.Subscription.GetPeriodEnd(), 0)

	replay, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: ref,
	})
	if err != nil {
		t.Fatalf("replay Activate: %v", err)
	}
	if replay.Activated || replay.IsRenewal {
		t.Fatalf("replay={activated:%v renewal:%v}, want {false false}（uk_ref 幂等）", replay.Activated, replay.IsRenewal)
	}
	if got := time.Unix(replay.Subscription.GetPeriodEnd(), 0); !got.Equal(end1) {
		t.Fatalf("replay period_end=%v, want unchanged %v（重放不得延期）", got, end1)
	}
	if replay.Subscription.GetCycleNum() != 1 {
		t.Fatalf("replay cycle_num=%d, want 1", replay.Subscription.GetCycleNum())
	}
	if n := e.markerCount(ref); n != 1 {
		t.Fatalf("marker rows=%d, want 1（重放不得新增锚）", n)
	}
	// 同 ref 不同 plan：仍 no-op（响应摘要反映库里真值）
	replay2, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(e.tenant), PlanId: int64(e.planAlt), Ref: ref,
	})
	if err != nil {
		t.Fatalf("replay with other plan: %v", err)
	}
	if replay2.Activated || replay2.Subscription.GetPlanId() != int64(e.plan) {
		t.Fatalf("replay(other plan)={activated:%v plan:%d}, want {false 主套餐}", replay2.Activated, replay2.Subscription.GetPlanId())
	}
	if bal := e.ledgerBalance(); bal != 10_000 {
		t.Fatalf("ledger balance=%d, want 10000", bal)
	}
}

// TestActivateSubscription_Renew_ExtendsOneCycle 续一期三分支：
// 未到期续购顺延一个周期（RenewDue 单租户等价：newStart=旧 end，无缝）/ 退订后续购复活并续期 /
// 跨套餐续购 fail-closed（换套餐走 ChangePlan 口径，不静默变更）。
func TestActivateSubscription_Renew_ExtendsOneCycle(t *testing.T) {
	t.Run("extends_one_cycle_from_period_end", func(t *testing.T) {
		e := newSubRpcTestEnv(t, 10_000)
		if _, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: e.ref("ord_act_r1"),
		}); err != nil {
			t.Fatalf("first Activate: %v", err)
		}
		end1 := time.Unix(e.subRow().CycleEnd.Unix(), 0)

		reply, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: e.ref("ord_act_r2"),
		})
		if err != nil {
			t.Fatalf("renew Activate: %v", err)
		}
		if !reply.Activated || !reply.IsRenewal {
			t.Fatalf("renew={activated:%v renewal:%v}, want {true true}", reply.Activated, reply.IsRenewal)
		}
		sub := e.subRow()
		if sub.CycleNum != 2 {
			t.Fatalf("cycle_num=%d, want 2", sub.CycleNum)
		}
		// RenewDue 等价：newStart=旧 end（当期权益无缝），newEnd=newStart+30 天
		within(t, "renewed cycle_start", *sub.CycleStart, end1, 2*time.Minute)
		within(t, "renewed cycle_end", *sub.CycleEnd, end1.Add(30*24*time.Hour), 2*time.Minute)
		if !sub.AutoRenew || sub.Status != store.SubActive {
			t.Fatalf("renewed sub={status:%s auto_renew:%v}, want active/true", sub.Status, sub.AutoRenew)
		}
		if n := e.markerCount(e.ref("ord_act_r2")); n != 1 {
			t.Fatalf("renew marker rows=%d, want 1", n)
		}
		if bal := e.ledgerBalance(); bal != 10_000 {
			t.Fatalf("ledger balance=%d, want 10000", bal)
		}
	})

	t.Run("renew_after_cancel_revives", func(t *testing.T) {
		e := newSubRpcTestEnv(t, 10_000)
		if _, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: e.ref("ord_act_c1"),
		}); err != nil {
			t.Fatalf("first Activate: %v", err)
		}
		end1 := time.Unix(e.subRow().CycleEnd.Unix(), 0)
		if _, err := e.svc.CancelSubscription(e.ctx, &billingv1.CancelSubscriptionRequest{
			TenantId: int64(e.tenant), Ref: e.ref("ord_cancel_c1"),
		}); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if e.subRow().Status != store.SubCancelled {
			t.Fatalf("pre-renew status=%s, want cancelled", e.subRow().Status)
		}

		reply, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: e.ref("ord_act_c2"),
		})
		if err != nil {
			t.Fatalf("renew after cancel: %v", err)
		}
		if !reply.Activated || !reply.IsRenewal {
			t.Fatalf("renew={activated:%v renewal:%v}, want {true true}", reply.Activated, reply.IsRenewal)
		}
		sub := e.subRow()
		if sub.Status != store.SubActive || !sub.AutoRenew || sub.CycleNum != 2 {
			t.Fatalf("revived sub={status:%s renew:%v num:%d}, want active/true/2", sub.Status, sub.AutoRenew, sub.CycleNum)
		}
		within(t, "revived cycle_end", *sub.CycleEnd, end1.Add(30*24*time.Hour), 2*time.Minute)
	})

	t.Run("different_plan_fails_closed", func(t *testing.T) {
		e := newSubRpcTestEnv(t, 10_000)
		if _, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: e.ref("ord_act_d1"),
		}); err != nil {
			t.Fatalf("first Activate: %v", err)
		}
		end1 := e.subRow().CycleEnd

		_, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: int64(e.planAlt), Ref: e.ref("ord_act_d2"),
		})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("cross-plan renew: code=%v, want FailedPrecondition（err=%v）", status.Code(err), err)
		}
		sub := e.subRow()
		if sub.CycleNum != 1 || !sub.CycleEnd.Equal(*end1) || sub.PlanID != e.plan {
			t.Fatalf("sub after reject={plan:%d num:%d}, want 主套餐不变/1（零副作用）", sub.PlanID, sub.CycleNum)
		}
		if n := e.markerCount(e.ref("ord_act_d2")); n != 0 {
			t.Fatalf("rejected renew must not write marker, got %d", n)
		}
	})
}

// TestActivateSubscription_CrossTenantRef_FailsConsistently 跨租户同 ref fail-closed
// （uk_ref 全库唯一，口径与 Credit 一致）：租户 B 重放租户 A 的 ref → FailedPrecondition
// 零副作用；租户 A 自身重放仍 no-op。
func TestActivateSubscription_CrossTenantRef_FailsConsistently(t *testing.T) {
	eA := newSubRpcTestEnv(t, 10_000)
	eB := newSubRpcTestEnv(t, 10_000)

	ref := eA.ref("ord_act_cross")
	if _, err := eA.svc.ActivateSubscription(eA.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(eA.tenant), PlanId: int64(eA.plan), Ref: ref,
	}); err != nil {
		t.Fatalf("tenant A first Activate: %v", err)
	}

	_, err := eB.svc.ActivateSubscription(eB.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(eB.tenant), PlanId: int64(eB.plan), Ref: ref,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("cross-tenant ref: code=%v, want FailedPrecondition（err=%v）", status.Code(err), err)
	}
	if eB.subRow() != nil {
		t.Fatalf("tenant B must have zero subscription side effects")
	}
	if n := eB.markerCount(ref); n != 0 {
		t.Fatalf("tenant B marker rows=%d, want 0", n)
	}
	if bal := eB.ledgerBalance(); bal != 10_000 {
		t.Fatalf("tenant B ledger=%d, want 10000", bal)
	}

	// 租户 A 自身重放 → 仍 no-op（幂等不受影响）
	replay, err := eA.svc.ActivateSubscription(eA.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(eA.tenant), PlanId: int64(eA.plan), Ref: ref,
	})
	if err != nil {
		t.Fatalf("tenant A replay: %v", err)
	}
	if replay.Activated {
		t.Fatalf("tenant A replay Activated=true, want false")
	}
}

// TestCancelSubscription_StopsRenewalKeepsPeriod 退订：置 auto_renew=0、status=cancelled，
// 本期窗口（cycle_end）原样保留，余额不动（退订不退费）。
func TestCancelSubscription_StopsRenewalKeepsPeriod(t *testing.T) {
	e := newSubRpcTestEnv(t, 10_000)
	if _, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: e.ref("ord_cancel_s1"),
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	end1 := e.subRow().CycleEnd

	reply, err := e.svc.CancelSubscription(e.ctx, &billingv1.CancelSubscriptionRequest{
		TenantId: int64(e.tenant), Ref: e.ref("ord_cancel_s2"),
	})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !reply.Cancelled {
		t.Fatalf("Cancelled=false, want true")
	}
	sum := reply.Subscription
	if sum.GetStatus() != "cancelled" || sum.GetAutoRenew() {
		t.Fatalf("summary={status:%q renew:%v}, want cancelled/false", sum.GetStatus(), sum.GetAutoRenew())
	}
	if got := sum.GetPeriodEnd(); got != end1.Unix() { // 摘要 period_end 为 Unix 秒
		t.Fatalf("period_end=%d, want unchanged %d（当期保留）", got, end1.Unix())
	}
	sub := e.subRow()
	if sub.Status != store.SubCancelled || sub.AutoRenew || sub.CancelledAt == nil {
		t.Fatalf("sub row={status:%s renew:%v cancelled_at:%v}, want cancelled/false/非空", sub.Status, sub.AutoRenew, sub.CancelledAt)
	}
	if !sub.CycleEnd.Equal(*end1) {
		t.Fatalf("cycle_end changed after cancel: %v -> %v", *end1, *sub.CycleEnd)
	}
	if bal := e.ledgerBalance(); bal != 10_000 {
		t.Fatalf("ledger balance=%d, want 10000（退订不退费）", bal)
	}
}

// TestCancelSubscription_CurrentPeriodEntitlementRetained 当期权益保留：退订后
// GetActiveSubscription 仍返回该订阅（cycle_end > now），而 GetActiveRecurring 为空
// （不再参与续费）；额度资格可用即 entitled 口径不变。
func TestCancelSubscription_CurrentPeriodEntitlementRetained(t *testing.T) {
	e := newSubRpcTestEnv(t, 10_000)
	if _, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: e.ref("ord_cancel_p1"),
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if _, err := e.svc.CancelSubscription(e.ctx, &billingv1.CancelSubscriptionRequest{
		TenantId: int64(e.tenant), Ref: e.ref("ord_cancel_p2"),
	}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	sub, err := e.st.GetActiveSubscription(e.ctx, e.tenant)
	if err != nil {
		t.Fatalf("GetActiveSubscription: %v", err)
	}
	if sub == nil || sub.Status != store.SubCancelled || sub.CycleEnd == nil || !sub.CycleEnd.After(time.Now()) {
		t.Fatalf("entitled sub = %+v, want cancelled 且 cycle_end 在未来（当期权益保留）", sub)
	}
	active, err := e.st.GetActiveRecurring(e.ctx, e.tenant)
	if err != nil {
		t.Fatalf("GetActiveRecurring: %v", err)
	}
	if active != nil {
		t.Fatalf("GetActiveRecurring = %+v, want nil（退订后不再续费）", active)
	}
}

// TestCancelSubscription_AlreadyCancelled_NoOp 已退订重放：no-op（cancelled=false，
// cancelled_at/cycle_end 不变，换 ref 也一样——定点收敛）；无任何订阅 → NotFound。
func TestCancelSubscription_AlreadyCancelled_NoOp(t *testing.T) {
	t.Run("replay_noop", func(t *testing.T) {
		e := newSubRpcTestEnv(t, 10_000)
		if _, err := e.svc.ActivateSubscription(e.ctx, &billingv1.ActivateSubscriptionRequest{
			TenantId: int64(e.tenant), PlanId: int64(e.plan), Ref: e.ref("ord_cancel_n1"),
		}); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if _, err := e.svc.CancelSubscription(e.ctx, &billingv1.CancelSubscriptionRequest{
			TenantId: int64(e.tenant), Ref: e.ref("ord_cancel_n2"),
		}); err != nil {
			t.Fatalf("first Cancel: %v", err)
		}
		cancelledAt := *e.subRow().CancelledAt
		end1 := *e.subRow().CycleEnd

		replay, err := e.svc.CancelSubscription(e.ctx, &billingv1.CancelSubscriptionRequest{
			TenantId: int64(e.tenant), Ref: e.ref("ord_cancel_n3"),
		})
		if err != nil {
			t.Fatalf("replay Cancel: %v", err)
		}
		if replay.Cancelled {
			t.Fatalf("replay Cancelled=true, want false（已取消 no-op）")
		}
		sub := e.subRow()
		if !sub.CancelledAt.Equal(cancelledAt) || !sub.CycleEnd.Equal(end1) {
			t.Fatalf("replay mutated sub: cancelled_at %v->%v end %v->%v", cancelledAt, *sub.CancelledAt, end1, *sub.CycleEnd)
		}
	})

	t.Run("no_subscription_not_found", func(t *testing.T) {
		e := newSubRpcTestEnv(t, 10_000)
		_, err := e.svc.CancelSubscription(e.ctx, &billingv1.CancelSubscriptionRequest{
			TenantId: int64(e.tenant), Ref: e.ref("ord_cancel_n4"),
		})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("cancel without sub: code=%v, want NotFound（err=%v）", status.Code(err), err)
		}
	})
}

// TestCancelSubscription_CrossTenantIsolated 跨租户隔离：退订只作用于调用方自己的租户——
// B 无订阅退订 → NotFound 且 A 的订阅原样（active/auto_renew=1）；B 开通后退订只取消 B 的。
func TestCancelSubscription_CrossTenantIsolated(t *testing.T) {
	eA := newSubRpcTestEnv(t, 10_000)
	eB := newSubRpcTestEnv(t, 10_000)

	if _, err := eA.svc.ActivateSubscription(eA.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(eA.tenant), PlanId: int64(eA.plan), Ref: eA.ref("ord_cancel_x1"),
	}); err != nil {
		t.Fatalf("tenant A Activate: %v", err)
	}

	// B 无订阅退订 → NotFound，A 零副作用
	_, err := eB.svc.CancelSubscription(eB.ctx, &billingv1.CancelSubscriptionRequest{
		TenantId: int64(eB.tenant), Ref: eB.ref("ord_cancel_x2"),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("tenant B cancel without sub: code=%v, want NotFound（err=%v）", status.Code(err), err)
	}
	subA := eA.subRow()
	if subA == nil || subA.Status != store.SubActive || !subA.AutoRenew {
		t.Fatalf("tenant A sub = %+v, want active/auto_renew（跨租户零副作用）", subA)
	}

	// B 开通自己的订阅再退订 → 只取消 B 的
	if _, err := eB.svc.ActivateSubscription(eB.ctx, &billingv1.ActivateSubscriptionRequest{
		TenantId: int64(eB.tenant), PlanId: int64(eB.plan), Ref: eB.ref("ord_cancel_x3"),
	}); err != nil {
		t.Fatalf("tenant B Activate: %v", err)
	}
	if _, err := eB.svc.CancelSubscription(eB.ctx, &billingv1.CancelSubscriptionRequest{
		TenantId: int64(eB.tenant), Ref: eB.ref("ord_cancel_x4"),
	}); err != nil {
		t.Fatalf("tenant B Cancel: %v", err)
	}
	if eB.subRow().Status != store.SubCancelled {
		t.Fatalf("tenant B sub = %s, want cancelled", eB.subRow().Status)
	}
	if subA2 := eA.subRow(); subA2.Status != store.SubActive {
		t.Fatalf("tenant A sub after B ops = %s, want active", subA2.Status)
	}
}
