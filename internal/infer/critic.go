package infer

import (
	"context"
	"encoding/json"
	"fmt"
)

// CriticTask 审查任务：一个 operation 的当前契约草稿 + handler 源码切片。
type CriticTask struct {
	Operation string          `json:"operation"` // "METHOD /path"
	Method    string          `json:"method"`
	Path      string          `json:"path"`
	Contract  CriticContract  `json:"contract"` // 当前已抽取的契约（供审查者对照）
	Sources   []SourceSnippet `json:"sources"`  // handler 与可达函数源码切片
}

// CriticContract 审查输入里的契约摘要（只给审查者「现状」，不含证据细节）。
type CriticContract struct {
	Params      []CriticParam `json:"params"`      // 已抽取的参数
	RequestBody bool          `json:"requestBody"` // 是否已声明请求体
	Responses   []CriticResp  `json:"responses"`   // 已抽取的响应
}

// CriticParam 契约草稿里的一个参数。
type CriticParam struct {
	In       string `json:"in"`
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Type     string `json:"type"`
}

// CriticResp 契约草稿里的一个响应。
type CriticResp struct {
	Status int  `json:"status"`
	Error  bool `json:"error"`
}

// CriticFinding 审查者报告的一条发现（kind 见 critic.system.md 的封闭集合）。
type CriticFinding struct {
	Kind     string     `json:"kind"`               // missing_param|missing_response|wrong_status|wrong_envelope
	In       string     `json:"in,omitempty"`       // missing_param: query/path/header/cookie
	Name     string     `json:"name,omitempty"`     // missing_param: 参数名
	Type     string     `json:"type,omitempty"`     // missing_param: string/integer/number/boolean
	Required bool       `json:"required,omitempty"` // missing_param: 是否必填
	Status   int        `json:"status,omitempty"`   // missing_response/wrong_status: HTTP 状态码
	Error    bool       `json:"error,omitempty"`    // missing_response: 是否错误分支
	Shape    *ShapeNode `json:"shape,omitempty"`    // missing_response: 响应体形状
	Detail   string     `json:"detail,omitempty"`   // 补充说明（wrong_status/wrong_envelope）
}

// CriticResult 审查产出。
type CriticResult struct {
	Findings []CriticFinding `json:"findings"`
}

// Criticize 对单个 operation 做自由审查（独立 context，不继承 executor 对话）。离线返回 ErrNoProvider。
func Criticize(ctx context.Context, p Provider, task CriticTask, tools []ToolSpec) (*CriticResult, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	ev, err := json.MarshalIndent(task, "", " ")
	if err != nil {
		return nil, fmt.Errorf("infer: marshal critic task: %w", err)
	}
	var out CriticResult
	if err := completeJSONInto(ctx, p, "critic.system.md", string(ev), tools, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}
