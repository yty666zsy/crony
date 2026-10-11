package models

import (
	"fmt"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
)

// probeSeq 同毫秒内的序号（request_id 有唯一索引，撞了就等于请求丢失）。
var probeSeq atomic.Uint64

// CronyNodeProbeTableName 节点侧只读探测请求表名。
//
// 背景（为什么需要这张表）：诊断/巡检要判断的是**目标机器本机的事实**——
// 端口有没有监听、进程在不在、服务什么状态、文件是否存在。这些事实 admin 侧
// 无论怎么查库都观测不到（库里没有），而模型不会说"我不知道"，它会直接断言
// （实测：`目标 MySQL 实例未在 node-it-01 上监听 127.0.0.1:13307`，而它连一次
// 工具都没调）。所以必须给节点开一条**只读、可审计、有界**的探测通道。
//
// 为什么用"请求表 + 节点轮询"而不是 admin 直连节点：
//   - 节点在 NAT/内网后面，admin 不一定能反向连上；
//   - 复用节点与 admin 已有的共享库连接，不新增监听端口（不扩大攻击面）；
//   - 每一次探测天然落库一条记录 = 审计（谁要的、为什么、跑了什么、结果如何）。
//
// 与 node_metric 一样，这里**不做时间序列**：探测请求是"一次性问答"，
// 过期请求由 status=expired 标记，不做历史清理策略（阶段 4 再谈保留期）。
const CronyNodeProbeTableName = "node_probe_request"

// 探测请求状态。
const (
	NodeProbePending  = "pending" // admin 已写入，节点尚未领取
	NodeProbeRunning  = "running" // 节点已领取并在执行
	NodeProbeDone     = "done"    // 节点执行完成（含命令返回非 0 退出码的情况，看 ExitCode）
	NodeProbeFailed   = "failed"  // 节点执行前/中失败（白名单拒绝、执行器错误）
	NodeProbeTimedOut = "timeout" // 节点侧执行超时
	NodeProbeExpired  = "expired" // 长时间无人领取（节点离线），admin 侧标记
)

// NodeProbeRequest 一条节点侧只读探测请求。
//
// 字段设计上的几个刻意选择：
//   - ArgvJSON 存 **argv 数组**而不是命令行字符串：没有 shell，就没有管道、重定向、
//     变量展开与命令注入（与 shell_readonly 同一条纪律）。节点侧执行时也不拼命令行。
//   - Reason/CreatedBy 是审计字段：审计要能回答"是谁、为什么让某台机器跑这条命令"，
//     只记命令名是不够的。
//   - Output 可能很大，节点侧写入前必须按策略截断（MaxOutputBytes）。
type NodeProbeRequest struct {
	ID        int64  `json:"id" gorm:"column:id;primary_key;auto_increment"`
	RequestID string `json:"request_id" gorm:"size:64;column:request_id;not null;uniqueIndex:uk_node_probe_request"`
	// NodeUUID 目标节点（与 node.uuid 对应）
	NodeUUID string `json:"node_uuid" gorm:"size:128;column:node_uuid;not null;index:idx_node_probe_node_status,priority:1"`
	// Command 只读命令名（必须在 tools.GuardShellCommand 白名单内）
	Command string `json:"command" gorm:"size:64;column:command;not null"`
	// ArgvJSON 参数数组的 JSON 文本
	ArgvJSON string `json:"argv_json" gorm:"type:text;column:argv_json"`
	// TimeoutSeconds 节点侧执行超时（有上限，防止长时间占用）
	TimeoutSeconds int `json:"timeout_seconds" gorm:"column:timeout_seconds;default:10"`
	// Status 见 NodeProbe* 常量
	Status string `json:"status" gorm:"size:16;column:status;not null;default:'pending';index:idx_node_probe_node_status,priority:2"`
	// Output 截断后的标准输出（含 [stderr] 段）
	Output string `json:"output" gorm:"type:text;column:output"`
	// ExitCode 命令退出码；-1 表示未能执行
	ExitCode int `json:"exit_code" gorm:"column:exit_code;default:-1"`
	// Error 失败原因（白名单拒绝 / 超时 / 执行器错误）
	Error string `json:"error" gorm:"size:512;column:error"`
	// Reason 为什么要探测（写进审计，也用于人话报告）
	Reason string `json:"reason" gorm:"size:255;column:reason"`
	// CreatedBy 发起人（admin 用户 / agent 会话）
	CreatedBy string `json:"created_by" gorm:"size:64;column:created_by"`
	// NodeHostname 执行节点的主机名（节点领取时填，便于事后追溯）
	NodeHostname string `json:"node_hostname" gorm:"size:128;column:node_hostname"`
	// Created/ClaimedAt/FinishedAt 毫秒时间戳
	Created    int64 `json:"created" gorm:"column:created;default:0"`
	ClaimedAt  int64 `json:"claimed_at" gorm:"column:claimed_at;default:0"`
	FinishedAt int64 `json:"finished_at" gorm:"column:finished_at;default:0"`
}

// TableName 实现 gorm.Tabler。
func (NodeProbeRequest) TableName() string { return CronyNodeProbeTableName }

// NewProbeRequestID 生成探测请求 ID。
//
// 形如 `p<jobID>-<UnixNano>-<seq>`：带序号是因为同一毫秒内可能连发多条，
// 而 request_id 上有唯一索引，撞了就是"请求丢失"，不能省。
func NewProbeRequestID(scope string, now time.Time) string {
	seq := probeSeq.Add(1)
	scope = sanitizeProbeScope(scope)
	if scope == "" {
		scope = "node"
	}
	return fmt.Sprintf("p%s-%d-%d", scope, now.UnixNano(), seq)
}

func sanitizeProbeScope(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		}
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return string(out)
}

// FindProbeRequest 按 request_id 查一条探测请求；不存在返回 (nil, nil)。
func FindProbeRequest(db *gorm.DB, requestID string) (*NodeProbeRequest, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库不可用")
	}
	var req NodeProbeRequest
	err := db.Where("request_id = ?", requestID).First(&req).Error
	if err != nil {
		if IsRecordNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &req, nil
}
