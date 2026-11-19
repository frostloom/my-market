// ActivateSubscription uk 兜底分支测试（R2.5 Fix Round 1，Important-3）。
//
// tx 内 !inserted（并发重放撞 uk_topup_ref）分支在 batch 模式下是活的正确性保障：
// lockCash 的进程内互斥挡不住跨进程同租户并发，两个进程同时预读 miss、后到者在
// tx 里撞提交点，必须收敛为单次变更。本文件与 store 包同包的原因：
// activateSubscriptionCash/lockCash 为包内私有，且需构造 RR 快照竞败方
// （server 包的黑盒测试无法直调）。
//
// 构造：竞败方绑定一个只读快照先于获胜事务提交的 RR 事务（预读在快照里看不见
// 获胜锚行 → 复刻「预读与提交穿插」的跨进程窗口）→ tx 撞 uk_ref → 锁读
// （FOR UPDATE，当前读）读回获胜行 → 同租户 → 跳过写入收敛 no-op。
// 连真实基础设施（ai_gateway_test 库 + Redis DB 1，newTestStore 每测清库）。
package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestActivateSubscriptionFallbackRefSingleCommit(t *testing.T) {
	s, done := newTestStore(t)
	defer done()
	ctx := context.Background()
	tid := seedTenant(t, s, 10_000)

	plan := Plan{
		Name: fmt.Sprintf("fb-plan-%d", time.Now().UnixNano()), PlanType: PlanTypeRecurring,
		PriceMoney: 80, ValidityDays: 30, RefreshHours: 5, WindowQuotaCent: 1_000_000,
		TierQuota: `{"轻量":2000000}`, Status: 0,
	}
	if err := s.db.Create(&plan).Error; err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	ref := fmt.Sprintf("ord-fb-%d", time.Now().UnixNano())

	// 竞败方载体：锁内 store（cashMutationAllowed 需要 cash 会话）+ 绑定 RR 事务的 db。
	baseLocked, releaseBase, err := s.lockCash(ctx, tid)
	if err != nil {
		t.Fatalf("lockCash: %v", err)
	}
	defer releaseBase()
	loserTx := s.db.Begin()
	t.Cleanup(func() { _ = loserTx.Rollback() }) // 安全网（正文主动 Rollback）
	var pinned uint64
	if err := loserTx.Raw("SELECT id FROM tenants WHERE id = ?", tid).Scan(&pinned).Error; err != nil || pinned != tid {
		t.Fatalf("pin snapshot: id=%d err=%v", pinned, err)
	}
	loserStore := *baseLocked
	loserStore.db = loserTx

	// 获胜方：锁内直调（baseLocked.cash != nil，lockCash 幂等返回自身），正常开通并提交。
	winner, err := baseLocked.activateSubscriptionCash(baseLocked.cashSQLContext(), tid, plan.ID, ref)
	if err != nil || !winner.Activated {
		t.Fatalf("winner activate: err=%v activated=%v", err, winner.Activated)
	}

	// 竞败方：快照预读 miss → tx 撞 uk_ref 兜底 → 锁读读回获胜行（同租户）→ 跳过写入。
	loser, err := loserStore.activateSubscriptionCash(loserStore.cashSQLContext(), tid, plan.ID, ref)
	if err != nil {
		t.Fatalf("loser activateSubscriptionCash: %v（兜底分支必须锁读读回获胜行收敛 no-op）", err)
	}
	if loser.Activated {
		t.Fatalf("loser Activated=true, want false（uk 兜底必须收敛 no-op）")
	}

	// 全局不变量：锚恰好一条、订阅只被获胜方变更一次（cycle_num=1 首次开通）。
	var markerCount int64
	if err := s.db.Model(&Topup{}).
		Where("ref_type = ? AND ref_id = ?", TopupSubActivateRefType, ref).Count(&markerCount).Error; err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if markerCount != 1 {
		t.Fatalf("marker rows=%d, want 1（竞败方不得新增锚）", markerCount)
	}
	var sub Subscription
	if err := s.db.Where("tenant_id = ?", tid).First(&sub).Error; err != nil {
		t.Fatalf("load sub: %v", err)
	}
	if sub.CycleNum != 1 || sub.Status != SubActive {
		t.Fatalf("sub={num:%d status:%s}, want 1/active（竞败方不得二次变更订阅）", sub.CycleNum, sub.Status)
	}

	if rb := loserTx.Rollback(); rb.Error != nil {
		t.Fatalf("rollback loser tx: %v", rb.Error)
	}
}
