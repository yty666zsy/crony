// Package jsonschema 是一个"够用且严格"的 JSON Schema 子集校验器（阶段 2 引入）。
//
// 为什么自己写而不是引第三方：
//   - 全项目只有两个使用场景：**LLM 结构化输出校验** 与 **工具参数校验**；
//   - 需要的子集很小（type/required/enum/数值区间/长度/pattern/items/properties）；
//   - 引一套完整 draft 实现会带来版本差异与一大堆编译期依赖，
//     而这里的定位是"给 LLM 兜底"——宁可少支持几个关键字，也不能在校验器里藏不确定性。
//
// 设计原则（与阶段 1 的 executor 一脉相承）：
//  1. **失败要能定位**：错误里必须带 JSON 路径（`$.evidence[1].source`），
//     否则 Agent 拿到"校验失败"也不知道该改哪里；
//  2. **不做隐式 coercion**：`"3"` 不等于 `3`。工具参数是安全边界，
//     这里悄悄转换类型会让"白名单校验"失效；
//  3. **额外字段默认允许，但可显式禁止**：工具参数一律用
//     `additionalProperties: false` 声明（见 common/pkg/tools），
//     防止调用方塞入未被校验的字段。
package jsonschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Schema JSON Schema 子集。字段名与 JSON Schema 保持一致，可直接序列化给 LLM 当
// function calling 的 parameters。
type Schema struct {
	Type                 string             `json:"type,omitempty"`
	Description          string             `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties *bool              `json:"additionalProperties,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Enum                 []interface{}      `json:"enum,omitempty"`
	Minimum              *float64           `json:"minimum,omitempty"`
	Maximum              *float64           `json:"maximum,omitempty"`
	MinLength            *int               `json:"minLength,omitempty"`
	MaxLength            *int               `json:"maxLength,omitempty"`
	MinItems             *int               `json:"minItems,omitempty"`
	MaxItems             *int               `json:"maxItems,omitempty"`
	Pattern              string             `json:"pattern,omitempty"`
	Default              interface{}        `json:"default,omitempty"`
}

// ValidationError 校验失败：带 JSON 路径，便于回给模型/落库到轨迹。
type ValidationError struct {
	Path    string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Message)
}

// IsValidationError 判断错误是否为校验失败（便于调用方区分"数据不合法"与"程序错误"）。
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

// 支持的 type 取值。
const (
	TypeObject  = "object"
	TypeArray   = "array"
	TypeString  = "string"
	TypeNumber  = "number"
	TypeInteger = "integer"
	TypeBoolean = "boolean"
	TypeNull    = "null"
)

var knownTypes = map[string]bool{
	TypeObject: true, TypeArray: true, TypeString: true, TypeNumber: true,
	TypeInteger: true, TypeBoolean: true, TypeNull: true,
}

// Object 构造一个"对象 + 禁止额外字段"的 Schema（工具参数的标准写法）。
func Object(props map[string]*Schema, required ...string) *Schema {
	no := false
	return &Schema{
		Type:                 TypeObject,
		Properties:           props,
		Required:             required,
		AdditionalProperties: &no,
	}
}

// String 构造字符串字段。
func String(description string) *Schema {
	return &Schema{Type: TypeString, Description: description}
}

// Integer 构造整数字段。
func Integer(description string) *Schema {
	return &Schema{Type: TypeInteger, Description: description}
}

// Bool 构造布尔字段。
func Bool(description string) *Schema {
	return &Schema{Type: TypeBoolean, Description: description}
}

// Array 构造数组字段。
func Array(items *Schema, description string) *Schema {
	return &Schema{Type: TypeArray, Items: items, Description: description}
}

// WithEnum 附加枚举约束。
func (s *Schema) WithEnum(values ...interface{}) *Schema {
	s.Enum = values
	return s
}

// WithRange 附加数值区间（含边界）。
func (s *Schema) WithRange(min, max float64) *Schema {
	s.Minimum, s.Maximum = &min, &max
	return s
}

// WithLength 附加字符串长度区间（按 Unicode 字符数，不按字节）。
func (s *Schema) WithLength(min, max int) *Schema {
	s.MinLength, s.MaxLength = &min, &max
	return s
}

// WithItems 附加数组长度区间。
func (s *Schema) WithItems(min, max int) *Schema {
	s.MinItems, s.MaxItems = &min, &max
	return s
}

// WithPattern 附加正则约束（校验时编译，编译失败视为 Schema 非法）。
func (s *Schema) WithPattern(pattern string) *Schema {
	s.Pattern = pattern
	return s
}

// Valid 检查 Schema 自身是否合法（类型名、正则、必填字段是否都在 properties 里）。
//
// 为什么要自检：工具参数 Schema 是**安全边界**。一个写错的 Schema
// （比如 required 里写了不存在的字段、type 拼成 "strng"）会让校验静默放行，
// 不如启动时就报出来。
func (s *Schema) Valid() error {
	return s.check("$")
}

func (s *Schema) check(path string) error {
	if s == nil {
		return &ValidationError{Path: path, Message: "Schema 为 nil"}
	}
	if s.Type != "" && !knownTypes[s.Type] {
		return &ValidationError{Path: path, Message: fmt.Sprintf("未知的 type %q", s.Type)}
	}
	if s.Pattern != "" {
		if _, err := regexp.Compile(s.Pattern); err != nil {
			return &ValidationError{Path: path, Message: fmt.Sprintf("非法正则 %q: %v", s.Pattern, err)}
		}
	}
	if s.Type == TypeObject || len(s.Properties) > 0 || len(s.Required) > 0 {
		for _, key := range s.Required {
			if _, ok := s.Properties[key]; !ok {
				return &ValidationError{Path: path, Message: fmt.Sprintf("required 字段 %q 未在 properties 中定义", key)}
			}
		}
	}
	if s.Type == TypeArray && s.Items == nil {
		return &ValidationError{Path: path, Message: "array 必须声明 items"}
	}
	for _, key := range sortedKeys(s.Properties) {
		if err := s.Properties[key].check(path + "." + key); err != nil {
			return err
		}
	}
	if s.Items != nil {
		if err := s.Items.check(path + "[]"); err != nil {
			return err
		}
	}
	return nil
}

// Validate 校验一个已 JSON 解码的值（map[string]interface{} / []interface{} / string / float64 / bool / nil）。
func (s *Schema) Validate(value interface{}) error {
	return s.validate("$", value)
}

// ValidateBytes 先 JSON 解码再校验，并返回解码后的对象。
// 注意：这里用 json.Decoder + UseNumber 之外的默认行为，数字统一为 float64，
// 便于与 LLM 的 JSON 输出（同样是 float64）保持一致；需要大整数时用 string 承载。
func (s *Schema) ValidateBytes(raw []byte) (interface{}, error) {
	var v interface{}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&v); err != nil {
		return nil, &ValidationError{Path: "$", Message: fmt.Sprintf("不是合法 JSON: %v", err)}
	}
	if err := s.Validate(v); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Schema) validate(path string, value interface{}) error {
	if s == nil {
		return nil
	}
	if s.Type != "" && !typeMatches(s.Type, value) {
		return &ValidationError{Path: path, Message: fmt.Sprintf("期望类型 %s，实际 %s", s.Type, typeOf(value))}
	}
	if len(s.Enum) > 0 && !enumContains(s.Enum, value) {
		return &ValidationError{Path: path, Message: fmt.Sprintf("取值 %s 不在允许枚举 %s 内", compact(value), compact(s.Enum))}
	}

	switch v := value.(type) {
	case string:
		n := len([]rune(v))
		if s.MinLength != nil && n < *s.MinLength {
			return &ValidationError{Path: path, Message: fmt.Sprintf("长度 %d 小于下限 %d", n, *s.MinLength)}
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			return &ValidationError{Path: path, Message: fmt.Sprintf("长度 %d 超过上限 %d", n, *s.MaxLength)}
		}
		if s.Pattern != "" {
			re, err := regexp.Compile(s.Pattern)
			if err != nil {
				return &ValidationError{Path: path, Message: fmt.Sprintf("Schema 正则非法: %v", err)}
			}
			if !re.MatchString(v) {
				return &ValidationError{Path: path, Message: fmt.Sprintf("取值 %q 不匹配 %q", v, s.Pattern)}
			}
		}
	case []interface{}:
		if s.MinItems != nil && len(v) < *s.MinItems {
			return &ValidationError{Path: path, Message: fmt.Sprintf("元素个数 %d 少于下限 %d", len(v), *s.MinItems)}
		}
		if s.MaxItems != nil && len(v) > *s.MaxItems {
			return &ValidationError{Path: path, Message: fmt.Sprintf("元素个数 %d 超过上限 %d", len(v), *s.MaxItems)}
		}
		for i, item := range v {
			if err := s.Items.validate(fmt.Sprintf("%s[%d]", path, i), item); err != nil {
				return err
			}
		}
	case map[string]interface{}:
		for _, key := range s.Required {
			if _, ok := v[key]; !ok {
				return &ValidationError{Path: path, Message: fmt.Sprintf("缺少必填字段 %q", key)}
			}
		}
		if s.AdditionalProperties != nil && !*s.AdditionalProperties {
			for _, key := range sortedMapKeys(v) {
				if _, ok := s.Properties[key]; !ok {
					return &ValidationError{Path: path + "." + key, Message: "不允许的额外字段"}
				}
			}
		}
		for _, key := range sortedMapKeys(v) {
			if sub, ok := s.Properties[key]; ok {
				if err := sub.validate(path+"."+key, v[key]); err != nil {
					return err
				}
			}
		}
		// 数值区间对 number/integer 的校验放在类型判定之后，但 Go 里数字落到 float64 分支之外，
		// 因此这里单独处理（见下方 numeric 检查）
	}

	if f, ok := numeric(value); ok {
		if s.Minimum != nil && f < *s.Minimum {
			return &ValidationError{Path: path, Message: fmt.Sprintf("取值 %v 小于下限 %v", f, *s.Minimum)}
		}
		if s.Maximum != nil && f > *s.Maximum {
			return &ValidationError{Path: path, Message: fmt.Sprintf("取值 %v 超过上限 %v", f, *s.Maximum)}
		}
	}
	return nil
}

// typeMatches 类型判定。integer 要求"数值且无小数部分"。
func typeMatches(want string, value interface{}) bool {
	switch want {
	case TypeNull:
		return value == nil
	case TypeBoolean:
		_, ok := value.(bool)
		return ok
	case TypeString:
		_, ok := value.(string)
		return ok
	case TypeNumber:
		_, ok := numeric(value)
		return ok
	case TypeInteger:
		f, ok := numeric(value)
		return ok && f == float64(int64(f))
	case TypeArray:
		_, ok := value.([]interface{})
		return ok
	case TypeObject:
		_, ok := value.(map[string]interface{})
		return ok
	default:
		// 未知类型：Valid() 已经拦过，这里保守放行
		return true
	}
}

// numeric 取出数值（兼容 Go 侧传入的 int/int64/float32 等）。
func numeric(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint8:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	default:
		return 0, false
	}
}

func typeOf(value interface{}) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	}
	if f, ok := numeric(value); ok {
		if f == float64(int64(f)) {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", value)
}

// enumContains 枚举比较：数字按数值比较（1 与 1.0 视为相同），字符串严格比较。
func enumContains(enum []interface{}, value interface{}) bool {
	for _, item := range enum {
		if item == nil && value == nil {
			return true
		}
		if a, ok := numeric(item); ok {
			if b, ok2 := numeric(value); ok2 && a == b {
				return true
			}
			continue
		}
		if a, ok := item.(string); ok {
			if b, ok2 := value.(string); ok2 && a == b {
				return true
			}
			continue
		}
		if a, ok := item.(bool); ok {
			if b, ok2 := value.(bool); ok2 && a == b {
				return true
			}
		}
	}
	return false
}

func compact(v interface{}) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(raw)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// sortedKeys 稳定遍历顺序：错误信息可复现（否则同一个输入可能报不同的错）。
func sortedKeys(props map[string]*Schema) []string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Parse 从 JSON 文本解析 Schema 并自检。
func Parse(raw string) (*Schema, error) {
	var s Schema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("jsonschema: 解析失败: %w", err)
	}
	if err := s.Valid(); err != nil {
		return nil, fmt.Errorf("jsonschema: Schema 非法: %w", err)
	}
	return &s, nil
}

// MustParse 用于内置 Schema 常量（写错了要在启动/测试时炸掉，而不是线上放行）。
func MustParse(raw string) *Schema {
	s, err := Parse(raw)
	if err != nil {
		panic(err)
	}
	return s
}
