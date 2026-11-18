// mallerr gRPC 载体：契约码在服务间（mall-bff ↔ mall-goods/mall-order 等）
// 的 gRPC 传输形态。HTTP 语义码只到 gRPC 码的一档（404→NotFound 等），
// 细粒度区分靠 errdetails.ErrorInfo.Reason 携带的 MALL 码与 metadata 的
// retryable——bff 侧 FromGRPC 还原后仍按 api-contract §1 映射对外错误体。
//
// 设计不变量（中期审计 Important-1）：
//   - 同 HTTP 语义码可区分：MALL_20001 与 MALL_20006 都是 404，但 Reason 不同；
//   - 5xxx 三态不坍缩：50001→Internal、50002→DeadlineExceeded、50003→Unavailable；
//   - 无载体优雅回落：非 mallerr 错误 ToGRPC 走现状行为（Internal+原文），
//     无 ErrorInfo 的 gRPC 错误 FromGRPC 返回 ok=false，调用方回落现状处理。
package mallerr

import (
	"errors"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcDomain ErrorInfo.Domain 固定值（bff 识别商城错误载体用）。
const grpcDomain = "mall"

// metadataRetryable ErrorInfo.Metadata 里 retryable 的键名与取值。
const (
	metadataRetryable      = "retryable"
	metadataRetryableTrue  = "true"
	metadataRetryableFalse = "false"
)

// grpcCodeFor HTTP 语义码 → gRPC 码（api-contract §1 各行 HTTP 列）。
// 映射原则：400 参数类→InvalidArgument；401→Unauthenticated；404→NotFound；
// 409 状态冲突→FailedPrecondition；429 限流→ResourceExhausted；
// 500/503/504 三态各归其位，不坍缩。表外码一律 Internal。
func grpcCodeFor(httpStatus int) codes.Code {
	switch httpStatus {
	case 400:
		return codes.InvalidArgument
	case 401:
		return codes.Unauthenticated
	case 403:
		return codes.PermissionDenied
	case 404:
		return codes.NotFound
	case 409:
		return codes.FailedPrecondition
	case 429:
		return codes.ResourceExhausted
	case 500:
		return codes.Internal
	case 503:
		return codes.Unavailable
	case 504:
		return codes.DeadlineExceeded
	default:
		return codes.Internal
	}
}

// ToGRPC 把错误映为 gRPC status 错误（服务端出口统一走这里）。
//   - mallerr 错误：gRPC 码按契约码的 HTTP 语义行取；details 附
//     errdetails.ErrorInfo{Reason: MALL 码, Domain: "mall",
//     Metadata: {retryable: "true"|"false"}}；message 为人话描述原文。
//   - 其他错误（无契约码）：现状行为——codes.Internal + err.Error() 原文。
//   - nil：返回 nil。
func ToGRPC(err error) error {
	if err == nil {
		return nil
	}
	var me *Error
	if !errors.As(err, &me) {
		return status.Error(codes.Internal, err.Error())
	}
	httpStatus := httpStatusFor(me)
	st := status.New(grpcCodeFor(httpStatus), me.Msg)
	withDetails, derr := st.WithDetails(&errdetails.ErrorInfo{
		Reason: string(me.Code),
		Domain: grpcDomain,
		Metadata: map[string]string{
			metadataRetryable: retryableFlag(me.Retryable),
		},
	})
	if derr != nil {
		// details 序列化失败不应吞错误本体：退回无载体 status（码不变）。
		return st.Err()
	}
	return withDetails.Err()
}

// retryableFlag bool → metadata 字符串（显式二值，避免空串歧义）。
func retryableFlag(b bool) string {
	if b {
		return metadataRetryableTrue
	}
	return metadataRetryableFalse
}

// httpStatusFor 取契约码的 HTTP 语义行；表外码按 500 处理（与 HTTPStatus 同口径）。
func httpStatusFor(me *Error) int {
	if info, ok := Codes[me.Code]; ok {
		return info.HTTP
	}
	return 500
}

// FromGRPC 从 gRPC 错误还原契约码与 retryable。
//   - 携带 mall 载体（ErrorInfo.Reason 形如 MALL_xxxxx）→ (码, retryable, true)；
//   - 其余（普通 status 错误、非 gRPC 错误、nil）→ ("", false, false)，
//     调用方优雅回落现状行为（按 gRPC 码/message 自行处理）。
func FromGRPC(err error) (code Code, retryable bool, ok bool) {
	if err == nil {
		return "", false, false
	}
	st, isStatus := status.FromError(err)
	if !isStatus {
		return "", false, false
	}
	for _, d := range st.Details() {
		ei, isInfo := d.(*errdetails.ErrorInfo)
		if !isInfo || !strings.HasPrefix(ei.Reason, "MALL_") {
			continue
		}
		retryable = ei.Metadata[metadataRetryable] == metadataRetryableTrue
		return Code(ei.Reason), retryable, true
	}
	return "", false, false
}
