package nodestatus

import (
	"math"
	"strings"
	"testing"
	"time"
)

// sampleProcStat 是真实 /proc/stat 的片段（8 核机器，保留 cpu0/cpu1 与尾部非 cpu 行），
// 用来验证"只认 `cpu` 汇总行，不会误取 cpu0"，以及 guest/guest_nice 不计入总量。
const sampleProcStat = `cpu  123456 789 12345 9876543 45678 0 1234 0 0 0
cpu0 61728 394 6172 4938271 22839 0 617 0 0 0
cpu1 61728 395 6173 4938272 22839 0 617 0 0 0
intr 12345678 0 0 0
ctxt 87654321
btime 1700000000
processes 12345
procs_running 2
procs_blocked 0
softirq 1234567 0 0 0
`

func TestParseProcStat(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantTotal uint64
		wantIdle  uint64
	}{
		{
			name:  "真实片段_只取cpu汇总行_guest不计入",
			input: sampleProcStat,
			// user+nice+system+idle+iowait+irq+softirq+steal
			wantTotal: 123456 + 789 + 12345 + 9876543 + 45678 + 0 + 1234 + 0,
			// idle + iowait：iowait 期间 CPU 在等 IO，算空闲
			wantIdle: 9876543 + 45678,
		},
		{
			name:      "只有前四个字段_老内核风格",
			input:     "cpu 100 0 100 800\n",
			wantTotal: 1000,
			wantIdle:  800,
		},
		{
			name: "含_iowait_与_guest",
			// guest=111、guest_nice=222 已被 user/nice 包含，不能再计入总量
			input:     "cpu 1000 200 300 5000 400 10 20 30 111 222\n",
			wantTotal: 1000 + 200 + 300 + 5000 + 400 + 10 + 20 + 30,
			wantIdle:  5000 + 400,
		},
		{
			name: "空输入",
			// 解析不出东西就必须是 0，由调用方当成采集失败
			input:     "",
			wantTotal: 0,
			wantIdle:  0,
		},
		{
			name:      "只有空白行",
			input:     "\n\n   \n",
			wantTotal: 0,
			wantIdle:  0,
		},
		{
			name:      "字段不足_只有user和nice",
			input:     "cpu 10 20\n",
			wantTotal: 30,
			wantIdle:  0,
		},
		{
			name: "非数字字段后截断",
			// 遇到 "x" 就停止累加：宁可少算也不猜，总量保持 30
			input:     "cpu 10 20 x 40 50\n",
			wantTotal: 30,
			wantIdle:  0,
		},
		{
			name:      "只有cpu0_没有cpu汇总行",
			input:     "cpu0 10 20 30 40\n",
			wantTotal: 0,
			wantIdle:  0,
		},
		{
			name:      "cpu行无字段",
			input:     "cpu\ncpu0 1 2 3 4\n",
			wantTotal: 0,
			wantIdle:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, idle := parseProcStat([]byte(tt.input))
			if total != tt.wantTotal || idle != tt.wantIdle {
				t.Fatalf("parseProcStat() = (total=%d, idle=%d)，期望 (total=%d, idle=%d)",
					total, idle, tt.wantTotal, tt.wantIdle)
			}
		})
	}
}

// sampleMemInfo 是真实 /proc/meminfo 的片段：含 MemAvailable，且带 SwapCached 干扰项。
const sampleMemInfo = `MemTotal:       16333804 kB
MemFree:         1234567 kB
MemAvailable:    9876543 kB
Buffers:          123456 kB
Cached:          4567890 kB
SwapCached:       111111 kB
SwapTotal:       2097148 kB
SwapFree:        2097148 kB
`

func TestParseMemInfo(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantTotalKB uint64
		wantAvailKB uint64
	}{
		{
			name:        "真实片段_优先MemAvailable_忽略SwapCached",
			input:       sampleMemInfo,
			wantTotalKB: 16333804,
			wantAvailKB: 9876543,
		},
		{
			name: "缺少MemAvailable_回退MemFree加Buffers加Cached",
			input: `MemTotal:        1000000 kB
MemFree:          200000 kB
Buffers:           50000 kB
Cached:           300000 kB
`,
			wantTotalKB: 1000000,
			wantAvailKB: 550000,
		},
		{
			name: "MemAvailable为0时同样回退",
			input: `MemTotal:        1000000 kB
MemFree:             100 kB
MemAvailable:          0 kB
Buffers:             200 kB
Cached:              300 kB
`,
			wantTotalKB: 1000000,
			wantAvailKB: 600,
		},
		{
			name:        "空输入",
			input:       "",
			wantTotalKB: 0,
			wantAvailKB: 0,
		},
		{
			name:        "无kB后缀也能解析",
			input:       "MemTotal: 500\nMemFree: 100\n",
			wantTotalKB: 500,
			wantAvailKB: 100,
		},
		{
			name: "跳过垃圾行与无冒号行",
			input: `garbage line without colon
MemTotal: 42 kB
: 999 kB
MemFree: 7 kB
`,
			wantTotalKB: 42,
			wantAvailKB: 7,
		},
		{
			name: "多行_后面的重复键覆盖前面的",
			input: `MemTotal: 1 kB
MemTotal: 2048 kB
MemAvailable: 1024 kB
`,
			wantTotalKB: 2048,
			wantAvailKB: 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			totalKB, availKB := parseMemInfo([]byte(tt.input))
			if totalKB != tt.wantTotalKB || availKB != tt.wantAvailKB {
				t.Fatalf("parseMemInfo() = (total=%d, avail=%d)，期望 (total=%d, avail=%d)",
					totalKB, availKB, tt.wantTotalKB, tt.wantAvailKB)
			}
		})
	}
}

func TestParseLoadAvg(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "真实片段_只取前三个字段",
			input: "0.52 0.58 0.59 2/1234 56789\n",
			want:  "0.52 0.58 0.59",
		},
		{
			name:  "不足三个字段_返回已有的部分",
			input: "1.00 2.00\n",
			want:  "1.00 2.00",
		},
		{
			name:  "空输入返回空串而不是panic",
			input: "",
			want:  "",
		},
		{
			name:  "单个字段",
			input: "0.15\n",
			want:  "0.15",
		},
		{
			name:  "多余空白被归一",
			input: "  0.10   0.20   0.30  \n",
			want:  "0.10 0.20 0.30",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseLoadAvg([]byte(tt.input)); got != tt.want {
				t.Fatalf("parseLoadAvg() = %q，期望 %q", got, tt.want)
			}
		})
	}
}

func TestPercentFromDelta(t *testing.T) {
	tests := []struct {
		name       string
		totalDelta uint64
		idleDelta  uint64
		want       float64
	}{
		{"总增量为0_无法判断", 0, 0, 0},
		{"总增量为0但空闲不为0", 0, 5, 0},
		{"全忙", 1000, 0, 100},
		{"空闲400of1000_占用60", 1000, 400, 60},
		{"全空闲", 1000, 1000, 0},
		{"空闲大于总量_计数器回绕_当作全忙", 1000, 1200, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentFromDelta(tt.totalDelta, tt.idleDelta); got != tt.want {
				t.Fatalf("percentFromDelta(%d, %d) = %v，期望 %v", tt.totalDelta, tt.idleDelta, got, tt.want)
			}
		})
	}
}

func TestPercentFrom(t *testing.T) {
	tests := []struct {
		name  string
		used  uint64
		total uint64
		want  float64
	}{
		{"总量为0_采集不到", 50, 0, 0},
		{"刚好一半", 50, 100, 50},
		{"用量为0", 0, 100, 0},
		{"用量超过总量被clamp到100", 200, 100, 100},
		{"三分之一", 1, 3, 100.0 / 3.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := percentFrom(tt.used, tt.total)
			// 乘除顺序不同会让最后一位不同（33.33333333333333 vs 100/3），
			// 用容差比较，别把浮点表示差异当成缺陷。
			if math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("percentFrom(%d, %d) = %v，期望 %v", tt.used, tt.total, got, tt.want)
			}
		})
	}
}

func TestSnapshotToMetric(t *testing.T) {
	collected := time.Unix(1700000000, 123000000)

	t.Run("字段映射与清洗", func(t *testing.T) {
		snap := Snapshot{
			CPUPercent:  33.33333,   // 必须被 round2 成 33.33
			MemPercent:  150,        // >100 必须被 clamp 到 100
			DiskPercent: math.NaN(), // NaN 必须落到 0，不能进数据库
			LoadAvg:     "  0.52 0.58 0.59  ",
			CPUCores:    8,
			MemTotalMB:  16000,
			MemUsedMB:   8000,
			DiskTotalMB: 500000,
			DiskFreeMB:  100000,
			DiskPath:    "  C:\\  ",
		}

		m := snap.ToMetric("  node-1  ", "  host-a  ", " v1.0 ", 3600, 3, collected)

		if m.NodeUUID != "node-1" {
			t.Errorf("NodeUUID = %q，期望 %q", m.NodeUUID, "node-1")
		}
		if m.Hostname != "host-a" {
			t.Errorf("Hostname = %q，期望 %q", m.Hostname, "host-a")
		}
		if m.Version != "v1.0" {
			t.Errorf("Version = %q，期望 %q", m.Version, "v1.0")
		}
		if m.CPUPercent != 33.33 {
			t.Errorf("CPUPercent = %v，期望 33.33（round2）", m.CPUPercent)
		}
		if m.MemPercent != 100 {
			t.Errorf("MemPercent = %v，期望 100（clamp >100）", m.MemPercent)
		}
		if m.DiskPercent != 0 {
			t.Errorf("DiskPercent = %v，期望 0（NaN 归零）", m.DiskPercent)
		}
		if m.LoadAvg != "0.52 0.58 0.59" {
			t.Errorf("LoadAvg = %q，期望去空白后的 %q", m.LoadAvg, "0.52 0.58 0.59")
		}
		if m.DiskPath != `C:\` {
			t.Errorf("DiskPath = %q，期望 %q", m.DiskPath, `C:\`)
		}
		if m.CPUCores != 8 || m.MemTotalMB != 16000 || m.MemUsedMB != 8000 ||
			m.DiskTotalMB != 500000 || m.DiskFreeMB != 100000 {
			t.Errorf("容量字段映射错误: cores=%d mem=%d/%d disk=%d/%d",
				m.CPUCores, m.MemUsedMB, m.MemTotalMB, m.DiskFreeMB, m.DiskTotalMB)
		}
		if m.RunningJobs != 3 {
			t.Errorf("RunningJobs = %d，期望 3", m.RunningJobs)
		}
		if m.UptimeSec != 3600 {
			t.Errorf("UptimeSec = %d，期望 3600", m.UptimeSec)
		}
		// 单位是 Unix **秒**：读取方 admin/internal/service/nodestatus.go 用
		// time.Unix(collected_at, 0) 还原，集成用例也按秒断言。
		if m.CollectedAt != collected.Unix() {
			t.Errorf("CollectedAt = %d，期望 %d（Unix 秒）", m.CollectedAt, collected.Unix())
		}
		if m.TableName() != "node_metric" {
			t.Errorf("TableName() = %q，期望 node_metric", m.TableName())
		}
	})

	t.Run("零值时间戳回填当前时间", func(t *testing.T) {
		before := time.Now().Unix()
		m := Snapshot{}.ToMetric("n", "h", "v", 0, 0, time.Time{})
		after := time.Now().Unix()
		if m.CollectedAt < before || m.CollectedAt > after {
			t.Fatalf("CollectedAt = %d，不在 [%d, %d] 内", m.CollectedAt, before, after)
		}
	})

	t.Run("超长字段被clip_负百分比归零", func(t *testing.T) {
		longHost := strings.Repeat("h", 100)
		longPath := strings.Repeat("p", 300)
		m := Snapshot{
			CPUPercent:  -1,
			MemPercent:  -0.001,
			DiskPercent: 99.999,
			DiskPath:    longPath,
		}.ToMetric(strings.Repeat("u", 200), longHost, strings.Repeat("v", 100), 0, 0, collected)

		if m.CPUPercent != 0 || m.MemPercent != 0 {
			t.Errorf("负百分比应归零: cpu=%v mem=%v", m.CPUPercent, m.MemPercent)
		}
		if m.DiskPercent != 100 {
			t.Errorf("DiskPercent = %v，期望 100（clamp + round2）", m.DiskPercent)
		}
		if len(m.Hostname) != 64 || len(m.Version) != 64 || len(m.DiskPath) != 255 {
			t.Errorf("clip 长度错误: hostname=%d version=%d diskPath=%d",
				len(m.Hostname), len(m.Version), len(m.DiskPath))
		}
		// NodeUUID 刻意不 clip：它是外键语义，截断会导致水位挂到别的节点上。
		if len(m.NodeUUID) != 200 {
			t.Errorf("NodeUUID 不应被 clip，len = %d", len(m.NodeUUID))
		}
	})
}

func TestRound2(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{33.333333, 33.33},
		{0.005, 0.01},
		{99.994, 99.99},
		{100, 100},
		{0, 0},
	}
	for _, tt := range tests {
		if got := round2(tt.in); got != tt.want {
			t.Errorf("round2(%v) = %v，期望 %v", tt.in, got, tt.want)
		}
	}
}
