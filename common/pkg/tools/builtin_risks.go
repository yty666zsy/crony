package tools

// 内置工具的**静态**风险表。
//
// 为什么要单独有一份：评测回放（阶段 5 的"轨迹 → 评测"）要判定**历史轨迹**里的一次
// 工具调用是不是写动作，但历史里只有 tool_name，没有活的工具实例。风险级别是工具的
// **静态属性**（只看类型，不看参数/环境），所以可以离线取。
//
// 为什么不手抄一张表：手抄表会与工具实现漂移，而漂移的后果是**安全断言悄悄失效**
// （例如 job_retry 被改成写工具后，回放评测仍按只读放行）。这里直接取各工具实例的
// Name()/Risk()，它们都是常量返回、对零值安全，因此**不可能漂移**。
// 单元测试 TestStaticRisksCoverBuiltins 会钉住这一点。

// builtinToolInstances 内置工具的零值实例。
//
// 只用于读取 name/risk 这类**静态**属性，不用于执行——
// 零值实例没有数据源，调用 Call 会失败，这是刻意的：这里不需要它执行任何东西。
func builtinToolInstances() []Tool {
	return []Tool{
		// 只读工具箱
		&ShellTool{},
		&HTTPGetTool{},
		&NodeStatusTool{},
		&NodeProbeTool{},
		&LogSearchTool{},
		&SQLTool{},
		// 写工具箱（唯一的内置写工具）
		&JobRetryTool{},
	}
}

// StaticRisks 返回内置工具的"名称 → 风险级别"。
//
// 注意：**只覆盖内置工具**。MCP/插件工具不在表里，查不到就是"未分类"——
// 调用方（评测）必须把未分类当成"判不了"并显式失败，绝不能默认放行。
func StaticRisks() map[string]RiskLevel {
	m := make(map[string]RiskLevel, len(builtinToolInstances()))
	for _, t := range builtinToolInstances() {
		m[t.Name()] = t.Risk()
	}
	return m
}

// StaticRisk 查一个内置工具的风险级别；ok=false 表示"不在内置表里/未分类"。
func StaticRisk(name string) (RiskLevel, bool) {
	r, ok := StaticRisks()[name]
	return r, ok
}
