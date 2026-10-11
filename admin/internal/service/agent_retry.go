package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// JobRetryExecutor 把 `job_retry` 这个写动作接到 crony 既有的"立即执行一次"通道上。
//
// 为什么复用 Once（etcd 键 + TTL 60s）而不是自己起协程跑命令：
//   - 同一条投递路径已被节点侧的调度/执行/日志链路完整覆盖（含并发策略、超时、重试）；
//   - 审批执行方**不应该**拥有第二条执行路径——两条路径意味着两种行为，
//     而"审批后到底会发生什么"必须唯一。
//
// 它是"写动作"的最后一公里：前面的网关负责"能不能做"，这里负责"做什么"。
type JobRetryExecutor struct {
	// Once 投递通道（默认走 DefaultJobService.Once，测试可注入替身）。
	Once func(req *request.ReqJobOnce) error
	// FindJob/FindNode 读取任务与节点（默认走 models，测试可注入）。
	FindJob  func(jobID int) (*models.Job, error)
	FindNode func(uuid string) (*models.Node, error)
}

// NewJobRetryExecutor 生产实现。
func NewJobRetryExecutor() *JobRetryExecutor {
	return &JobRetryExecutor{
		Once: func(req *request.ReqJobOnce) error { return DefaultJobService.Once(req) },
		FindJob: func(jobID int) (*models.Job, error) {
			job := &models.Job{ID: jobID}
			if err := job.FindById(); err != nil {
				return nil, err
			}
			return job, nil
		},
		FindNode: func(uuid string) (*models.Node, error) {
			node := &models.Node{UUID: uuid}
			if err := node.FindByUUID(); err != nil {
				return nil, err
			}
			return node, nil
		},
	}
}

// RetryJob 实现 tools.RetryExecutor。
//
// 失败一律返回 `Result{OK:false, Err:...}` 而不是 error：写动作的失败是**业务事实**，
// 要让审批者与模型都看到"没跑成、为什么没跑成"；返回 error 会被上层当成基础设施故障，
// 反而把可解释的失败变成 500。
func (e *JobRetryExecutor) RetryJob(ctx context.Context, req tools.RetryRequest) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	if req.JobID <= 0 {
		return failedResult("任务 ID 非法", fmt.Errorf("job_id=%d", req.JobID))
	}
	if e == nil || e.FindJob == nil || e.Once == nil {
		return tools.Result{}, fmt.Errorf("%w: 重跑执行器未装配", tools.ErrToolDenied)
	}
	job, err := e.FindJob(req.JobID)
	if err != nil {
		return failedResult(fmt.Sprintf("任务 %d 不存在（可能已被删除）", req.JobID), err)
	}
	// 重跑**必须**落到原节点：换一台节点执行同一份任务，语义已经变了
	// （可能是不同的数据目录、不同的运行时），那已经不是"重跑"而是"再跑一次"。
	nodeUUID := strings.TrimSpace(job.RunOn)
	if nodeUUID == "" {
		return failedResult(fmt.Sprintf("任务 %d 未分配节点（status=%d），无法重跑", req.JobID, job.Status), nil)
	}
	if e.FindNode != nil {
		node, err := e.FindNode(nodeUUID)
		if err != nil {
			return failedResult(fmt.Sprintf("目标节点 %s 不存在，无法重跑", nodeUUID), err)
		}
		if node != nil && node.Status == models.NodeConnFail {
			return failedResult(fmt.Sprintf("目标节点 %s 连接异常，重跑无法投递", nodeUUID), nil)
		}
	}
	if err := e.Once(&request.ReqJobOnce{JobId: req.JobID, NodeUUID: nodeUUID}); err != nil {
		return failedResult(fmt.Sprintf("投递重跑失败（job_id=%d node=%s）", req.JobID, nodeUUID), err)
	}
	summary := fmt.Sprintf("已按审批结果触发任务 %d 在节点 %s 上重跑一次", req.JobID, nodeUUID)
	output := fmt.Sprintf("动作: 重跑任务 %d\n目标节点: %s\n触发方式: 立即执行一次（etcd 投递，TTL 60s）\n审批理由: %s\n",
		req.JobID, nodeUUID, llm.Truncate(req.Reason, 200))
	return tools.Result{
		OK:      true,
		Summary: summary,
		Output:  output,
		// 证据：这次动作**有据可查**（job_id + 节点 + 投递时间），符合"结论必须挂证据"。
		Evidence: []tools.Evidence{{
			Source: "job",
			Ref:    fmt.Sprintf("job:%d", req.JobID),
			Quote:  fmt.Sprintf("任务 %d 已在节点 %s 投递一次重跑（%s）", req.JobID, nodeUUID, time.Now().Format("2006-01-02 15:04:05")),
		}},
		Meta: map[string]interface{}{
			"job_id":    req.JobID,
			"node_uuid": nodeUUID,
			"reason":    llm.Truncate(req.Reason, 200),
		},
	}, nil
}

// failedResult 组装"失败的写动作结果"（带上可读原因与原始错误）。
func failedResult(msg string, err error) (tools.Result, error) {
	detail := msg
	if err != nil {
		detail = msg + "：" + err.Error()
	}
	return tools.Result{OK: false, Summary: msg, Err: llm.Redact(detail)}, nil
}
