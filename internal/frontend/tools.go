package frontend

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/infer"
)

// maxToolSourceLines read_source 工具单次最多返回的行数。
const maxToolSourceLines = 200

// maxToolFindResults find_symbol 工具最多返回的匹配数。
const maxToolFindResults = 20

// maxToolSourceBytes read_symbol 返回的源码上限（超长截断）。
const maxToolSourceBytes = 6000

// toolTruncatedMark 源码截断标记（提示模型可用 read_source 取剩余部分）。
const toolTruncatedMark = "\n// …(truncated; use read_source for the rest)"

// idInputSchema 只有一个 id 字符串入参的工具 schema。
var idInputSchema = map[string]any{
	"type":       "object",
	"properties": map[string]any{"id": map[string]any{"type": "string", "description": "fully-qualified symbol ID"}},
	"required":   []any{"id"},
}

// lineArgSchema 行号入参：模型常把数字写成字符串，schema 校验失败会中断整轮生成，故两者都收（intArg 统一转换）。
var lineArgSchema = map[string]any{"type": []any{"integer", "string"}, "description": "1-based line number"}

// Tools LLM 子 Agent 工具集：对程序视图的只读、确定性查询（缓存层据此重放校验读集）。任何前端的 Program 都可用。
func Tools(prog Program) []infer.ToolSpec {
	return []infer.ToolSpec{
		{
			Name:        "read_symbol",
			Description: "Read a function/method/type symbol: location, doc and source (truncated).",
			InputSchema: idInputSchema,
			Call: func(_ context.Context, in map[string]any) (string, error) {
				id, _ := in["id"].(string)
				sym, ok := prog.Symbol(id)
				if !ok {
					return `{"error":"symbol not found; use find_symbol"}`, nil
				}
				m := map[string]any{"id": sym.ID, "kind": sym.Kind, "file": prog.RelPath(sym.File), "line": sym.Line, "doc": sym.Doc}
				if _, _, _, text, ok := prog.FuncSource(id); ok {
					if len(text) > maxToolSourceBytes {
						text = text[:maxToolSourceBytes] + toolTruncatedMark
					}
					m["source"] = text
				}
				return marshalTool(m), nil
			},
		},
		{
			Name:        "callees",
			Description: "List a function's direct callees (symbol IDs, interface calls expanded to implementations).",
			InputSchema: idInputSchema,
			Call: func(_ context.Context, in map[string]any) (string, error) {
				id, _ := in["id"].(string)
				return marshalTool(map[string]any{"callees": prog.Callees(id)}), nil
			},
		},
		{
			Name:        "read_type",
			Description: "Read a named type's fields (source name, JSON name, source type, required).",
			InputSchema: idInputSchema,
			Call: func(_ context.Context, in map[string]any) (string, error) {
				id, _ := in["id"].(string)
				ti, ok := prog.TypeOf(id)
				if !ok {
					return `{"error":"type not found; use find_symbol"}`, nil
				}
				fields := make([]map[string]any, 0, len(ti.Fields))
				for _, f := range ti.Fields {
					fields = append(fields, map[string]any{"name": f.Name, "json": f.JSON, "type": f.Type, "required": f.Required})
				}
				return marshalTool(map[string]any{"id": ti.ID, "isStruct": ti.IsStruct, "fields": fields}), nil
			},
		},
		{
			Name:        "read_source",
			Description: "Read source lines of a repository source file (repo-relative path, 1-based inclusive range, max 200 lines).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file": map[string]any{"type": "string"},
					"from": lineArgSchema,
					"to":   lineArgSchema,
				},
				"required": []any{"file", "from", "to"},
			},
			Call: func(_ context.Context, in map[string]any) (string, error) {
				rel, _ := in["file"].(string)
				from, to := intArg(in["from"]), intArg(in["to"])
				if !prog.IsSourceFile(rel) {
					return `{"error":"only repository source files are readable"}`, nil
				}
				if to-from+1 > maxToolSourceLines {
					to = from + maxToolSourceLines - 1
				}
				return marshalTool(map[string]any{"file": rel, "from": from, "to": to, "source": prog.SourceRange(rel, from, to)}), nil
			},
		},
		{
			Name:        "find_symbol",
			Description: "Find symbol IDs whose ID contains the query (case-insensitive), max 20 results.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": map[string]any{"type": "string"}},
				"required":   []any{"query"},
			},
			Call: func(_ context.Context, in map[string]any) (string, error) {
				q, _ := in["query"].(string)
				ids := prog.FindSymbols(q)
				if len(ids) > maxToolFindResults {
					ids = ids[:maxToolFindResults]
				}
				if ids == nil {
					ids = []string{}
				}
				return marshalTool(map[string]any{"matches": ids}), nil
			},
		},
	}
}

// marshalTool 工具结果序列化（map 键有序，输出确定）。
func marshalTool(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"marshal failed"}`
	}
	return string(b)
}

// intArg JSON 数字入参 → int（模型给的整数经 JSON 解码为 float64）。
func intArg(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}
