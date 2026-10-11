package handler_test

import (
	"runtime"
	"testing"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/node/internal/handler"
)

// TestCmdRunReturnsErrorForMissingBinary 原测试执行写死的 /home/tmnh/hello.sh
// （Linux 主机上不存在的路径，且依赖 logger 先被 Init，否则空指针 panic），
// 因此在干净环境里必然失败。
//
// 这里改为验证"命令无法启动时必须返回 error 而不 panic"：
// 这条路径同样覆盖 logger 调用点，且不依赖外部脚本/etcd/MySQL。
func TestCmdRunReturnsErrorForMissingBinary(t *testing.T) {
	missing := "crony-definitely-missing-binary-xyz"
	if runtime.GOOS == "windows" {
		missing = "crony-definitely-missing-binary-xyz.exe"
	}
	var cmd handler.CMDHandler
	job := &handler.Job{
		Job: &models.Job{
			Name:    "missing-binary",
			Command: missing,
			Cmd:     []string{missing},
			Type:    models.JobTypeCmd,
			Timeout: 0,
		},
	}
	if _, err := cmd.Run(job); err == nil {
		t.Fatalf("Run() with a missing binary must return an error")
	}
}

// TestRunPresetScriptReturnsErrorForMissingScript 覆盖预置脚本路径的错误处理。
func TestRunPresetScriptReturnsErrorForMissingScript(t *testing.T) {
	missing := "crony-definitely-missing-script-xyz"
	if runtime.GOOS == "windows" {
		missing = "crony-definitely-missing-script-xyz.cmd"
	}
	script := &models.Script{
		Name:    "missing-script",
		Command: missing,
		Cmd:     []string{missing},
	}
	if _, err := handler.RunPresetScript(script); err == nil {
		t.Fatal("RunPresetScript() with a missing script must return an error")
	}
}
