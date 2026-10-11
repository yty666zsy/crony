package service

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// FileLogSource 节点本机日志文件数据源。
//
// 为什么节点侧才有：诊断/巡检最有价值的证据往往是**本机文件日志**（服务崩溃栈、
// 调度器自己的运行日志），它们不在数据库里。只有 node 进程能读本机文件。
//
// 安全与成本边界（都在这里硬编码，不交给模型）：
//   - 只读最近修改过的有限个文件（MaxFiles），只在文件尾部 N 字节内搜（MaxBytesPerFile）；
//   - 拒绝符号链接与目录逃逸：只读 Root 下的直接文件；
//   - 单次检索有总行数上限，避免把磁盘日志整体读进内存。
type FileLogSource struct {
	// Roots 允许读取的目录（例如日志目录）。
	Roots []string
	// MaxFiles 单次检索最多检查多少个文件（按修改时间倒序）。
	MaxFiles int
	// MaxBytesPerFile 单文件最多从尾部读取多少字节。
	MaxBytesPerFile int
	// MaxHits 单次检索最多返回多少行。
	MaxHits int
}

func (s *FileLogSource) maxFiles() int {
	if s.MaxFiles > 0 {
		return s.MaxFiles
	}
	return 20
}

func (s *FileLogSource) maxBytes() int {
	if s.MaxBytesPerFile > 0 {
		return s.MaxBytesPerFile
	}
	return 512 << 10
}

func (s *FileLogSource) maxHits() int {
	if s.MaxHits > 0 {
		return s.MaxHits
	}
	return 100
}

type logFile struct {
	path    string
	modTime time.Time
}

// SearchLogs 实现 tools.LogSource。
func (s *FileLogSource) SearchLogs(ctx context.Context, q tools.LogQuery) ([]tools.LogHit, error) {
	hits := make([]tools.LogHit, 0, 16)
	if len(s.Roots) == 0 {
		return hits, nil
	}
	keyword := strings.TrimSpace(q.Keyword)
	// 没有关键字就不搜：文件日志没法像数据库那样按 job_id 过滤，
	// 全量回读磁盘日志既慢又会把噪声当成证据。
	if keyword == "" {
		return hits, nil
	}
	since := time.Now().Add(-time.Duration(q.SinceMinutes) * time.Minute)

	files := s.recentFiles(since)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return hits, err
		}
		lines, err := tailLines(f.path, s.maxBytes())
		if err != nil {
			continue // 单个文件不可读（权限/被轮转）不影响整体检索
		}
		for i, line := range lines {
			if !strings.Contains(line, keyword) {
				continue
			}
			hits = append(hits, tools.LogHit{
				Source:    "file",
				Ref:       fmt.Sprintf("%s:%d", f.path, i+1),
				Line:      llm.Truncate(llm.Redact(line), 500),
				Timestamp: f.modTime,
			})
			if len(hits) >= s.maxHits() || len(hits) >= q.Limit {
				return hits, nil
			}
		}
	}
	return hits, nil
}

// recentFiles 收集最近修改过的常规文件（按修改时间倒序）。
func (s *FileLogSource) recentFiles(since time.Time) []logFile {
	files := make([]logFile, 0, 16)
	for _, root := range s.Roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			// 符号链接不跟随：避免用日志目录当跳板读到别处的文件。
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(since) {
				continue
			}
			files = append(files, logFile{
				path:    filepath.Join(root, e.Name()),
				modTime: info.ModTime(),
			})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	if len(files) > s.maxFiles() {
		files = files[:s.maxFiles()]
	}
	return files
}

// tailLines 读取文件尾部 maxBytes 字节并按行切分（首行可能是半行，直接丢弃）。
func tailLines(path string, maxBytes int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	start := int64(0)
	if size > int64(maxBytes) {
		start = size - int64(maxBytes)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}

	lines := make([]string, 0, 128)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	first := start > 0
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil && err != bufio.ErrTooLong {
		return lines, err
	}
	return lines, nil
}

var _ tools.LogSource = (*FileLogSource)(nil)
