package tools

import "testing"

// 静态风险表（评测回放用：从历史轨迹的 tool_name 反推是不是写动作）必须与
// "实际能注册进来的内置工具"**完全对齐**：
//   - 注册表里的每个工具都必须在表里，且风险级别一致——漏一个，
//     评测回放就会把写动作当"未分类"甚至当只读放行；
//   - 表里不能有注册表里没有的名字——多一个就是幽灵条目，
//     会在工具改名后继续"证明"一个不存在的工具是只读的。
//
// 这条用例的价值在于：这张表是从工具实例的 Name()/Risk() 推导出来的，
// 所以"实现改了、表没改"这种漂移不可能发生——但仍要钉住"覆盖完整"。
func TestStaticRisksMatchRegistrableBuiltins(t *testing.T) {
	// 写工具在 allow_write=false 的策略下注册会被拒（这正是"默认不动手"），
	// 但本用例要验的是**风险级别**，所以显式打开写能力再注册。
	policy := DefaultPolicy()
	policy.AllowWrite = true
	r, err := NewReadonlyRegistry(policy, Sources{
		Logs:       &fakeLogSource{},
		NodeStatus: &fakeNodeSource{},
		SQL:        &fakeSQLSource{},
		Probe:      &fakeProbeSource{},
	})
	if err != nil {
		t.Fatalf("注册只读工具失败: %v", err)
	}
	// job_retry 是唯一的内置写工具，单独注册进来一起对齐。
	if err := r.Register(&JobRetryTool{}); err != nil {
		t.Fatalf("注册 job_retry 失败: %v", err)
	}

	risks := StaticRisks()
	for _, name := range r.Names() {
		tool, ok := r.Get(name)
		if !ok {
			t.Fatalf("注册表自相矛盾：Names 里有 %s 但 Get 取不到", name)
		}
		got, ok := risks[name]
		if !ok {
			t.Fatalf("静态风险表缺少内置工具 %s", name)
		}
		if got != tool.Risk() {
			t.Fatalf("静态风险表与实现不一致：%s 表=%s 实现=%s", name, got, tool.Risk())
		}
		if !got.Valid() {
			t.Fatalf("静态风险表里 %s 的风险级别非法：%s", name, got)
		}
	}
	for name := range risks {
		if _, ok := r.Get(name); !ok {
			t.Fatalf("静态风险表存在注册表里没有的工具 %s", name)
		}
	}
}

// 未知工具（MCP/插件）必须返回 not-ok，**绝不能猜一个风险级别**：
// 猜"只读"会让安全断言静默放行，猜"写"会让评测满屏误报。两种都在骗人。
func TestStaticRiskUnknownToolNotGuessed(t *testing.T) {
	if r, ok := StaticRisk("mcp_some_tool"); ok {
		t.Fatalf("未知工具不该有风险级别：%s", r)
	}
	if _, ok := StaticRisk(""); ok {
		t.Fatalf("空工具名不该命中任何风险级别")
	}
}
