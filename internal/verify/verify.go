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
func Shape(n *infer.ShapeNode, text string, tokens map[string]bool) (*schema.Schema, []string, error) {
	return shape(n, text, tokens, 0)
}

// shape Shape 的递归实现。
func shape(n *infer.ShapeNode, text string, tokens map[string]bool, depth int) (*schema.Schema, []string, error) {
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
		return sc, nil, nil
	case n.Type == "array":
		items, names, err := shape(n.Items, text, tokens, depth+1)
		if err != nil {
			return nil, nil, err
		}
		sc := &schema.Schema{Type: "array", Items: items, Nullable: n.Nullable,
			MinItems: n.MinItems, MaxItems: n.MaxItems}
		if err := verifyConstraints(n, text, tokens); err != nil {
			return nil, nil, err
		}
		return sc, names, nil
	case n.Type == "object":
		sc := &schema.Schema{Type: "object", Nullable: n.Nullable}
		var names []string
		for _, p := range n.Properties {
			if !NameInSource(p.Name, text, tokens) {
				return nil, nil, fmt.Errorf("%w: field %q not in source", ErrRejected, p.Name)
			}
			child, cn, err := shape(p.Shape, text, tokens, depth+1)
			if err != nil {
				return nil, nil, err
			}
			sc.Props = append(sc.Props, schema.Prop{Name: p.Name, Schema: child, Required: p.Required})
			if p.Required {
				sc.Required = append(sc.Required, p.Name)
			}
			names = append(append(names, p.Name), cn...)
		}
		sort.Strings(sc.Required)
		return sc, names, nil
	}
	return nil, nil, fmt.Errorf("%w: type %q outside closed set", ErrRejected, n.Type)
}

// verifyConstraints 核对形状节点的约束值是否都能在源码里找到字面量出处：
// enum 每个取值、pattern 正则、format、以及数值/长度边界的字面量都须原样出现，否则整棵拒绝。
// 防幻觉：模型不能编一个源码里没有的正则、格式名或边界值。
func verifyConstraints(n *infer.ShapeNode, text string, tokens map[string]bool) error {
	for _, v := range n.Enum {
		if !NameInSource(v, text, tokens) {
			return fmt.Errorf("%w: enum value %q not in source", ErrRejected, v)
		}
	}
	if n.Pattern != "" && !NameInSource(n.Pattern, text, tokens) {
		return fmt.Errorf("%w: pattern %q not in source", ErrRejected, n.Pattern)
	}
	if n.Format != "" && !ValidFormat(n.Format) {
		return fmt.Errorf("%w: format %q outside closed set", ErrRejected, n.Format)
	}
	nums := numberLiterals(text)
	for _, v := range []*float64{n.Min, n.Max} {
		if v != nil && !nums[strconv.FormatFloat(*v, 'f', -1, 64)] && !nums[strconv.FormatInt(int64(*v), 10)] {
			return fmt.Errorf("%w: numeric bound %v not in source", ErrRejected, *v)
		}
	}
	for _, v := range []*int{n.MinLen, n.MaxLen, n.MinItems, n.MaxItems} {
		if v != nil && !nums[strconv.Itoa(*v)] {
			return fmt.Errorf("%w: length bound %d not in source", ErrRejected, *v)
		}
	}
	return nil
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
