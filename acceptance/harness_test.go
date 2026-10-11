//go:build acceptance

package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// 验收报告是交付物：必须**任何长度截断后都是合法 UTF-8**。
//
// 实测过的真实缺陷：报告第 1831 字节出现悬挂的 E6（一个汉字被切了一半），
// 导致严格 UTF-8 解码失败、编辑器打不开整个文件。根因是按字节切片，
// 而报告里掺的正是被测系统返回的中文文本。
func TestTruncateNeverSplitsRunes(t *testing.T) {
	samples := []string{
		"中文中文中文",
		"a中b文c",
		"emoji🙂结尾",
		"混合 english 与中文",
	}
	for _, s := range samples {
		for n := 0; n <= len(s)+2; n++ {
			got := truncate(s, n)
			if !utf8.ValidString(got) {
				t.Fatalf("truncate(%q, %d) 产出非法 UTF-8: %q", s, n, got)
			}
			if n > 0 && len(s) > n && !strings.HasSuffix(got, "…") {
				t.Fatalf("truncate(%q, %d) 应带省略号: %q", s, n, got)
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Fatalf("truncate(%q, %d) 含替换字符: %q", s, n, got)
			}
		}
	}
}

// 即使调用方塞进了非法字节，落盘的报告也必须能被打**开**（兜底 ToValidUTF8）。
func TestReportWriteAlwaysProducesValidUTF8(t *testing.T) {
	rep := newReport()
	rep.fact("故意掺入非法字节：%s", string([]byte{0xE6})) // 悬挂的多字节起始字节
	rep.add("AC-99", "含非法字节的证据", statusPass, "截断片段="+truncate("中文中文", 5))
	path := filepath.Join(t.TempDir(), "report.md")
	if err := rep.write(path); err != nil {
		t.Fatalf("写报告失败: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读报告失败: %v", err)
	}
	if !utf8.Valid(b) {
		t.Fatalf("报告不是合法 UTF-8（size=%d）", len(b))
	}
	if !strings.Contains(string(b), "AC-99") {
		t.Fatalf("报告内容缺失: %s", b)
	}
}
