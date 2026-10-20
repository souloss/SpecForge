// Package verify 语言无关的 LLM 产出核对：只依赖源码文本（单词表 + 字面量），任何语言前端都能用；
// 有类型信息的前端可在此之上做更强的核对。
package verify

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/schema"
)

// MaxShapeDepth 形状树的最大嵌套深度（超出整棵拒绝）。
const MaxShapeDepth = 4

// ErrRejected 产出未通过核对（类型集外、深度超限、名字在源码中找不到）。
var ErrRejected = errors.New("rejected by verification")

// identPattern 源码中的标识符 / 字面量单词。
var identPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// Tokens 源码文本中出现的全部单词（标识符、字符串与注解/tag 中的单词）。
func Tokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range identPattern.FindAllString(text, -1) {
		out[w] = true
	}
	return out
}

// NameInSource 名字是否出现在源码中：作为完整单词，或作为带引号的字面量（含 - 等非标识符字符的键）。
func NameInSource(name, text string, tokens map[string]bool) bool {
	if name == "" {
		return false
	}
	return tokens[name] || strings.Contains(text, `"`+name+`"`) || strings.Contains(text, `'`+name+`'`)
}

// scalarTypes 形状树允许的标量类型。
var scalarTypes = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true}

// validInParams 参数位置封闭集合（query/path/header/cookie）。
var validInParams = map[string]bool{"query": true, "path": true, "header": true, "cookie": true}

// ValidParamIn 参数位置是否为封闭集合成员（query/path/header/cookie）。
// 与 generic 前端、engine critic 共用，避免多处各自维护一份参数位置白名单。
func ValidParamIn(in string) bool { return validInParams[in] }

// ScalarType 参数/字段基础类型取封闭集合，其余退回 string（与 generic 前端 scalarTypes 一致）。
func ScalarType(t string) string {
	if scalarTypes[t] {
		return t
	}
	return "string"
}

// validFormat 参数与形状节点允许的 format（封闭集合，防模型编造格式名）。
var validFormat = map[string]bool{"date-time": true, "date": true, "int64": true, "int32": true,
	"double": true, "float": true, "byte": true, "binary": true, "uuid": true}

// ValidFormat 判断 format 是否在封闭集合内。
func ValidFormat(f string) bool { return validFormat[f] }

// MinHTTPStatus / MaxHTTPStatus 合法 HTTP 状态码范围。
const (
	MinHTTPStatus = 100
	MaxHTTPStatus = 599
)

// ValidStatus HTTP 状态码是否在合法范围。
func ValidStatus(s int) bool { return s >= MinHTTPStatus && s <= MaxHTTPStatus }

// Shape 形状树 → schema，逐节点核对：类型取封闭集合、深度受限、每个字段名在源码中出现；
// 约束（enum/pattern/format/数值与长度边界/nullable）逐项核对「字面量须出现在源码中」。
// 任一节点不合规整棵拒绝（返回 ErrRejected）。names 为树中全部字段名（供上层定位证据行）。
//
// alias 是「模型报的字段名 → 源码里的真实序列化名」映射（如 Go 的 json tag：Data → data），
// 用于在模型用语言内名字（PascalCase）而序列化名不同（camelCase）时仍能核对；无映射时行为与旧版一致。
func Shape(n *infer.ShapeNode, text string, tokens map[string]bool, alias map[string]string) (*schema.Schema, []string, error) {
	return shape(n, text, tokens, alias, 0)
}

// shape Shape 的递归实现。
func shape(n *infer.ShapeNode, text string, tokens map[string]bool, alias map[string]string, depth int) (*schema.Schema, []string, error) {
	if n == nil {
		return nil, nil, fmt.Errorf("%w: missing shape node", ErrRejected)
	}
	if depth > MaxShapeDepth {
		return nil, nil, fmt.Errorf("%w: depth exceeds %d", ErrRejected, MaxShapeDepth)
	}
	switch {
	case scalarTypes[n.Type]:
		sc := &schema.Schema{Type: n.Type, Format: n.Format, Pattern: n.Pattern,
			Nullable: n.Nullable, Min: n.Min, Max: n.Max, MinLen: n.MinLen, MaxLen: n.MaxLen}
		if err := verifyConstraints(n, text, tokens); err != nil {
			return nil, nil, err
		}
		if len(n.Enum) > 0 {
			sc.Enum = n.Enum
		}
		sc = dropUnverifiedConstraints(sc, n, text, tokens)
		return sc, nil, nil
	case n.Type == "array":
		items, names, err := shape(n.Items, text, tokens, alias, depth+1)
		if err != nil {
			return nil, nil, err
		}
		sc := &schema.Schema{Type: "array", Items: items, Nullable: n.Nullable,
			MinItems: n.MinItems, MaxItems: n.MaxItems}
		if err := verifyConstraints(n, text, tokens); err != nil {
			return nil, nil, err
		}
		sc = dropUnverifiedConstraints(sc, n, text, tokens)
		return sc, names, nil
	case n.Type == "object":
		sc := &schema.Schema{Type: "object", Nullable: n.Nullable}
		var names []string
		for _, p := range n.Properties {
			// 模型报 Go 字段名（PascalCase）而 json tag 是 camelCase 时，用 alias 映射回源码名核对，
			// 产物字段名统一用源码里的序列化名（json tag），避免 PascalCase 幻觉。
			srcName := p.Name
			if v, ok := alias[p.Name]; ok {
				srcName = v
			}
			if !NameInSource(srcName, text, tokens) {
				return nil, nil, fmt.Errorf("%w: field %q not in source", ErrRejected, p.Name)
			}
			child, cn, err := shape(p.Shape, text, tokens, alias, depth+1)
			if err != nil {
				return nil, nil, err
			}
			sc.Props = append(sc.Props, schema.Prop{Name: srcName, Schema: child, Required: p.Required})
			if p.Required {
				sc.Required = append(sc.Required, srcName)
			}
			names = append(append(names, srcName), cn...)
		}
		sort.Strings(sc.Required)
		return sc, names, nil
	}
	return nil, nil, fmt.Errorf("%w: type %q outside closed set", ErrRejected, n.Type)
}

// verifyConstraints 核对形状节点的约束值是否都能在源码里找到字面量出处：
// enum 每个取值、pattern 正则、format、以及数值/长度边界的字面量都须原样出现，否则整棵拒绝。
// 防幻觉：模型不能编一个源码里没有的正则、格式名或边界值。
//
// 注意：Pydantic 等动态语言把约束写在模型类（min_length/max_length/pattern），而核对文本
// 只含 handler 切片，找不到这些字面量——为不整棵拒绝掉正确的字段形状，此处仅对 enum/format
// 保持硬核对，pattern 与数值/长度边界在找不到字面量时「丢弃该约束」而非拒绝（见
// dropUnverifiedConstraints）。硬核对的是离散取值（防编造 enum）与格式名（防编造 format）。
func verifyConstraints(n *infer.ShapeNode, text string, tokens map[string]bool) error {
	for _, v := range n.Enum {
		if !NameInSource(v, text, tokens) {
			return fmt.Errorf("%w: enum value %q not in source", ErrRejected, v)
		}
	}
	if n.Format != "" && !ValidFormat(n.Format) {
		return fmt.Errorf("%w: format %q outside closed set", ErrRejected, n.Format)
	}
	return nil
}

// dropUnverifiedConstraints 清除「源码中找不到字面量出处」的约束（pattern 与数值/长度边界）：
// 这类约束常写在 handler 切片之外的模型类里，丢弃不误拒；enum/format 已由 verifyConstraints 硬核对。
func dropUnverifiedConstraints(sc *schema.Schema, n *infer.ShapeNode, text string, tokens map[string]bool) *schema.Schema {
	if sc == nil {
		return sc
	}
	nums := numberLiterals(text)
	if n.Pattern != "" && !NameInSource(n.Pattern, text, tokens) {
		sc.Pattern = ""
	}
	for _, v := range []*float64{n.Min, n.Max} {
		if v != nil && !nums[strconv.FormatFloat(*v, 'f', -1, 64)] && !nums[strconv.FormatInt(int64(*v), 10)] {
			if sc.Min == v {
				sc.Min = nil
			}
			if sc.Max == v {
				sc.Max = nil
			}
		}
	}
	for _, v := range []*int{n.MinLen, n.MaxLen, n.MinItems, n.MaxItems} {
		if v != nil && !nums[strconv.Itoa(*v)] {
			if sc.MinLen == v {
				sc.MinLen = nil
			}
			if sc.MaxLen == v {
				sc.MaxLen = nil
			}
			if sc.MinItems == v {
				sc.MinItems = nil
			}
			if sc.MaxItems == v {
				sc.MaxItems = nil
			}
		}
	}
	return sc
}

// numberLiteral 源码中的数值字面量（整数与小数）。
var numberLiteral = regexp.MustCompile(`\b[0-9]+(?:\.[0-9]+)?\b`)

// numberLiterals 收集文本中出现的全部数值字面量（去重）。
func numberLiterals(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range numberLiteral.FindAllString(text, -1) {
		out[m] = true
	}
	return out
}
