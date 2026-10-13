package handler

import (
	"testing"
	"unicode/utf8"
)

// TestTruncateTextMultiByteNoPanic 回归测试：中文等多字节文本在
// len(s) > limit 但 rune 数 <= limit 时，旧实现 runes[:limit] 会 panic。
func TestTruncateTextMultiByteNoPanic(t *testing.T) {
	// 600 个汉字 = 1800 字节 > 1000，rune 数 600 <= 1000（旧实现必然越界 panic）。
	s := ""
	for i := 0; i < 600; i++ {
		s += "汉"
	}
	got := truncateText(s, 1000)
	if len(got) > 1000 {
		t.Fatalf("结果超过字节上限: len=%d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("结果不是合法 UTF-8（切坏了 rune）")
	}
	if got == "" {
		t.Fatalf("不应截断成空串")
	}
}

// TestTruncateTextBoundaries 校验常规边界语义：limit<=0 返回空串；未超限原样返回；
// 超限时结果 <= limit 且不切断 rune。
func TestTruncateTextBoundaries(t *testing.T) {
	if got := truncateText("abc", 0); got != "" {
		t.Fatalf("limit=0 应返回空串，得到 %q", got)
	}
	if got := truncateText("abc", -1); got != "" {
		t.Fatalf("limit<0 应返回空串，得到 %q", got)
	}
	if got := truncateText("abc", 3); got != "abc" {
		t.Fatalf("恰好等于上限应原样返回，得到 %q", got)
	}
	if got := truncateText("abcdef", 10); got != "abcdef" {
		t.Fatalf("未超限应原样返回，得到 %q", got)
	}
	// 纯 ASCII 截断
	if got := truncateText("abcdef", 3); got != "abc" {
		t.Fatalf("ASCII 截断应为 abc，得到 %q", got)
	}
	// 汉字每次 3 字节：limit=8 → 最多 2 个汉字（6 字节），不会切出第 3 个的半个 rune
	got := truncateText("汉字汉字", 8)
	if !utf8.ValidString(got) {
		t.Fatalf("多字节截断结果非法 UTF-8: %q", got)
	}
	if len(got) > 8 {
		t.Fatalf("超上限: len=%d %q", len(got), got)
	}
	if got != "汉字" {
		t.Fatalf("期望 2 个汉字（6 字节），得到 %q", got)
	}
}
