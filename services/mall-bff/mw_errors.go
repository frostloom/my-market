// mw_errors.go：统一错误出口（api-contract §0.2：所有非 2xx 响应体
// {"error": "人话描述", "code": "MALL_xxxxx", "retryable": bool}）。
//
// 还原优先级（task-6 裁定 2）：
//  1. mallerr.Error 直接错误 → 用其码（bff 自产：参数校验/限流/验签等）；
//  2. 下游 gRPC 错误经 mallerr.FromGRPC 还原（ErrorInfo 载体携 MALL 码 + retryable，
//     message 为下游人话描述）——这正是载体存在的目的（审计 Important-1）；
//  3. 无载体 gRPC 错误：按 gRPC 码粗映射 HTTP（404/400/409/429/…），
//     契约码用 MALL_50001/50002/50003 兜底（真实下游均带载体，此路径仅防御）；
//  4. 其余错误（含 panic recovery）→ 500 MALL_50001 retryable=true。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frostloom/my-market/pkg/mallerr"
)

// gin context 键（中间件 → handler 传值）。
const (
	ctxKeyTenant uint8 = iota + 1
	ctxKeyUser
	ctxKeyToken
)

// abortWith handler 侧统一报错入口：挂错误、中止（状态码与错误体由
// errorShape 统一产出，禁止 handler 自行 WriteHeader）。
func abortWith(c *gin.Context, err error) {
	_ = c.Error(err)
	c.Abort()
}

// errShape 统一错误出口：c.Next() 之后取 c.Errors 最后一条，按上文优先级
// 映射并写错误体。无错误不干预（成功响应由 handler 自己写）。
func errShape(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if len(c.Errors) == 0 {
			return
		}
		err := c.Errors.Last().Err
		c.Errors = c.Errors[:0]
		writeErr(c, log, err)
	}
}

// recoverShape panic recovery：统一形状（500 MALL_50001 retryable=true）。
// 响应已写出时（panic 发生在写响应后）只记日志不再写体。
func recoverShape(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic", "path", c.Request.URL.Path, "err", r)
				if !c.Writer.Written() {
					c.Abort()
					writeErr(c, log, mallerr.E(mallerr.CodeInternal, false, "内部错误，请稍后重试"))
					return
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}

// writeErr 单点错误形状产出（见包注释优先级 1-4）。
func writeErr(c *gin.Context, log *slog.Logger, err error) {
	// 1) mallerr 直接错误（bff 自产：校验/限流/验签/网关映射）。
	var me *mallerr.Error
	if errors.As(err, &me) {
		respondErr(c, mallerr.HTTPStatus(err), me.Code, me.Retryable, me.Msg)
		return
	}

	// 2) 下游 gRPC 载体还原（ErrorInfo.Reason = MALL 码）。
	if code, retryable, ok := mallerr.FromGRPC(err); ok {
		httpStatus := 500
		if info, okInfo := mallerr.Codes[code]; okInfo {
			httpStatus = info.HTTP
		}
		msg := "下游错误"
		if st, okStatus := status.FromError(err); okStatus {
			msg = st.Message()
		}
		respondErr(c, httpStatus, code, retryable, msg)
		return
	}

	// 3) 无载体 gRPC 错误：按 gRPC 码粗映射 HTTP 状态，契约码 5xxx 兜底。
	st, isStatus := status.FromError(err)
	if isStatus {
		httpStatus, code := coarseMap(st.Code())
		log.Warn("downstream error without mallerr carrier", "grpc_code", st.Code().String(), "msg", st.Message())
		respondErr(c, httpStatus, code, retryableOf(code), st.Message())
		return
	}

	// 4) 非 gRPC 错误：ctx 超时 → 50002；其余 50001。
	if errors.Is(err, context.DeadlineExceeded) {
		respondErr(c, http.StatusGatewayTimeout, mallerr.CodeUpstreamTimeout, true, "下游超时，请重试")
		return
	}
	log.Error("unshaped error", "err", err)
	respondErr(c, http.StatusInternalServerError, mallerr.CodeInternal, true, "内部错误，请稍后重试")
}

// coarseMap gRPC 码 → HTTP 状态（无载体防御路径）；契约码同取 5xxx 兜底族。
func coarseMap(gc codes.Code) (int, mallerr.Code) {
	switch gc {
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout, mallerr.CodeUpstreamTimeout
	case codes.Unavailable:
		return http.StatusServiceUnavailable, mallerr.CodeUpstreamUnavailable
	case codes.NotFound:
		return http.StatusNotFound, mallerr.CodeInternal
	case codes.InvalidArgument:
		return http.StatusBadRequest, mallerr.CodeInternal
	case codes.FailedPrecondition, codes.AlreadyExists:
		return http.StatusConflict, mallerr.CodeInternal
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests, mallerr.CodeInternal
	case codes.Unauthenticated:
		return http.StatusUnauthorized, mallerr.CodeInternal
	case codes.PermissionDenied:
		return http.StatusForbidden, mallerr.CodeInternal
	default:
		return http.StatusInternalServerError, mallerr.CodeInternal
	}
}

// retryableOf 兜底码的 retryable（5xxx 族按契约全 true）。
func retryableOf(code mallerr.Code) bool {
	if info, ok := mallerr.Codes[code]; ok {
		return info.Retryable
	}
	return true
}

// respondErr 写统一错误体（api-contract §0.2）。
func respondErr(c *gin.Context, httpStatus int, code mallerr.Code, retryable bool, msg string) {
	c.JSON(httpStatus, gin.H{
		"error":     msg,
		"code":      string(code),
		"retryable": retryable,
	})
}
