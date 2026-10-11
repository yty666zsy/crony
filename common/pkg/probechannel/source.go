package probechannel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/tools"
	"gorm.io/gorm"
)

// Source admin 侧的节点只读探测数据源。
//
// 工作方式：写一条 pending 请求进 `node_probe_request` → 等目标节点轮询领取并执行
// → 读回结果。**不直连节点**：节点可能在内网/NAT 后面，且 admin 直连就得再开一个
// 监听端口（扩大攻击面），而复用共享库既没有新端口，又天然留下一条审计记录。
//
// 为什么超时必须是**硬上限**：工具调用挂在模型的一次请求上，节点如果离线，
// 无限等下去会把整个诊断会话卡死（实测过一次 20s WriteTimeout 把连接打断的故障）。
// 所以到时就返回"未在时限内完成"，并把**节点可能离线**这条信息明确带出去。
type Source struct {
	db *gorm.DB
	// pollInterval 轮询间隔（默认 300ms：探测是交互式使用，要"感觉像是实时的"）
	pollInterval time.Duration
	// waitTimeout 等待上限（默认 15s；工具自身 Timeout 是 20s，留出余量）
	waitTimeout time.Duration
	// policy 用于"绕过工具层直接调数据源"时的兜底再校验（目录白名单等）。
	//
	// 阶段 4 起由装配方注入**同一份**生效策略：此前这里硬编码 DefaultPolicy()，
	// 于是"部署方收窄了可读路径"与"兜底检查仍按默认放行"会不一致——
	// 兜底检查比主检查更松，等于给绕行者留了一条路。
	policy tools.Policy
}

var _ tools.ProbeSource = (*Source)(nil)

// NewSource 创建数据源。db 为空时返回 error（静默降级会让模型以为
// "通道存在但没结果"，比明确不可用更糟）。
func NewSource(db *gorm.DB) (*Source, error) {
	if db == nil {
		return nil, fmt.Errorf("节点探测数据源需要数据库连接")
	}
	return &Source{db: db, pollInterval: 300 * time.Millisecond, waitTimeout: 15 * time.Second, policy: tools.DefaultPolicy()}, nil
}

// SetPolicy 注入生效的只读工具策略（装配方调用；未注入时退化为保守默认策略）。
func (s *Source) SetPolicy(p tools.Policy) { s.policy = p.Normalize() }

// SetWaitTimeout 调整等待上限（测试与"交互式快问快答"场景使用）。
//
// 非正值忽略：0 等待会让每次探测都必然 expired，那不是配置而是故障。
func (s *Source) SetWaitTimeout(d time.Duration) {
	if d > 0 {
		s.waitTimeout = d
	}
}

// SetPollInterval 调整轮询间隔（测试用；间隔越小越跟手，但库压力越大）。
func (s *Source) SetPollInterval(d time.Duration) {
	if d > 0 {
		s.pollInterval = d
	}
}

// Probe 实现 tools.ProbeSource。
func (s *Source) Probe(ctx context.Context, req tools.ProbeRequest) (tools.ProbeResult, error) {
	node := strings.TrimSpace(req.NodeUUID)
	if node == "" {
		return tools.ProbeResult{}, fmt.Errorf("node_uuid 不能为空（探测必须指明目标节点，admin 侧没有\"当前节点\"语义）")
	}
	if strings.TrimSpace(req.Command) == "" {
		return tools.ProbeResult{}, fmt.Errorf("command 不能为空")
	}
	// 防御性再校验一次（工具层已校验；这里是"即使有人绕过工具直接调数据源"的兜底）。
	// policy 为零值时 Normalize 会补齐成保守默认策略，不会退化成"不校验"。
	if err := tools.GuardShellCommand(req.Command, req.Args, s.policy.Normalize()); err != nil {
		return tools.ProbeResult{}, err
	}
	// 目标节点必须存在且在库里有登记，否则请求会一直挂着等一个不存在的节点，
	// 调用方（模型）拿到的却是"超时"——原因会被误读成"节点没响应"。
	var nodeRow models.Node
	if err := s.db.Where("uuid = ?", node).First(&nodeRow).Error; err != nil {
		if models.IsRecordNotFound(err) {
			return tools.ProbeResult{}, fmt.Errorf("节点 %s 不存在（无法探测：没有这台机器）", node)
		}
		return tools.ProbeResult{}, fmt.Errorf("查询节点失败: %s", err.Error())
	}

	argvJSON, err := json.Marshal(append([]string{}, req.Args...))
	if err != nil {
		return tools.ProbeResult{}, fmt.Errorf("参数序列化失败: %s", err.Error())
	}
	timeoutSec := int(req.Timeout.Seconds())
	if timeoutSec <= 0 || timeoutSec > 60 {
		timeoutSec = 15
	}
	row := &models.NodeProbeRequest{
		RequestID:      models.NewProbeRequestID(node, time.Now()),
		NodeUUID:       node,
		Command:        req.Command,
		ArgvJSON:       string(argvJSON),
		TimeoutSeconds: timeoutSec,
		Status:         models.NodeProbePending,
		Reason:         clipText(req.Reason, 255),
		CreatedBy:      clipText(req.CreatedBy, 64),
		Created:        time.Now().UnixMilli(),
	}
	if err := s.db.Create(row).Error; err != nil {
		return tools.ProbeResult{}, fmt.Errorf("写入探测请求失败: %s", err.Error())
	}

	deadline := time.Now().Add(s.waitTimeout)
	for {
		cur, err := models.FindProbeRequest(s.db, row.RequestID)
		if err != nil {
			return tools.ProbeResult{}, fmt.Errorf("读取探测结果失败: %s", err.Error())
		}
		if cur != nil && cur.Status != models.NodeProbePending && cur.Status != models.NodeProbeRunning {
			return toProbeResult(cur), nil
		}
		if time.Now().After(deadline) {
			// 明确标记 expired 并**如实说明**可能的原因：节点离线 / 探测通道未启用。
			// 不要把"没人来领"说成"命令执行失败"——那会让模型得出错误的结论。
			reason := fmt.Sprintf("节点 %s 在 %s 内没有领取这条探测请求：该节点可能离线，"+
				"或它的探测通道未启用（节点侧未运行/未连接数据库）。这不等于命令执行失败。",
				node, s.waitTimeout)
			s.markExpired(row.RequestID, reason)
			return tools.ProbeResult{
				RequestID: row.RequestID,
				Status:    models.NodeProbeExpired,
				ExitCode:  -1,
				Error:     reason,
			}, nil
		}
		select {
		case <-ctx.Done():
			reason := "调用被取消（诊断会话结束/超时），探测请求已标记 expired"
			s.markExpired(row.RequestID, reason)
			return tools.ProbeResult{
				RequestID: row.RequestID,
				Status:    models.NodeProbeExpired,
				ExitCode:  -1,
				Error:     reason,
			}, nil
		case <-time.After(s.pollInterval):
		}
	}
}

// markExpired 把没人领取的请求标记为 expired，并**把原因写进审计行**。
//
// 为什么原因必须落库：审计行如果只有 status 没有理由，事后回看会得到
// "这行没有输出" 而无法区分"节点离线"和"命令跑了没输出"——实测中模型正是
// 在探测不可用时凭"没有输出"编造了 netstat 结果（session r54）。
func (s *Source) markExpired(requestID, reason string) {
	_ = s.db.Model(&models.NodeProbeRequest{}).
		Where("request_id = ? AND status IN ?", requestID, []string{models.NodeProbePending, models.NodeProbeRunning}).
		Updates(map[string]interface{}{
			"status":      models.NodeProbeExpired,
			"exit_code":   -1,
			"error":       clipText(reason, 500),
			"finished_at": time.Now().UnixMilli(),
		}).Error
}

func toProbeResult(cur *models.NodeProbeRequest) tools.ProbeResult {
	return tools.ProbeResult{
		RequestID: cur.RequestID,
		Status:    cur.Status,
		Output:    cur.Output,
		ExitCode:  cur.ExitCode,
		Error:     cur.Error,
		Hostname:  cur.NodeHostname,
	}
}

func clipText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// DefaultSource 使用全局数据库连接创建数据源（装配用）。
func DefaultSource() (*Source, error) {
	return NewSource(dbclient.GetMysqlDB())
}
