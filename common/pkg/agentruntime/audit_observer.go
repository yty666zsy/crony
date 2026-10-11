package agentruntime

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 审计参数列的字节上限。
//
// 为什么要收口：工具参数里可能有整段日志/大段 SQL，直接落库会把审计表撑成
// 第二个轨迹表（而审计表的用途是"快速复盘"，不是"存全文"）。
const maxAuditArgsBytes = 4096

// ToolAuditObserver 返回"把工具调用审计落库"的回调。
//
// 设计取舍：
//   - **落库失败只记日志，绝不影响工具调用**：审计的价值在于"通常有"，
//     而不是"必须有，否则功能不可用"——把审计做成硬依赖，等于让一次
//     数据库抖动直接变成 Agent 不可用（运维现场最怕这种"次要组件拖垮主流程"）。
//   - **append-only**：审计表只有 Insert，没有 Update/Delete（见 models.AppendAgentToolAudit）。
//     一个可以被改写的审计日志，在被质疑时一文不值。
//   - **被拒的调用同样落库**：Decision/Reason 字段就是为"哪道闸拦的"设计的。
func ToolAuditObserver() tools.Observer {
	return func(rec tools.CallRecord) {
		if rec.SessionID == "" && rec.Tool == "" {
			return
		}
		db := dbclient.GetMysqlDB()
		if db == nil {
			// 数据库未初始化：不写库、不报错。工具调用本身照常进行，
			// 这段时间的审计缺失会由这条 warn 明示（而不是静默当作"没发生过"）。
			logger.GetLogger().Warn("[agent] 工具审计未落库：数据库未初始化")
			return
		}
		now := time.Now()
		row := &models.AgentToolAudit{
			AuditID:       models.NewToolAuditID(),
			SessionID:     clip(rec.SessionID, 128),
			AgentKind:     clip(rec.AgentKind, 32),
			RunID:         clip(rec.RunID, 64),
			Tool:          clip(rec.Tool, 64),
			Risk:          clip(rec.Risk.String(), 16),
			ArgsJSON:      auditArgsJSON(rec.Args),
			Decision:      clip(rec.Decision, 32),
			Reason:        clip(rec.Reason, 512),
			ApprovalID:    clip(rec.ApprovalID, 64),
			OK:            rec.OK,
			Err:           clip(rec.Err, 512),
			DurationMS:    rec.DurationMS,
			OutputBytes:   rec.OutputBytes,
			EvidenceCount: rec.Evidence,
			// 双口径时间必须自己填：零值会让"按时间排序的审计列表"看起来像坏了
			// （真库集成测试抓到的缺陷：行落了库，但 created/created_ms 全是 0）。
			Created:   now.Unix(),
			CreatedMS: now.UnixMilli(),
		}
		if len(rec.Targets) > 0 {
			row.TargetNode = clip(rec.Targets[0], 128)
		}
		if err := models.AppendAgentToolAudit(db, row); err != nil {
			logger.GetLogger().Warn("[agent] 工具审计落库失败（不影响本次调用）: " + err.Error())
		}
	}
}

// auditArgsJSON 序列化审计用的参数：**脱敏 + 长度收口**，且保证结果是合法 JSON。
//
// 为什么超限后不直接截断字符串：截断后的文本不再是合法 JSON，
// 控制台把它当 JSON 渲染时会解析失败——审计字段自己变成"坏数据"就太讽刺了。
// 因此超限时改写成 `{"_truncated":true,...}` 结构，保留前缀原文供人看。
func auditArgsJSON(args map[string]interface{}) string {
	if len(args) == 0 {
		return ""
	}
	buf, err := json.Marshal(args)
	if err != nil {
		// 参数无法序列化（理论上 Schema 已挡住）：存一句可读的说明，
		// 而不是存半截 JSON。
		return fmt.Sprintf("{\"_unserializable\":true,\"error\":%q}", clip(err.Error(), 200))
	}
	redacted := llm.Redact(string(buf))
	if len(redacted) <= maxAuditArgsBytes {
		return redacted
	}
	shadow := map[string]interface{}{
		"_truncated":   true,
		"_total_bytes": len(redacted),
		"_prefix":      llm.Truncate(redacted, maxAuditArgsBytes-200),
	}
	out, merr := json.Marshal(shadow)
	if merr != nil {
		return "{\"_truncated\":true}"
	}
	return string(out)
}

// clip 按字节上限收口（落库前列宽兜底；回退到 UTF-8 边界，避免砍出非法 UTF-8）。
//
// 预留 32 字节给 llm.Truncate 追加的省略标记：否则"截断后反而更长"会撑破列宽，
// 在 MySQL 严格模式下直接 `Error 1406 Data too long` 让整条审计插入失败。
func clip(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return llm.Truncate(s, limit-32)
}
