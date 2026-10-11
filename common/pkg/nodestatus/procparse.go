package nodestatus

// 本文件**不带任何 build tag**：/proc 文本解析是纯字符串 + 整数算术，最容易写错
// （字段顺序、iowait 口径、guest 重复计入），却又最容易被"只能在 Linux 上跑"挡住验证。
// 把解析从 collect_linux.go 里拆出来，就能在 Windows 开发机上用真实文本片段 +
// 边界用例直接跑单测，而不是等到上线才发现 CPU 占用率一直偏低。

import (
	"strconv"
	"strings"
)

// procStatCountedFields 是 /proc/stat 的 `cpu` 行里**计入总时间**的字段个数：
// user nice system idle iowait irq softirq steal，共 8 个。
//
// 为什么必须在这里截断：内核在 steal 之后还会继续输出 guest、guest_nice，
// 但这两个值**已经被包含在 user/nice 里**（内核文档明确如此）。若把它们也加进
// 总时间，总增量会被虚增、CPU 占用率被算低——一个只在虚拟化环境才暴露的隐性错误。
const procStatCountedFields = 8

// parseProcStat 解析 /proc/stat，返回 `cpu` 汇总行的总时间和空闲时间（单位：节拍）。
//
// 只认第一行 `cpu ...`，不认 cpu0/cpu1：水位要的是整机，不是某一个核。
// 找不到该行、字段非数字或输入为空时，返回已解析到的部分（通常为 0，两个返回值
// 都是 0 表示"没解析出来"），由调用方把"总量为 0"当成采集失败——绝不能顺势
// 算出 0% 蒙混过去，那会被读成"CPU 完全空闲"。
func parseProcStat(data []byte) (total, idle uint64) {
	fields := cpuSummaryFields(data)
	if len(fields) == 0 {
		return 0, 0
	}
	for i, f := range fields {
		if i >= procStatCountedFields {
			break // guest/guest_nice 及以后一律不计入
		}
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			break // 遇到非数字就截断：宁可少算，也不猜
		}
		total += v
		switch i {
		case 3, 4: // idle、iowait
			// iowait 期间 CPU 同样没有在干活（在等 IO），必须算进空闲；
			// 否则磁盘密集的机器会被误报成 CPU 繁忙。
			idle += v
		}
	}
	return total, idle
}

// cpuSummaryFields 找出 /proc/stat 里第一行 `cpu` 的字段列表（去掉开头的 "cpu"）。
func cpuSummaryFields(data []byte) []string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "cpu" {
			return fields[1:]
		}
	}
	return nil
}

// parseMemInfo 解析 /proc/meminfo，返回总内存与可用内存（单位：kB）。
//
// 可用内存优先取 MemAvailable：它是内核估算的"还能拿来用"的量（已剔除不可回收的
// page cache），最接近"再起一个任务会不会 OOM"。只有老内核（< 3.14）或裁剪过的
// /proc 没有这个键时，才回退到 MemFree+Buffers+Cached——若反过来只认 MemFree，
// 在正常机器上会把"缓存占用"误报成"内存吃紧"。
func parseMemInfo(data []byte) (totalKB, availKB uint64) {
	var memFree, buffers, cached uint64
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := splitMemInfoLine(line)
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			totalKB = val
		case "MemAvailable":
			availKB = val
		case "MemFree":
			memFree = val
		case "Buffers":
			buffers = val
		case "Cached": // 精确匹配：SwapCached 不能被当成 Cached
			cached = val
		}
	}
	if availKB == 0 {
		// 键缺失（或值为 0）时回退老口径；两者都为 0 说明确实解析不到，
		// 由调用方按总量判断是否采集失败。
		availKB = memFree + buffers + cached
	}
	return totalKB, availKB
}

// splitMemInfoLine 把 `MemTotal:       16333804 kB` 拆成 ("MemTotal", 16333804, true)。
//
// 单位不参与换算：所有行都是 kB，调用方统一处理；这里只负责取数字，
// 所以有没有 "kB" 后缀都能解析（老内核/容器里偶尔会少）。
func splitMemInfoLine(line string) (key string, val uint64, ok bool) {
	idx := strings.IndexByte(line, ':')
	if idx <= 0 {
		return "", 0, false
	}
	key = strings.TrimSpace(line[:idx])
	rest := strings.Fields(line[idx+1:])
	if len(rest) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseUint(rest[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return key, v, true
}

// parseLoadAvg 取 /proc/loadavg 的前三个字段（1/5/15 分钟负载），拼成 "a b c"。
//
// 不足三个字段时返回已解析到的部分而不是 panic：容器里 /proc 可能被裁剪，
// "缺指标"应该表现为短字符串甚至空串，让调用方与模型知道这项没有，
// 而不是拿到一个补齐了 0.00 的假值。
func parseLoadAvg(data []byte) string {
	fields := strings.Fields(string(data))
	if len(fields) > 3 {
		fields = fields[:3]
	}
	return strings.Join(fields, " ")
}
