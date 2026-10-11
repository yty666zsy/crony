package handler

import (
	"encoding/json"
	"strings"
	"testing"
)

// 控制台回放面板按工具名渲染调用分布，所以 `tool_stats.per_tool` 必须**恒定存在**：
// 字段消失会让前端无法区分"这次没调工具"和"服务端版本不支持"，
// 也会让契约类验收用例时红时绿（取决于本轮模型恰好用没用工具）。
func TestToolStatsViewAlwaysSerializesPerTool(t *testing.T) {
	// 1) 没有工具调用（nil → 必须归一成 {}）
	b, err := json.Marshal(agentToolStatsView{Calls: 0, PerTool: orEmptyIntMap(nil)})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(b), `"per_tool":{}`) {
		t.Fatalf("空统计时 per_tool 必须序列化成 {}（不能消失，也不能是 null）: %s", b)
	}

	// 2) 有工具调用：分布照常输出，且与 calls 自洽
	v := agentToolStatsView{Calls: 3, PerTool: orEmptyIntMap(map[string]int{"shell_readonly": 2, "node_status": 1})}
	b2, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(b2), `"per_tool":{"node_status":1,"shell_readonly":2}`) {
		t.Fatalf("per_tool 分布不符: %s", b2)
	}
	sum := 0
	for _, n := range v.PerTool {
		sum += n
	}
	if sum != v.Calls {
		t.Fatalf("per_tool 合计 %d 与 calls %d 不自洽", sum, v.Calls)
	}
}
