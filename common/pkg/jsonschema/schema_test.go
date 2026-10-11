package jsonschema

import (
	"strings"
	"testing"
)

func TestSchemaValidCatchesAuthoringErrors(t *testing.T) {
	cases := []struct {
		name   string
		schema *Schema
		want   string
	}{
		{
			name:   "未知 type",
			schema: &Schema{Type: "strng"},
			want:   "未知的 type",
		},
		{
			name: "required 字段未定义",
			schema: &Schema{
				Type:       TypeObject,
				Properties: map[string]*Schema{"a": String("a")},
				Required:   []string{"b"},
			},
			want: "未在 properties 中定义",
		},
		{
			name:   "array 缺 items",
			schema: &Schema{Type: TypeArray},
			want:   "array 必须声明 items",
		},
		{
			name:   "非法正则",
			schema: String("a").WithPattern("(["),
			want:   "非法正则",
		},
		{
			name:   "正常对象",
			schema: Object(map[string]*Schema{"a": String("a")}, "a"),
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.schema.Valid()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("期望合法，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错包含 %q，实际通过", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestValidateReportsJSONPath 校验错误必须带路径：LLM/调用方要据此定位。
func TestValidateReportsJSONPath(t *testing.T) {
	schema := Object(map[string]*Schema{
		"evidence": Array(Object(map[string]*Schema{
			"source": String("证据来源").WithEnum("log", "step"),
			"quote":  String("证据原文"),
		}, "source", "quote"), "证据链"),
	}, "evidence")

	value := map[string]interface{}{
		"evidence": []interface{}{
			map[string]interface{}{"source": "log", "quote": "ok"},
			map[string]interface{}{"source": "猜的", "quote": "..."},
		},
	}
	err := schema.Validate(value)
	if err == nil {
		t.Fatal("期望校验失败")
	}
	if !strings.Contains(err.Error(), "$.evidence[1].source") {
		t.Fatalf("错误缺少可定位路径: %v", err)
	}
	if !IsValidationError(err) {
		t.Fatalf("应可用 IsValidationError 识别: %v", err)
	}
}

func TestValidateTypeAndRangeRules(t *testing.T) {
	cases := []struct {
		name    string
		schema  *Schema
		value   interface{}
		wantErr bool
		wantMsg string
	}{
		{name: "integer 接受 3", schema: Integer("n"), value: float64(3)},
		{name: "integer 拒绝 3.5", schema: Integer("n"), value: float64(3.5), wantErr: true, wantMsg: "期望类型 integer"},
		{name: "number 接受 3.5", schema: &Schema{Type: TypeNumber}, value: 3.5},
		{name: "不做隐式转换：字符串 3 不是 integer", schema: Integer("n"), value: "3", wantErr: true},
		{name: "整数区间下限", schema: Integer("n").WithRange(1, 10), value: float64(0), wantErr: true, wantMsg: "小于下限"},
		{name: "整数区间上限", schema: Integer("n").WithRange(1, 10), value: float64(11), wantErr: true, wantMsg: "超过上限"},
		{name: "字符串长度按字符数（中文）", schema: String("s").WithLength(1, 2), value: "中文"},
		{name: "字符串超长（中文 3 字）", schema: String("s").WithLength(1, 2), value: "中文字", wantErr: true, wantMsg: "超过上限"},
		{name: "pattern 不匹配", schema: String("s").WithPattern(`^df$`), value: "df -h", wantErr: true, wantMsg: "不匹配"},
		{name: "枚举字符串", schema: String("s").WithEnum("a", "b"), value: "c", wantErr: true, wantMsg: "不在允许枚举"},
		{name: "枚举数字 1 与 1.0 等价", schema: Integer("n").WithEnum(1, 2), value: float64(1)},
		{name: "数组元素个数下限", schema: Array(String("x"), "").WithItems(2, 3), value: []interface{}{"a"}, wantErr: true, wantMsg: "少于下限"},
		{name: "数组元素个数上限", schema: Array(String("x"), "").WithItems(1, 2), value: []interface{}{"a", "b", "c"}, wantErr: true, wantMsg: "超过上限"},
		{name: "数组元素类型错误定位到下标", schema: Array(Integer("x"), ""), value: []interface{}{float64(1), "b"}, wantErr: true, wantMsg: "[1]"},
		{name: "禁止额外字段", schema: Object(map[string]*Schema{"a": String("a")}, "a"), value: map[string]interface{}{"a": "1", "b": "2"}, wantErr: true, wantMsg: "不允许的额外字段"},
		{name: "缺必填字段", schema: Object(map[string]*Schema{"a": String("a")}, "a"), value: map[string]interface{}{}, wantErr: true, wantMsg: `缺少必填字段 "a"`},
		{name: "null 允许", schema: &Schema{Type: TypeNull}, value: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.schema.Validate(tc.value)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("期望通过，实际: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望失败，实际通过")
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("错误 %q 未包含 %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestValidateErrorIsDeterministic 同一输入多次校验必须报同一个错（额外字段遍历要稳定排序）。
func TestValidateErrorIsDeterministic(t *testing.T) {
	schema := Object(map[string]*Schema{"a": String("a")}, "a")
	value := map[string]interface{}{"a": "1", "z": 1, "b": 2, "m": 3}
	first := schema.Validate(value)
	for i := 0; i < 20; i++ {
		if got := schema.Validate(value); got == nil || got.Error() != first.Error() {
			t.Fatalf("第 %d 次校验结果不稳定: %v vs %v", i, got, first)
		}
	}
	if !strings.Contains(first.Error(), "$.b") {
		t.Fatalf("应报字典序第一个额外字段 $.b，实际 %v", first)
	}
}

func TestValidateBytesAndParse(t *testing.T) {
	schema := MustParse(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`)
	obj, err := schema.ValidateBytes([]byte(`{"n":3}`))
	if err != nil {
		t.Fatalf("期望通过: %v", err)
	}
	m, ok := obj.(map[string]interface{})
	if !ok || m["n"] != float64(3) {
		t.Fatalf("解码结果不符: %#v", obj)
	}
	if _, err := schema.ValidateBytes([]byte(`{`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, err := Parse(`{"type":"strng"}`); err == nil {
		t.Fatal("Parse 应该做自检并报错")
	}
}
