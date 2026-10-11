package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
)

// ExtractJSON 从模型输出里提取 JSON 文本。
//
// 为什么需要它：即使声明了 `response_format=json_object`，真实模型（尤其是
// 自建/小模型）仍常见三种跑偏形态：
//  1. 包在 Markdown 代码块里：```json { ... } ```
//  2. 前后带解释文字："好的，以下是结果：{ ... } 希望对你有帮助"
//  3. 直接给数组：[ { ... } ]
//
// 这里做"最小容忍"：先整体解析，失败则扫描第一个 `{`/`[` 并做**括号配对**
// （正确处理字符串内的括号与转义），配不上就判定失败。
// 容忍度刻意保守：不修 JSON 语法（不改单引号、不补逗号），因为"帮模型改错"
// 会让下一个环节拿到看似合法、实则语义漂移的数据。
func ExtractJSON(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "", false
	}
	if json.Valid([]byte(trimmed)) {
		return trimmed, true
	}
	// 剥掉 ``` 围栏后再试一次
	if un := stripFence(trimmed); un != trimmed {
		if json.Valid([]byte(un)) {
			return un, true
		}
		trimmed = un
	}
	start := strings.IndexAny(trimmed, "{[")
	if start < 0 {
		return "", false
	}
	end, ok := matchBrackets(trimmed[start:])
	if !ok {
		return "", false
	}
	candidate := trimmed[start : start+end]
	if !json.Valid([]byte(candidate)) {
		return "", false
	}
	return candidate, true
}

// stripFence 去掉 Markdown 代码围栏。
func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// 跳过 ```json 这一行
	if nl := strings.Index(s, "\n"); nl >= 0 {
		s = s[nl+1:]
	} else {
		return s
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// matchBrackets 从第一个括号开始做配对，返回"结束位置（不含）"。
// 处理后：字符串内的括号/转义、以及 [] 与 {} 混用。
func matchBrackets(s string) (int, bool) {
	var stack []byte
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, ch)
		case '}', ']':
			if len(stack) == 0 {
				return 0, false
			}
			open := stack[len(stack)-1]
			if (open == '{' && ch != '}') || (open == '[' && ch != ']') {
				return 0, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// ValidateStructured 从模型输出中提取 JSON 并按 Schema 校验。
//
// 返回的第二个值始终是"提取到的对象"（可能为 nil），便于调用方在失败时
// 把原始输出写进轨迹——诊断场景下"模型到底说了什么"本身就是证据。
func ValidateStructured(schema *jsonschema.Schema, content string) (map[string]interface{}, error) {
	if schema == nil {
		return nil, fmt.Errorf("%w: 未提供 ResponseSchema", ErrSchemaViolation)
	}
	raw, ok := ExtractJSON(content)
	if !ok {
		return nil, fmt.Errorf("%w: 输出中未找到合法 JSON（前 200 字符: %s）", ErrSchemaViolation, Truncate(content, 200))
	}
	var value interface{}
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("%w: JSON 解析失败: %v", ErrSchemaViolation, err)
	}
	if err := schema.Validate(value); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSchemaViolation, err)
	}
	obj, ok := value.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%w: 顶层必须是 JSON 对象，实际 %T", ErrSchemaViolation, value)
	}
	return obj, nil
}

// StringOr 从结构化输出里取字符串字段，兼容模型把数字/布尔写成字符串的情况。
func StringOr(obj map[string]interface{}, key, fallback string) string {
	v, ok := obj[key]
	if !ok || v == nil {
		return fallback
	}
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return fallback
		}
		return s
	}
	return fmt.Sprintf("%v", v)
}

// SliceOf 从结构化输出里取数组字段（元素为对象）。
// 模型有时会把"只有一个元素的数组"写成对象，这里做一次宽容处理并记录在调用方。
func SliceOf(obj map[string]interface{}, key string) []map[string]interface{} {
	v, ok := obj[key]
	if !ok || v == nil {
		return nil
	}
	switch typed := v.(type) {
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(typed))
		for _, item := range typed {
			if m, ok := item.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]interface{}:
		return []map[string]interface{}{typed}
	default:
		return nil
	}
}

// FloatOr 取数值字段。
func FloatOr(obj map[string]interface{}, key string, fallback float64) float64 {
	v, ok := obj[key]
	if !ok || v == nil {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
			return f
		}
	}
	return fallback
}
