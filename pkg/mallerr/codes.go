// Package mallerr 定义 my-market 的错误码（docs/spec/api-contract.md §1）、
// 错误构造与码→HTTP 映射。App 可见的错误体统一为
// {"error": "人话描述", "code": "MALL_xxxxx", "retryable": bool}。
package mallerr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code 是契约错误码，形如 "MALL_10001"。
type Code string

// 契约错误码（api-contract §1 全表 27 码，1xxxx 鉴权 / 2xxxx 商品购物车 /
// 3xxxx 订单支付 / 4xxxx 秒杀 / 5xxxx 系统）。
const (
	CodeUnauthenticated       Code = "MALL_10001" // 未登录 / 会话无效
	CodeSessionExpired        Code = "MALL_10002" // 会话过期（重新登录）
	CodeUsernameTaken         Code = "MALL_10003" // 用户名已被占用
	CodeBadCredentials        Code = "MALL_10004" // 用户名或密码错误
	CodeInvalidParam          Code = "MALL_10005" // 参数校验失败
	CodeRateLimited           Code = "MALL_10006" // 请求过于频繁
	CodeGoodsNotFound         Code = "MALL_20001" // 商品不存在
	CodeGoodsOffSale          Code = "MALL_20002" // 商品已下架
	CodeOutOfStock            Code = "MALL_20003" // 库存不足
	CodeCartRowsExceeded      Code = "MALL_20004" // 购物车行数超上限（20）
	CodeGoodsQtyExceeded      Code = "MALL_20005" // 商品数量超限（qty>99 或 <1）
	CodeCategoryNotFound      Code = "MALL_20006" // 品类不存在
	CodeOrderNotFound         Code = "MALL_30001" // 订单不存在
	CodeOrderStateInvalid     Code = "MALL_30002" // 订单状态不允许该操作
	CodeIdempotencyConflict   Code = "MALL_30003" // 幂等键冲突
	CodePaymentNotFound       Code = "MALL_30004" // 支付单不存在
	CodeCallbackSignInvalid   Code = "MALL_30005" // 回调验签失败
	CodeGoodsTypeNotAllowed   Code = "MALL_30007" // 订单含不可购类型组合
	CodeFulfillmentInProgress Code = "MALL_30008" // 履约进行中，稍后再试
	CodeSeckillNotOpen        Code = "MALL_40001" // 秒杀活动未开始/已结束
	CodeSeckillSoldOut        Code = "MALL_40002" // 已抢完
	CodeSeckillDuplicate      Code = "MALL_40003" // 不可重复抢购
	CodeSeckillRateLimited    Code = "MALL_40004" // 秒杀限流（令牌桶）
	CodeVoucherNotFound       Code = "MALL_40005" // 抢购凭证不存在
	CodeInternal              Code = "MALL_50001" // 内部错误
	CodeUpstreamTimeout       Code = "MALL_50002" // 下游超时
	CodeUpstreamUnavailable   Code = "MALL_50003" // 下游不可用
)

// Codes 是契约错误码全表：码 → HTTP 状态码与默认可重试标记。
// 变更唯一依据：docs/spec/api-contract.md §1。
var Codes = map[Code]struct {
	HTTP      int
	Retryable bool
}{
	CodeUnauthenticated:       {HTTP: http.StatusUnauthorized, Retryable: false},
	CodeSessionExpired:        {HTTP: http.StatusUnauthorized, Retryable: false},
	CodeUsernameTaken:         {HTTP: http.StatusConflict, Retryable: false},
	CodeBadCredentials:        {HTTP: http.StatusUnauthorized, Retryable: false},
	CodeInvalidParam:          {HTTP: http.StatusBadRequest, Retryable: false},
	CodeRateLimited:           {HTTP: http.StatusTooManyRequests, Retryable: true},
	CodeGoodsNotFound:         {HTTP: http.StatusNotFound, Retryable: false},
	CodeGoodsOffSale:          {HTTP: http.StatusConflict, Retryable: false},
	CodeOutOfStock:            {HTTP: http.StatusConflict, Retryable: false},
	CodeCartRowsExceeded:      {HTTP: http.StatusConflict, Retryable: false},
	CodeGoodsQtyExceeded:      {HTTP: http.StatusBadRequest, Retryable: false},
	CodeCategoryNotFound:      {HTTP: http.StatusNotFound, Retryable: false},
	CodeOrderNotFound:         {HTTP: http.StatusNotFound, Retryable: false},
	CodeOrderStateInvalid:     {HTTP: http.StatusConflict, Retryable: false},
	CodeIdempotencyConflict:   {HTTP: http.StatusConflict, Retryable: false},
	CodePaymentNotFound:       {HTTP: http.StatusNotFound, Retryable: false},
	CodeCallbackSignInvalid:   {HTTP: http.StatusBadRequest, Retryable: false},
	CodeGoodsTypeNotAllowed:   {HTTP: http.StatusBadRequest, Retryable: false},
	CodeFulfillmentInProgress: {HTTP: http.StatusConflict, Retryable: true},
	CodeSeckillNotOpen:        {HTTP: http.StatusConflict, Retryable: false},
	CodeSeckillSoldOut:        {HTTP: http.StatusConflict, Retryable: false},
	CodeSeckillDuplicate:      {HTTP: http.StatusConflict, Retryable: false},
	CodeSeckillRateLimited:    {HTTP: http.StatusTooManyRequests, Retryable: true},
	CodeVoucherNotFound:       {HTTP: http.StatusNotFound, Retryable: false},
	CodeInternal:              {HTTP: http.StatusInternalServerError, Retryable: true},
	CodeUpstreamTimeout:       {HTTP: http.StatusGatewayTimeout, Retryable: true},
	CodeUpstreamUnavailable:   {HTTP: http.StatusServiceUnavailable, Retryable: true},
}

// Error 是携带契约码的错误。用 errors.As 取回码与 retryable。
type Error struct {
	Code      Code
	Retryable bool
	Msg       string
}

func (e *Error) Error() string { return e.Msg }

// E 构造携带码的错误。format 为"人话描述"（直接进入错误体的 error 字段）。
// retryable 缺省取 Codes 表默认值；显式传入可按现场覆盖。
func E(code Code, retryable bool, format string, a ...any) error {
	return &Error{
		Code:      code,
		Retryable: retryable || Codes[code].Retryable,
		Msg:       fmt.Sprintf(format, a...),
	}
}

// HTTPStatus 把错误映射为 HTTP 状态码（api-contract §1）。
// 非 *Error、无码错误或表外码一律 500。
func HTTPStatus(err error) int {
	var me *Error
	if errors.As(err, &me) {
		if info, ok := Codes[me.Code]; ok {
			return info.HTTP
		}
	}
	return http.StatusInternalServerError
}
