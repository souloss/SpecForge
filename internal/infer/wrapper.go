package infer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// WrapperTask L2 包装器摘要任务：一个「把形参写进响应」但静态摘要失败的函数。
type WrapperTask struct {
	Symbol  string          `json:"symbol"`  // 函数符号 ID
	Params  []WrapperParam  `json:"params"`  // 形参（下标、名字、类型）
	Sources []SourceSnippet `json:"sources"` // 函数本体 + 其直接调用的仓库内辅助函数源码
}

// WrapperParam 包装器的一个形参。
type WrapperParam struct {
	Index int    `json:"index"` // 下标（0 起，不含接收者）
	Name  string `json:"name"`  // 形参名
	Type  string `json:"type"`  // 类型串
}

// WrapperSummary L2 包装器摘要：调用点第几个实参是业务数据 / 错误，响应信封有哪些键、各分支固定码。
type WrapperSummary struct {
	DataParam   int            `json:"dataParam"`             // 业务数据形参下标；-1 = 无
	ErrParam    int            `json:"errParam"`              // 错误形参下标；-1 = 无
	Fields      []WrapperField `json:"fields"`                // 写出的 JSON 信封键
	SuccessCode *int           `json:"successCode,omitempty"` // 成功分支信封中的固定业务码
	FailureCode *int           `json:"failureCode,omitempty"` // 失败分支信封中的固定业务码
}

// WrapperField 信封的一个键。
type WrapperField struct {
	Key    string `json:"key"`    // JSON 键
	Source string `json:"source"` // data（业务数据形参）/ err（由错误形参派生）/ other
	Type   string `json:"type"`   // string/integer/number/boolean/object/array（data 键可填 object）
}

// wrapperSystemPrompt L2 包装器摘要的 system prompt（固定前缀）。
const wrapperSystemPrompt = "You analyze one helper function of a Go HTTP service that writes a JSON response. " +
	"Static analysis could not determine how its parameters end up in the response body. Using the function " +
	"source and the helpers it calls, report: which parameter index carries the business data (dataParam, -1 if " +
	"none), which carries the error (errParam, -1 if none), every top-level JSON key of the written body with its " +
	"source (data / err / other) and type, and integer business codes that are literal constants on the success " +
	"and failure branches. Only report keys that literally appear in the source. Respond with a single JSON " +
	`object: {"dataParam":int,"errParam":int,"fields":[{"key":string,"source":string,"type":string}],` +
	`"successCode":int?,"failureCode":int?}`

// SummarizeWrapper 对一个包装器做一次 LLM 摘要（结构校验在此，语义复核在前端）。离线返回 ErrNoProvider。
func SummarizeWrapper(ctx context.Context, p Provider, task WrapperTask) (*WrapperSummary, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	ev, err := json.MarshalIndent(task, "", " ")
	if err != nil {
		return nil, fmt.Errorf("infer: marshal wrapper task: %w", err)
	}
	var out WrapperSummary
	err = completeJSON(ctx, p, Request{System: wrapperSystemPrompt, Prompt: string(ev)}, func(raw []byte) error {
		out = WrapperSummary{DataParam: -1, ErrParam: -1}
		if err := json.Unmarshal(extractJSON(raw), &out); err != nil {
			return fmt.Errorf("infer: wrapper summary output invalid: %w", err)
		}
		if len(out.Fields) == 0 {
			return errors.New("infer: wrapper summary has no fields")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
