//go:build !linux && !windows

package nodestatus

import (
	"fmt"
	"runtime"
)

// collectPlatform 在尚未实现采集的平台上只做一件事：明确失败。
//
// 为什么不是"返回零值快照 + nil error"：零值会被当成 CPU 0%、内存 0%、磁盘 0%
// 写进 node_metric，而 admin 侧正是靠这些数字判断节点健不健康——给不出数据就
// 必须说"给不出"，让调用方跳过这次上报（表里保持旧值或没有值）。
// 把"没采集到"伪装成"资源空闲"，是比没有数据更危险的谎话。
func collectPlatform() (Snapshot, error) {
	return Snapshot{}, fmt.Errorf(
		"当前平台 %s/%s 未实现资源水位采集；调用方必须跳过本次上报，不得伪造 0 水位",
		runtime.GOOS, runtime.GOARCH)
}
