package infer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// L4 通用 LLM 前端的任务协议：不依赖任何语言分析，输入都是源码文本，输出都由上层按文本核对。

// RouteTask 路由发现任务：一个候选源文件（带行号）。
type RouteTask struct {
	File  string `json:"file"`  // 仓库相对路径
	Lines string `json:"lines"` // 带行号的源码（"12| app.get('/x', h)"）
}

// DiscoveredRoute LLM 发现的一条路由。
type DiscoveredRoute struct {
	Method  string `json:"method"`  // HTTP 方法（大写）
	Path    string `json:"path"`    // 完整路径（拼上同文件内可见的挂载/分组前缀），参数保留源码写法
	Handler string `json:"handler"` // handler 名（内联函数写 "inline"）
	Line    int    `json:"line"`    // 注册语句所在行号
}

// RouteList 路由发现结果。
type RouteList struct {
	Routes []DiscoveredRoute `json:"routes"` // 路由
}

// DiscoverRoutes 对一个文件做路由发现。离线返回 ErrNoProvider。
func DiscoverRoutes(ctx context.Context, p Provider, task RouteTask) (*RouteList, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	ev, err := json.MarshalIndent(task, "", " ")
	if err != nil {
		return nil, fmt.Errorf("infer: marshal route task: %w", err)
	}
	var out RouteList
	err = completeJSON(ctx, p, Request{System: prompt("route.system.md"), Prompt: string(ev)}, func(raw []byte) error {
		out = RouteList{}
		if err := json.Unmarshal(extractJSON(raw), &out); err != nil {
			return fmt.Errorf("infer: route list invalid: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ContractTask 契约抽取任务：一个 operation 的 handler 源码切片。
type ContractTask struct {
	Source        string          `json:"source,omitempty"`        // generic|static|openapi|runtime|documentation
	Operation     string          `json:"operation,omitempty"`     // stable operation identity
	AllowedFields []string        `json:"allowedFields,omitempty"` // fields the model may fill
	Method        string          `json:"method"`                  // HTTP 方法
	Path          string          `json:"path"`                    // 路径
	Sources       []SourceSnippet `json:"sources"`                 // handler 与其调用的函数源码
}

// ExtractedContract LLM 抽取的契约（字段名须能在源码中找到，形状取封闭集合）。
type ExtractedContract struct {
	Params      []ExtractedParam    `json:"params"`      // 参数
	RequestBody *ShapeNode          `json:"requestBody"` // 请求体形状（无则 null）
	Responses   []ExtractedResponse `json:"responses"`   // 响应
}

// ExtractedParam 一个参数。
type ExtractedParam struct {
	In       string   `json:"in"`                  // query/path/header/cookie
	Name     string   `json:"name"`                // 参数名
	Type     string   `json:"type"`                // string/integer/number/boolean
	Required bool     `json:"required"`            // 是否必填
	Format   string   `json:"format,omitempty"`    // date-time/int64...
	Enum     []string `json:"enum,omitempty"`      // 枚举取值
	Pattern  string   `json:"pattern,omitempty"`   // 正则
	Min      *float64 `json:"min,omitempty"`       // 数值下限
	Max      *float64 `json:"max,omitempty"`       // 数值上限
	MinLen   *int     `json:"minLength,omitempty"` // 最小长度
	MaxLen   *int     `json:"maxLength,omitempty"` // 最大长度
}

// ExtractedResponse 一种响应。
type ExtractedResponse struct {
	Status int        `json:"status"` // HTTP 状态码
	Error  bool       `json:"error"`  // 是否为错误分支
	Body   *ShapeNode `json:"body"`   // 响应体形状（无则 null）
}

// ExtractContract 对一个 operation 抽取契约（tools 非空时可按需读更多源码）。离线返回 ErrNoProvider。
//
// 保留整体入口：内部把任务拆成 params / requestBody / responses 三个独立子调用
// （见 ExtractParams / ExtractRequestBody / ExtractResponses），单步子调用失败只留下对应的
// 空结果，不再「一个 JSON 定终身」整份丢弃。三个子调用互不依赖，并行执行。
func ExtractContract(ctx context.Context, p Provider, task ContractTask, tools []ToolSpec) (*ExtractedContract, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	ev, err := contractPrompt(task)
	if err != nil {
		return nil, err
	}
	out := &ExtractedContract{}
	var errs [3]error
	extractSub(ctx, p, tools, []subExtraction{
		{name: "params.system.md", prompt: ev, key: "params", into: &out.Params, err: &errs[0]},
		{name: "request-body.system.md", prompt: ev, key: "requestBody", into: &out.RequestBody, err: &errs[1]},
		{name: "responses.system.md", prompt: ev, key: "responses", into: &out.Responses, err: &errs[2]},
	})
	if errs[0] != nil && errs[1] != nil && errs[2] != nil {
		return nil, fmt.Errorf("infer: all contract sub-extractions failed: params=%v body=%v responses=%v", errs[0], errs[1], errs[2])
	}
	return out, nil
}

// contractPrompt 契约子任务的公共用户消息：操作身份 + handler 源码切片（三个子任务共享，只 marshal 一次）。
func contractPrompt(task ContractTask) (string, error) {
	ev, err := json.MarshalIndent(task, "", " ")
	if err != nil {
		return "", fmt.Errorf("infer: marshal contract task: %w", err)
	}
	return string(ev), nil
}

// subExtraction 一个契约子抽取任务：system prompt 名 + 已 marshal 的共享用户消息 + 输出目标。
// into 是指向 out 结构体字段的指针；模型输出为 {key: value} 单键对象，按 key 取值后写入 into。
// 三路子任务同构，收敛到一个骨架。
type subExtraction struct {
	name   string // prompts/ 下的 system prompt 文件名
	prompt string // 共享的用户消息（已 marshal）
	key    string // 模型输出 JSON 的顶层键（params / requestBody / responses）
	into   any    // 输出字段指针（*[]ExtractedParam / **ShapeNode / *[]ExtractedResponse）
	err    *error // 结果错误回填
}

// extractSub 并行执行多个同构的子抽取（共享用户消息，各自独立 system prompt 与缓存键）。
// 子任务失败只回填 err，不阻断其它子任务；Provider 须并发安全（接口契约要求）。
func extractSub(ctx context.Context, p Provider, tools []ToolSpec, subs []subExtraction) {
	var wg sync.WaitGroup
	for i := range subs {
		wg.Add(1)
		go func(s *subExtraction) {
			defer wg.Done()
			*s.err = completeJSONInto(ctx, p, s.name, s.prompt, tools, s.key, s.into)
		}(&subs[i])
	}
	wg.Wait()
}

// completeJSONInto 组装 Request（system prompt 名 + 共享用户消息 + 工具装配）→ completeJSON → 解析进 into。
// 模型输出为 {key: value} 单键对象时按 key 取值，否则直接解析整段（key 为空）。
// 所有「marshal task + Request 组装 + 工具接线 + 解析」骨架收敛于此（DiscoverRoutes/ResolveGaps/Criticize 同构）。
func completeJSONInto(ctx context.Context, p Provider, systemName, userPrompt string, tools []ToolSpec, key string, into any) error {
	req := Request{System: prompt(systemName), Prompt: userPrompt}
	if len(tools) > 0 {
		req.Tools, req.MaxTurns = tools, defaultToolTurns
	}
	return completeJSON(ctx, p, req, func(raw []byte) error {
		raw = extractJSON(raw)
		if key != "" {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			raw = m[key]
			if raw == nil {
				return fmt.Errorf("infer: sub-extraction missing key %q", key)
			}
		}
		return json.Unmarshal(raw, into)
	})
}

// ExtractParams 抽取 operation 的参数子契约（独立 prompt + 独立缓存键）。
func ExtractParams(ctx context.Context, p Provider, task ContractTask, tools []ToolSpec) ([]ExtractedParam, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	ev, err := contractPrompt(task)
	if err != nil {
		return nil, err
	}
	var params []ExtractedParam
	if err := completeJSONInto(ctx, p, "params.system.md", ev, tools, "params", &params); err != nil {
		return nil, err
	}
	return params, nil
}

// ExtractRequestBody 抽取 operation 的请求体子契约（独立 prompt + 独立缓存键）。
func ExtractRequestBody(ctx context.Context, p Provider, task ContractTask, tools []ToolSpec) (*ShapeNode, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	ev, err := contractPrompt(task)
	if err != nil {
		return nil, err
	}
	var body *ShapeNode
	if err := completeJSONInto(ctx, p, "request-body.system.md", ev, tools, "requestBody", &body); err != nil {
		return nil, err
	}
	return body, nil
}

// ExtractResponses 抽取 operation 的响应子契约（独立 prompt + 独立缓存键）。
func ExtractResponses(ctx context.Context, p Provider, task ContractTask, tools []ToolSpec) ([]ExtractedResponse, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	ev, err := contractPrompt(task)
	if err != nil {
		return nil, err
	}
	var responses []ExtractedResponse
	if err := completeJSONInto(ctx, p, "responses.system.md", ev, tools, "responses", &responses); err != nil {
		return nil, err
	}
	return responses, nil
}
