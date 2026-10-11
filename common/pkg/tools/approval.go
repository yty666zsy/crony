package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ApprovalInput 一份待人工审批的写动作。
//
// 为什么 Reason（模型自述理由）是必填语义：运维人员批的不是"job_retry(21)"，
// 而是"模型为什么认为要重跑"。没有理由的审批单会退化成"闭眼点同意"，
// 而"闭眼点同意"比"没有审批"更危险——它给人一种有人在把关的错觉。
type ApprovalInput struct {
	SessionID string
	RunID     string
	AgentKind string
	Tool      string
	Risk      RiskLevel
	Args      map[string]interface{}
	// ArgsHash 是 Args 的规范哈希：批准与执行之间必须能证明"执行的就是被批准的那件事"。
	ArgsHash string
	// Reason 模型自述的"为什么要做这件事"。
	Reason string
	// Targets 本次动作影响的目标（节点 UUID 等）。
	Targets []string
	// Impact 人类可读的影响面摘要（"1 个任务（job_id=21）/ 1 台节点"）。
	Impact string
	// RequestedBy 发起方（provider/model）。
	RequestedBy string
	// TTL 审批有效期（<=0 时由调用方/策略默认值兜底）。
	TTL time.Duration
}

// ApprovalOutcome 审批单生成结果。
type ApprovalOutcome struct {
	// ApprovalID 审批单号（形如 ap-<nano>）。
	ApprovalID string
	// Status 生成时的状态，正常为 pending。
	Status string
	// ExpiresAt 到期时刻（Unix 秒；0 表示未设置）。
	ExpiresAt int64
	// Summary 一句话人话摘要（既给人看，也回灌给模型）。
	Summary string
}

// Approver 审批单的**生成方**。
//
// 为什么放在 tools 包做接口、实现放 admin：
// tools 包不依赖数据库（它只做"闸门"），落库是控制面的职责。
// 这样做的直接好处是：网关的全部闸门都能用纯内存单测覆盖，
// 而"审批单真的落库了吗"由 admin 侧的集成测试用真库回答。
type Approver interface {
	RequestApproval(ctx context.Context, in ApprovalInput) (ApprovalOutcome, error)
}

// ArgsHash 计算参数的规范哈希（sha256 十六进制，64 字符，正好等于列宽）。
//
// 规范性的来源：Go 的 encoding/json 编码 `map[string]interface{}` 时对键**排序**，
// 嵌套 map 同样排序，数组保序。因此"同一份参数"在任何时刻、任何进程算出的哈希一致。
// 数值口径需注意：模型给的数字经 JSON 解码后是 float64（`3` 与 `3.0` 都会编码成 `3`），
// 而入库后再读出来仍是 float64，两次一致——这正是我们要的性质。
func ArgsHash(args map[string]interface{}) string {
	if args == nil {
		args = map[string]interface{}{}
	}
	buf, err := json.Marshal(args)
	if err != nil {
		// 参数里出现无法序列化的值（理论上不会：Schema 已校验过类型）。
		// 这里**不能**退化成"随便给个哈希"——那会让 TOCTOU 校验失去意义，
		// 因此返回一个绝不会与真实哈希相等的标记值，调用方会因不匹配而拒绝执行。
		return "unhashable-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// approvalObservation 生成"已提交人工审批"的工具观测。
//
// 为什么把它做成**成功**观测而不是错误：
//   模型需要知道"这一步我做不了主，需要人"，才能如实写"已提交审批、等待人工"；
//   如果返回错误，模型会反复重试（重试会被爆炸半径预算拒掉，白烧步数），
//   更糟的是它可能换个说法把"没执行"写成"已执行"——阶段 3 正是在这一点上吃过亏。
//
// 观测里**必须**写明三件事：还没有任何副作用、审批单号、批准后会发生什么。
// 只说"需要审批"而不说"尚未执行"，模型很容易读成"已经在执行了"。
func approvalObservation(in ApprovalInput, out ApprovalOutcome) Result {
	target := ""
	if len(in.Targets) > 0 {
		target = strings.Join(in.Targets, "、")
	}
	var b strings.Builder
	b.WriteString("动作: " + in.Tool + "（风险级别 " + in.Risk.String() + "）\n")
	if in.Impact != "" {
		b.WriteString("影响面: " + in.Impact + "\n")
	}
	if target != "" {
		b.WriteString("目标: " + target + "\n")
	}
	b.WriteString("审批单: " + out.ApprovalID + "\n")
	b.WriteString("**尚未执行**：写动作只有人工批准后才会执行；批准前不会产生任何副作用。\n")
	b.WriteString("建议在结论里如实写\"已提交人工审批、等待批准\"，不要写成已执行。")
	if out.ExpiresAt > 0 {
		b.WriteString(fmt.Sprintf("\n审批有效期至: %s（超时未决策将自动作废）",
			time.Unix(out.ExpiresAt, 0).Format("2006-01-02 15:04:05")))
	}

	summary := out.Summary
	if summary == "" {
		summary = fmt.Sprintf("写动作已提交人工审批，尚未执行（审批单 %s）", out.ApprovalID)
	}
	return Result{
		OK:      true,
		Summary: summary,
		Output:  b.String(),
		// 证据：这句话**有据可查**（审批单号可查库），符合"结论必须挂证据"的纪律。
		Evidence: []Evidence{{
			Source: "approval",
			Ref:    "approval:" + out.ApprovalID,
			Quote:  summary,
		}},
		Meta: map[string]interface{}{
			"approval_id": out.ApprovalID,
			"approval_status": func() string {
				if out.Status == "" {
					return "pending"
				}
				return out.Status
			}(),
			"executed":   false,
			"risk":       in.Risk.String(),
			"targets":    in.Targets,
			"expires_at": out.ExpiresAt,
		},
	}
}