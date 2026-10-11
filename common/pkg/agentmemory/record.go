package agentmemory

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
)

// Store 经验库的写入口（由 admin 侧用 gorm 实现）。
//
// 只暴露"写一条"这一个方法：抽取向量的正确性由 Extract 的单测保证，
// 存储细节（去重、计数累加）由实现负责。接口面越小，越不容易在两边都写一半逻辑。
type Store interface {
	Upsert(mem *models.AgentMemory) (created bool, err error)
}

// RecordResult 一次沉淀的结果（用于日志与接口回执）。
type RecordResult struct {
	// Extracted 抽取出的观测条数。
	Extracted int `json:"extracted"`
	// Created 新建的经验条数（去重键首次出现）。
	Created int `json:"created"`
	// Updated 命中去重键、计数被累加的条数。
	Updated int `json:"updated"`
	// MemoryIDs 本次涉及的经验条目号（含被累加的已有条目）。
	MemoryIDs []string `json:"memory_ids"`
	// Errors 单条写入失败的原因（**不吞**：沉淀失败要被看见，但不阻断会话）。
	Errors []string `json:"errors,omitempty"`
}

// Recorder 把一次会话沉淀进经验库。
//
// 设计原则（与审计观察者一致）：**失败绝不阻断主流程**。
// 会话落库、接口返回结论都不应该因为"经验没存上"而失败——
// 但也不能静默：失败必须进日志，并且在 RecordResult.Errors 里留下痕迹。
type Recorder struct {
	store Store
	// now 可注入，便于测试去重计数的时间语义。
	now func() time.Time
	// logger 可替换（默认标准库 log）。
	logf func(format string, args ...interface{})
}

// NewRecorder 创建沉淀器。
func NewRecorder(store Store) *Recorder {
	return &Recorder{store: store, now: time.Now, logf: log.Printf}
}

// WithLogger 替换日志函数（测试用，避免把告警打进测试输出造成噪声）。
func (r *Recorder) WithLogger(f func(format string, args ...interface{})) *Recorder {
	if f != nil {
		r.logf = f
	}
	return r
}

// WithClock 替换时钟（测试用）。
func (r *Recorder) WithClock(f func() time.Time) *Recorder {
	if f != nil {
		r.now = f
	}
	return r
}

// Record 抽取并落库。
//
// 返回结果里带 Errors：调用方（admin/agentruntime）可以把它写进日志或响应，
// 但**不应该**据此返回失败——经验沉淀是旁路，不是主流程的一环。
func (r *Recorder) Record(session *models.AgentSession, audits []models.AgentToolAudit) RecordResult {
	res := RecordResult{MemoryIDs: []string{}}
	if r == nil || r.store == nil || session == nil {
		return res
	}
	for _, obs := range Extract(session, audits) {
		res.Extracted++
		mem := obs.ToAgentMemory(r.now())
		created, err := r.store.Upsert(mem)
		if err != nil {
			msg := fmt.Sprintf("经验沉淀失败（session=%s action=%s）：%v", obs.SessionID, obs.ActionText, err)
			res.Errors = append(res.Errors, msg)
			if r.logf != nil {
				r.logf("[agent-memory] %s", msg)
			}
			continue
		}
		res.MemoryIDs = append(res.MemoryIDs, mem.MemoryID)
		if created {
			res.Created++
		} else {
			res.Updated++
		}
	}
	if res.Extracted > 0 && r.logf != nil {
		r.logf("[agent-memory] 会话 %s 沉淀：抽取 %d 条（新建 %d / 去重累加 %d），涉及条目 %s",
			session.SessionID, res.Extracted, res.Created, res.Updated, strings.Join(res.MemoryIDs, ","))
	}
	return res
}
