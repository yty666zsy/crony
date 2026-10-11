package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/retry"
)

// ShellExecutor 执行本地命令。
//
// 两种模式（显式区分，避免旧实现"把 argv 交给 shell 再解析一遍"的引号地狱）：
//   - Args 非空：直接 exec.Command(argv...)，等价于旧任务的行为（兼容）；
//   - Args 为空：Command 作为脚本字符串交给平台 shell（sh -c / cmd.exe /C）。
type ShellExecutor struct {
	// ShellCommand 允许覆盖 shell 解析方式（测试与特殊平台使用）。
	// 返回可执行文件与参数前缀。默认：Windows → cmd.exe /C；其它 → sh -c。
	ShellCommand func() (string, []string)
	// Env 额外注入的基础环境变量（例如 PATH 兜底）。
	Env []string
}

// NewShellExecutor 创建默认 shell 执行器。
func NewShellExecutor() *ShellExecutor { return &ShellExecutor{} }

func (e *ShellExecutor) Kind() Kind { return KindShell }

func (e *ShellExecutor) Validate(req Request) error {
	if !req.RawArgv && len(req.Args) == 0 && strings.TrimSpace(req.Command) == "" {
		return fmt.Errorf("%w: shell command is required", ErrInvalidRequest)
	}
	if req.RawArgv && strings.TrimSpace(req.Command) == "" {
		return fmt.Errorf("%w: raw argv command is required", ErrInvalidRequest)
	}
	if req.Timeout < 0 {
		return fmt.Errorf("%w: negative timeout", ErrInvalidRequest)
	}
	return nil
}

// shellCommand 构造底层命令。
func (e *ShellExecutor) shellCommand(ctx context.Context, req Request) *exec.Cmd {
	if req.RawArgv || len(req.Args) > 0 {
		return exec.CommandContext(ctx, req.Command, req.Args...)
	}
	var name string
	var prefix []string
	if e.ShellCommand != nil {
		name, prefix = e.ShellCommand()
	} else if runtime.GOOS == "windows" {
		name, prefix = "cmd.exe", []string{"/C"}
	} else {
		name, prefix = "sh", []string{"-c"}
	}
	args := append(append([]string{}, prefix...), req.Command)
	return exec.CommandContext(ctx, name, args...)
}

func (e *ShellExecutor) Execute(ctx context.Context, req Request) Result {
	res := newResult(req)
	if err := e.Validate(req); err != nil {
		return res.finish(models.RunStatusFailed, retry.Fatal(err))
	}

	timeout := req.EffectiveTimeout()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := e.shellCommand(runCtx, req)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	env := append([]string{}, e.Env...)
	env = append(env, os.Environ()...)
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	buf := &limitedBuffer{limit: req.OutputLimit()}
	cmd.Stdout = buf
	cmd.Stderr = buf
	// 独立进程组：取消时终止整棵进程树（shell 脚本里的子进程也要一起收拾）
	configureProcessGroup(cmd)
	// 覆盖 exec.CommandContext 的默认 Kill，改为整组终止
	cmd.Cancel = func() error { return killProcessTree(cmd) }
	// 兜底：整组终止失败（例如孙进程仍握着管道写端）时，最多等 WaitDelay 后让 Wait 返回，
	// 避免调度 goroutine 被一个卡死的命令永久占住。取 3s：足够覆盖 taskkill/SIGKILL 生效，
	// 又不会让"取消"变成用户可感知的漫长等待。
	cmd.WaitDelay = 3 * time.Second

	startErr := cmd.Start()
	if startErr != nil {
		res.ExitCode = -1
		res.Output = buf.String()
		res.Truncated = buf.Truncated()
		res.Artifacts["phase"] = "start"
		// 找不到可执行文件/无权限：重试没有意义
		return res.finish(models.RunStatusFailed, retry.Fatal(fmt.Errorf("启动命令失败: %w", startErr)))
	}
	res.Artifacts["pid"] = fmt.Sprintf("%d", cmd.Process.Pid)

	waitErr := cmd.Wait()
	res.Output = buf.String()
	res.Truncated = buf.Truncated()
	res.ExitCode = exitCodeOf(waitErr)

	switch {
	case waitErr == nil:
		return res.finish(models.RunStatusSuccess, nil)
	case runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		// 超时算可重试：多数超时来自下游瞬时变慢；上层策略有总时长上限兜底，
		// 不会出现"无限重试一个已经卡死的命令"。
		res.Artifacts["timeout"] = timeout.String()
		return res.finish(models.RunStatusTimeout,
			retry.Retryable(fmt.Errorf("%w: 超过 %s", ErrTimeout, timeout)))
	case ctx.Err() != nil:
		return res.finish(models.RunStatusCanceled, retry.Fatal(fmt.Errorf("执行被取消: %w", ctx.Err())))
	default:
		return res.finish(models.RunStatusFailed, classifyExitCode(res.ExitCode, res.Output))
	}
}

// retryableExitCodes 判断退出码是否值得重试。
//
// 依据 shell 约定：
//   - 2     命令用法错误（参数写错）
//   - 126   不可执行
//   - 127   命令不存在（Unix）
//   - 9009  命令不存在（Windows cmd："不是内部或外部命令"）
//
// 这几类是"配置问题"，重试 N 次只会同样失败，还会刷屏告警——标为不可重试。
func classifyExitCode(exitCode int, output string) error {
	base := fmt.Errorf("命令退出码 %d", exitCode)
	switch exitCode {
	case 2, 126, 127, 9009:
		return retry.Fatal(fmt.Errorf("%w（命令/参数配置错误，不重试）: %s", base, tail(output, 200)))
	default:
		return retry.Retryable(base)
	}
}

// tail 截取输出尾部用于错误信息（避免把整段输出塞进 error）。
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// limitedBuffer 输出限制缓冲：超过上限后继续消费（防止子进程因管道写满而阻塞），
// 但不再累积内容，并标记 Truncated。
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.limit = DefaultOutputLimit
	}
	if b.buf.Len() >= b.limit {
		b.truncated = true
		return len(p), nil
	}
	room := b.limit - b.buf.Len()
	if len(p) <= room {
		return b.buf.Write(p)
	}
	if _, err := b.buf.Write(p[:room]); err != nil {
		return 0, err
	}
	b.truncated = true
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	out := b.buf.String()
	if b.truncated {
		out += fmt.Sprintf("\n...（输出超过 %d 字节已截断）", b.limit)
	}
	return out
}

func (b *limitedBuffer) Truncated() bool { return b.truncated }

var _ io.Writer = (*limitedBuffer)(nil)

// copyLimited 读取 r 的前 limit 字节（HTTP 执行器复用）。
func copyLimited(r io.Reader, limit int) (string, bool, error) {
	if limit <= 0 {
		limit = DefaultOutputLimit
	}
	buf := &limitedBuffer{limit: limit}
	if _, err := io.Copy(buf, r); err != nil {
		return buf.String(), buf.Truncated(), err
	}
	return buf.String(), buf.Truncated(), nil
}

// 编译期断言：确保 ShellExecutor 实现 Executor。
var _ Executor = (*ShellExecutor)(nil)
