package infer

import (
	"context"
	"encoding/json"
	"fmt"
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

// routeSystemPrompt 路由发现的 system prompt。
const routeSystemPrompt = "You find HTTP route registrations in one source file of a web service written in any " +
	"language or framework (Express, Koa, Flask, FastAPI, Django, Spring, Rails, Laravel, ASP.NET, net/http, ...). " +
	"For every route registered in this file, report the HTTP method, the full path (prepend prefixes from " +
	"router mounts, groups or class-level annotations that are visible in this file; keep path parameters as " +
	"written in the source), the handler function/method name (\"inline\" for an inline closure) and the line " +
	"number of the registration (the line holding the path literal). Do not report client calls or routes you " +
	"cannot see. Respond with a single JSON object: " +
	`{"routes":[{"method":"GET","path":"/api/users/:id","handler":"getUser","line":12}]}`

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
	err = completeJSON(ctx, p, Request{System: routeSystemPrompt, Prompt: string(ev)}, func(raw []byte) error {
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
	In       string `json:"in"`       // query/path/header/cookie
	Name     string `json:"name"`     // 参数名
	Type     string `json:"type"`     // string/integer/number/boolean
	Required bool   `json:"required"` // 是否必填
}

// ExtractedResponse 一种响应。
type ExtractedResponse struct {
	Status int        `json:"status"` // HTTP 状态码
	Error  bool       `json:"error"`  // 是否为错误分支
	Body   *ShapeNode `json:"body"`   // 响应体形状（无则 null）
}

// contractSystemPrompt 契约抽取的 system prompt。
const contractSystemPrompt = "You extract the HTTP contract of one operation from its handler source code (any " +
	"language). Report: parameters (query/path/header/cookie; name exactly as read in code), the JSON request " +
	"body shape, and every response the handler can write (status code, whether it is an error branch, JSON body " +
	"shape). Shapes are trees of {type: object|array|string|integer|number|boolean, properties: [{name, required, " +
	"shape}], items}. Every name must appear literally in the source. Omit what you cannot see. Respond with a " +
	`single JSON object: {"params":[{"in":"query","name":"q","type":"string","required":false}],` +
	`"requestBody":{"type":"object","properties":[...]}|null,` +
	`"responses":[{"status":200,"error":false,"body":{"type":"object","properties":[...]}}]}`

// ExtractContract 对一个 operation 抽取契约（tools 非空时可按需读更多源码）。离线返回 ErrNoProvider。
func ExtractContract(ctx context.Context, p Provider, task ContractTask, tools []ToolSpec) (*ExtractedContract, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	ev, err := json.MarshalIndent(task, "", " ")
	if err != nil {
		return nil, fmt.Errorf("infer: marshal contract task: %w", err)
	}
	req := Request{System: contractSystemPrompt, Prompt: string(ev)}
	if len(tools) > 0 {
		req.Tools, req.MaxTurns = tools, defaultToolTurns
	}
	var out ExtractedContract
	err = completeJSON(ctx, p, req, func(raw []byte) error {
		out = ExtractedContract{}
		if err := json.Unmarshal(extractJSON(raw), &out); err != nil {
			return fmt.Errorf("infer: contract invalid: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
