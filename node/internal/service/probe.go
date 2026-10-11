package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/tools"
	"gorm.io/gorm"
)

// ProbeWorker 节点侧只读探测执行器。
//
// 它在节点上做三件事：领取本节点的待处理探测请求 → **重新校验**参数白名单 →
// 不用 shell 执行 → 把结果写回同一行。
//
// 三条安全纪律（这是本项目第一条"admin 让节点跑东西"的通道，必须有明确边界）：
//  1. **节点侧自己校验，不信任 admin 写入的内容**：admin 侧已经校验过一次，
//     但请求表在共享库里，任何能写库的人都能塞一条 argv。节点领取后重新走
//     tools.GuardShellCommand，白名单外的一律 failed，不执行。
//  2. **只读**：白名单是只读命令集（netstat/ss/tasklist/sc query/ipconfig /all +
//     df/ps/tail/journalctl）。写语义的命令名（sc stop/config、ipconfig /release）
//     根本不在白名单里。
//  3. **有界**：单条命令有超时上限，输出按策略上限截断（截断要显式标注，
//     否则模型会把"被截断"当成"就这么多"）。
//
// 不做的事（刻意）：不重试、不排队补跑、不级联。探测失败就是失败，
// 让调用方（admin 侧工具）拿到明确的失败，而不是等到超时。
type ProbeWorker struct {
	db       *gorm.DB
	nodeUUID string
	hostname string
	policy   tools.Policy
	runner   tools.ShellRunner
	// pollInterval 轮询间隔（默认 1s）
	pollInterval time.Duration
	// maxPerRound 单轮最多领取多少条（默认 4，防止一次把节点占满）
	maxPerRound int
	// maxTimeout 单条命令超时上限（默认 20s）
	maxTimeout time.Duration
}

// NewProbeWorker 创建探测执行器。db 为空时返回 error（静默不启动比报错更糟）。
func NewProbeWorker(db *gorm.DB, nodeUUID, hostname string) (*ProbeWorker, error) {
	if db == nil {
		return nil, errors.New("探测执行器需要数据库连接")
	}
	if strings.TrimSpace(nodeUUID) == "" {
		return nil, errors.New("探测执行器需要 node_uuid（否则会领取到别的节点的请求）")
	}
	return &ProbeWorker{
		db:           db,
		nodeUUID:     strings.TrimSpace(nodeUUID),
		hostname:     hostname,
		policy:       tools.DefaultPolicy(),
		runner:       tools.DefaultShellRunner,
		pollInterval: 1 * time.Second,
		maxPerRound:  4,
		maxTimeout:   20 * time.Second,
	}, nil
}

// Run 持续轮询直到 ctx 结束。调用方负责在节点退出时取消 ctx。
func (w *ProbeWorker) Run(ctx context.Context) {
	logger.GetLogger().Info(fmt.Sprintf("[probe] 节点侧只读探测已启用：node_uuid=%s 轮询=%s 单轮上限=%d 命令白名单=%s",
		w.nodeUUID, w.pollInterval, w.maxPerRound, strings.Join(w.AllowedCommands(), ",")))
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.GetLogger().Info("[probe] 节点侧只读探测退出")
			return
		case <-ticker.C:
			if n, err := w.RunOnce(ctx); err != nil {
				// 库暂时不可用不该刷爆日志，也不该退出：下一轮再来
				logger.GetLogger().Warn("[probe] 领取探测请求失败: " + err.Error())
			} else if n > 0 {
				logger.GetLogger().Info(fmt.Sprintf("[probe] 本轮处理 %d 条探测请求", n))
			}
		}
	}
}

// RunOnce 处理一轮：领取 → 执行 → 回写。返回本轮处理条数。
//
// 抽成公开方法是为了可测：集成测试直接驱动它，不必等 ticker。
func (w *ProbeWorker) RunOnce(ctx context.Context) (int, error) {
	reqs, err := w.claim()
	if err != nil {
		return 0, err
	}
	for i := range reqs {
		w.execOne(ctx, &reqs[i])
	}
	return len(reqs), nil
}

// claim 领取本节点最早的若干条 pending 请求（原子更新为 running）。
//
// 用 "先查 ID 再带条件更新" 的两步法：条件里带 status=pending，
// 只有真正把状态改过来才算领取成功（多节点/多进程下不会重复执行）。
func (w *ProbeWorker) claim() ([]models.NodeProbeRequest, error) {
	var candidates []models.NodeProbeRequest
	err := w.db.
		Where("node_uuid = ? AND status = ?", w.nodeUUID, models.NodeProbePending).
		Order("id asc").Limit(w.maxPerRound).Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	claimed := make([]models.NodeProbeRequest, 0, len(candidates))
	now := time.Now().UnixMilli()
	for _, c := range candidates {
		res := w.db.Model(&models.NodeProbeRequest{}).
			Where("id = ? AND status = ?", c.ID, models.NodeProbePending).
			Updates(map[string]interface{}{
				"status":        models.NodeProbeRunning,
				"claimed_at":    now,
				"node_hostname": w.hostname,
			})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			continue // 被别的进程抢走了
		}
		c.Status = models.NodeProbeRunning
		claimed = append(claimed, c)
	}
	return claimed, nil
}

// execOne 执行一条请求并回写结果。
func (w *ProbeWorker) execOne(ctx context.Context, req *models.NodeProbeRequest) {
	out := w.execute(ctx, req)
	w.finish(req.ID, out.Status, out.Output, out.ExitCode, out.Error)
}

// probeOutcome 一次探测的结果（尚未落库）。
//
// 把"执行"与"落库"分开是为了可测：白名单拒绝、超时、截断这些安全语义
// 必须能在**不连数据库**的情况下被断言，否则它们只会在有 MySQL 的
// 环境里才被覆盖，而那些正是最不该漏测的路径。
type probeOutcome struct {
	Status   string
	Output   string
	ExitCode int
	Error    string
}

// execute 执行一条探测请求（不落库）。
func (w *ProbeWorker) execute(ctx context.Context, req *models.NodeProbeRequest) probeOutcome {
	argv, err := decodeArgv(req.ArgvJSON)
	if err != nil {
		return probeOutcome{Status: models.NodeProbeFailed, ExitCode: -1,
			Error: "参数不是合法的 argv 数组: " + err.Error()}
	}
	// 纪律 1：节点侧自己再校验一次，白名单外一律不执行
	if err := tools.GuardShellCommand(req.Command, argv, w.policy); err != nil {
		return probeOutcome{Status: models.NodeProbeFailed, ExitCode: -1,
			Error: "命令被节点侧白名单拒绝（未执行）: " + err.Error()}
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if w.maxTimeout > 0 && timeout > w.maxTimeout {
		timeout = w.maxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr, code, runErr := w.runner(runCtx, req.Command, argv)
	output, truncated := truncateProbeOutput(stdout, stderr, w.policy.MaxOutputBytes)
	meta := fmt.Sprintf("\n[节点 %s 执行: %s %s]", w.hostname, req.Command, strings.Join(argv, " "))
	output += meta
	if truncated {
		output += "\n[输出已按策略截断，上面不是全部输出]"
	}
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			return probeOutcome{Status: models.NodeProbeTimedOut, Output: output, ExitCode: code,
				Error: fmt.Sprintf("执行超时（%s）", timeout)}
		}
		return probeOutcome{Status: models.NodeProbeFailed, Output: output, ExitCode: code,
			Error: llm.Redact(runErr.Error())}
	}
	// 退出码非 0 也算"探测执行成功但命令失败"：这是**有效证据**（例如 tasklist
	// 没找到匹配进程返回 1），不能丢掉，否则模型又会去猜。
	return probeOutcome{Status: models.NodeProbeDone, Output: output, ExitCode: code}
}

func (w *ProbeWorker) finish(id int64, status, output string, code int, errText string) {
	if len(errText) > 500 {
		errText = errText[:500]
	}
	upd := map[string]interface{}{
		"status":      status,
		"output":      output,
		"exit_code":   code,
		"error":       errText,
		"finished_at": time.Now().UnixMilli(),
	}
	if err := w.db.Model(&models.NodeProbeRequest{}).Where("id = ?", id).Updates(upd).Error; err != nil {
		logger.GetLogger().Error("[probe] 回写探测结果失败: " + err.Error())
	}
}

// AllowedCommands 本节点实际允许的探测命令（用于启动日志与文档）。
func (w *ProbeWorker) AllowedCommands() []string { return tools.AllowedShellCommands() }

func decodeArgv(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}, nil
	}
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		return nil, err
	}
	if argv == nil {
		argv = []string{}
	}
	return argv, nil
}

// truncateProbeOutput 按上限截断输出；返回的第二个值表示**确实发生了截断**。
//
// 截断必须显式标注：否则模型会把"被截断的输出"当成"就这么多"，
// 进而得出"没有这个进程"的结论——这又是一次捏造证据。
func truncateProbeOutput(stdout, stderr string, max int) (string, bool) {
	if max <= 0 {
		max = 64 * 1024
	}
	out := stdout
	if strings.TrimSpace(stderr) != "" {
		if out != "" {
			out += "\n"
		}
		out += "[stderr]\n" + stderr
	}
	if len(out) <= max {
		return out, false
	}
	// 保留头尾：开头的头部信息与结尾的汇总行都可能是关键证据
	head := max / 2
	tail := max - head
	return out[:head] + "\n...（中间省略）...\n" + out[len(out)-tail:], true
}
