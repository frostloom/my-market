package mallerr

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mustFromGRPC 断言 err 携带 mall 载体并返回（gRPC 码, 契约码, retryable）。
func mustFromGRPC(t *testing.T, err error) (codes.Code, Code, bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil，要求携带载体")
	}
	code, retryable, ok := FromGRPC(err)
	if !ok {
		t.Fatalf("err = %v 未携带 mall 载体", err)
	}
	st, _ := status.FromError(err)
	return st.Code(), code, retryable
}

// 1) 同 HTTP 语义码可区分：MALL_20001（商品不存在）与 MALL_20006（品类不存在）
//    都是 404 → gRPC 同为 NotFound，但 Reason 载体不同。
func TestToGRPC_SameHTTPCodeDistinguishable(t *testing.T) {
	goodsErr := ToGRPC(E(CodeGoodsNotFound, false, "商品不存在: 999"))
	catErr := ToGRPC(E(CodeCategoryNotFound, false, "品类不存在: 7"))

	gotCode1, reason1, retry1 := mustFromGRPC(t, goodsErr)
	gotCode2, reason2, retry2 := mustFromGRPC(t, catErr)

	if gotCode1 != codes.NotFound || gotCode2 != codes.NotFound {
		t.Fatalf("gRPC 码 = (%v, %v)，要求同为 NotFound", gotCode1, gotCode2)
	}
	if reason1 != CodeGoodsNotFound || reason2 != CodeCategoryNotFound {
		t.Fatalf("Reason = (%s, %s)，要求可区分 (MALL_20001, MALL_20006)", reason1, reason2)
	}
	if retry1 || retry2 {
		t.Fatalf("retryable = (%v, %v)，要求均 false", retry1, retry2)
	}
	// message 保留人话描述（bff 错误体 error 字段来源）
	if st, _ := status.FromError(goodsErr); st.Message() != "商品不存在: 999" {
		t.Fatalf("message = %q，要求保留原文", st.Message())
	}
}

// 2) 5xxx 三态不坍缩：50001→Internal、50002→DeadlineExceeded、50003→Unavailable。
func TestToGRPC_SystemCodesNotCollapsed(t *testing.T) {
	cases := []struct {
		code     Code
		wantGRPC codes.Code
	}{
		{CodeInternal, codes.Internal},
		{CodeUpstreamTimeout, codes.DeadlineExceeded},
		{CodeUpstreamUnavailable, codes.Unavailable},
	}
	seen := map[codes.Code]bool{}
	for _, c := range cases {
		gotCode, reason, retry := mustFromGRPC(t, ToGRPC(E(c.code, false, "系统错误")))
		if gotCode != c.wantGRPC {
			t.Fatalf("%s → gRPC %v，要求 %v", c.code, gotCode, c.wantGRPC)
		}
		if reason != c.code {
			t.Fatalf("Reason = %s，要求 %s", reason, c.code)
		}
		if !retry {
			t.Fatalf("%s retryable = false，要求 true（契约 5xxx 均可重试）", c.code)
		}
		seen[gotCode] = true
	}
	if len(seen) != 3 {
		t.Fatalf("三个系统码坍缩为 %d 种 gRPC 码，要求 3 种互不相同", len(seen))
	}
}

// 3) 无载体优雅回落 + 往返：非 mallerr 错误 ToGRPC = 现状行为（Internal+原文）；
//    无 ErrorInfo 的 status 错误、非 gRPC 错误、nil 的 FromGRPC = ok=false。
func TestFromGRPC_FallbackWithoutCarrier(t *testing.T) {
	// 非 mallerr 错误 → Internal（现状行为），FromGRPC ok=false
	plain := errors.New("boom")
	got := ToGRPC(plain)
	st, _ := status.FromError(got)
	if st.Code() != codes.Internal || st.Message() != "boom" {
		t.Fatalf("ToGRPC(普通错误) = (%v, %q)，要求 (Internal, boom)", st.Code(), st.Message())
	}
	if _, _, ok := FromGRPC(plain); ok {
		t.Fatalf("FromGRPC(普通错误) ok=true，要求 false（无载体回落）")
	}

	// 有 status 但无 ErrorInfo details 的 gRPC 错误 → ok=false
	bare := status.Error(codes.NotFound, "no carrier")
	if _, _, ok := FromGRPC(bare); ok {
		t.Fatalf("FromGRPC(无载体 status) ok=true，要求 false")
	}
	// 非 gRPC 错误、nil 同样 ok=false
	if _, _, ok := FromGRPC(fmt.Errorf("wrap: %w", plain)); ok {
		t.Fatalf("FromGRPC(包装普通错误) ok=true，要求 false")
	}
	if _, _, ok := FromGRPC(nil); ok {
		t.Fatalf("FromGRPC(nil) ok=true，要求 false")
	}
	if ToGRPC(nil) != nil {
		t.Fatalf("ToGRPC(nil) ≠ nil")
	}

	// 往返：包装后的 mallerr 也携带载体；retryable 与 message 保真
	rt := ToGRPC(fmt.Errorf("服务层: %w", E(CodeRateLimited, false, "请求过于频繁")))
	gotCode, reason, retry := mustFromGRPC(t, rt)
	if gotCode != codes.ResourceExhausted || reason != CodeRateLimited || !retry {
		t.Fatalf("往返 = (%v, %s, %v)，要求 (ResourceExhausted, MALL_10006, true)",
			gotCode, reason, retry)
	}
}
