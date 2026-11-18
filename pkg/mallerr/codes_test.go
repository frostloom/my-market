package mallerr

import (
	"errors"
	"fmt"
	"testing"
)

// contractCodes 与 docs/spec/api-contract.md §1 错误码全表逐项硬对齐（27 码）。
// 这里刻意使用码字符串字面量而非常量，保证与契约文本逐字核对。
var contractCodes = map[string]struct {
	HTTP      int
	Retryable bool
}{
	"MALL_10001": {401, false}, // 未登录 / 会话无效
	"MALL_10002": {401, false}, // 会话过期（重新登录）
	"MALL_10003": {409, false}, // 用户名已被占用
	"MALL_10004": {401, false}, // 用户名或密码错误
	"MALL_10005": {400, false}, // 参数校验失败
	"MALL_10006": {429, true},  // 请求过于频繁
	"MALL_20001": {404, false}, // 商品不存在
	"MALL_20002": {409, false}, // 商品已下架
	"MALL_20003": {409, false}, // 库存不足
	"MALL_20004": {409, false}, // 购物车行数超上限（20）
	"MALL_20005": {400, false}, // 商品数量超限（qty>99 或 <1）
	"MALL_20006": {404, false}, // 品类不存在
	"MALL_30001": {404, false}, // 订单不存在
	"MALL_30002": {409, false}, // 订单状态不允许该操作
	"MALL_30003": {409, false}, // 幂等键冲突
	"MALL_30004": {404, false}, // 支付单不存在
	"MALL_30005": {400, false}, // 回调验签失败
	"MALL_30007": {400, false}, // 订单含不可购类型组合
	"MALL_30008": {409, true},  // 履约进行中，稍后再试
	"MALL_40001": {409, false}, // 秒杀活动未开始/已结束
	"MALL_40002": {409, false}, // 已抢完
	"MALL_40003": {409, false}, // 不可重复抢购
	"MALL_40004": {429, true},  // 秒杀限流（令牌桶）
	"MALL_40005": {404, false}, // 抢购凭证不存在
	"MALL_50001": {500, true},  // 内部错误
	"MALL_50002": {504, true},  // 下游超时
	"MALL_50003": {503, true},  // 下游不可用
}

func TestCodesTableMatchesContract(t *testing.T) {
	if got := len(Codes); got != len(contractCodes) {
		t.Fatalf("Codes 表大小 = %d, 契约要求 %d", got, len(contractCodes))
	}
	for code, want := range contractCodes {
		got, ok := Codes[Code(code)]
		if !ok {
			t.Errorf("Codes 缺少契约码 %s", code)
			continue
		}
		if got.HTTP != want.HTTP {
			t.Errorf("%s HTTP = %d, 契约要求 %d", code, got.HTTP, want.HTTP)
		}
		if got.Retryable != want.Retryable {
			t.Errorf("%s Retryable = %v, 契约要求 %v", code, got.Retryable, want.Retryable)
		}
	}
	for code := range Codes {
		if _, ok := contractCodes[string(code)]; !ok {
			t.Errorf("Codes 含契约之外的码 %s", code)
		}
	}
}

func TestHTTPStatus_UnknownCodeIs500(t *testing.T) {
	// 非 mallerr 错误
	if got := HTTPStatus(errors.New("boom")); got != 500 {
		t.Errorf("普通 error 的 HTTPStatus = %d, 要求 500", got)
	}
	// nil 错误
	if got := HTTPStatus(nil); got != 500 {
		t.Errorf("nil 的 HTTPStatus = %d, 要求 500", got)
	}
	// 表外码
	err := E(Code("MALL_99999"), false, "未知码")
	if got := HTTPStatus(err); got != 500 {
		t.Errorf("未知码 MALL_99999 的 HTTPStatus = %d, 要求 500", got)
	}
}

func TestE_CarriesCodeRetryableAndMsg(t *testing.T) {
	err := E(CodeOutOfStock, false, "商品 %s 库存不足", "SKU-1")
	var me *Error
	if !errors.As(err, &me) {
		t.Fatalf("E 未返回 *Error")
	}
	if me.Code != CodeOutOfStock {
		t.Errorf("Code = %q, 要求 %q", me.Code, CodeOutOfStock)
	}
	if me.Retryable {
		t.Errorf("Retryable = true, 要求 false")
	}
	if me.Error() != "商品 SKU-1 库存不足" {
		t.Errorf("Error() = %q, 要求 %q", me.Error(), "商品 SKU-1 库存不足")
	}
	if got := HTTPStatus(err); got != 409 {
		t.Errorf("HTTPStatus = %d, 要求 409（MALL_20003）", got)
	}
	// 包装后仍可取回码
	if got := HTTPStatus(fmt.Errorf("下单: %w", err)); got != 409 {
		t.Errorf("包装后 HTTPStatus = %d, 要求 409", got)
	}
}
