// mall-order 集成测试：连 dev MySQL（deploy/compose，宿主 3307，market 库）。
//
// TDD 硬要求：本文件先于实现编写（RED），实现后转 GREEN。
// 断言锚点 = scripts/seed-goods.sql 种子（101-140 general、201-203 recharge、
// 301 subscription 月卡已上架，R2.5）。测试写入的 orders/items/cart 行与扣减的 stock
// 一律在 t.Cleanup 还原（种子 44 商品锚点不被污染），可重复跑。
//
// DSN 来源：环境变量 MARKET_MYSQL_DSN 优先；未设置则从 deploy/compose/.env
// 读 MYSQL_ROOT_PASSWORD 拼接（凭据不硬编码、不提交、不打日志）。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// 测试专用租户段位（远离真实租户自增值；cleanup 按 tenant_id 精确清除）。
var (
	testTenantA = uint64(910101)
	testTenantB = uint64(910102)
	testTenantC = uint64(910103) // 购物车测试
)

const seedHostPort = "127.0.0.1:3307"

func testDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("MARKET_MYSQL_DSN"); dsn != "" {
		return dsn
	}
	envFile := filepath.Join("..", "..", "deploy", "compose", ".env")
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("集成测试需要 MySQL：设 MARKET_MYSQL_DSN，或提供 %s（读取失败: %v）", envFile, err)
	}
	var pw string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if v, ok := strings.CutPrefix(line, "MYSQL_ROOT_PASSWORD="); ok && v != "" {
			pw = v
			break
		}
	}
	if pw == "" {
		t.Fatalf("%s 缺 MYSQL_ROOT_PASSWORD，无法拼 DSN（也可直接设 MARKET_MYSQL_DSN）", envFile)
	}
	return fmt.Sprintf("root:%s@tcp(%s)/market?charset=utf8mb4&parseTime=true&loc=Local", pw, seedHostPort)
}

func openOrderTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(testDSN(t))
	if err != nil {
		t.Fatalf("OpenStore: %v（dev MySQL 未就绪？docker compose --profile infra up -d）", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// snapshotStock 记录商品库存快照并在测试结束后按快照绝对还原
// （比回加差值更稳：即使实现多扣/少扣也能复位种子锚点）。
func snapshotStock(t *testing.T, s *Store, ids ...uint64) {
	t.Helper()
	snap := map[uint64]int{}
	for _, id := range ids {
		snap[id] = stockOf(t, s, id)
	}
	t.Cleanup(func() {
		for id, v := range snap {
			setStockRaw(t, s, id, v)
		}
	})
}

func stockOf(t *testing.T, s *Store, id uint64) int {
	t.Helper()
	row, err := s.GetGoodsRow(context.Background(), id)
	if err != nil {
		t.Fatalf("读商品 %d 库存: %v", id, err)
	}
	return row.Stock
}

func setStockRaw(t *testing.T, s *Store, id uint64, stock int) {
	t.Helper()
	if err := s.db.Exec("UPDATE goods SET stock = ? WHERE id = ?", stock, id).Error; err != nil {
		t.Fatalf("还原商品 %d 库存为 %d: %v", id, stock, err)
	}
}

// cleanupTenant 测试结束后清除本测试租户写入的 orders/items/cart 行，
// 以及这些订单派生的支付流水与履约任务行（先于 mall_orders 删除）。
func cleanupTenant(t *testing.T, s *Store, tenants ...uint64) {
	t.Helper()
	t.Cleanup(func() {
		for _, tn := range tenants {
			if err := s.db.Exec(
				"DELETE FROM fulfillment_tasks WHERE order_no IN (SELECT order_no FROM mall_orders WHERE tenant_id = ?)",
				tn).Error; err != nil {
				t.Fatalf("cleanup 履约任务 tenant=%d: %v", tn, err)
			}
			if err := s.db.Exec(
				"DELETE FROM payment_records WHERE order_no IN (SELECT order_no FROM mall_orders WHERE tenant_id = ?)",
				tn).Error; err != nil {
				t.Fatalf("cleanup 支付流水 tenant=%d: %v", tn, err)
			}
			if err := s.db.Exec(
				"DELETE FROM mall_order_items WHERE order_no IN (SELECT order_no FROM mall_orders WHERE tenant_id = ?)",
				tn).Error; err != nil {
				t.Fatalf("cleanup 订单行 tenant=%d: %v", tn, err)
			}
			if err := s.db.Exec("DELETE FROM mall_orders WHERE tenant_id = ?", tn).Error; err != nil {
				t.Fatalf("cleanup 订单 tenant=%d: %v", tn, err)
			}
			if err := s.db.Exec("DELETE FROM cart_items WHERE tenant_id = ?", tn).Error; err != nil {
				t.Fatalf("cleanup 购物车 tenant=%d: %v", tn, err)
			}
		}
	})
}

func countOrders(t *testing.T, s *Store, tenant uint64) int64 {
	t.Helper()
	var n int64
	if err := s.db.Raw("SELECT COUNT(*) FROM mall_orders WHERE tenant_id = ?", tenant).Scan(&n).Error; err != nil {
		t.Fatalf("count orders tenant=%d: %v", tenant, err)
	}
	return n
}

// assertMallCode 断言 err 是携带指定契约码的错误（errors.As 薄包装）。
func assertMallCode(t *testing.T, err error, want mallerr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil，want %s", want)
	}
	var me *mallerr.Error
	if !errors.As(err, &me) || me.Code != want {
		t.Fatalf("err = %v（code=%v），want %s", err, me, want)
	}
}

// ---------------------------------------------------------------------------
// 1. TestCreateOrder_HappyPath_PersistsOrderAndDeductsStock
// 建单落库（kind=2、行号 1..n、价格快照）且事务内直扣库存。
func TestCreateOrder_HappyPath_PersistsOrderAndDeductsStock(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 101)

	before := stockOf(t, s, 101)
	ord, err := s.CreateCartOrder(ctx, testTenantA, "happy-1", []Line{{GoodsID: 101, Qty: 2}})
	if err != nil {
		t.Fatalf("CreateCartOrder: %v", err)
	}

	// 订单号形态 M+14 位时间戳+6 位随机
	if len(ord.OrderNo) != 21 || ord.OrderNo[0] != 'M' {
		t.Fatalf("order_no = %q，want M+yyyyMMddHHmmss+6位随机（21 字符）", ord.OrderNo)
	}
	if ord.Status != "created" || ord.AmountCent != 499900*2 {
		t.Fatalf("订单 = (status=%s, amount=%d)，want (created, %d)", ord.Status, ord.AmountCent, 499900*2)
	}
	if ord.TenantID != testTenantA || ord.IdemKey != "happy-1" {
		t.Fatalf("订单归属错: tenant=%d idem=%q", ord.TenantID, ord.IdemKey)
	}
	if len(ord.Items) != 1 {
		t.Fatalf("订单行 = %d，want 1", len(ord.Items))
	}
	it := ord.Items[0]
	if it.LineNo != 1 || it.GoodsID != 101 || it.Qty != 2 ||
		it.PriceCent != 499900 || it.GoodsName == "" {
		t.Fatalf("订单行快照不符: %+v", it)
	}

	// 库存直扣
	if got := stockOf(t, s, 101); got != before-2 {
		t.Fatalf("stock(101) = %d，want %d（事务内直扣）", got, before-2)
	}
}

// ---------------------------------------------------------------------------
// 2. TestCreateOrder_IdemKeyScopedPerTenant（Review Focus #1）
// 幂等键作用域 (tenant_id, key)：tenant1/tenant2 同 key 各自成单。
func TestCreateOrder_IdemKeyScopedPerTenant(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA, testTenantB)
	snapshotStock(t, s, 102)
	before := stockOf(t, s, 102)

	ordA, err := s.CreateCartOrder(ctx, testTenantA, "scoped-1", []Line{{GoodsID: 102, Qty: 1}})
	if err != nil {
		t.Fatalf("tenant A 建单: %v", err)
	}
	ordB, err := s.CreateCartOrder(ctx, testTenantB, "scoped-1", []Line{{GoodsID: 102, Qty: 1}})
	if err != nil {
		t.Fatalf("tenant B 同 key 建单: %v（幂等键按租户隔离，不应冲突）", err)
	}
	if ordA.OrderNo == ordB.OrderNo {
		t.Fatalf("两租户同 key 生成同一单 %q（作用域未隔离）", ordA.OrderNo)
	}
	if countOrders(t, s, testTenantA) != 1 || countOrders(t, s, testTenantB) != 1 {
		t.Fatalf("orders(A=%d, B=%d)，want 各 1", countOrders(t, s, testTenantA), countOrders(t, s, testTenantB))
	}
	if got := stockOf(t, s, 102); got != before-2 {
		t.Fatalf("stock(102) = %d，want 累扣 2（%d）", got, before-2)
	}
}

// ---------------------------------------------------------------------------
// 3. TestCreateOrder_IdemConflict_DifferentPayload
// 同租户同 key 不同 payload → MALL_30003，且原单保持不变。
func TestCreateOrder_IdemConflict_DifferentPayload(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 103, 104)
	before104 := stockOf(t, s, 104)

	orig, err := s.CreateCartOrder(ctx, testTenantA, "conflict-1", []Line{{GoodsID: 103, Qty: 1}})
	if err != nil {
		t.Fatalf("首次建单: %v", err)
	}
	_, err = s.CreateCartOrder(ctx, testTenantA, "conflict-1", []Line{{GoodsID: 104, Qty: 1}})
	assertMallCode(t, err, mallerr.CodeIdempotencyConflict)

	if got := countOrders(t, s, testTenantA); got != 1 {
		t.Fatalf("orders = %d，want 1（冲突请求不得落单）", got)
	}
	again, err := s.GetOrder(ctx, testTenantA, orig.OrderNo)
	if err != nil {
		t.Fatalf("读回原单: %v", err)
	}
	if len(again.Items) != 1 || again.Items[0].GoodsID != 103 {
		t.Fatalf("原单被冲突请求污染: %+v", again.Items)
	}
	// 冲突请求未扣 104 的库存
	if got := stockOf(t, s, 104); got != before104 {
		t.Fatalf("stock(104) = %d，want %d（冲突请求应整体回滚）", got, before104)
	}
}

// ---------------------------------------------------------------------------
// 4. TestCreateOrder_IdemSamePayload_ReturnsOriginal
// 同租户同 key 同 payload → 返回原单（200 语义）；行集相同但顺序不同也算同 payload。
func TestCreateOrder_IdemSamePayload_ReturnsOriginal(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 107, 108)

	before107, before108 := stockOf(t, s, 107), stockOf(t, s, 108)

	ord1, err := s.CreateCartOrder(ctx, testTenantA, "same-1",
		[]Line{{GoodsID: 107, Qty: 1}, {GoodsID: 108, Qty: 2}})
	if err != nil {
		t.Fatalf("首次建单: %v", err)
	}
	// 行序不同、集合相同 → 仍视为同 payload
	ord2, err := s.CreateCartOrder(ctx, testTenantA, "same-1",
		[]Line{{GoodsID: 108, Qty: 2}, {GoodsID: 107, Qty: 1}})
	if err != nil {
		t.Fatalf("重放建单: %v（同 key 同 payload 应返回原单）", err)
	}
	if ord2.OrderNo != ord1.OrderNo || ord2.AmountCent != ord1.AmountCent {
		t.Fatalf("重放 = (%s, %d)，want 原单 (%s, %d)",
			ord2.OrderNo, ord2.AmountCent, ord1.OrderNo, ord1.AmountCent)
	}
	if got := countOrders(t, s, testTenantA); got != 1 {
		t.Fatalf("orders = %d，want 1", got)
	}
	// 库存只扣一次
	if stockOf(t, s, 107) != before107-1 || stockOf(t, s, 108) != before108-2 {
		t.Fatalf("库存重复扣减: 107=%d(前%d), 108=%d(前%d)",
			stockOf(t, s, 107), before107, stockOf(t, s, 108), before108)
	}
}

// ---------------------------------------------------------------------------
// 5. TestCreateStock_InsufficientRollback（Review Focus #4）
// stock=1 并发两个 CreateCartOrder（不同 idem key）→ 恰一单成功、另一败 MALL_20003，
// 最终 stock=0、该租户订单恰 1 条（条件更新 + 整体回滚）。
func TestCreateStock_InsufficientRollback(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)
	snapshotStock(t, s, 109)

	setStockRaw(t, s, 109, 1)

	var (
		orders [2]*Order
		errs   [2]error
	)
	g := &errgroup.Group{}
	for i := range 2 {
		i := i
		g.Go(func() error {
			orders[i], errs[i] = s.CreateCartOrder(ctx, testTenantA,
				fmt.Sprintf("race-%d", i), []Line{{GoodsID: 109, Qty: 1}})
			return nil // 结果经 errs 数组检查，不依赖 errgroup 首错
		})
	}
	_ = g.Wait()

	successes, failures := 0, 0
	for i := range 2 {
		switch {
		case errs[i] == nil:
			successes++
		default:
			var me *mallerr.Error
			if errors.As(errs[i], &me) && me.Code == mallerr.CodeOutOfStock {
				failures++
			} else {
				t.Fatalf("goroutine %d 败因非 MALL_20003: %v", i, errs[i])
			}
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("并发结果 = (%d 成, %d 败)，want (1, 1)", successes, failures)
	}
	if got := stockOf(t, s, 109); got != 0 {
		t.Fatalf("stock(109) = %d，want 0", got)
	}
	if got := countOrders(t, s, testTenantA); got != 1 {
		t.Fatalf("orders = %d，want 1（失败方必须整体回滚，不留脏单）", got)
	}
}

// ---------------------------------------------------------------------------
// 6. TestCreateOrder_RejectsRechargeType
// recharge（type=2）不可进购物车单 → MALL_30007；不落单、不动库存。
func TestCreateOrder_RejectsRechargeType(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA)

	_, err := s.CreateCartOrder(ctx, testTenantA, "recharge-1", []Line{{GoodsID: 201, Qty: 1}})
	assertMallCode(t, err, mallerr.CodeGoodsTypeNotAllowed)
	if got := countOrders(t, s, testTenantA); got != 0 {
		t.Fatalf("orders = %d，want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 7. TestGetOrder_TenantIsolation
// B 租户查 A 租户订单 → 视为不存在（MALL_30001）；A 自己可查。
func TestGetOrder_TenantIsolation(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA, testTenantB)
	snapshotStock(t, s, 110)

	ord, err := s.CreateCartOrder(ctx, testTenantA, "iso-1", []Line{{GoodsID: 110, Qty: 1}})
	if err != nil {
		t.Fatalf("建单: %v", err)
	}

	got, err := s.GetOrder(ctx, testTenantA, ord.OrderNo)
	if err != nil {
		t.Fatalf("A 查自己订单: %v", err)
	}
	if got.OrderNo != ord.OrderNo || len(got.Items) != 1 {
		t.Fatalf("A 读回不符: %+v", got)
	}

	_, err = s.GetOrder(ctx, testTenantB, ord.OrderNo)
	assertMallCode(t, err, mallerr.CodeOrderNotFound)

	_, err = s.GetOrder(ctx, testTenantA, "M9999999999999999999")
	assertMallCode(t, err, mallerr.CodeOrderNotFound)
}

// ---------------------------------------------------------------------------
// 补充：TestListOrders_CursorPagination —— ListOrders 复用 mallpage 键集游标，
// 按租户隔离翻页、不重不漏、到末页 has_more=false 且 next_cursor 空。
func TestListOrders_CursorPagination(t *testing.T) {
	s := openOrderTestStore(t)
	ctx := context.Background()
	cleanupTenant(t, s, testTenantA, testTenantB)
	snapshotStock(t, s, 111, 112)

	wantNos := make(map[string]bool)
	for i := range 5 {
		ord, err := s.CreateCartOrder(ctx, testTenantA, fmt.Sprintf("page-%d", i),
			[]Line{{GoodsID: uint64(111), Qty: 1}})
		if err != nil {
			t.Fatalf("建单 %d: %v", i, err)
		}
		wantNos[ord.OrderNo] = true
	}
	if _, err := s.CreateCartOrder(ctx, testTenantB, "page-other", []Line{{GoodsID: 112, Qty: 1}}); err != nil {
		t.Fatalf("B 建单: %v", err)
	}

	// 翻页：page_size=2 → 3 页（2/2/1），末页到底
	var gotNos []string
	cursor := ""
	hasMore := true
	pages := 0
	for hasMore && pages < 10 {
		page, err := s.ListOrders(ctx, testTenantA, PageReq{Cursor: cursor, PageSize: 2})
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		pages++
		for _, o := range page.Orders {
			gotNos = append(gotNos, o.OrderNo)
			if o.TenantID != testTenantA {
				t.Fatalf("列表混入他租户订单 tenant=%d", o.TenantID)
			}
		}
		cursor, hasMore = page.NextCursor, page.HasMore
	}
	if pages != 3 || len(gotNos) != 5 || hasMore || cursor != "" {
		t.Fatalf("翻页 = (%d 页, %d 行, more=%v, cursor=%q)，want (3, 5, false, 空)",
			pages, len(gotNos), hasMore, cursor)
	}
	if len(gotNos) != len(wantNos) {
		t.Fatalf("翻到行数 = %d，want %d", len(gotNos), len(wantNos))
	}
	for _, no := range gotNos {
		if !wantNos[no] {
			t.Fatalf("翻页出现非本测试订单 %q（列表查询未按租户过滤）", no)
		}
		delete(wantNos, no)
	}
	if len(wantNos) != 0 {
		t.Fatalf("翻页漏单: %v", wantNos)
	}
}
