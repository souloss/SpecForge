// Package frontend 定义语言前端契约：流水线（engine）只通过这里的接口理解代码。
//
// 分层：
//
//	engine（编排：缓存、LLM 兜底、证据闸门、编译、产物）
//	  └─ frontend.Frontend  —— 把仓库分析成「事实 + 可查询的程序视图」
//	        └─ golang/       —— Go 前端（go/packages + 调用图 + 值流 + 框架适配器）
//	  └─ facts / schema     —— 语言无关的 IR
//	  └─ compiler           —— IR → OpenAPI
//
// 接入新语言 = 实现 Frontend 与 Program 并注册到 engine；缓存、LLM 兜底与复核、
// 证据闸门、编译、CLI 全部复用。
package frontend

import (
	"context"
	"errors"
	"log/slog"

	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/infer"
)

// ErrConfig 配置/输入类错误（仓库不是该语言的项目、画像不合法等）：CLI 据此返回配置错误退出码。
var ErrConfig = errors.New("config error")

// Frontend 语言前端。
type Frontend interface {
	// Name 前端标识（如 "go"）。
	Name() string
	// Detect 仓库是否属于该语言（如存在 go.mod）。
	Detect(repoDir string) bool
	// IsSource 文件名是否属于分析输入（运行级缓存据此计算指纹）。
	IsSource(fileName string) bool
	// Analyze 静态分析仓库，产出事实与程序视图。
	Analyze(ctx context.Context, req Request) (*Analysis, error)
}

// Request 一次分析请求。
type Request struct {
	RepoDir      string              // 仓库根
	Service      string              // 服务过滤名；空 = 全仓
	ServiceRoot  string              // 可选服务源码根；空 = 由前端按 Service 发现
	Manifest     ServiceManifest     // 语言无关服务边界描述
	ProfilePath  string              // 约定画像文件；空 = 前端默认查找
	Provider     infer.Provider      // 可选 LLM（前端内的 LLM 步骤，如画像学习）；nil = 离线
	LearnProfile bool                // 是否用 LLM 学习仓库约定
	Concurrency  int                 // 前端内 LLM 步骤的并发调用数（<1 = 前端默认；结果与并发数无关）
	Log          *slog.Logger        // 日志（非 nil）
	Stage        func(string) func() // 阶段计时：Stage(name) 开始，返回的函数结束（非 nil）
}

// Analysis 前端分析结果。
type Analysis struct {
	Program          Program              // 可查询的程序视图（LLM 证据、工具、证据闸门）
	Service          string               // 产物标题用的服务名
	Framework        string               // 识别出的框架（逗号分隔；未识别为 unknown）
	Facts            []*facts.Fact        // 静态事实（route/contract/schema/security/enrichment）
	Handlers         map[string]string    // operation 键（facts.OpKey）→ handler 符号 ID
	Catalog          map[string]ErrorCode // 错误码目录（符号 ID → 码）
	SuccessCode      int                  // 业务信封的成功码（无信封约定时为 0）
	Stats            Stats                // 统计
	UnresolvedRoutes []RouteIssue         // 未能生成 operation 的路由注册及原因
	// MissedRoutes 通用前端 recall 守卫报出的「疑似遗漏路由」：候选源码里 LLM 未报出的路由注册行。
	// 只进清单供报告/CI 审阅，不擅自进 spec（正则启发式可能误报 config 行）。
	MissedRoutes []RouteIssue
	OpFailures   []OpFailure // 分析失败被隔离的 operation
	Wrappers     StepStats   // L2 LLM 包装器摘要统计
	Adapter      StepStats   // L3 LLM 适配器生成统计
	Routes       StepStats   // L4 通用前端路由发现统计（按路由计）
	Contracts    StepStats   // L4 通用前端契约抽取统计（按 operation 计）
}

// StepStats 前端内一个 LLM 步骤的统计。
type StepStats struct {
	Attempted int `json:"attempted"` // 发起数
	Accepted  int `json:"accepted"`  // 通过复核采纳数
	Rejected  int `json:"rejected"`  // 复核拒绝数
	Failed    int `json:"failed"`    // 调用失败数
}

// Stats 前端统计（CLI 摘要与 --json 使用）。
type Stats struct {
	Packages         int // 分析的包/模块数
	Symbols          int // 符号数
	CallEdges        int // 调用边数
	Routes           int // 抽取的路由数（展开前）
	RoutesResolved   int // 可生成 operation 的路由数
	RoutesUnresolved int // 仍未解析的路由数
	RoutesUnique     int // 按 HTTP method + path 去重后的 operation 身份数
	RoutesCollapsed  int // 因 method + path 相同而折叠的注册数
	SinkSites        int // 事实构造阶段产出的事实数
}

// RouteIssue describes one route registration that could not become an OpenAPI operation.
type RouteIssue struct {
	Method  string `json:"method,omitempty"`
	Path    string `json:"path,omitempty"`
	RawPath string `json:"raw_path,omitempty"`
	Handler string `json:"handler,omitempty"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Reason  string `json:"reason"`
}

// OpFailure 单个 operation 分析失败（被隔离，不影响其它 operation）。
type OpFailure struct {
	Op    string `json:"op"`    // "METHOD /path"
	Error string `json:"error"` // 失败原因
}

// ErrorCode 错误码目录条目。
type ErrorCode struct {
	Symbol string // 常量符号 ID
	Code   int    // 码值
	Msg    string // 说明文案
}

// Program 语言无关的程序查询面。路径参数一律接受绝对路径或仓库相对路径（实现负责归一）。
// 所有方法须确定性、并发安全（LLM 阶段并发调用；缓存层重放工具调用比对输出）。
type Program interface {
	RelPath(path string) string                   // 绝对路径 → 仓库相对路径
	AbsPath(path string) string                   // 仓库相对路径 → 绝对路径
	FileHash(path string) string                  // 文件内容指纹（不可读时为固定占位值）
	FileLines(path string) []string               // 文件按行切分的源码
	SourceRange(path string, from, to int) string // [from,to] 行源码
	ValidLine(path string, line int) bool         // 行号是否在文件内
	// FuncSource 函数的源码位置与文本（file 为绝对路径）；无函数体返回 ok=false。
	FuncSource(id string) (file string, start, end int, text string, ok bool)
	// Reach handler 的正向可达函数及调用深度（handler 为接口方法时从全部实现出发）。
	Reach(handler string) map[string]int
	Callees(id string) []string          // 直接被调函数（有序）
	Symbol(id string) (SymbolInfo, bool) // 符号信息
	TypeOf(id string) (TypeInfo, bool)   // 命名类型信息
	FindSymbols(query string) []string   // ID 含 query（大小写不敏感）的符号与类型（有序去重）
	FieldTypes() map[string]string       // JSON 字段名 → 全仓唯一的基础类型（同名冲突者不收录）
	SymbolExists(id string) bool         // 符号是否存在（复核 LLM 给出的符号）
	// IsSourceFile 路径是否为仓库内的源码文件：LLM 工具只允许读这类文件（防止读到 .env 等敏感文件）。
	IsSourceFile(path string) bool
}

// SymbolInfo 符号信息。
type SymbolInfo struct {
	ID   string // 符号 ID
	Kind string // func/method/type/const/interface
	File string // 声明所在文件（绝对路径）
	Line int    // 声明行号
	Doc  string // 文档注释首段
}

// TypeInfo 命名类型信息。
type TypeInfo struct {
	ID       string      // 类型 ID
	IsStruct bool        // 是否结构体
	Fields   []FieldInfo // 字段（结构体）
}

// FieldInfo 结构体字段。
type FieldInfo struct {
	Name     string // 源码字段名
	JSON     string // 序列化字段名
	Type     string // 源码类型串
	Required bool   // 是否必填
}

// StructIndex 可选能力（有类型信息的前端实现）：函数体内引用到的结构体类型。
// LLM 形状缺口的字段名常只出现在这些结构体的序列化 tag 里（切片函数体中只有源码字段名），
// 复核据此把它们的 JSON 字段名视为「源码中出现」，并以字段声明行作证据。
type StructIndex interface {
	// JSONFieldsIn 函数集合体内引用到的结构体（命名或匿名，含字段类型的递归引用）的序列化字段，
	// 按 JSON 名去重（同名取首个声明位置），按 JSON 名排序。
	JSONFieldsIn(funcIDs []string) []FieldSite
}

// FieldSite 一个结构体序列化字段的声明位置。
type FieldSite struct {
	JSON string // 序列化字段名
	File string // 声明所在文件（绝对路径）
	Line int    // 声明行号
}

// Diagnoser 可选能力：前端自己的环境与仓库自检（doctor 命令）。未实现时 doctor 只做语言无关检查。
type Diagnoser interface {
	// Diagnose 语言相关检查（工具链、项目清单、框架、约定画像等），顺序固定。
	Diagnose(repoDir string) []Check
	// Services 仓库内可独立分析的服务（gen --service 的取值），按名排序。
	Services(repoDir string) []Service
}

// Check 一项自检结果。
type Check struct {
	Name   string `json:"name"`   // 检查项
	OK     bool   `json:"ok"`     // 是否通过
	Detail string `json:"detail"` // 结果说明 / 修复建议
}

// Service 仓库内的一个服务。
type Service struct {
	Name       string   `json:"name"`                 // 服务名（gen --service 取值）
	Dir        string   `json:"dir"`                  // 入口目录（仓库相对路径）
	Entrypoint string   `json:"entrypoint,omitempty"` // 服务入口符号或文件
	Evidence   []string `json:"evidence,omitempty"`   // 服务识别依据
}

type ServiceManifest struct {
	Name       string   `json:"name,omitempty"`
	Root       string   `json:"root,omitempty"`
	Entrypoint string   `json:"entrypoint,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`
}
