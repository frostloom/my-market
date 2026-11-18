// mall-goods 集成测试：连 dev MySQL（deploy/compose，宿主 3307，market 库）。
// 只读断言 R1-T1 种子数据（scripts/seed-goods.sql：44 行商品、六品类、
// 手机/服饰/穿搭/家居 各 10 件在售、301 subscription 未上架），不写库、可重复跑。
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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frostloom/my-market/pkg/mallerr"
	"github.com/frostloom/my-market/services/mall-gen/mallv1"
)

// 种子锚点（scripts/seed-goods.sql 显式主键段位；断言即种子完整性校验）。
const (
	seedGoodsTotal      = 44  // 40 general + 3 recharge + 1 subscription
	seedOnSaleTotal     = 43  // 301 subscription on_sale=0
	seedPhoneCount      = 10  // 101-110
	seedRechargeFirstID = 201 // ¥10 档
	seedSubscriptionID  = 301 // 未上架占位
	seedHostPort        = "127.0.0.1:3307"
)

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

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(testDSN(t))
	if err != nil {
		t.Fatalf("OpenStore: %v（dev MySQL 未就绪？docker compose --profile infra up -d）", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// categoryIDByName 从种子品类表按名称解析 id（不硬编码自增值）。
func categoryIDByName(t *testing.T, s *Store, name string) uint64 {
	t.Helper()
	rows, err := s.ListCategories(context.Background())
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	t.Fatalf("品类 %q 不在种子数据中", name)
	return 0
}

// assertKeysetDesc 断言行序严格按 (created_at,id) 降序（键集游标的排序不变式）。
func assertKeysetDesc(t *testing.T, rows []goodsRow) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		after := prev.CreatedAt.After(cur.CreatedAt) ||
			(prev.CreatedAt.Equal(cur.CreatedAt) && prev.ID > cur.ID)
		if !after {
			t.Fatalf("键集顺序被破坏 @%d: prev(id=%d,at=%s) !> cur(id=%d,at=%s)",
				i, prev.ID, prev.CreatedAt, cur.ID, cur.CreatedAt)
		}
	}
}

// TestListGoods_PaginationToEnd 手机品类 10 件、page_size=4 翻页到底：
// 页序 4/4/2，末页 has_more=false 且 next_cursor 空；全量 id 不重不漏、严格降序。
func TestListGoods_PaginationToEnd(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cat := categoryIDByName(t, s, "手机")

	var (
		gotIDs   []uint64
		pages    [][]goodsRow
		cursor   = ""
		hasMore  = true
		maxPages = 10 // 防御死循环
	)
	for i := 0; hasMore && i < maxPages; i++ {
		rows, next, more, err := s.ListGoods(ctx, cat, cursor, 4)
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		pages = append(pages, rows)
		for _, r := range rows {
			gotIDs = append(gotIDs, r.ID)
		}
		cursor, hasMore = next, more
	}

	if len(pages) != 3 {
		t.Fatalf("页数 = %d，want 3（10 件 / 每页 4）", len(pages))
	}
	if lens := fmt.Sprint(len(pages[0]), len(pages[1]), len(pages[2])); lens != "4 4 2" {
		t.Fatalf("每页行数 = %s，want 4 4 2", lens)
	}
	if hasMore {
		t.Fatalf("末页后 has_more = true，want false")
	}
	if cursor != "" {
		t.Fatalf("末页 next_cursor = %q，want 空（契约 §0.3：空即到底）", cursor)
	}
	if len(gotIDs) != seedPhoneCount {
		t.Fatalf("翻页合计 %d 件，want %d", len(gotIDs), seedPhoneCount)
	}
	seen := map[uint64]bool{}
	for _, id := range gotIDs {
		if seen[id] {
			t.Fatalf("翻页出现重复商品 id=%d（游标键集丢/重行）", id)
		}
		seen[id] = true
	}

	// 行序校验：整列（跨页拼接后）仍严格键集降序
	var all []goodsRow
	for _, p := range pages {
		all = append(all, p...)
	}
	assertKeysetDesc(t, all)
}

// TestListGoods_FirstPageNoCursor 首页不带游标：返回恰好 page_size 件且
// has_more=true、next_cursor 非空；下一页从游标位点继续、不重复上一页末行。
func TestListGoods_FirstPageNoCursor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cat := categoryIDByName(t, s, "手机")

	rows, next, more, err := s.ListGoods(ctx, cat, "", 5)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(rows) != 5 || !more || next == "" {
		t.Fatalf("首页 = (%d 行, cursor=%q, more=%v)，want (5, 非空, true)", len(rows), next, more)
	}
	assertKeysetDesc(t, rows)

	rows2, _, _, err := s.ListGoods(ctx, cat, next, 5)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(rows2) == 0 {
		t.Fatalf("第二页为空")
	}
	if rows2[0].ID == rows[len(rows)-1].ID {
		t.Fatalf("第二页首行与上一页末行重复 id=%d（游标未推进）", rows2[0].ID)
	}
	assertKeysetDesc(t, rows2)
}

// TestListGoods_CategoryFilter 品类过滤：服饰页内全是服饰；不带品类=全在售；
// 不带品类也不混入未上架商品。
func TestListGoods_CategoryFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cloth := categoryIDByName(t, s, "服饰")
	outfit := categoryIDByName(t, s, "穿搭")

	rows, _, more, err := s.ListGoods(ctx, cloth, "", 50)
	if err != nil {
		t.Fatalf("cloth list: %v", err)
	}
	if len(rows) != 10 || more {
		t.Fatalf("服饰 = (%d 行, more=%v)，want (10, false)", len(rows), more)
	}
	for _, r := range rows {
		if r.CategoryID != cloth {
			t.Fatalf("服饰页混入品类 %d 的商品 id=%d", r.CategoryID, r.ID)
		}
	}

	rows, _, _, err = s.ListGoods(ctx, outfit, "", 50)
	if err != nil {
		t.Fatalf("outfit list: %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("穿搭 = %d 行，want 10", len(rows))
	}

	all, _, more, err := s.ListGoods(ctx, 0, "", 50) // category_id=0 不限品类
	if err != nil {
		t.Fatalf("all list: %v", err)
	}
	if len(all) != seedOnSaleTotal || more {
		t.Fatalf("全量 = (%d 行, more=%v)，want (%d, false)", len(all), more, seedOnSaleTotal)
	}
	for _, r := range all {
		if r.OnSale != 1 {
			t.Fatalf("列表混入未上架商品 id=%d（on_sale 应恒为 1）", r.ID)
		}
	}
}

// TestListGoods_CategoryNotFound 品类不存在 → MALL_20006（契约 GET /goods 错误表）。
func TestListGoods_CategoryNotFound(t *testing.T) {
	s := openTestStore(t)
	_, _, _, err := s.ListGoods(context.Background(), 999999, "", 20)
	var me *mallerr.Error
	if !asMallErr(err, &me) || me.Code != mallerr.CodeCategoryNotFound {
		t.Fatalf("err = %v，want MALL_20006", err)
	}
}

// TestListGoods_InvalidCursor 非法游标 → MALL_10005；gRPC 侧映 InvalidArgument。
func TestListGoods_InvalidCursor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, _, _, err := s.ListGoods(ctx, 0, "!!!not-base64", 20)
	var me *mallerr.Error
	if !asMallErr(err, &me) || me.Code != mallerr.CodeInvalidParam {
		t.Fatalf("store err = %v，want MALL_10005", err)
	}

	srv := newGoodsServer(s)
	_, err = srv.ListGoods(ctx, &mallv1.ListGoodsReq{Page: &mallv1.PageReq{Cursor: "!!!not-base64", PageSize: 20}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("gRPC code = %v，want InvalidArgument（MALL_10005）", err)
	}
}

// TestListGoods_PageSizeClamp page_size 钳制：0→默认 20；1000→上限 50。
// 依据种子：全量在售 43 件 → 默认页 20 件 more=true；钳后单页装满 43 more=false。
func TestListGoods_PageSizeClamp(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rows, _, more, err := s.ListGoods(ctx, 0, "", 0) // 0 → 默认 20
	if err != nil {
		t.Fatalf("default page: %v", err)
	}
	if len(rows) != 20 || !more {
		t.Fatalf("page_size=0 → (%d 行, more=%v)，want (20, true)", len(rows), more)
	}

	rows, _, more, err = s.ListGoods(ctx, 0, "", 1000) // 1000 → 钳 50
	if err != nil {
		t.Fatalf("clamped page: %v", err)
	}
	if len(rows) != seedOnSaleTotal || more {
		t.Fatalf("page_size=1000 → (%d 行, more=%v)，want (%d, false)", len(rows), more, seedOnSaleTotal)
	}
	if len(rows) > 50 {
		t.Fatalf("钳制失效：返回 %d 行 > 50", len(rows))
	}
}

// TestGetGoods_Found 详情含 detail 字段位（种子 detail 为 NULL → 空串）；
// recharge 附 token_estimate（(面值+赠送)×1000，营销展示口径）。
func TestGetGoods_Found(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	general, err := s.GetGoods(ctx, 101)
	if err != nil {
		t.Fatalf("get 101: %v", err)
	}
	if general.Name == "" || general.PriceCent != 499900 || general.OnSale != 1 {
		t.Fatalf("种子 101 字段不符: %+v", general)
	}

	recharge, err := s.GetGoods(ctx, seedRechargeFirstID)
	if err != nil {
		t.Fatalf("get 201: %v", err)
	}
	if recharge.FaceValueCent != 1000 || recharge.BonusCent != 0 {
		t.Fatalf("种子 201 面值字段不符: %+v", recharge)
	}

	// 走 server 映射验证契约字段：detail 仅详情填充、token_estimate 仅 recharge。
	srv := newGoodsServer(s)
	item, err := srv.GetGoods(ctx, &mallv1.GetGoodsReq{Id: seedRechargeFirstID})
	if err != nil {
		t.Fatalf("server get: %v", err)
	}
	if item.GetTokenEstimate() != 1_000_000 { // (1000+0) 分 × 1000
		t.Fatalf("201 token_estimate = %d，want 1000000", item.GetTokenEstimate())
	}
	if item.GetDetail() != "" {
		t.Fatalf("种子 detail 为 NULL，want 空串，got %q", item.GetDetail())
	}

	// 列表项同样带 token_estimate（契约 §3：recharge 附 token_estimate），
	// 但 detail 恒为空。
	page, err := srv.ListGoods(ctx, &mallv1.ListGoodsReq{Page: &mallv1.PageReq{PageSize: 50}})
	if err != nil {
		t.Fatalf("server list: %v", err)
	}
	foundRecharge, foundGeneral := false, false
	for _, it := range page.GetItems() {
		if it.GetType() == typeRecharge {
			foundRecharge = true
			if it.GetTokenEstimate() <= 0 {
				t.Fatalf("列表内 recharge id=%d 缺 token_estimate", it.GetId())
			}
			if it.GetDetail() != "" {
				t.Fatalf("列表项 id=%d 不应带 detail（仅 GetGoods 填充）", it.GetId())
			}
		}
		if it.GetId() == 101 {
			foundGeneral = true
			if it.GetTokenEstimate() != 0 {
				t.Fatalf("general id=101 不应有 token_estimate，got %d", it.GetTokenEstimate())
			}
		}
	}
	if !foundRecharge || !foundGeneral {
		t.Fatalf("全量列表缺锚点商品（recharge=%v, 101=%v）", foundRecharge, foundGeneral)
	}
}

// TestGetGoods_NotFound 不存在 → MALL_20001；gRPC 侧映 NotFound。
func TestGetGoods_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.GetGoods(ctx, 999999)
	var me *mallerr.Error
	if !asMallErr(err, &me) || me.Code != mallerr.CodeGoodsNotFound {
		t.Fatalf("store err = %v，want MALL_20001", err)
	}

	srv := newGoodsServer(s)
	_, err = srv.GetGoods(ctx, &mallv1.GetGoodsReq{Id: 999999})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("gRPC code = %v，want NotFound（MALL_20001）", err)
	}
}

// TestGetGoods_OffSaleReturned 未上架商品原样返回（on_sale=false），
// 错误与 409 的裁量在 bff——goods 服务不吞 301。
func TestGetGoods_OffSaleReturned(t *testing.T) {
	s := openTestStore(t)
	row, err := s.GetGoods(context.Background(), seedSubscriptionID)
	if err != nil {
		t.Fatalf("get 301: %v", err)
	}
	if row.OnSale != 0 {
		t.Fatalf("301 on_sale = %d，want 0（未上架原样返回）", row.OnSale)
	}
}

// TestListCategories_SixSorted 六品类、按 sort 升序、全部上架。
func TestListCategories_SixSorted(t *testing.T) {
	s := openTestStore(t)
	rows, err := s.ListCategories(context.Background())
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("品类 = %d 行，want 6", len(rows))
	}
	want := []string{"精选", "手机", "服饰", "穿搭", "家居", "福利"}
	for i, r := range rows {
		if r.Name != want[i] {
			t.Fatalf("品类序 %d = %q，want %q（sort 升序）", i, r.Name, want[i])
		}
		if r.Status != 1 {
			t.Fatalf("品类 %q 未上架", r.Name)
		}
	}
}

// TestTokenEstimate 换算常数口径（纯函数）。
func TestTokenEstimate(t *testing.T) {
	cases := []struct {
		cent, want int64
	}{
		{0, 0},
		{1000, 1_000_000},   // ¥10 无赠
		{5300, 5_300_000},   // ¥50+¥3
		{11000, 11_000_000}, // ¥100+¥10
	}
	for _, tc := range cases {
		if got := tokenEstimate(tc.cent); got != tc.want {
			t.Fatalf("tokenEstimate(%d) = %d，want %d", tc.cent, got, tc.want)
		}
	}
}

// TestClampPageSize 钳制边界（纯函数）。
func TestClampPageSize(t *testing.T) {
	cases := []struct {
		in   int32
		want int
	}{{0, 20}, {-5, 20}, {1, 1}, {20, 20}, {50, 50}, {51, 50}, {1 << 20, 50}}
	for _, tc := range cases {
		if got := clampPageSize(tc.in); got != tc.want {
			t.Fatalf("clampPageSize(%d) = %d，want %d", tc.in, got, tc.want)
		}
	}
}

// asMallErr 取回契约错误（errors.As 薄包装，测试可读性）。
func asMallErr(err error, target **mallerr.Error) bool {
	return errors.As(err, target)
}
