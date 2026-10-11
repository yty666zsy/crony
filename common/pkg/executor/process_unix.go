//go:build !windows

package executor

import (
	"fmt"
	"os/exec"
	"syscall"
)

// configureProcessGroup 让命令运行在独立进程组中，
// 这样取消时可以"整组"终止，而不是只杀直接子进程（子进程再 fork 的场景很常见：
// shell 脚本里起 nohup/后台任务、调用 wrapper 等）。
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree 终止整棵进程树：向进程组发送 SIGKILL。
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		// 进程组不存在时回退到单进程终止
		if killErr := cmd.Process.Kill(); killErr != nil {
			return fmt.Errorf("终止进程树失败(组=%d): %v; 单进程终止也失败: %v", pid, err, killErr)
		}
	}
	return nil
}
