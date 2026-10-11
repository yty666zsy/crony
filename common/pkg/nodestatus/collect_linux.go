//go:build linux

package nodestatus

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
)

// 采集源都写成常量，便于审查时一眼看清"到底读了什么"。
const (
	procStatPath    = "/proc/stat"
	procMemInfoPath = "/proc/meminfo"
	procLoadAvgPath = "/proc/loadavg"
	// linuxDiskPath 固定看根文件系统：crony 的任务、日志、临时文件默认都落在 /
	// （或挂在 / 上的数据盘），/proc 里没有"哪个挂载点最重要"的信息。
	linuxDiskPath = "/"
)

// cpuSampleInterval 是 CPU 两次采样的间隔（与 Windows 实现同一口径）。
//
// /proc/stat 的 cpu 行是开机以来的累计节拍，必须取差值；200ms 既能让差值
// 稳定到两位小数，又不会明显拖慢上报周期。
const cpuSampleInterval = 200 * time.Millisecond

// 本文件只负责"读文件"，文本解析全部放在无 build tag 的 procparse.go 里，
// 这样解析逻辑能在任何平台上被单测覆盖。

func collectPlatform() (Snapshot, error) {
	snap := Snapshot{
		CPUCores: runtime.NumCPU(),
		DiskPath: linuxDiskPath,
	}

	cpuPercent, err := collectCPUPercent()
	if err != nil {
		return Snapshot{}, err
	}
	snap.CPUPercent = cpuPercent

	memPercent, memTotalMB, memUsedMB, err := collectMemory()
	if err != nil {
		return Snapshot{}, err
	}
	snap.MemPercent, snap.MemTotalMB, snap.MemUsedMB = memPercent, memTotalMB, memUsedMB

	diskPercent, diskTotalMB, diskFreeMB, err := collectDisk()
	if err != nil {
		return Snapshot{}, err
	}
	snap.DiskPercent, snap.DiskTotalMB, snap.DiskFreeMB = diskPercent, diskTotalMB, diskFreeMB
	snap.DiskPath = linuxDiskPath

	snap.LoadAvg = readLoadAvg()

	return snap, nil
}

// collectCPUPercent 用两次 /proc/stat 的差值算整机 CPU 占用率。
func collectCPUPercent() (float64, error) {
	first, err := os.ReadFile(procStatPath)
	if err != nil {
		return 0, fmt.Errorf("读取 %s 失败: %w", procStatPath, err)
	}
	total1, idle1 := parseProcStat(first)
	if total1 == 0 {
		// 总量为 0 = 解析不到 cpu 汇总行（容器裁剪 /proc、内核异常）。
		// 这是"采集不到"，必须报错跳过上报，而不是算出一个假的 0%。
		return 0, fmt.Errorf("%s 未解析出 cpu 汇总行，无法判断 CPU 水位", procStatPath)
	}

	time.Sleep(cpuSampleInterval)

	second, err := os.ReadFile(procStatPath)
	if err != nil {
		return 0, fmt.Errorf("读取 %s 失败: %w", procStatPath, err)
	}
	total2, idle2 := parseProcStat(second)
	if total2 == 0 {
		return 0, fmt.Errorf("%s 第二次采样未解析出 cpu 汇总行，无法判断 CPU 水位", procStatPath)
	}
	if total2 < total1 {
		return 0, fmt.Errorf("%s 累计节拍回退（%d -> %d），无法计算 CPU 水位", procStatPath, total1, total2)
	}

	// idle 里已含 iowait（解析层处理），这里只做差值与百分比。
	return percentFromDelta(total2-total1, counterDelta(idle2, idle1)), nil
}

// collectMemory 读 /proc/meminfo。
func collectMemory() (percent float64, totalMB, usedMB int64, err error) {
	data, err := os.ReadFile(procMemInfoPath)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("读取 %s 失败: %w", procMemInfoPath, err)
	}
	totalKB, availKB := parseMemInfo(data)
	if totalKB == 0 {
		// MemTotal 缺失只可能是解析失败：宁可报错跳过上报，也不能写 0%
		// （0% 会被 admin 侧读成"内存完全空闲"，比没有数据更糟）。
		return 0, 0, 0, fmt.Errorf("%s 未解析出 MemTotal，无法判断内存水位", procMemInfoPath)
	}
	if availKB > totalKB {
		availKB = totalKB // 内核偶发的口径抖动：当作全空闲，而不是算出负数
	}

	usedKB := totalKB - availKB
	percent = percentFrom(usedKB, totalKB)
	// kB → 字节后交给共用的换算函数，避免在这里再写一遍 1024 的除法口径。
	totalMB = mbFromBytes(totalKB * 1024)
	usedMB = mbFromBytes(usedKB * 1024)
	return percent, totalMB, usedMB, nil
}

// collectDisk 用 Statfs 读根文件系统容量。
func collectDisk() (percent float64, totalMB, freeMB int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(linuxDiskPath, &st); err != nil {
		return 0, 0, 0, fmt.Errorf("Statfs(%q) 失败: %w", linuxDiskPath, err)
	}

	total := uint64(st.Blocks) * uint64(st.Bsize)
	if total == 0 {
		return 0, 0, 0, fmt.Errorf("%q 文件系统总块数为 0，无法判断磁盘水位", linuxDiskPath)
	}

	// 取 Bavail（非特权用户真正可用的空闲块）而不是 Bfree：Bfree 里含预留给
	// root 的 5%，用 Bfree 会把"普通任务已经写不进去了"报成"还有空闲"。
	free := uint64(st.Bavail) * uint64(st.Bsize)
	if free > total {
		free = total
	}

	totalMB = mbFromBytes(total)
	freeMB = mbFromBytes(free)
	percent = percentFrom(total-free, total)
	return percent, totalMB, freeMB, nil
}

// readLoadAvg 读 /proc/loadavg。
//
// 读不到不算采集失败：CPU/内存/磁盘才是判断"节点还能不能干活"的关键指标，
// 而负载只是辅助信息。返回空字符串表示"本机拿不到这项"，
// 调用方与模型看到空值就不会误判，也绝不补一个 0.00 充数。
func readLoadAvg() string {
	data, err := os.ReadFile(procLoadAvgPath)
	if err != nil {
		return ""
	}
	return parseLoadAvg(data)
}

// counterDelta 计算累计计数器的增量；计数器回绕/异常（新值小于旧值）时返回 0。
func counterDelta(newer, older uint64) uint64 {
	if newer < older {
		return 0
	}
	return newer - older
}
