package models

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// CronyApprovalRequestTableName 审批单表名。
const CronyApprovalRequestTableName = "approval_request"

// 审批状态常量。
//
// 终态（不可逆）：executed / execution_failed / rejected / expired / canceled。
// 只有 pending 能被人工决策；**一次批准只能执行一次**，所以 executed 之后
// 任何 approve 都必须失败——否则"批一次、执行多次"就是一个静默的放大器。
const (
	ApprovalStatusPending         = "pending"
	ApprovalStatusApproved        = "approved"
	ApprovalStatusRejected        = "rejected"
	ApprovalStatusExpired         = "expired"
	ApprovalStatusCanceled        = "canceled"
	ApprovalStatusExecuted        = "executed"
	ApprovalStatusExecutionFailed = "execution_failed"
	// ApprovalStatusBlocked 执行前复核未通过（工具下线 / 策略收紧 / 参数被改）。
	//
	// 为什么不复用 execution_failed："已执行但失败"与"根本没执行"是两件完全不同的事，
	// 用一个状态表示会让 executed_at 变得不可信——而 executed_at 正是事后判断
	// "这次动作到底有没有发生"的唯一依据（验收 AC-18 第一次跑就抓到了这个问题：
	// 复核失败的行带上了非零 executed_at，等于凭空宣称"执行过了"）。
	ApprovalStatusBlocked = "blocked"
)

// DefaultApprovalTTL 审批单默认有效期：超过它仍未决策就作废。
//
// 为什么必须有 TTL：写动作的合理性依赖"当时的现场"。三天前模型判断"该重跑"，
// 三天后节点已经修好了，此时一键执行等于用过期判断做现在的事。
const DefaultApprovalTTL = 15 * time.Minute

// ApprovalRequest 一次写动作的审批单。
//
// 设计要点：
//   - **存档模型的自述理由**（Reason 字段）。运维人员批的不是"job_retry(21)"，
//     而是"模型为什么认为要重跑"；没有理由的审批单会退化成"闭眼点同意"；
//   - **存 ArgsHash 而不是只存参数**：批准与执行之间有时间窗，必须能证明
//     "执行的正是被批准的那件事"（TOCTOU：批准 A 执行 B）；
//   - **DecidedBy 必填**：谁批的必须能追到人。Agent 自己不能批准自己（入口层拦）。
type ApprovalRequest struct {
	ID int `json:"id" gorm:"column:id;primary_key;auto_increment"`
	// ApprovalID 外部引用（形如 ap-<nano>），不暴露自增 id。
	ApprovalID string `json:"approval_id" gorm:"size:64;column:approval_id;not null;uniqueIndex:uk_approval_request_id"`
	// SessionID 发起会话；Tool/Risk 被批准的动作与风险级别。
	SessionID string `json:"session_id" gorm:"size:128;column:session_id;not null;index:idx_approval_request_session"`
	Tool      string `json:"tool" gorm:"size:64;column:tool;not null;index:idx_approval_request_tool"`
	Risk      string `json:"risk" gorm:"size:16;column:risk;not null;default:'write'"`
	// ArgsJSON 待执行参数（**原文**存档）；ArgsHash 是同一份参数的规范哈希。
	//
	// 为什么存原文而不是存脱敏后的文本：批准与执行之间必须能证明"执行的就是被批准的那件事"，
	// 而哈希校验需要原文。一旦这里存的是被改写过的 JSON，自校验必然失败（要么永远执行不了，
	// 要么把改写后的参数当成"原件"）。脱敏发生在**对外展示**这一层（接口返回时再脱敏），
	// 而不是在存档层——审计表要留事实，展示层才做裁剪。
	ArgsJSON string `json:"args_json" gorm:"type:text;column:args_json"`
	ArgsHash string `json:"args_hash" gorm:"size:64;column:args_hash;not null;default:''"`
	// RunID 关联的执行记录（诊断类会话）；审批执行时用它把审计行归到同一次运行上。
	RunID string `json:"run_id" gorm:"size:128;column:run_id;default:'';index:idx_approval_request_run"`
	// Reason 模型自述的"为什么要做这件事"（原样存档，不加工）。
	Reason string `json:"reason" gorm:"type:text;column:reason"`
	// TargetNode 目标节点；Impact 影响面的人类可读摘要（"1 个任务 / 1 台节点"）。
	TargetNode string `json:"target_node" gorm:"size:128;column:target_node;default:'';index:idx_approval_request_node"`
	Impact     string `json:"impact" gorm:"size:512;column:impact;default:''"`
	// RequestedBy 发起方（provider/model + 会话类型），用于回答"哪个模型在什么场景下提的"。
	RequestedBy string `json:"requested_by" gorm:"size:128;column:requested_by;default:''"`
	// Status 见 ApprovalStatus* 常量。
	Status string `json:"status" gorm:"size:32;column:status;not null;default:'pending';index:idx_approval_request_status"`
	// DecidedBy/DecidedAt/DecisionNote 人工决策的事实（"谁批的"必须落库）。
	DecidedBy    string `json:"decided_by" gorm:"size:64;column:decided_by;default:''"`
	DecidedAt    int64  `json:"decided_at" gorm:"column:decided_at;default:0"`
	DecisionNote string `json:"decision_note" gorm:"size:512;column:decision_note;default:''"`
	// ExecutedAt/ExecutionResult 执行事实（结果截断后存档）。
	ExecutedAt      int64  `json:"executed_at" gorm:"column:executed_at;default:0"`
	ExecutionResult string `json:"execution_result" gorm:"type:text;column:execution_result"`
	// ExpiresAt 到期时刻（Unix 秒）；过期判定是**惰性**的（见 ExpirePendingApprovals）。
	ExpiresAt int64 `json:"expires_at" gorm:"column:expires_at;not null;default:0;index:idx_approval_request_expires"`
	Created   int64 `json:"created" gorm:"column:created;not null;default:0;index:idx_approval_request_created"`
	CreatedMS int64 `json:"created_ms" gorm:"column:created_ms;not null;default:0"`
}

// TableName 实现 gorm 表名约定。
func (a *ApprovalRequest) TableName() string { return CronyApprovalRequestTableName }

// NewApprovalID 生成审批单号（形如 ap-<nano>）：不暴露自增 id，
// 也不带时间格式之外的信息（单号本身不该泄漏"是谁提的"）。
func NewApprovalID() string {
	return "ap-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// CreateApprovalRequest 落一张待审批单。
func CreateApprovalRequest(db *gorm.DB, a *ApprovalRequest) error {
	return db.Table(CronyApprovalRequestTableName).Create(a).Error
}

// FindApprovalRequest 按 approval_id 查一张审批单（查不到返回 gorm.ErrRecordNotFound 包装后的错误）。
func FindApprovalRequest(db *gorm.DB, approvalID string) (*ApprovalRequest, error) {
	var out ApprovalRequest
	if err := db.Table(CronyApprovalRequestTableName).
		Where("approval_id = ?", approvalID).First(&out).Error; err != nil {
		return nil, err
	}
	return &out, nil
}

// ListApprovalRequests 分页查询（控制台审批台：默认按"最快到期"排序，
// 因为快过期的单子最需要人看一眼）。
func ListApprovalRequests(db *gorm.DB, status, tool, targetNode string, page, pageSize int) ([]ApprovalRequest, int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}
	q := db.Table(CronyApprovalRequestTableName)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if tool != "" {
		q = q.Where("tool = ?", tool)
	}
	if targetNode != "" {
		q = q.Where("target_node = ?", targetNode)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []ApprovalRequest
	err := q.Order("status = 'pending' desc, expires_at asc, id desc").
		Offset((page - 1) * pageSize).Limit(pageSize).Find(&out).Error
	return out, total, err
}

// DecideApprovalRequest 以 CAS 方式做一次人工决策（pending → approved/rejected/canceled）。
//
// 为什么必须用 `where status = 'pending'` 做条件更新而不是"先查后写"：
// 两个运维同时点"批准"时，先查后写会让两次批准都"成功"，
// 而写动作可能被执行两次。CAS 的 RowsAffected 就是"我是否真的抢到了这次决策"，
// 调用方必须检查它——返回 false 表示动作没生效（已被别人决策/已过期）。
func DecideApprovalRequest(db *gorm.DB, approvalID, toStatus, decidedBy, note string, now int64) (bool, error) {
	if toStatus != ApprovalStatusApproved && toStatus != ApprovalStatusRejected && toStatus != ApprovalStatusCanceled {
		return false, gorm.ErrInvalidValue
	}
	res := casUpdate(3, func() *gorm.DB {
		return db.Table(CronyApprovalRequestTableName).
			Where("approval_id = ? and status = ?", approvalID, ApprovalStatusPending).
			Updates(map[string]interface{}{
				"status":        toStatus,
				"decided_by":    decidedBy,
				"decided_at":    now,
				"decision_note": note,
			})
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// MarkApprovalExecuted 记录执行结果（approved → executed/execution_failed），同样是 CAS。
//
// approved → executed 这一步也必须是 CAS：否则"同一张已执行的单子"会被再执行一次。
func MarkApprovalExecuted(db *gorm.DB, approvalID, status, result string, now int64) (bool, error) {
	if status != ApprovalStatusExecuted && status != ApprovalStatusExecutionFailed {
		return false, gorm.ErrInvalidValue
	}
	res := casUpdate(3, func() *gorm.DB {
		return db.Table(CronyApprovalRequestTableName).
			Where("approval_id = ? and status = ?", approvalID, ApprovalStatusApproved).
			Updates(map[string]interface{}{
				"status":           status,
				"executed_at":      now,
				"execution_result": result,
			})
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// MarkApprovalBlocked 记录"执行前复核未通过"（approved → blocked），同样是 CAS。
//
// 与 MarkApprovalExecuted 的关键区别：**不动 executed_at**。
// 复核未通过意味着动作从未交给执行器，写上执行时间戳等于伪造执行事实。
func MarkApprovalBlocked(db *gorm.DB, approvalID, result string) (bool, error) {
	res := casUpdate(3, func() *gorm.DB {
		return db.Table(CronyApprovalRequestTableName).
			Where("approval_id = ? and status = ?", approvalID, ApprovalStatusApproved).
			Updates(map[string]interface{}{
				"status":           ApprovalStatusBlocked,
				"execution_result": result,
			})
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ---------------------------------------------------------------------------
// 并发 CAS 的锁竞争处理
//
// 真库集成测试（TestIntegrationConcurrentApproveExecutesExactlyOnce）实测到：
// 4 个并发"批准"打到同一张单子上时，MySQL 会对其中一条 CAS UPDATE 报
//   Error 1213: Deadlock found when trying to get lock; try restarting transaction
// 四分之一概率复现。后果不是数据错乱（赢家仍然只有一个），而是**输家看到的错误很奇怪**：
// 运维同时点"批准"，其中一个看到的是"死锁"，完全不知道这张单子其实已经被处理了。
//
// 为什么重试是安全的：被重试的语句都是纯 CAS（where 里带 status 条件），
// 重试不可能把"本该失败"的更新变成"成功"——别人赢了，where 就不再匹配。
// 这跟"重试一次副作用"有本质区别，后者才是绝对不能做的。
// ---------------------------------------------------------------------------

// isRetryableLockErr 判断是否为 InnoDB 锁竞争（死锁 / 锁等待超时）。
// 这两类错误的正确处置都是"重来一次"，而不是把它们上报成业务失败。
func isRetryableLockErr(err error) bool {
	if err == nil {
		return false
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1213 || myErr.Number == 1205
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadlock") || strings.Contains(msg, "lock wait timeout")
}

// casUpdate 执行一条 CAS 语句，遇到锁竞争时最多重试三次（退避 10ms/20ms）。
func casUpdate(attempts int, fn func() *gorm.DB) *gorm.DB {
	if attempts < 1 {
		attempts = 1
	}
	res := fn()
	for attempt := 1; attempt < attempts && res.Error != nil && isRetryableLockErr(res.Error); attempt++ {
		time.Sleep(time.Duration(attempt) * 10 * time.Millisecond)
		res = fn()
	}
	return res
}

// ExpirePendingApprovals 把超时未决策的单子置为 expired，并**写下原因**（返回生效行数）。
//
// 为什么是惰性的（在读取/决策时顺手调用）而不是后台定时器：
// 多实例部署下定时器会互相重复处理，而"读的时候顺手清一下过期"在任何部署形态下都成立。
// 原因文本必须自述"超时未决策"，不能只留 status——阶段 3 的 expired 不带原因已经吃过一次亏
// （事后回看无法区分"没人处理"与"处理了但没生效"）。
func ExpirePendingApprovals(db *gorm.DB, now int64) (int64, error) {
	res := casUpdate(3, func() *gorm.DB {
		return db.Table(CronyApprovalRequestTableName).
			Where("status = ? and expires_at > 0 and expires_at <= ?", ApprovalStatusPending, now).
			Updates(map[string]interface{}{
				"status":        ApprovalStatusExpired,
				"decision_note": "审批超时未决策（系统自动作废）：写动作的合理性依赖当时的现场，过期单子不允许再执行",
			})
	})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}
