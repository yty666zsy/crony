//go:build windows

package nodestatus

import (
	"strings"
	"testing"
)

// TestCollectWindowsRealMachine 在真机上跑一次完整采集。
//
// 为什么必须真机跑：结构体布局写错、syscall 参数顺序写错、单位换算写错，
// **都不会编译失败**，只会读出垃圾数字或静默失败。这些错只有真调一次 Win32
// 才能暴露。耗时约 200ms（CPU 两次采样间隔），可接受。
func TestCollectWindowsRealMachine(t *testing.T) {
	snap, err := Collect()
	if err != nil {
		t.Fatalf("Collect() 在真实 Windows 机器上不应失败: %v", err)
	}

	t.Logf("真机采集结果: CPU=%.2f%% Mem=%.2f%% (%d/%d MB) Disk=%.2f%% (%d/%d MB) DiskPath=%q CPUCores=%d LoadAvg=%q",
		snap.CPUPercent, snap.MemPercent, snap.MemUsedMB, snap.MemTotalMB,
		snap.DiskPercent, snap.DiskFreeMB, snap.DiskTotalMB, snap.DiskPath, snap.CPUCores, snap.LoadAvg)

	// 总量必须为正：为 0 说明采集失败，此时 Collect 应该已经返回 error。
	if snap.MemTotalMB <= 0 {
		t.Errorf("MemTotalMB = %d，必须 > 0", snap.MemTotalMB)
	}
	if snap.DiskTotalMB <= 0 {
		t.Errorf("DiskTotalMB = %d，必须 > 0", snap.DiskTotalMB)
	}
	if snap.CPUCores <= 0 {
		t.Errorf("CPUCores = %d，必须 > 0", snap.CPUCores)
	}

	// 百分比必须落在合法区间（clampPercent 的兜底也要真机验证一次）。
	if snap.MemPercent < 0 || snap.MemPercent > 100 {
		t.Errorf("MemPercent = %v，必须落在 [0,100]", snap.MemPercent)
	}
	if snap.DiskPercent < 0 || snap.DiskPercent > 100 {
		t.Errorf("DiskPercent = %v，必须落在 [0,100]", snap.DiskPercent)
	}
	// CPUPercent 允许为 0：两次采样可能撞在同一 tick，样本太少时不编造数据。
	if snap.CPUPercent < 0 || snap.CPUPercent > 100 {
		t.Errorf("CPUPercent = %v，必须落在 [0,100]（允许为 0）", snap.CPUPercent)
	}

	// 一致性：已用不能超过总量，空闲不能超过总量。
	if snap.MemUsedMB < 0 || snap.MemUsedMB > snap.MemTotalMB {
		t.Errorf("MemUsedMB = %d，必须落在 [0, %d]", snap.MemUsedMB, snap.MemTotalMB)
	}
	if snap.DiskFreeMB < 0 || snap.DiskFreeMB > snap.DiskTotalMB {
		t.Errorf("DiskFreeMB = %d，必须落在 [0, %d]", snap.DiskFreeMB, snap.DiskTotalMB)
	}

	// Windows 没有 load average：必须留空，不能编造 "0.00"。
	if snap.LoadAvg != "" {
		t.Errorf("LoadAvg = %q，Windows 没有该指标，必须留空而不是编造数值", snap.LoadAvg)
	}

	if snap.DiskPath == "" {
		t.Error("DiskPath 不能为空：水位必须能对应到一个具体盘符")
	}
	if !strings.HasSuffix(snap.DiskPath, `\`) {
		t.Errorf("DiskPath = %q，期望形如 `C:\\` 的盘符根路径", snap.DiskPath)
	}
}
