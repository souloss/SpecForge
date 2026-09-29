// Package verify 语言无关的 LLM 产出核对：只依赖源码文本（单词表 + 字面量），任何语言前端都能用；
// 有类型信息的前端可在此之上做更强的核对。
package verify

import (
	"errors"
	"regexp"
	"sort"
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

// Shape 形状树 → schema，逐节点核对：类型取封闭集合、深度受限、每个字段名在源码中出现；
// 任一节点不合规整棵拒绝（返回 ErrRejected）。names 为树中全部字段名（供上层定位证据行）。
func Shape(n *infer.ShapeNode, text string, tokens map[string]bool) (*schema.Schema, []string, error) {
	return shape(n, text, tokens, 0)
}

// shape Shape 的递归实现。
func shape(n *infer.ShapeNode, text string, tokens map[string]bool, depth int) (*schema.Schema, []string, error) {
	if n == nil || depth > MaxShapeDepth {
		return nil, nil, ErrRejected
	}
	switch {
	case scalarTypes[n.Type]:
		return &schema.Schema{Type: n.Type}, nil, nil
	case n.Type == "array":
		items, names, err := shape(n.Items, text, tokens, depth+1)
		if err != nil {
			return nil, nil, err
		}
		return &schema.Schema{Type: "array", Items: items}, names, nil
	case n.Type == "object":
		sc := &schema.Schema{Type: "object"}
		var names []string
		for _, p := range n.Properties {
			if !NameInSource(p.Name, text, tokens) {
				return nil, nil, ErrRejected
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
	return nil, nil, ErrRejected
}
