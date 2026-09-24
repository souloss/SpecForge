// Package adapter 定义语言/框架适配器接口（设计文档 §10.2）。
//
// P0 范围: Go + fiber。适配器只产出数据与 Unresolved，
// 不感知缓存、失效、调度（「适配器实现者不需要理解增量引擎」）。
package adapter

import (
	"go/ast"

	"github.com/specforge/specforge/internal/codegraph"
)

// Route 一条静态抽取出的路由候选。
type Route struct {
	Method     string   // GET/POST/...
	Path       string   // 归一化 OpenAPI 路径（:p → {p}）
	RawPath    string   // 原始拼接路径（含通配符）
	Handler    string   // handler 符号 ID
	Middleware []string // 中间件符号 ID 链（按注册顺序）
	File       string
	Line       int
	Unresolved string // 非空 = 抽取不完整的归因（wildcard/dynamic/table-driven）
}

// Unresolved 归因枚举。
const (
	ReasonWildcard  = "wildcard-path"     // 路径含通配符，非合法 OpenAPI path
	ReasonLoop      = "loop-registration" // 循环/表驱动注册，path 非字面量
	ReasonDynamic   = "dynamic-path"      // 运行时拼接路径
	ReasonNoHandler = "no-handler"        // 末参无法解析为符号
)

// FrameworkAdapter 框架模式集合。
type FrameworkAdapter interface {
	Name() string
	RouterMethods() []string // .Get .Post ...
	ExtractRoutes(p *PkgCtx) []Route
}

// PkgCtx 单包的抽取上下文（语法树 + 类型信息 + 代码图）。
type PkgCtx struct {
	Path   string
	Syntax []*ast.File
	Info   *TypeInfoView
	Graph  *codegraph.Graph
	File   func(ast.Node) string
	Line   func(ast.Node) int
}

// TypeInfoView 类型信息的窄视图（避免适配器直接依赖 go/packages）。
type TypeInfoView struct {
	// Selector 定位: expr → 符号 ID
	ResolveExpr func(ast.Expr) string
	// 调用点实参类型
	ArgTypes func(call *ast.CallExpr) []string
	// 表达式的静态类型串（接收者判别用）
	RecvTypeOf func(ast.Expr) string
}
