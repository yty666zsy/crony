//go:build windows

package nodestatus

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// 只用标准库 syscall：本包是节点侧最底层的采集代码，只需要三个 Win32 调用，
// 不值得为此引入 gopsutil / golang.org/x/sys（节点二进制会平白多背一个依赖）。
//
// 用 NewLazyDLL 而不是 MustLoadDLL：进程启动阶段不加载 DLL，真到采集时缺 DLL
// 只会让 Collect() 返回 error（调用方跳过上报），而不是在 init 阶段 panic
// 掉整个节点进程——采集失败不该让节点起不来。
var (
	kernel32DLL              = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes       = kernel32DLL.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32DLL.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = kernel32DLL.NewProc("GetDiskFreeSpaceExW")
)

// cpuSampleInterval 是 CPU 两次采样的间隔。
//
// GetSystemTimes 给的是开机以来的累计计数器，必须取两次差值才能算占用率。
// 200ms 是"精度"与"上报开销"的折中：再短则单次调度抖动就能让占用率上下跳几点，
// 再长则会拖慢上报周期。
const cpuSampleInterval = 200 * time.Millisecond

// winFiletime 对应 Win32 的 FILETIME（单位 100ns）。
type winFiletime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

// value 把高低两个 32 位拼回 64 位计数值。
func (f winFiletime) value() uint64 {
	return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime)
}

// winMemoryStatusEx 对应 Win32 的 MEMORYSTATUSEX。
//
// 字段顺序与大小必须和 Win32 结构体严格一致：GlobalMemoryStatusEx 会按
// Length 指定的字节数直接写入这块内存，布局错了不会编译失败，只会读到垃圾数字。
type winMemoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func collectPlatform() (Snapshot, error) {
	snap := Snapshot{
		CPUCores: runtime.NumCPU(),
		// Windows 没有 load average 这个概念（也没有 /proc/loadavg 的等价物）。
		// 留空而不是写 "0.00"：admin 侧要靠"空"来区分"这台机器不提供该指标"，
		// 写 0 会被读成"负载为零、非常空闲"——等于凭空编出一个不存在的指标。
		LoadAvg: "",
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

	diskPath := diskRootPath()
	diskPercent, diskTotalMB, diskFreeMB, err := collectDisk(diskPath)
	if err != nil {
		return Snapshot{}, err
	}
	snap.DiskPercent, snap.DiskTotalMB, snap.DiskFreeMB = diskPercent, diskTotalMB, diskFreeMB
	snap.DiskPath = diskPath

	return snap, nil
}

// collectCPUPercent 用两次 GetSystemTimes 的差值算整机 CPU 占用率。
func collectCPUPercent() (float64, error) {
	idle1, kernel1, user1, err := systemTimes()
	if err != nil {
		return 0, err
	}

	time.Sleep(cpuSampleInterval)

	idle2, kernel2, user2, err := systemTimes()
	if err != nil {
		return 0, err
	}

	// 坑：Windows 的 kernel 时间**已经包含 idle 时间**（GetSystemTimes 文档：
	// KernelTime "includes idle time"）。所以总增量只能是 kernel+user；
	// 若照搬 Linux 的 total=idle+kernel+user，idle 会被算两遍，
	// 空闲机器上会得出 50% 这种明显偏高的占用率。
	totalDelta := counterDelta(kernel2, kernel1) + counterDelta(user2, user1)
	idleDelta := counterDelta(idle2, idle1)

	// 计数器没动（两次采样撞在同一 tick）时占用率无意义；percentFromDelta 会返回 0。
	// CPU 允许为 0：判断"采集是否成功"的硬标准是内存/磁盘总量，不是 CPU。
	return percentFromDelta(totalDelta, idleDelta), nil
}

// systemTimes 读一次 GetSystemTimes 的 idle/kernel/user 累计值。
func systemTimes() (idle, kernel, user uint64, err error) {
	var idleFT, kernelFT, userFT winFiletime
	ret, _, callErr := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFT)),
		uintptr(unsafe.Pointer(&kernelFT)),
		uintptr(unsafe.Pointer(&userFT)),
	)
	if ret == 0 {
		return 0, 0, 0, fmt.Errorf("GetSystemTimes 调用失败: %v", callErr)
	}
	return idleFT.value(), kernelFT.value(), userFT.value(), nil
}

// counterDelta 计算累计计数器的增量；计数器回绕/异常（新值小于旧值）时返回 0。
func counterDelta(newer, older uint64) uint64 {
	if newer < older {
		return 0
	}
	return newer - older
}

// collectMemory 用 GlobalMemoryStatusEx 读物理内存。
func collectMemory() (percent float64, totalMB, usedMB int64, err error) {
	var status winMemoryStatusEx
	status.Length = uint32(unsafe.Sizeof(status))
	ret, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ret == 0 {
		return 0, 0, 0, fmt.Errorf("GlobalMemoryStatusEx 调用失败: %v", callErr)
	}
	if status.TotalPhys == 0 {
		// 总量为 0 只可能是调用失败或系统刚启动还没算完——这是"采集不到"，
		// 必须返回 error 让调用方跳过本次上报，绝不能写一行 MemPercent=0 的假数据。
		return 0, 0, 0, fmt.Errorf("GlobalMemoryStatusEx 返回物理内存总量为 0，无法判断内存水位")
	}

	// dwMemoryLoad 是系统自己算的"已用百分比"，直接采信，避免我们和系统口径打架。
	percent = clampPercent(float64(status.MemoryLoad))
	totalMB = mbFromBytes(status.TotalPhys)
	used := status.TotalPhys
	if status.AvailPhys < status.TotalPhys {
		used = status.TotalPhys - status.AvailPhys
	}
	usedMB = mbFromBytes(used)
	return percent, totalMB, usedMB, nil
}

// collectDisk 用 GetDiskFreeSpaceExW 读指定盘符的容量。
func collectDisk(root string) (percent float64, totalMB, freeMB int64, err error) {
	pathPtr, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("盘符 %q 无法转换为 UTF-16: %w", root, err)
	}

	var freeToCaller, totalBytes, totalFree uint64
	ret, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 {
		return 0, 0, 0, fmt.Errorf("GetDiskFreeSpaceExW(%q) 调用失败: %v", root, callErr)
	}
	if totalBytes == 0 {
		// 同上：容量为 0 是采集失败，不是"磁盘全空"。
		return 0, 0, 0, fmt.Errorf("盘符 %q 容量为 0，无法判断磁盘水位", root)
	}

	// 用"调用者可用空间"而不是整卷剩余空间：与 Linux 侧取 Bavail（非特权用户
	// 真正能用的空间）保持同一口径，有磁盘配额时更贴近"任务还能不能写文件"。
	free := freeToCaller
	if free > totalBytes {
		free = totalBytes
	}
	totalMB = mbFromBytes(totalBytes)
	freeMB = mbFromBytes(free)
	percent = percentFrom(totalBytes-free, totalBytes)
	return percent, totalMB, freeMB, nil
}

// diskRootPath 决定"看哪个盘"。
//
// 优先当前工作目录所在盘符：节点的工作目录才是它写日志/落临时文件的地方，
// 那个盘满了才会真出事；SystemDrive 与 C:\ 只是兜底。
func diskRootPath() string {
	if cwd, err := os.Getwd(); err == nil {
		if vol := filepath.VolumeName(cwd); vol != "" {
			return vol + `\`
		}
	}
	if drive := os.Getenv("SystemDrive"); drive != "" {
		return drive + `\`
	}
	return `C:\`
}
