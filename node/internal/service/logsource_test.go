package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/tools"
)

// TestFileLogSourceBasic 本机日志检索：命中行必须能定位到"文件:行号"。
func TestFileLogSourceBasic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.log"), "start\nall good\nconnection refused to 10.0.0.9:3306\ndone\n")
	writeFile(t, filepath.Join(dir, "other.log"), "nothing interesting\n")

	src := &FileLogSource{Roots: []string{dir}}
	hits, err := src.SearchLogs(context.Background(), tools.LogQuery{Keyword: "refused", SinceMinutes: 60, Limit: 50})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("应命中 1 行，实际 %d：%+v", len(hits), hits)
	}
	if !strings.Contains(hits[0].Ref, "app.log:3") {
		t.Errorf("证据定位应包含文件与行号，实际 %q", hits[0].Ref)
	}
	if !strings.Contains(hits[0].Line, "connection refused") {
		t.Errorf("命中行内容不对：%q", hits[0].Line)
	}
}

// TestFileLogSourceRequiresKeyword 无关键字不检索：文件日志没有 job_id 维度，
// 全量回读磁盘日志既慢又会把噪声当成证据。
func TestFileLogSourceRequiresKeyword(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.log"), "line\n")
	src := &FileLogSource{Roots: []string{dir}}
	hits, err := src.SearchLogs(context.Background(), tools.LogQuery{Keyword: "  ", SinceMinutes: 60, Limit: 50})
	if err != nil {
		t.Fatalf("检索不应报错: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("无关键字应返回空结果，实际 %d 条", len(hits))
	}
}

// TestFileLogSourceSkipsStale 过期文件不读：日志检索按时间窗收敛，
// 否则"最近一小时"的检索会把上周的旧日志当成当前证据。
func TestFileLogSourceSkipsStale(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "stale.log")
	writeFile(t, stale, "refused old\n")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("设置修改时间失败: %v", err)
	}
	writeFile(t, filepath.Join(dir, "fresh.log"), "refused new\n")

	src := &FileLogSource{Roots: []string{dir}}
	hits, err := src.SearchLogs(context.Background(), tools.LogQuery{Keyword: "refused", SinceMinutes: 60, Limit: 50})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	var fresh, staleHit bool
	for _, h := range hits {
		if strings.Contains(h.Ref, "stale.log") {
			staleHit = true
		}
		if strings.Contains(h.Ref, "fresh.log") {
			fresh = true
		}
	}
	if staleHit {
		t.Error("过期文件不应被读取")
	}
	if !fresh {
		t.Error("窗口内的文件必须被读取（否则检索会静默漏证据）")
	}
}

// TestFileLogSourceSkipsSymlink 符号链接不跟随：避免用日志目录当跳板读到别处的文件。
func TestFileLogSourceSkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.log")
	writeFile(t, outside, "refused secret\n")
	if err := os.Symlink(outside, filepath.Join(dir, "link.log")); err != nil {
		t.Skipf("当前环境不支持创建符号链接，跳过：%v", err)
	}
	src := &FileLogSource{Roots: []string{dir}}
	hits, err := src.SearchLogs(context.Background(), tools.LogQuery{Keyword: "refused", SinceMinutes: 60, Limit: 50})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	for _, h := range hits {
		if strings.Contains(h.Ref, "link.log") || strings.Contains(h.Ref, "secret.log") {
			t.Errorf("符号链接不应被跟随：%s", h.Ref)
		}
	}
}

// TestFileLogSourceTailsBigFile 大文件只读尾部，避免把整个文件读进内存。
func TestFileLogSourceTailsBigFile(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("refused early\n")
	for i := 0; i < 20000; i++ {
		b.WriteString("noise line that is long enough to fill the buffer\n")
	}
	b.WriteString("refused tail\n")
	writeFile(t, filepath.Join(dir, "big.log"), b.String())

	src := &FileLogSource{Roots: []string{dir}, MaxBytesPerFile: 4096}
	hits, err := src.SearchLogs(context.Background(), tools.LogQuery{Keyword: "refused", SinceMinutes: 60, Limit: 50})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("只读尾部时应只命中尾部那一行，实际 %d 条：%+v", len(hits), hits)
	}
	if !strings.Contains(hits[0].Line, "refused tail") {
		t.Errorf("应命中文件尾部，实际 %q", hits[0].Line)
	}
}

func TestTailLinesKeepsWholeFileWhenSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "small.log")
	writeFile(t, path, "a\nb\nc\n")
	lines, err := tailLines(path, 1<<20)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(lines) != 3 || lines[0] != "a" || lines[2] != "c" {
		t.Fatalf("小文件应完整读取，实际 %v", lines)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
}
