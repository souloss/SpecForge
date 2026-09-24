// Package codegraph 构建代码图：符号表、调用边、类型图（设计文档 §3.1 存储层 CG）。
//
// 全部产物带指纹（FileHash / SymbolHash），调用边解析使用 go/types 的
// 精确类型信息（等价于设计文档的 LSP 桥接，P0 用 go/packages 一次完成）。
package codegraph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// ---- 符号与类型 -------------------------------------------------------

type SymKind string

const (
	KindFunc      SymKind = "func"
	KindMethod    SymKind = "method"
	KindType      SymKind = "type"
	KindConst     SymKind = "const"
	KindInterface SymKind = "interface"
)

// Symbol 代码图中的一个具名符号。
type Symbol struct {
	ID      string // 全限定 ID: pkg.Func / pkg.(*Recv).Method
	Kind    SymKind
	Pkg     string
	File    string
	Line    int
	EndLine int
	Doc     string // godoc 首段（enrichment 零成本来源，设计文档 §5.8）
	Recv    string // 方法所属接收者类型 ID（方法才有）
}

// CallSite 一条已解析的调用边（含实参静态类型，响应追踪的关键）。
type CallSite struct {
	Caller   string
	Callee   string
	File     string
	Line     int
	ArgTypes []string   // 调用点实参的静态类型串
	ArgSyms  []string   // 实参值身份: 符号 ID / 字面量 / "nil" / "call:X"
	ArgExprs []ast.Expr // 原始表达式（响应追踪深用）
	ViaIface bool       // 经接口分发的近似边
}

// Field 结构体字段的规范化视图。
type Field struct {
	Name     string // Go 字段名
	JSONName string // json tag 名（无 tag 等于 Name）
	TypeID   string // 命名类型 ID；内联类型为类型串
	TypeStr  string
	Kind     string // basic|slice|map|ptr|struct|named|any|rawmsg|time
	Tag      string
	Required bool // 无 omitempty 即必填（设计文档 §7.2.6）
	Nullable bool // 指针字段
}

// TypeInfo 命名类型规范化视图。
type TypeInfo struct {
	ID          string
	Pkg         string
	File        string
	Line        int
	IsStruct    bool
	Fields      []Field
	Underlying  string
	Consts      []ConstValue // 同类型 const 组
	Enumish     bool         // 枚举判定结果（§7.2.12）
	IsTime      bool
	IsRawMsg    bool
	GenericArgs []string
}

// ConstValue 常量字面值（错误码目录 / 枚举值）。
type ConstValue struct {
	ID    string
	Name  string
	Value string
}

// Graph 代码图。
type Graph struct {
	Fset        *token.FileSet
	Syms        map[string]*Symbol
	Types       map[string]*TypeInfo
	Callees     map[string][]string   // caller → callee IDs
	Callers     map[string][]CallSite // callee → 调用点
	constValue  map[string]string
	constTypeID map[string]string // const → 声明类型 ID（枚举归属）
	fileHash    map[string]string
	pkgOf       map[string]*packages.Package
	pkgs        []*packages.Package
	// funcDecls 记录「符号 ID → 函数声明」，供 O(1) 查询函数体；
	// 接口方法没有 FuncDecl，因此不在表中——这是区分接口方法与具体实现的依据。
	funcDecls   map[string]*ast.FuncDecl
	funcDeclPkg map[string]*packages.Package
	// namedOf 记录「类型 ID → 命名类型」，供接口方法解析到具体实现（ConcreteImplsOf）。
	namedOf map[string]*types.Named
}

// NewGraph 创建空图。
func NewGraph() *Graph {
	return &Graph{
		Syms:        map[string]*Symbol{},
		Types:       map[string]*TypeInfo{},
		Callees:     map[string][]string{},
		Callers:     map[string][]CallSite{},
		constValue:  map[string]string{},
		constTypeID: map[string]string{},
		fileHash:    map[string]string{},
		pkgOf:       map[string]*packages.Package{},
		funcDecls:   map[string]*ast.FuncDecl{},
		funcDeclPkg: map[string]*packages.Package{},
		namedOf:     map[string]*types.Named{},
	}
}

// Build 遍历全部包构建代码图（含非导出符号——响应链在 service 深处）。
func Build(pkgs []*packages.Package, fset *token.FileSet) (*Graph, error) {
	g := NewGraph()
	g.Fset = fset
	for _, p := range pkgs {
		if p.TypesInfo == nil || p.Types == nil || len(p.Syntax) == 0 {
			continue
		}
		g.pkgOf[p.PkgPath] = p
		g.pkgs = append(g.pkgs, p)
		g.buildPkg(p)
	}
	g.expandInterfaceDispatch()
	return g, nil
}

// ---- 包处理 -----------------------------------------------------------

func (g *Graph) buildPkg(p *packages.Package) {
	info := p.TypesInfo
	scope := p.Types.Scope()

	// 包级 func / type / const
	for _, name := range scope.Names() {
		switch o := scope.Lookup(name).(type) {
		case *types.Func:
			g.addFuncSym(p, o)
		case *types.TypeName:
			g.addTypeSym(p, o)
		case *types.Const:
			g.addConstSym(p, o)
		}
	}
	// 命名类型的方法集
	for _, name := range scope.Names() {
		if tn, ok := scope.Lookup(name).(*types.TypeName); ok {
			if named, ok := tn.Type().(*types.Named); ok {
				for i := 0; i < named.NumMethods(); i++ {
					g.addFuncSym(p, named.Method(i))
				}
			}
		}
	}

	// 函数内局部类型注册（response 深处的 local struct，F5 完整性）
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				gd, ok := n.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					return true
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					g.addLocalTypeSym(p, ts)
				}
				return true
			})
		}
	}
	// 函数体: 调用边 + 实参静态类型
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			callerID := g.funcDeclID(p, fd, info)
			if callerID == "" {
				continue
			}
			g.funcDecls[callerID] = fd
			g.funcDeclPkg[callerID] = p
			g.Syms[callerID].Doc = g.docForFuncDecl(p, fd, info)
			g.walkCalls(callerID, fd, info)
		}
	}
}

// funcDeclID 由语法+类型信息确定函数符号 ID。
func (g *Graph) funcDeclID(p *packages.Package, fd *ast.FuncDecl, info *types.Info) string {
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		recvT := info.TypeOf(fd.Recv.List[0].Type)
		if recvT == nil {
			return ""
		}
		recvID := typeID(stripPtr(recvT))
		id := fmt.Sprintf("%s.(*%s).%s", p.PkgPath, recvID, fd.Name.Name)
		if g.Syms[id] == nil {
			g.Syms[id] = &Symbol{ID: id, Kind: KindMethod, Pkg: p.PkgPath, Recv: recvID}
		}
		return id
	}
	id := p.PkgPath + "." + fd.Name.Name
	if g.Syms[id] == nil {
		g.Syms[id] = &Symbol{ID: id, Kind: KindFunc, Pkg: p.PkgPath}
	}
	return id
}

// walkCalls 收集函数体内全部调用边（含实参静态类型——响应追踪的依据）。
func (g *Graph) walkCalls(caller string, fd *ast.FuncDecl, info *types.Info) {
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		calleeID, viaIface := resolveCallee(call, info)
		if calleeID == "" {
			return true
		}
		site := CallSite{Caller: caller, Callee: calleeID, ViaIface: viaIface}
		if pos := g.Fset.Position(call.Lparen); pos.IsValid() {
			site.File = pos.Filename
			site.Line = pos.Line
		}
		for _, arg := range call.Args {
			if tv, ok := info.Types[arg]; ok && tv.Type != nil {
				site.ArgTypes = append(site.ArgTypes, tv.Type.String())
			} else {
				site.ArgTypes = append(site.ArgTypes, "?")
			}
			site.ArgSyms = append(site.ArgSyms, valueIdentityOf(arg, info))
			site.ArgExprs = append(site.ArgExprs, arg)
		}
		g.addCall(caller, calleeID, site)
		return true
	})
}

// resolveCallee 调用表达式 → 精确符号 ID；接口分发 → 接口方法 ID + 模糊标记。
func resolveCallee(call *ast.CallExpr, info *types.Info) (string, bool) {
	fn := ast.Unparen(call.Fun)
	switch f := fn.(type) {
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[f]; ok && sel.Obj() != nil {
			recv := sel.Recv()
			if types.IsInterface(recv) {
				ifaceID := typeID(stripPtr(recv))
				return fmt.Sprintf("%s.%s", ifaceID, sel.Obj().Name()), true
			}
			if fnObj, ok := sel.Obj().(*types.Func); ok {
				return methodIDOf(fnObj), false
			}
		}
		if obj := info.Uses[f.Sel]; obj != nil {
			if fnObj, ok := obj.(*types.Func); ok {
				return fnObj.FullName(), false
			}
		}
	case *ast.Ident:
		if obj := info.Uses[f]; obj != nil {
			if fnObj, ok := obj.(*types.Func); ok {
				return fnObj.FullName(), false
			}
		}
	case *ast.IndexExpr, *ast.IndexListExpr: // 泛型实例化调用
		base := fn.(interface{ X() ast.Expr })
		_ = base
		if idx, ok := fn.(*ast.IndexExpr); ok {
			if id, ok := idx.X.(*ast.Ident); ok {
				if obj := info.Uses[id]; obj != nil {
					if fnObj, ok := obj.(*types.Func); ok {
						return fnObj.FullName(), false
					}
				}
			}
		}
	}
	return "", false
}

func (g *Graph) addCall(caller, callee string, site CallSite) {
	if !contains(g.Callees[caller], callee) {
		g.Callees[caller] = append(g.Callees[caller], callee)
	}
	g.Callers[callee] = append(g.Callers[callee], site)
}

// expandInterfaceDispatch 把接口方法调用边展开到全部同名方法实现
// （调用图过近似；置信度由使用方降级——设计文档 §7.2.3 原则）。
func (g *Graph) expandInterfaceDispatch() {
	methodsByName := map[string][]string{}
	for id, s := range g.Syms {
		if s.Kind == KindMethod {
			name := id[strings.LastIndex(id, ".")+1:]
			methodsByName[name] = append(methodsByName[name], id)
		}
	}
	for caller, callees := range g.Callees {
		for _, c := range callees {
			if !isIfaceMethodID(c) {
				continue
			}
			name := c[strings.LastIndex(c, ".")+1:]
			for _, impl := range methodsByName[name] {
				if impl == c || contains(g.Callees[caller], impl) {
					continue
				}
				g.Callees[caller] = append(g.Callees[caller], impl)
				g.Callers[impl] = append(g.Callers[impl], CallSite{
					Caller: caller, Callee: impl, ViaIface: true,
				})
			}
		}
	}
}

// ---- 符号注册 ---------------------------------------------------------

func (g *Graph) addFuncSym(p *packages.Package, fn *types.Func) {
	id := methodIDOf(fn)
	sym := &Symbol{ID: id, Pkg: pkgPathOf(fn)}
	if fn.Signature().Recv() != nil {
		sym.Kind = KindMethod
		sym.Recv = typeID(stripPtr(fn.Signature().Recv().Type()))
	} else {
		sym.Kind = KindFunc
	}
	if pos := fn.Pos(); pos.IsValid() {
		pp := g.Fset.Position(pos)
		sym.File, sym.Line = pp.Filename, pp.Line
	}
	if sym.Doc == "" {
		sym.Doc = g.docForSymbol(p, id)
	}
	g.Syms[id] = sym
}

func (g *Graph) addConstSym(p *packages.Package, c *types.Const) {
	id := constID(c)
	sym := &Symbol{ID: id, Kind: KindConst, Pkg: p.PkgPath}
	if pos := c.Pos(); pos.IsValid() {
		pp := g.Fset.Position(pos)
		sym.File, sym.Line = pp.Filename, pp.Line
	}
	if c.Val() != nil {
		g.constValue[id] = c.Val().ExactString()
	}
	if t := c.Type(); t != nil {
		g.constTypeID[id] = typeID(t)
	}
	g.Syms[id] = sym
}

func (g *Graph) addTypeSym(p *packages.Package, tn *types.TypeName) {
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return
	}
	ti := &TypeInfo{ID: typeID(tn.Type()), Pkg: p.PkgPath}
	g.namedOf[ti.ID] = named
	if pos := tn.Pos(); pos.IsValid() {
		pp := g.Fset.Position(pos)
		ti.File, ti.Line = pp.Filename, pp.Line
	}
	if tp := named.TypeParams(); tp != nil {
		for i := 0; i < tp.Len(); i++ {
			ti.GenericArgs = append(ti.GenericArgs, tp.At(i).Obj().Name())
		}
	}
	g.fillTypeInfo(ti, named.Underlying())
	g.Types[ti.ID] = ti
	g.Syms[ti.ID] = &Symbol{
		ID: ti.ID, Kind: KindType, Pkg: p.PkgPath, File: ti.File, Line: ti.Line,
		Doc: g.docForSymbol(p, "TYPE:"+ti.ID),
	}
	g.attachEnums(ti)
}

func (g *Graph) fillTypeInfo(ti *TypeInfo, u types.Type) {
	switch ut := u.(type) {
	case *types.Struct:
		ti.IsStruct = true
		for i := 0; i < ut.NumFields(); i++ {
			ti.Fields = append(ti.Fields, fieldOf(ut.Field(i), ut.Tag(i)))
		}
	case *types.Basic:
		ti.Underlying = ut.String()
	default:
		ti.Underlying = u.String()
	}
	if isTimeType(ti.ID) {
		ti.IsTime = true
	}
	if isRawMsgID(ti.ID) {
		ti.IsRawMsg = true
	}
}

func fieldOf(f *types.Var, tag string) Field {
	fl := Field{
		Name: f.Name(), Tag: tag,
		JSONName: jsonNameOf(f.Name(), tag),
		Required: !hasOmitempty(tag),
		Nullable: isPointer(f.Type()),
		TypeStr:  f.Type().String(),
		TypeID:   typeID(f.Type()),
		Kind:     kindOf(f.Type()),
	}
	return fl
}

// attachEnums 枚举判定: 同类型 const ≥ 2（设计文档 §7.2.12 宁缺勿滥）。
func (g *Graph) attachEnums(ti *TypeInfo) {
	var vals []ConstValue
	for id, v := range g.constValue {
		// 同包且类型 ID 匹配（const 声明的类型 = 该命名类型）
		if strings.HasPrefix(id, ti.Pkg+".") {
			if g.constTypeID[id] == ti.ID {
				vals = append(vals, ConstValue{
					ID: id, Name: id[strings.LastIndex(id, ".")+1:], Value: v,
				})
			}
		}
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i].Name < vals[j].Name })
	ti.Consts = vals
	ti.Enumish = len(vals) >= 2
}

// ---- 文档注释 ---------------------------------------------------------

func (g *Graph) docForSymbol(p *packages.Package, key string) string {
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Doc == nil {
					continue
				}
				if g.funcDeclIDQuiet(p, decl) == key && key != "" {
					return firstParagraph(decl.Doc.Text())
				}
			case *ast.GenDecl:
				if decl.Doc == nil || len(decl.Specs) == 0 {
					continue
				}
				if ts, ok := decl.Specs[0].(*ast.TypeSpec); ok && key == "TYPE:"+p.PkgPath+"."+ts.Name.Name {
					return firstParagraph(decl.Doc.Text())
				}
			}
		}
	}
	return ""
}

func (g *Graph) docForFuncDecl(p *packages.Package, fd *ast.FuncDecl, info *types.Info) string {
	if fd.Doc == nil {
		return ""
	}
	return firstParagraph(fd.Doc.Text())
}

func (g *Graph) funcDeclIDQuiet(p *packages.Package, fd *ast.FuncDecl) string {
	id := p.PkgPath + "." + fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) > 0 && p.TypesInfo != nil {
		recvT := p.TypesInfo.TypeOf(fd.Recv.List[0].Type)
		if recvT != nil {
			id = fmt.Sprintf("%s.(*%s).%s", p.PkgPath, typeID(stripPtr(recvT)), fd.Name.Name)
		}
	}
	return id
}

// ---- 查询 API ---------------------------------------------------------

// ForwardReach 正向可达闭包（程序切片的前向半，设计文档 §6.1 步骤 5）。
func (g *Graph) ForwardReach(from string, maxDepth int) map[string]int {
	seen := map[string]int{from: 0}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		depth := seen[cur]
		if depth >= maxDepth {
			continue
		}
		for _, next := range g.Callees[cur] {
			if _, ok := seen[next]; !ok {
				seen[next] = depth + 1
				queue = append(queue, next)
			}
		}
	}
	return seen
}

// CallersOf 查询反向调用点。
func (g *Graph) CallersOf(id string) []CallSite { return g.Callers[id] }

// CalleesOf 查询直接被调集（集合型查询，设计文档 §5.4）。
func (g *Graph) CalleesOf(id string) []string { return g.Callees[id] }

// ConstValueOf 常量字面值。
func (g *Graph) ConstValueOf(id string) (string, bool) { v, ok := g.constValue[id]; return v, ok }

// Sym 符号查询。
func (g *Graph) Sym(id string) *Symbol { return g.Syms[id] }

// FuncDeclOf 按 symbol ID 查找函数声明（handler 分析入口）。
// 接口方法没有 FuncDecl，返回 nil——调用方可据此区分接口方法与具体实现。
func (g *Graph) FuncDeclOf(symbolID string) *ast.FuncDecl { return g.funcDecls[symbolID] }

// TypeInfoOfFunc 按 symbol ID 返回所在包的类型信息。
func (g *Graph) TypeInfoOfFunc(symbolID string) *types.Info {
	if p := g.funcDeclPkg[symbolID]; p != nil {
		return p.TypesInfo
	}
	return nil
}

// Type 类型查询。
func (g *Graph) Type(id string) *TypeInfo { return g.Types[id] }

// FileHashOf 文件指纹（懒计算）。
func (g *Graph) FileHashOf(path string) string {
	if h, ok := g.fileHash[path]; ok {
		return h
	}
	h := sha256File(path)
	g.fileHash[path] = h
	return h
}

// Pkgs 返回已加载的全部包（按加载序）。
func (g *Graph) Pkgs() []*packages.Package { return g.pkgs }

// ConstTypeIDOf 返回 const 声明类型的 ID（枚举归属判定用）。
func (g *Graph) ConstTypeIDOf(id string) string { return g.constTypeID[id] }

// ConcreteImplsOf 返回接口方法的全部具体实现符号 ID。
//
// 语义：ifaceMethod 形如 "pkg.(*pkg.Iface).Method"（接口方法 ID）。
// 用 go/types 的 types.Implements 精确判定：遍历 namedOf 中全部命名类型，
// 仅当 concrete 或 *concrete 满足接口类型时才视为实现（不按方法名近似）。
// 返回已排序的符号 ID 列表，保证确定性。
func (g *Graph) ConcreteImplsOf(ifaceMethod string) []string {
	name := methodNameOf(ifaceMethod)
	recvID := recvTypeIDOf(ifaceMethod)
	if name == "" || recvID == "" {
		return nil
	}
	ifaceNamed, ok := g.namedOf[recvID]
	if !ok {
		return nil
	}
	iface, ok := ifaceNamed.Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	var out []string
	for id, named := range g.namedOf {
		if id == recvID {
			continue
		}
		// 跳过其它接口类型（Underlying 为 interface 的命名类型不是具体实现）。
		if _, isIface := named.Underlying().(*types.Interface); isIface {
			continue
		}
		if !types.Implements(named, iface) && !types.Implements(types.NewPointer(named), iface) {
			continue
		}
		implID := named.Obj().Pkg().Path() + ".(*" + id + ")." + name
		out = append(out, implID)
	}
	sort.Strings(out)
	return out
}

// recvTypeIDOf 从方法符号 ID 提取接收者类型 ID。
// "pkg.(*pkg.Recv).Method" → "pkg.Recv"。
func recvTypeIDOf(methodID string) string {
	i := strings.Index(methodID, ".(*")
	if i < 0 {
		return ""
	}
	rest := methodID[i+3:] // "pkg.Recv).Method"
	j := strings.Index(rest, ").")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// methodNameOf 从符号 ID 提取方法名（最后一段）。
func methodNameOf(id string) string {
	i := strings.LastIndex(id, ".")
	if i < 0 {
		return ""
	}
	return id[i+1:]
}

// ---- 辅助函数 ---------------------------------------------------------

func methodIDOf(fn *types.Func) string {
	sig := fn.Signature()
	if sig.Recv() != nil {
		recvID := typeID(stripPtr(sig.Recv().Type()))
		return fmt.Sprintf("%s.(*%s).%s", pkgPathOf(fn), recvID, fn.Name())
	}
	return fn.FullName()
}

func pkgPathOf(fn *types.Func) string {
	if fn.Pkg() != nil {
		return fn.Pkg().Path()
	}
	full := fn.FullName()
	if i := strings.LastIndex(full, "."); i > 0 {
		return full[:i]
	}
	return ""
}

func typeID(t types.Type) string {
	if n, ok := t.(*types.Named); ok {
		if n.Obj().Pkg() != nil {
			return n.Obj().Pkg().Path() + "." + n.Obj().Name()
		}
		return n.Obj().Name()
	}
	if p, ok := t.(*types.Pointer); ok {
		return typeID(p.Elem())
	}
	return t.String()
}

func stripPtr(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

func kindOf(t types.Type) string {
	// time.Time / json.RawMessage 优先判定（命名类型，Underlying 是 struct）
	if isTimeType(typeID(t)) {
		return "time"
	}
	if isRawMsgID(typeID(t)) {
		return "rawmsg"
	}
	u := t.Underlying()
	switch ut := u.(type) {
	case *types.Basic:
		return "basic"
	case *types.Slice:
		if b, ok := ut.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Uint8 {
			return "basic" // []byte
		}
		return "slice"
	case *types.Array:
		return "slice"
	case *types.Map:
		return "map"
	case *types.Struct:
		return "struct"
	case *types.Interface:
		if ut.Empty() {
			return "any"
		}
		return "iface"
	case *types.Pointer:
		return kindOf(ut.Elem())
	}
	return "other"
}

func isPointer(t types.Type) bool {
	_, ok := t.(*types.Pointer)
	return ok
}

func isTimeType(id string) bool { return strings.HasSuffix(id, "/time.Time") || id == "time.Time" }
func isRawMsgID(id string) bool {
	return strings.HasSuffix(id, "/encoding/json.RawMessage") || id == "encoding/json.RawMessage"
}

func isIfaceMethodID(id string) bool {
	// 接口方法形如 pkg.Iface.Method；具体方法形如 pkg.(*Recv).Method
	return strings.Contains(id, ".") && !strings.Contains(id, "(*") && strings.Count(id, ".") == 2
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func firstParagraph(s string) string {
	if i := strings.Index(s, "\n\n"); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func sha256File(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "missing"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// addLocalTypeSym 注册函数内局部命名类型（ID = pkg.TypeName，冲突加行号）。
func (g *Graph) addLocalTypeSym(p *packages.Package, ts *ast.TypeSpec) {
	info := p.TypesInfo
	obj := info.Defs[ts.Name]
	if obj == nil {
		return
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return
	}
	id := typeID(named)
	if g.Types[id] != nil {
		return // 同名局部类型: 保守跳过（避免误合并）
	}
	ti := &TypeInfo{ID: id, Pkg: p.PkgPath}
	if pos := tn.Pos(); pos.IsValid() {
		pp := g.Fset.Position(pos)
		ti.File, ti.Line = pp.Filename, pp.Line
	}
	g.fillTypeInfo(ti, named.Underlying())
	g.Types[ti.ID] = ti
	g.Syms[ti.ID] = &Symbol{ID: ti.ID, Kind: KindType, Pkg: p.PkgPath, File: ti.File, Line: ti.Line}
}
