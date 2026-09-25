package infer

import (
	"encoding/json"
	"errors"
	"fmt"
)

// GapTask 一次兜底采集任务（档位2 模板化单次调用，设计文档 §3.3）。
//
// 输入全部是「静态管线已算出的、可序列化的证据」，LLM 只负责把
// 静态够不到的缺口转成结构化事实，不接触代码图、不接触编译器。
type GapTask struct {
	Method       string          `json:"method"`       // 如 POST
	Path         string          `json:"path"`         // 如 /ipo/v1/OrderCreate
	Gaps         []string        `json:"gaps"`         // 缺口描述
	Evidence     string          `json:"evidence"`     // 已切片的代码证据（handler + service 关键分支）
	ErrorCatalog []ErrorCodeItem `json:"errorCatalog"` // 错误码目录（LLM 只能从这里选码）
}

// ErrorCodeItem 错误码目录条目（序列化给 LLM）。
type ErrorCodeItem struct {
	Symbol string `json:"symbol"` // 全限定常量符号（codeRef 证据）
	Code   int    `json:"code"`
	Msg    string `json:"msg"`
}

// GapResolution LLM 兜底采集的产出（结构化，反序列化为强类型）。
type GapResolution struct {
	// ErrorCodes 补全 err 变量未解析的错误分支。每个 codeRef 必须命中
	// 输入的 ErrorCatalog，否则由 engine 侧校验后丢弃（防幻觉防线）。
	ErrorCodes []ResolvedError `json:"errorCodes,omitempty"`
	// ResponseSchema 补全 any 响应的实际结构（字段名 + 类型 + 必填）。
	ResponseSchema *ResolvedSchema `json:"responseSchema,omitempty"`
}

// ResolvedError 一条被 LLM 解析出的错误分支。
type ResolvedError struct {
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	CodeRef string `json:"codeRef"`
	Msg     string `json:"msg"`
}

// ResolvedSchema any 响应的简化结构（扁平字段列表，不含嵌套）。
type ResolvedSchema struct {
	Properties []ResolvedProp `json:"properties"`
}

// ResolvedProp 单个响应字段。
type ResolvedProp struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // string/integer/number/boolean/object/array
	Required bool   `json:"required"`
}

// gapOutputSchema GapResolution 的 JSON Schema（随 system prompt 下发，
// 网关可能不做约束解码，故 engine 侧仍做事后结构校验）。
const gapOutputSchema = `{
  "type": "object",
  "properties": {
    "errorCodes": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "status": {"type": "integer"},
          "code": {"type": "integer"},
          "codeRef": {"type": "string"},
          "msg": {"type": "string"}
        },
        "required": ["status", "code", "codeRef"]
      }
    },
    "responseSchema": {
      "type": "object",
      "properties": {
        "properties": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "name": {"type": "string"},
              "type": {"type": "string"},
              "required": {"type": "boolean"}
            },
            "required": ["name", "type"]
          }
        }
      }
    }
  }
}`

// ResolveGaps 对单个 operation 的缺口做一次模板化兜底采集。
// 离线（p == nil）返回 ErrNoProvider，由 engine 显式降级。
func ResolveGaps(p Provider, task GapTask) (*GapResolution, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	system := gapSystemPrompt
	ev, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("infer: marshal gap task: %w", err)
	}
	raw, err := p.Complete(system, string(ev), []byte(gapOutputSchema))
	if err != nil {
		return nil, err
	}
	return parseGapResolution(raw)
}

// ResolveGapsWithTools 档位3：带工具的兜底采集（工具循环由 provider 侧
// WithMaxTurns 驱动）。离线或 provider 不支持工具时返回 ErrNoProvider /
// ErrToolsUnsupported，由 engine 降级到档位2。
func ResolveGapsWithTools(p Provider, task GapTask, tools []ToolSpec, maxTurns int) (*GapResolution, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	tu, ok := p.(ToolUser)
	if !ok {
		return nil, ErrToolsUnsupported
	}
	ev, err := json.Marshal(task)
	if err != nil {
		return nil, fmt.Errorf("infer: marshal gap task: %w", err)
	}
	raw, err := tu.CompleteWithTools(gapSystemPrompt, string(ev), []byte(gapOutputSchema), tools, maxTurns)
	if err != nil {
		return nil, err
	}
	return parseGapResolution(raw)
}

// ErrToolsUnsupported provider 不支持工具调用（档位3 不可用）。
var ErrToolsUnsupported = errors.New("infer: provider does not support tools")

// gapSystemPrompt 档位2/3 共用的 system prompt。
const gapSystemPrompt = "You are an API contract extractor. Given code evidence and a list of " +
	"unresolved gaps, produce ONLY the concrete facts that resolve them. " +
	"For error codes, you may only use codes whose symbol appears in the " +
	"provided errorCatalog — never invent a code. For response schemas, " +
	"only report fields you can see in the evidence (use tools to read " +
	"source when the evidence is insufficient). Respond as JSON matching " +
	"the output schema:\n" + gapOutputSchema

// parseGapResolution 反序列化 + 防线2 结构校验。
func parseGapResolution(raw []byte) (*GapResolution, error) {
	var out GapResolution
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("infer: gap resolution output invalid: %w", err)
	}
	for _, e := range out.ErrorCodes {
		if e.CodeRef == "" {
			return nil, errors.New("infer: gap resolution contains empty codeRef")
		}
	}
	return &out, nil
}
