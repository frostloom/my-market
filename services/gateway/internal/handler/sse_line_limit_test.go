package handler

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

// readSSELine 语义回归：普通行、无换行 EOF 尾行、超长行封顶。
func TestReadSSELineSemantics(t *testing.T) {
	// 普通行（含 \n）
	r := bufio.NewReader(strings.NewReader("data: a\n\ndata: b\n"))
	line, err := readSSELine(r, maxSSELine)
	if err != nil || string(line) != "data: a\n" {
		t.Fatalf("line = %q err = %v, want %q", line, err, "data: a\n")
	}
	// 空行
	line, err = readSSELine(r, maxSSELine)
	if err != nil || string(line) != "\n" {
		t.Fatalf("empty line = %q err = %v", line, err)
	}
	// 尾行完整（含换行）：正常返回无错
	line, err = readSSELine(r, maxSSELine)
	if err != nil || string(line) != "data: b\n" {
		t.Fatalf("third line = %q err = %v", line, err)
	}
	// 读尽后再读：空 + io.EOF
	line, err = readSSELine(r, maxSSELine)
	if err != io.EOF || len(line) != 0 {
		t.Fatalf("past EOF = %q err = %v, want empty+EOF", line, err)
	}
	// 无换行的尾行：返回已读部分 + io.EOF（与 ReadBytes 语义一致）
	r1b := bufio.NewReader(strings.NewReader("data: c"))
	line, err = readSSELine(r1b, maxSSELine)
	if err != io.EOF || string(line) != "data: c" {
		t.Fatalf("partial tail = %q err = %v, want data: c + EOF", line, err)
	}

	// 超长行（不含换行，跨多个 bufio 缓冲）：封顶报错而不是无限累积
	huge := bytes.Repeat([]byte("x"), maxSSELine+4096)
	r2 := bufio.NewReader(bytes.NewReader(huge))
	line, err = readSSELine(r2, maxSSELine)
	if err != errSSELineTooLong {
		t.Fatalf("oversized line err = %v, want errSSELineTooLong", err)
	}
	if len(line) != 0 {
		t.Fatalf("oversized line should not return accumulated bytes, got %d", len(line))
	}

	// 恰好在上限内的行正常返回（边界）
	boundary := append(bytes.Repeat([]byte("y"), maxSSELine-1), '\n')
	r3 := bufio.NewReader(bytes.NewReader(boundary))
	line, err = readSSELine(r3, maxSSELine)
	if err != nil || len(line) != maxSSELine {
		t.Fatalf("boundary line err = %v len = %d, want %d", err, len(line), maxSSELine)
	}
}

func TestIsInternalUpstream(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"localhost", true},
		{"::1", true},
		{"10.1.2.3", true},
		{"192.168.1.5", true},
		{"172.16.0.9", true},
		{"api.openai.com", false}, // 主机名未解析 → 保守按公网
		{"8.8.8.8", false},
		{"1.2.3.4", false},
		{"169.254.1.1", true},
	}
	for _, tc := range cases {
		if got := isInternalUpstream(tc.host); got != tc.want {
			t.Errorf("isInternalUpstream(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
