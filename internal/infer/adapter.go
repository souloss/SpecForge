package infer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// AdapterTask L3 适配器生成任务：一个未登记的 Web 框架的导出 API 摘要。
type AdapterTask struct {
	Module   string       `json:"module"`   // 仓库依赖的框架模块路径
	Packages []PackageAPI `json:"packages"` // 候选框架包的 API 摘要
}

// PackageAPI 一个包的导出 API 摘要。
type PackageAPI struct {
	Path  string    `json:"path"`            // 包路径
	Types []TypeAPI `json:"types,omitempty"` // 相关命名类型（路由对象、请求上下文等）
	Funcs []string  `json:"funcs,omitempty"` // 包级函数签名
}

// TypeAPI 一个命名类型的方法签名。
type TypeAPI struct {
	Name    string   `json:"name"`    // 类型名
	Kind    string   `json:"kind"`    // struct / interface / func / other
	Methods []string `json:"methods"` // 方法签名（如 "GET(path string, h HandlerFunc, m ...MiddlewareFunc) *Route"）
}

// AdapterProposal LLM 提出的适配器声明（符号均为 "包路径.函数" 或 "包路径.类型.方法"）。
type AdapterProposal struct {
	Name          string              `json:"name"`          // 框架短名（如 echo）
	RouterTypes   []string            `json:"routerTypes"`   // 路由对象类型（包路径.类型名）
	GroupMethod   string              `json:"groupMethod"`   // 返回子路由的分组方法名（无则空）
	Verbs         map[string]string   `json:"verbs"`         // 注册方法名 → HTTP 方法
	HandlerArg    int                 `json:"handlerArg"`    // 注册调用中 handler 的实参下标（路径为 0；-1 = 末位）
	BodyBinders   []ProposedBinder    `json:"bodyBinders"`   // 请求体绑定
	StructBinders []ProposedStructBnd `json:"structBinders"` // 结构体参数绑定
	ParamReaders  []ProposedReader    `json:"paramReaders"`  // 单参数读取
	Writers       []ProposedWriter    `json:"writers"`       // 响应写出
}

// ProposedBinder 请求体绑定原语。
type ProposedBinder struct {
	Symbol string `json:"symbol"` // 符号
	Arg    int    `json:"arg"`    // 绑定目标实参下标
}

// ProposedStructBnd 结构体参数绑定原语。
type ProposedStructBnd struct {
	Symbol string `json:"symbol"` // 符号
	Arg    int    `json:"arg"`    // 绑定目标实参下标
	In     string `json:"in"`     // query/path/header/cookie
	TagKey string `json:"tagKey"` // 字段名 tag 键
}

// ProposedReader 单参数读取原语。
type ProposedReader struct {
	Symbol  string `json:"symbol"`  // 符号
	In      string `json:"in"`      // query/path/header/cookie
	NameArg int    `json:"nameArg"` // 参数名实参下标
	Type    string `json:"type"`    // string/integer/number/boolean
}

// ProposedWriter 响应写出原语。
type ProposedWriter struct {
	Symbol    string `json:"symbol"`    // 符号
	BodyArg   int    `json:"bodyArg"`   // 响应体实参下标
	StatusArg int    `json:"statusArg"` // 状态码实参下标；-1 = 无
}

// adapterSystemPrompt L3 适配器生成的 system prompt。
const adapterSystemPrompt = "You write a declarative adapter for a Go web framework so that a static analyzer can " +
	"extract HTTP routes and contracts. From the framework's exported API, identify: router types (types whose " +
	"methods register routes), the grouping method that returns a sub-router with a path prefix, the route " +
	"registration methods and their HTTP verbs, the argument index of the handler in those calls (the path is " +
	"argument 0; -1 means the last argument), and request/response primitives on the handler's context type: " +
	"JSON body binding, struct binding of query/path/header params, single parameter readers (the argument index " +
	"of the parameter name), and JSON response writers (argument index of the body and of the status code, -1 if " +
	"none). Use symbols exactly as <package path>.<Func> or <package path>.<Type>.<Method>. Only use APIs present " +
	"in the digest. Include only primitives that exist in the digest; omit a list rather than guess. " +
	"Respond with a single JSON object with EXACTLY this structure (example for a hypothetical package " +
	"example.com/webkit; replace every value with the real framework's):\n" + adapterExample + "\n"

// adapterExample 输出结构示例（虚构框架，防止模型照抄答案；只示范字段形状）。
const adapterExample = `{
  "name": "webkit",
  "routerTypes": ["example.com/webkit.Server", "example.com/webkit.RouteGroup"],
  "groupMethod": "Group",
  "verbs": {"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE"},
  "handlerArg": 1,
  "bodyBinders": [{"symbol": "example.com/webkit.Ctx.DecodeBody", "arg": 0}],
  "structBinders": [{"symbol": "example.com/webkit.Ctx.DecodeQuery", "arg": 0, "in": "query", "tagKey": "query"}],
  "paramReaders": [{"symbol": "example.com/webkit.Ctx.PathValue", "in": "path", "nameArg": 0, "type": "string"}],
  "writers": [{"symbol": "example.com/webkit.Ctx.WriteJSON", "bodyArg": 1, "statusArg": 0}]
}`

// ProposeAdapter 让 LLM 提出适配器声明（结构校验在此，符号与签名复核在前端）。离线返回 ErrNoProvider。
func ProposeAdapter(ctx context.Context, p Provider, task AdapterTask) (*AdapterProposal, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	ev, err := json.MarshalIndent(task, "", " ")
	if err != nil {
		return nil, fmt.Errorf("infer: marshal adapter task: %w", err)
	}
	var out AdapterProposal
	err = completeJSON(ctx, p, Request{System: adapterSystemPrompt, Prompt: string(ev)}, func(raw []byte) error {
		out = AdapterProposal{}
		if err := json.Unmarshal(extractJSON(raw), &out); err != nil {
			return fmt.Errorf("infer: adapter proposal invalid: %w", err)
		}
		if len(out.RouterTypes) == 0 || len(out.Verbs) == 0 {
			return errors.New("infer: adapter proposal lacks router types or verbs")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
