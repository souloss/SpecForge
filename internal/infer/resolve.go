package infer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// GapTask 一次兜底采集任务（档位2 单次调用 / 档位3 工具循环，设计文档 §3.3）。
//
// 输入全部是静态管线已算出的证据：LLM 只负责把静态够不到的缺口转成结构化事实，
// 产出由 engine 在代码图上逐条复核（字段/错误码必须能在切片源码里找到出处）。
type GapTask struct {
	Method string   `json:"method"` // HTTP 方法，如 POST
	Path   string   `json:"path"`   // OpenAPI 路径，如 /ipo/v1/OrderCreate
	Gaps   []string `json:"gaps"`   // 缺口描述（静态管线原文）
	// Sources 调用图切片源码（handler → service → 错误/数据来源），按相关性排序并受体积上限约束。
	Sources []SourceSnippet `json:"sources"`
	// DynamicErrorSites 值流不可反推的错误来源点「表达式@file:line」（错误缺口的精确定位）。
	DynamicErrorSites []string `json:"dynamicErrorSites,omitempty"`
	// ErrCandidates 切片内已出现的具体错误码候选（证据注入）：LLM 优先从这里选。
	ErrCandidates []ErrCandidateItem `json:"errCandidates,omitempty"`
	// DataCandidates any 响应的字段候选（证据注入）：LLM 只能从这里选字段。
	DataCandidates []DataCandidateItem `json:"dataCandidates,omitempty"`
	// ShapeGaps 形状缺口：某个响应 schema 路径上的值静态为 any，需要从源码读出其结构。
	ShapeGaps []ShapeGap `json:"shapeGaps,omitempty"`
	// ErrorCatalog 错误码目录子集：仅含常量名在本 operation 切片源码中出现过的条目（按符号排序）。
	// 与复核口径一致（不在切片中出现的码必被拒绝），既省 token 又消除不可能的答案。
	ErrorCatalog []ErrorCodeItem `json:"errorCatalog,omitempty"`
}

// SourceSnippet 一段函数源码证据。
type SourceSnippet struct {
	Symbol string `json:"symbol"` // 函数符号 ID
	File   string `json:"file"`   // 仓库相对路径
	Line   int    `json:"line"`   // 起始行号（源码首行对应的行号）
	Code   string `json:"code"`   // 源码文本
}

// ErrorCodeItem 错误码目录条目（序列化进 system prompt）。
type ErrorCodeItem struct {
	Symbol string `json:"symbol"` // 全限定常量符号（codeRef 证据）
	Code   int    `json:"code"`   // 码值
	Msg    string `json:"msg"`    // 码的说明文案
}

// ErrCandidateItem 切片内错误码候选（证据注入，供 LLM 优先选码）。
type ErrCandidateItem struct {
	Symbol string `json:"symbol"` // 全限定常量符号
	Name   string `json:"name"`   // 末段常量名
	Code   int    `json:"code"`   // 码值（-1 未解析）
}

// DataCandidateItem any 响应兜底的字段候选（证据注入，供 LLM 收窄结构）。
type DataCandidateItem struct {
	Name string `json:"name"` // 字段名（JSON 键）
}

// GapResolution LLM 兜底采集的产出（结构化，反序列化为强类型）。
type GapResolution struct {
	// ErrorCodes 补全 err 变量未解析的错误分支；codeRef 须命中错误码目录且能在切片源码中找到，
	// 否则 engine 侧丢弃（防幻觉防线）。
	ErrorCodes []ResolvedError `json:"errorCodes,omitempty"`
	// Exhaustive LLM 声明已列全该 operation 的全部错误码（无下游透传的动态码）。
	// 仅当所有列出的码都通过复核时才据此移除「未解析」行。
	Exhaustive bool `json:"exhaustive,omitempty"`
	// Shapes 形状缺口的答案：每个缺口一棵形状树（字段名须能在源码中找到，类型取封闭集合）。
	Shapes []ResolvedShape `json:"shapes,omitempty"`
}

// ShapeGap 一个形状缺口：schema 的某条路径（"" 表示响应 data 本身）静态为 any。
type ShapeGap struct {
	ID     string `json:"id"`     // 缺口 ID（答案按此回指）
	Schema string `json:"schema"` // 所在 schema 的类型 ID（空 = 成功响应 data 本身）
	Path   string `json:"path"`   // JSON 路径（a.b[].c；空 = 根）
}

// ResolvedShape 一个形状缺口的答案。
type ResolvedShape struct {
	Gap   string     `json:"gap"`   // 对应 ShapeGap.ID
	Shape *ShapeNode `json:"shape"` // 形状树
}

// ShapeNode 形状树节点（JSON Schema 的子集）。
type ShapeNode struct {
	Type       string      `json:"type"`                 // object/array/string/integer/number/boolean
	Properties []ShapeProp `json:"properties,omitempty"` // object 的字段
	Items      *ShapeNode  `json:"items,omitempty"`      // array 的元素
}

// ShapeProp object 的一个字段。
type ShapeProp struct {
	Name     string     `json:"name"`     // JSON 字段名
	Required bool       `json:"required"` // 是否必定出现
	Shape    *ShapeNode `json:"shape"`    // 字段的形状
}

// ResolvedError 一条被 LLM 解析出的错误分支。
type ResolvedError struct {
	Status  int    `json:"status,omitempty"` // HTTP 状态（信封模式下通常为 200，可缺省）
	Code    int    `json:"code,omitempty"`   // 模型报的码值（仅参考，以目录为准）
	CodeRef string `json:"codeRef"`          // 目录中的常量符号（全限定或末段名）
	Msg     string `json:"msg,omitempty"`    // 模型报的说明（仅参考，以目录为准）
}

// gapOutputSchema GapResolution 的 JSON Schema（内嵌 system prompt：实测三方网关在
// structured-output 模式下会把全 optional schema 合法地输出成空对象，内嵌则按语义作答）。
const gapOutputSchema = `{
  "type": "object",
  "properties": {
    "errorCodes": {"type": "array", "items": {"type": "object",
      "properties": {"codeRef": {"type": "string"}, "status": {"type": "integer"}},
      "required": ["codeRef"]}},
    "exhaustive": {"type": "boolean"},
    "shapes": {"type": "array", "items": {"type": "object",
      "properties": {"gap": {"type": "string"}, "shape": {"$ref": "#/$defs/node"}},
      "required": ["gap", "shape"]}}
  },
  "$defs": {"node": {"type": "object",
    "properties": {
      "type": {"enum": ["object", "array", "string", "integer", "number", "boolean"]},
      "properties": {"type": "array", "items": {"type": "object",
        "properties": {"name": {"type": "string"}, "required": {"type": "boolean"}, "shape": {"$ref": "#/$defs/node"}},
        "required": ["name", "shape"]}},
      "items": {"$ref": "#/$defs/node"}},
    "required": ["type"]}}
}`

// gapRules 档位2/3 共用的任务规则（system prompt 固定前缀）。
const gapRules = "You are an API contract extractor for a Go HTTP service. You receive one operation's " +
	"call-graph source slice and a list of gaps that static analysis could not resolve. Produce ONLY facts " +
	"that close those gaps and that are directly supported by the source.\n" +
	"Rules:\n" +
	"1. Error codes: only use symbols from the task's errorCatalog. A code is valid only if its constant is " +
	"actually returned/propagated on some path of this operation's source. Prefer errCandidates (already " +
	"observed in the slice). Look at dynamicErrorSites to see where statically-untraceable errors originate.\n" +
	"2. Set exhaustive=true only if every error this operation can return is a catalog constant you listed " +
	"(no pass-through of downstream/dynamic codes). If unsure, false.\n" +
	"3. Shapes: for every entry in shapeGaps, read the source to find what value is actually placed at that " +
	"schema path and describe it as a shape tree (nested objects/arrays allowed, max depth 4). Every field name " +
	"must appear literally in the source (string literal, struct tag or field name); dataCandidates lists keys " +
	"already found statically. Never invent a field.\n" +
	"4. If a gap cannot be closed from the evidence, leave it out. Omission is always better than guessing.\n" +
	"Respond with a single JSON object matching this schema (no prose, no comments):\n" + gapOutputSchema + "\n"

// GapSystemPrompt 档位2/3 的 system prompt：只含固定规则，全仓所有 operation 共享同一前缀（利于网关前缀缓存）。
func GapSystemPrompt() string { return gapRules }

// SortCatalog 错误码目录按符号排序（任务 JSON 确定，调用缓存可命中）。
func SortCatalog(items []ErrorCodeItem) []ErrorCodeItem {
	out := append([]ErrorCodeItem(nil), items...)
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// defaultToolTurns 档位3 工具循环的默认最大轮数。
const defaultToolTurns = 12

// ResolveGaps 对单个 operation 的缺口做一次兜底采集。tools 非空时走档位3（工具循环），
// provider 不支持工具则自动退回档位2 单次调用。离线（p == nil）返回 ErrNoProvider。
func ResolveGaps(ctx context.Context, p Provider, system string, task GapTask, tools []ToolSpec) (*GapResolution, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	req, err := GapRequest(system, task, tools)
	if err != nil {
		return nil, err
	}
	var out GapResolution
	if err := completeJSON(ctx, p, req, func(raw []byte) error {
		r, err := parseGapResolution(raw)
		if err == nil {
			out = *r
		}
		return err
	}); err != nil {
		return nil, err
	}
	return &out, nil
}

// completeJSON 发送请求并用 parse 解析输出：provider 不支持工具时退回单次调用；输出不合法时带上
// 解析错误重试一次（不再开工具），仍失败返回错误由上层降级。
func completeJSON(ctx context.Context, p Provider, req Request, parse func([]byte) error) error {
	resp, err := p.Complete(ctx, req)
	if errors.Is(err, ErrToolsUnsupported) {
		req.Tools, req.MaxTurns = nil, 0
		resp, err = p.Complete(ctx, req)
	}
	if err != nil {
		return err
	}
	perr := parse(resp.Text)
	if perr == nil {
		return nil
	}
	req.Tools, req.MaxTurns = nil, 0
	req.Prompt += fmt.Sprintf(repairPromptFmt, perr)
	if resp, err = p.Complete(ctx, req); err != nil {
		return err
	}
	return parse(resp.Text)
}

// repairPromptFmt 输出修复重试的追加提示（%v 为上次的解析错误）。
const repairPromptFmt = "\n\nYour previous reply could not be parsed (%v). Reply again with ONLY the JSON object, " +
	"double-quoted keys, no comments, no trailing commas."

// GapRequest 兜底任务 → 模型请求（ResolveGaps 发送的首个请求；上层据此预查缓存以分配预算）。
func GapRequest(system string, task GapTask, tools []ToolSpec) (Request, error) {
	ev, err := json.MarshalIndent(task, "", " ")
	if err != nil {
		return Request{}, fmt.Errorf("infer: marshal gap task: %w", err)
	}
	req := Request{System: system, Prompt: string(ev)}
	if len(tools) > 0 {
		req.Tools, req.MaxTurns = tools, defaultToolTurns
	}
	return req, nil
}

// parseGapResolution 反序列化 + 防线2 结构校验。
func parseGapResolution(raw []byte) (*GapResolution, error) {
	var out GapResolution
	if err := json.Unmarshal(extractJSON(raw), &out); err != nil {
		return nil, fmt.Errorf("infer: gap resolution output invalid: %w", err)
	}
	for _, e := range out.ErrorCodes {
		if e.CodeRef == "" {
			return nil, errors.New("infer: gap resolution contains empty codeRef")
		}
	}
	return &out, nil
}
