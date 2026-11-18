// Package mallpage 提供商城键集游标分页的游标编解码（api-contract §0.3）。
// 纯函数、无外部状态：游标 = Base64("<unixnano>,<id>")，mall-goods 与 bff 共用。
// 键集以 (created_at,id) 复合序推进：时间戳毫秒精度（MySQL DATETIME(3)）内
// 同毫秒多行靠 id 区分，单独用时间会丢行/重行。
package mallpage

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrBadCursor 非法游标（非 Base64、格式错、字段溢出）。
// 服务层映契约错误码 MALL_10005（参数校验失败）。
var ErrBadCursor = errors.New("mallpage: bad cursor")

// EncodeCursor 把键集位点（created_at 纳秒 + 行 id）编码为不透明游标串。
func EncodeCursor(t time.Time, id uint64) string {
	payload := fmt.Sprintf("%d,%d", t.UnixNano(), id)
	return base64.StdEncoding.EncodeToString([]byte(payload))
}

// DecodeCursor 还原游标位点。任何解析失败一律返回 ErrBadCursor
// （包裹原始错误以便排查），调用方无须区分失败原因。
func DecodeCursor(s string) (time.Time, uint64, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("%w: not base64: %v", ErrBadCursor, err)
	}
	parts := strings.Split(string(raw), ",")
	if len(parts) != 2 {
		return time.Time{}, 0, fmt.Errorf("%w: want <nano>,<id>, got %d fields", ErrBadCursor, len(parts))
	}
	nano, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("%w: bad nano: %v", ErrBadCursor, err)
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("%w: bad id: %v", ErrBadCursor, err)
	}
	return time.Unix(0, nano), id, nil
}
