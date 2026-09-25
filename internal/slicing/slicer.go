// Package slicing 实现程序切片与响应汇聚点追踪
// （设计文档 §6.1 步骤 5、§9.1 ShapeClosure、F5 响应契约推断）。
//
// 核心思想: 响应契约不在 handler 里，在调用图里。
// 正向可达集 ∩（到达响应汇聚点的反向可达集）→ 最小分析切片。
package slicing

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/profile"
)

// MaxDepth 切片调用深度上限（设计文档 §9.1 D=6）。
// 真实分层仓库（controller→interface→service→access 多层）需要更深穿透，
// 否则正向可达闭包到不了 service 层的响应汇聚点，导致「no response sink reached」。
const MaxDepth = 12

// SinkHit 一次响应写出点的解析结果。
type SinkHit struct {
	Site          codegraph.CallSite // WriteResponse 调用点
	Status        int                // HTTP 状态码（画像给出）
	Success       bool               // err 槽为 nil → 成功信封
	DataTypeID    string             // data 槽静态类型 ID（nil/unknown 时为空）
	DataUnknown   bool               // any/interface{}: 无法定型
	HasBody       bool
	ErrConstID    string   // 错误码常量符号（可解析时）
	ErrCallCallee string   // 错误构造器调用（如 code.NewDefaultError）
	ErrUnresolved bool     // err 为变量: 信封码不可静态解析
	ErrSource     string   // err 变量的来源证据（定义点的调用符号 + file:line）
	HandlerPath   []string // 从 handler 到 sink 的调用路径（证据链）
}

// Slicer 响应追踪器。
type Slicer struct {
	g    *codegraph.Graph
	prof *profile.Profile
}

// New 创建追踪器。
func New(g *codegraph.Graph, prof *profile.Profile) *Slicer {
	return &Slicer{g: g, prof: prof}
}

// Trace 对一个 handler 做正向可达闭包内的汇聚点扫描。
//
// handler 可能是接口方法（真实仓库 handler 常经接口分发到 service 实现）。
// 此时 g.ForwardReach(handler) 只能走到接口方法自身（无函数体、无调用边），
// 需先把接口方法解析到全部具体实现，再对每个实现做可达闭包与 sink 扫描，
// 证据链标记为经接口分发。
func (s *Slicer) Trace(handlerID string) []SinkHit {
	roots := []string{handlerID}
	if s.g.FuncDeclOf(handlerID) == nil {
		impls := s.g.ConcreteImplsOf(handlerID)
		roots = append(roots, impls...)
	}
	var hits []SinkHit
	for _, root := range roots {
		hits = append(hits, s.traceFrom(root, handlerID)...)
	}
	return hits
}

// traceFrom 从单个根符号做正向可达闭包内的 sink 扫描。
func (s *Slicer) traceFrom(root, handlerID string) []SinkHit {
	reach := s.g.ForwardReach(root, MaxDepth)
	var hits []SinkHit
	// 确定性: 按 (函数, 行号) 排序遍历
	var fns []string
	for fn := range reach {
		fns = append(fns, fn)
	}
	sortStrings(fns)
	for _, fn := range fns {
		for _, callee := range sortedCallees(s.g, fn) {
			if _, ok := s.prof.IsSink(callee); !ok {
				continue
			}
			for _, site := range s.g.Callers[callee] {
				if site.Caller != fn {
					continue
				}
				hits = append(hits, s.analyzeHit(site, reach, handlerID))
			}
		}
	}
	return hits
}

// analyzeHit 解析单个汇聚点调用: data 槽类型 + err 槽语义。
func (s *Slicer) analyzeHit(site codegraph.CallSite, reach map[string]int, handler string) SinkHit {
	pat, _ := s.prof.IsSink(site.Callee)
	hit := SinkHit{Site: site, Status: pat.Status}

	// ---- data 槽 ----
	if pat.DataSlot < len(site.ArgTypes) {
		t := site.ArgTypes[pat.DataSlot]
		switch {
		case t == "untyped nil" || t == "nil":
			hit.HasBody = false
		case t == "interface{}" || t == "any" || strings.HasSuffix(t, "any"):
			// §7.2.3: any 静态不可定型
			if sym := site.ArgSyms[pat.DataSlot]; strings.HasPrefix(sym, "call:") {
				// data 是函数调用: 从返回类型再看一次（实参表达式的类型
				// 在 ArgTypes 里已是表达式静态类型，any 变量才是 any）
				hit.DataUnknown = true
			} else {
				hit.DataUnknown = true
			}
		default:
			id := normalizeTypeID(t)
			if id != "" {
				hit.DataTypeID = id
				hit.HasBody = true
			} else {
				hit.DataUnknown = true
			}
		}
		// 切片数据响应: []pkg.T → 数组 schema（设计文档 §7.1）
		if strings.HasPrefix(t, "[]") {
			if elem := normalizeTypeID(strings.TrimPrefix(t, "[]")); elem != "" {
				hit.DataTypeID = "[]" + elem
				hit.HasBody = true
				hit.DataUnknown = false
			}
		}
	}

	// ---- err 槽 ----
	if pat.ErrSlot < len(site.ArgSyms) {
		sym := site.ArgSyms[pat.ErrSlot]
		switch {
		case sym == "nil":
			hit.Success = true
		case strings.HasPrefix(sym, "call:"):
			hit.ErrCallCallee = strings.TrimPrefix(sym, "call:")
			// 从调用表达式中提取常量参数（错误码目录联动）
			if pat.ErrSlot < len(site.ArgExprs) {
				if expr := site.ArgExprs[pat.ErrSlot]; expr != nil {
					if constID, ok := s.constFromErrCall(expr); ok {
						hit.ErrConstID = constID
					}
				}
			}
		default:
			// err 变量: 错误码无法静态确定，始终标未解析；溯源信息仅供报告展示，
			// 不改变「未解析」的定性（否则会把 errgroup.Wait 等 sink 静默丢弃）。
			hit.ErrSource = s.traceErrSource(site, pat.ErrSlot)
			hit.ErrUnresolved = true
		}
	}
	return hit
}

// traceErrSource 对 err 变量做保守的局部数据流：定位其在 sink 之前最近一次赋值的来源。
//
// 只处理两种确定性来源，其余显式返回空（交由上层按未解析降级，绝不臆测）：
//  1. `if err = g.Wait(); err != nil { return code.Response(c, err, nil) }` ——
//     来源为 errgroup.Wait，错误码不可静态确定（返回 "errgroup.Wait" 标记）。
//  2. 直接 `err := call(...)` 且该 call 返回 error —— 错误码来自被调函数内部。
//
// 返回人类可读的来源证据串（如 "errgroup.Wait@ipoServer.go:233"），
// 供 report 展示；本函数只负责溯源，不负责解析出具体错误码。
func (s *Slicer) traceErrSource(site codegraph.CallSite, errSlot int) string {
	fd := s.funcDeclOf(site.Caller)
	if fd == nil || fd.Body == nil {
		return ""
	}
	sinkLine := site.Line
	var errName string
	if errSlot < len(site.ArgExprs) {
		if id, ok := ast.Unparen(site.ArgExprs[errSlot]).(*ast.Ident); ok {
			errName = id.Name
		}
	}
	if errName == "" {
		return ""
	}
	var source string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if source != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		if s.g.Fset.Position(as.Pos()).Line > sinkLine {
			return false
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != errName || i >= len(as.Rhs) {
				continue
			}
			rhs := as.Rhs[i]
			if call, ok := ast.Unparen(rhs).(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Wait" {
					source = "errgroup.Wait"
				} else {
					source = "call"
				}
			}
		}
		return true
	})
	if source == "" {
		return ""
	}
	return source + "@" + shortFileOf(site.File) + ":" + itoa(sinkLine)
}

// funcDeclOf 定位符号 ID 对应的函数声明。
func (s *Slicer) funcDeclOf(symbolID string) *ast.FuncDecl {
	return s.g.FuncDeclOf(symbolID)
}

// shortFileOf 取文件名（无目录）。
func shortFileOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// itoa 小整数转字符串（避免引入 strconv 全量）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// constFromErrCall 从错误构造器调用中提取常量实参。
func (s *Slicer) constFromErrCall(expr ast.Expr) (string, bool) {
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	info := s.typeInfoOf(call)
	if info == nil {
		return "", false
	}
	return codegraph.ConstArgOfCall(expr, info, 0)
}

// typeInfoOf 通过文件路径定位所属包的类型信息。
func (s *Slicer) typeInfoOf(n ast.Node) *types.Info {
	pos := s.g.Fset.Position(n.Pos())
	if !pos.IsValid() {
		return nil
	}
	for _, p := range s.g.Pkgs() {
		for _, f := range p.Syntax {
			if s.g.Fset.Position(f.Pos()).Filename == pos.Filename {
				return p.TypesInfo
			}
		}
	}
	return nil
}

// ---- 错误码目录 ---------------------------------------------------------

// BuildErrorCatalog 扫描 profile 指定的错误码包（F7 横切）。
func (s *Slicer) BuildErrorCatalog() map[string]ErrorCodeEntry {
	out := map[string]ErrorCodeEntry{}
	src := s.prof.ErrorCodesSource
	for id, sym := range s.g.Syms {
		if sym.Kind != codegraph.KindConst {
			continue
		}
		if src != "" && !strings.HasSuffix(id, "/"+src+".") && !strings.Contains(id, "/"+src+"/") {
			// 只收录错误码包的常量（末段包名匹配）
			if !strings.HasSuffix(strings.TrimSuffix(id, "."+lastSeg(id)), src) {
				continue
			}
		}
		v, ok := s.g.ConstValueOf(id)
		if !ok {
			continue
		}
		out[id] = ErrorCodeEntry{
			Symbol: id, Name: lastSeg(id),
			Code: parseCodeValue(v),
			Msg:  s.errMsgOf(id),
		}
	}
	return out
}

// ErrorCodeEntry 错误码条目。
type ErrorCodeEntry struct {
	Symbol string
	Name   string
	Code   int
	Msg    string
}

// errMsgOf 从错误码包的 map[Code]string 字面量提取 msg（静态证据）。
func (s *Slicer) errMsgOf(constID string) string {
	constName := lastSeg(constID)
	for _, p := range s.g.Pkgs() {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Values) == 0 {
						continue
					}
					cl, ok := vs.Values[0].(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, elt := range cl.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == constName {
							if lit, ok := kv.Value.(*ast.BasicLit); ok {
								return trimQuotes(lit.Value)
							}
						}
					}
				}
			}
		}
	}
	return ""
}

// ---- 辅助 --------------------------------------------------------------

func normalizeTypeID(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "*")
	// 泛型实例化 "pkg.T[pkg.U]" → "pkg.T"
	if i := strings.Index(t, "["); i > 0 && strings.HasSuffix(t, "]") {
		t = t[:i]
	}
	if strings.ContainsAny(t, " ()") || t == "" || t == "string" || t == "int" ||
		t == "bool" || t == "float64" || strings.HasPrefix(t, "[]") ||
		strings.HasPrefix(t, "map[") || t == "any" || t == "interface{}" {
		return ""
	}
	return t
}

func parseCodeValue(v string) int {
	v = strings.Trim(v, `"`)
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
		return n
	}
	return -1
}

func trimQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func lastSeg(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func sortedCallees(g *codegraph.Graph, fn string) []string {
	out := append([]string{}, g.Callees[fn]...)
	sortStrings(out)
	return out
}
