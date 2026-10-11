//go:build windows

package executor

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
)

// createNewProcessGroup 对应 Windows 的 CREATE_NEW_PROCESS_GROUP (0x00000200)。
const createNewProcessGroup = 0x00000200

// configureProcessGroup 让命令运行在独立进程组中，便于整组终止。
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
}

// killProcessTree 用 taskkill /T 结束整棵进程树（Windows 上等价于 SIGKILL 到进程组）。
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run(); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			return fmt.Errorf("终止进程树失败(pid=%d): %v; 单进程终止也失败: %v", pid, err, killErr)
		}
	}
	return nil
}
