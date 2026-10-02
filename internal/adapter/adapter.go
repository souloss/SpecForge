// Package adapter 定义框架适配器契约（设计文档 §10.2）与内置 Go Web 框架。
//
// 一个框架 = 路由抽取 + 一张声明式「原语表」（请求绑定 / 参数读取 / 响应写出的符号与实参位置）。
// 响应追踪、包装器发现、值流反推、信封识别、收窄、LLM 兜底等分析算法全部与框架无关，
// 只消费原语表——接入新的 Go 框架只需新增一个声明文件（见 gin.go），不改内核。
// 适配器只产出数据与 Unresolved 归因，不感知缓存、失效、调度。
package adapter

import (
	"go/ast"
	"sort"
	"strings"

	"github.com/specforge/specforge/internal/codegraph"
)

// Route 一条静态抽取出的路由候选。
type Route struct {
	Method     string   // GET/POST/...
	Path       string   // 归一化 OpenAPI 路径（:p → {p}）
	RawPath    string   // 原始拼接路径（含通配符）
	Handler    string   // handler 符号 ID
	Middleware []string // 中间件符号 ID 链（按注册顺序）
	File       string   // 注册语句所在文件
	Line       int      // 注册语句行号
	Unresolved string   // 非空 = 抽取不完整的归因（wildcard/dynamic/table-driven）
	// WildcardParam 通配符模板化产生的路径参数名（空 = 非模板化路由）。
	WildcardParam string
}

// Unresolved 归因枚举。
const (
	ReasonWildcard          = "wildcard-path"      // 路径含通配符，非合法 OpenAPI path
	ReasonLoop              = "loop-registration"  // 循环/表驱动注册，path 非字面量
	ReasonDynamic           = "dynamic-path"       // 运行时拼接路径
	ReasonDynamicMethod     = "dynamic-method"     // HTTP 方法不是静态字符串
	ReasonUnsupportedMethod = "unsupported-method" // 框架方法无法表示为 OpenAPI operation
	ReasonNoHandler         = "no-handler"         // 末参无法解析为符号
)

// Framework 框架适配器契约。
type Framework interface {
	// Name 框架标识（如 gofiber/v2），出现在产物与 doctor 输出中。
	Name() string
	// Short 证据来源标签前缀（如 fiber → 参数来源 "fiber:Query"）。
	Short() string
	// Module 框架模块路径前缀：仓库 import 了它即视为使用该框架。
	Module() string
	// ExtractRoutes 抽取一个包内的全部路由注册。
	ExtractRoutes(p *PkgCtx) []Route
	// Primitives 框架原语表（分析算法据此识别请求绑定、参数读取与响应写出）。
	Primitives() Primitives
}

// NoArg 原语表中「无此实参」的下标占位（如写出器没有状态码实参）。
const NoArg = -1

// Primitives 框架原语表：全部按全限定符号 ID 匹配（不按方法名，避免 db.Query 之类误判）。
type Primitives struct {
	BodyBinders   []BodyBinder   // 请求体绑定：实参类型即请求体 schema
	StructBinders []StructBinder // 结构体绑定到 query/path/header/cookie：字段按 tag 展开为参数
	ParamReaders  []ParamReader  // 单个参数读取：名字取字符串字面量实参
	Writers       []Writer       // 响应写出：实参类型即响应体 schema
}

// BodyBinder 请求体绑定原语（如 fiber Ctx.BodyParser、gin Context.ShouldBindJSON）。
type BodyBinder struct {
	Symbol      string `json:"symbol"`      // 方法符号 ID
	Arg         int    `json:"arg"`         // 绑定目标实参下标
	ContentType string `json:"contentType"` // 请求体媒体类型
}

// StructBinder 结构体绑定原语（如 fiber QueryParser、gin ShouldBindQuery）。
type StructBinder struct {
	Symbol string `json:"symbol"` // 方法符号 ID
	Arg    int    `json:"arg"`    // 绑定目标实参下标
	In     string `json:"in"`     // 参数位置 query/path/header/cookie
	TagKey string `json:"tagKey"` // 字段名取值的 struct tag 键
}

// ParamReader 单参数读取原语（如 fiber Ctx.Query("name")、gin Context.Param("id")）。
type ParamReader struct {
	Symbol  string `json:"symbol"`  // 方法符号 ID
	In      string `json:"in"`      // 参数位置 query/path/header/cookie
	NameArg int    `json:"nameArg"` // 参数名字符串字面量的实参下标
	Type    string `json:"type"`    // 参数 JSON Schema 类型（QueryInt → integer）
}

// Writer 响应写出原语（如 fiber Ctx.JSON(body)、gin Context.JSON(status, body)）。
type Writer struct {
	Symbol        string `json:"symbol"`        // 方法符号 ID
	BodyArg       int    `json:"bodyArg"`       // 响应体实参下标
	StatusArg     int    `json:"statusArg"`     // 状态码实参下标；NoArg = 由框架默认值决定
	DefaultStatus int    `json:"defaultStatus"` // 未显式给出状态码时的 HTTP 状态
}

// Merge 合并两张原语表（多框架仓库）。
func (p Primitives) Merge(o Primitives) Primitives {
	return Primitives{
		BodyBinders:   append(append([]BodyBinder(nil), p.BodyBinders...), o.BodyBinders...),
		StructBinders: append(append([]StructBinder(nil), p.StructBinders...), o.StructBinders...),
		ParamReaders:  append(append([]ParamReader(nil), p.ParamReaders...), o.ParamReaders...),
		Writers:       append(append([]Writer(nil), p.Writers...), o.Writers...),
	}
}

// registry 内置框架（按识别优先级）。新增框架：在同包新增声明文件并加入此表。
var registry = []Framework{Fiber, Gin, ChiV4, ChiV5}

// Detect 按仓库 import 的包路径识别使用的全部框架（按 registry 顺序，结果确定）。
func Detect(imports []string) []Framework {
	var out []Framework
	for _, fw := range registry {
		for _, imp := range imports {
			if moduleMatches(imp, fw.Module()) {
				out = append(out, fw)
				break
			}
		}
	}
	return out
}

func moduleMatches(importPath, module string) bool {
	if importPath == module {
		return true
	}
	rest, ok := strings.CutPrefix(importPath, module+"/")
	if !ok {
		return false
	}
	// A major-version module path is a distinct module, not a subpackage of
	// the unversioned path (for example chi/v5 vs chi/middleware).
	return !(len(rest) >= 2 && rest[0] == 'v' && rest[1] >= '0' && rest[1] <= '9' && (len(rest) == 2 || rest[2] == '/'))
}

// DetectFramework 首个识别出的框架名（F2，doctor 与产物标题用）；未识别返回空。
func DetectFramework(imports []string) string {
	if fws := Detect(imports); len(fws) > 0 {
		return fws[0].Name()
	}
	return ""
}

// Supported 内置支持的框架名（有序）。
func Supported() []string {
	out := make([]string, 0, len(registry))
	for _, fw := range registry {
		out = append(out, fw.Name())
	}
	sort.Strings(out)
	return out
}

// MethodSymbol 框架方法的符号 ID（与代码图的方法 ID 口径一致）。
func MethodSymbol(pkg, recv, method string) string {
	return codegraph.MethodID(pkg, recv, method)
}

// PkgCtx 单包的抽取上下文（语法树 + 类型信息 + 代码图）。
type PkgCtx struct {
	Path   string                // 包导入路径
	Syntax []*ast.File           // 语法树
	Info   *TypeInfoView         // 类型信息窄视图
	Graph  *codegraph.Graph      // 代码图
	File   func(ast.Node) string // 节点所在文件
	Line   func(ast.Node) int    // 节点所在行
}

// TypeInfoView 类型信息的窄视图（避免适配器直接依赖 go/packages）。
type TypeInfoView struct {
	ResolveExpr func(ast.Expr) string             // 表达式 → 符号 ID（函数/方法/构造器调用）
	ArgTypes    func(call *ast.CallExpr) []string // 调用点实参类型
	RecvTypeOf  func(ast.Expr) string             // 表达式的静态类型串（接收者判别用）
}
