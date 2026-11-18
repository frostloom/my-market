package mallpage

import (
	"testing"
	"time"
)

// TestCursorRoundtrip 编码→解码还原（键集 (created_at,id)，毫秒精度同 MySQL DATETIME(3)）。
func TestCursorRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		id   uint64
	}{
		{"zero-ish earliest", time.Unix(0, 0).UTC(), 1},
		{"millisecond precision", time.Unix(1700000000, 123000000).UTC(), 42},
		{"max id", time.Unix(1799999999, 999000000).UTC(), ^uint64(0)},
		{"id 0", time.Unix(1600000000, 500000000).UTC(), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := EncodeCursor(tc.at, tc.id)
			gotAt, gotID, err := DecodeCursor(cur)
			if err != nil {
				t.Fatalf("DecodeCursor(%q) err = %v, want nil", cur, err)
			}
			if !gotAt.Equal(tc.at) {
				t.Fatalf("time = %v, want %v", gotAt, tc.at)
			}
			if gotID != tc.id {
				t.Fatalf("id = %d, want %d", gotID, tc.id)
			}
		})
	}
}

// TestDecodeCursor_Garbage 非 Base64 与合法 Base64 但格式错 → 一律报错（服务层映 MALL_10005）。
func TestDecodeCursor_Garbage(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"not base64", "!!!not-base64"},
		{"valid base64, wrong format", "YWJj"}, // "abc"：无逗号分隔
		{"base64 but bad nano", "MTIzLHg="},    // "123,x"：时间戳非整数
		{"empty", ""},
		{"three fields", "MSwyLDM="}, // "1,2,3"
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at, id, err := DecodeCursor(tc.in)
			if err == nil {
				t.Fatalf("DecodeCursor(%q) = (%v,%d), want error", tc.in, at, id)
			}
		})
	}
}

// TestEncodeCursor_StableOrdering 同输入同输出（游标可跨请求稳定复用）。
func TestEncodeCursor_StableOrdering(t *testing.T) {
	at := time.Unix(1700000000, 123000000).UTC()
	a := EncodeCursor(at, 42)
	b := EncodeCursor(at, 42)
	if a != b {
		t.Fatalf("EncodeCursor unstable: %q vs %q", a, b)
	}
	// 不同 id 必须产出不同游标（键集不含 id 会丢行/重行）
	if a == EncodeCursor(at, 43) {
		t.Fatalf("cursor collides across ids: %q", a)
	}
}
