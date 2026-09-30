// Package codegraph 构建代码图：符号表、调用边、类型图（设计文档 §3.1 存储层 CG）。
//
// 全部产物带指纹（FileHash / SymbolHash），调用边解析使用 go/types 的
// 精确类型信息（等价于设计文档的 LSP 桥接，P0 用 go/packages 一次完成）。
package codegraph

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

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
	Required bool       // 必填：有 validate/binding 约束时以其 required 为准，否则无 omitempty 即必填（设计文档 §7.2.6）
	Nullable bool       // 指针字段
	Embedded bool       // 匿名嵌入字段（无显式 json 名时 JSON 编码展平到外层）
	Doc      string     // 字段 doc/行尾注释（单行化），作为 schema description 来源
	Type     types.Type `yaml:"-" json:"-"` // 字段的 go/types 类型（类型驱动合成：切片元素/匿名结构体），不参与序列化
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
	// fieldDoc 记录「字段标识符位置 → 注释文本」，types.Var 不携带注释，需从 AST 预先索引。
	fieldDoc map[token.Pos]string
	// ifaceCallees 经接口分发调用的接口方法 ID 集合（expandInterfaceDispatch 的展开对象）。
	ifaceCallees map[string]bool
	// implMu 保护 implCache（LLM 兜底并发阶段工具闭包可能并发查询）。
	implMu sync.Mutex
	// implCache 接口方法 ID → 具体实现 ID 列表的记忆表（types.Implements 遍历全部命名类型，代价高）。
	implCache map[string][]string
	// fileMu 保护 fileHash / fileLines（LLM 兜底并发阶段工具闭包会并发读源码）。
	fileMu sync.Mutex
	// fileLines 文件路径 → 按行切分的源码（证据引用与源码切片的读缓存）。
	fileLines map[string][]string
	// root 仓库根绝对路径（证据相对化用；空 = 不相对化）。
	root string
	// docIndex 包路径 → 「符号键 → doc 首段」索引（buildDocIndex 惰性构建）。
	docIndex map[string]map[string]string
	// methodOwners 方法名 → 拥有该方法（值或指针方法集）的非接口命名类型 ID（有序；实现查找的候选预筛）。
	methodOwners map[string][]string
}

// NewGraph 创建空图。
func NewGraph() *Graph {
	return &Graph{
		Syms:         map[string]*Symbol{},
		Types:        map[string]*TypeInfo{},
		Callees:      map[string][]string{},
		Callers:      map[string][]CallSite{},
		constValue:   map[string]string{},
		constTypeID:  map[string]string{},
		fileHash:     map[string]string{},
		pkgOf:        map[string]*packages.Package{},
		funcDecls:    map[string]*ast.FuncDecl{},
		funcDeclPkg:  map[string]*packages.Package{},
		namedOf:      map[string]*types.Named{},
		fieldDoc:     map[token.Pos]string{},
		ifaceCallees: map[string]bool{},
		implCache:    map[string][]string{},
		fileLines:    map[string][]string{},
	}
}

// Build 遍历全部包构建代码图（含非导出符号——响应链在 service 深处）。
func Build(pkgs []*packages.Package, fset *token.FileSet) (*Graph, error) {
	g := NewGraph()
	g.Fset = fset
	// 字段注释先全量索引：类型注册会跨包递归（A 包引用 B 包类型时 B 可能尚未处理）
	for _, p := range pkgs {
		g.indexFieldDocs(p)
	}
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
		if viaIface {
			g.ifaceCallees[calleeID] = true
		}
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
				if r := fnObj.Signature().Recv(); r != nil && types.IsInterface(r.Type()) {
					// 结构体嵌入接口后提升的方法：实际是接口分派，按接口方法解析到具体实现。
					return fmt.Sprintf("%s.%s", typeID(stripPtr(r.Type())), fnObj.Name()), true
				}
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
	case *ast.IndexExpr: // 泛型单类型实参实例化：F[T](...) / pkg.F[T](...)
		return resolveGenericCallee(f.X, info)
	case *ast.IndexListExpr: // 泛型多类型实参实例化：F[K, V](...)
		return resolveGenericCallee(f.X, info)
	}
	return "", false
}

// resolveGenericCallee 解析泛型实例化调用的被调函数（去掉类型实参后的 F / pkg.F）。
func resolveGenericCallee(x ast.Expr, info *types.Info) (string, bool) {
	var id *ast.Ident
	switch e := ast.Unparen(x).(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	}
	if id == nil {
		return "", false
	}
	if fnObj, ok := info.Uses[id].(*types.Func); ok {
		return fnObj.Origin().FullName(), false
	}
	return "", false
}

func (g *Graph) addCall(caller, callee string, site CallSite) {
	if !contains(g.Callees[caller], callee) {
		g.Callees[caller] = append(g.Callees[caller], callee)
	}
	g.Callers[callee] = append(g.Callers[callee], site)
}

// expandInterfaceDispatch 把接口方法调用边展开到该接口的全部具体实现。
//
// 实现集由 types.Implements 精确判定（ConcreteImplsOf），不按方法名近似：
// 按名近似会把 `srv.Holdings` 连到仓库里所有叫 Holdings 的方法（含其它 controller），
// 污染响应汇聚点、错误码与值流。接口不在代码图内（外部接口）时不展开。
// 按调用方排序遍历，保证 Callers 追加顺序确定。
func (g *Graph) expandInterfaceDispatch() {
	callers := make([]string, 0, len(g.Callees))
	for caller := range g.Callees {
		callers = append(callers, caller)
	}
	sort.Strings(callers)
	for _, caller := range callers {
		for _, c := range append([]string(nil), g.Callees[caller]...) {
			if !g.ifaceCallees[c] {
				continue
			}
			for _, impl := range g.ConcreteImplsOf(c) {
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
			fv := ut.Field(i)
			ti.Fields = append(ti.Fields, fieldOf(fv, ut.Tag(i), g.fieldDoc[fv.Pos()]))
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

// fieldOf 结构体字段 → 规范化视图；doc 为 AST 预索引的字段注释。
func fieldOf(f *types.Var, tag, doc string) Field {
	fl := Field{
		Name: f.Name(), Tag: tag,
		JSONName: jsonNameOf(f.Name(), tag),
		Required: requiredOf(tag),
		Nullable: isPointer(f.Type()),
		Embedded: f.Embedded(),
		Doc:      doc,
		Type:     f.Type(),
		TypeStr:  f.Type().String(),
		TypeID:   typeID(f.Type()),
		Kind:     kindOf(f.Type()),
	}
	return fl
}

// requiredOf 字段必填判定（JSON 体字段）：有校验约束时以约束为准，
// 无约束时回落序列化语义（无 omitempty 即总会出现）。
func requiredOf(tag string) bool {
	if req, ok := ConstraintRequired(tag); ok {
		return req
	}
	return !hasOmitempty(tag)
}

// ConstraintRequired 按 validator 语义从校验约束（validate/binding）判定必填：
// 显式 required，或无 omitempty 且存在拒绝零值的下界规则（如 min=1：缺省零值必然校验失败）。
// 第二返回值表示是否存在约束 tag；查询/路径等非 JSON 参数无约束即为可选。
func ConstraintRequired(tag string) (required, hasConstraint bool) {
	for _, key := range constraintTagKeys {
		v, ok := reflect.StructTag(tag).Lookup(key)
		if !ok {
			continue
		}
		rejectsZero, omitEmpty := false, false
		for _, rule := range strings.Split(v, ",") {
			name, arg, _ := strings.Cut(strings.TrimSpace(rule), "=")
			switch name {
			case ruleRequired:
				return true, true
			case ruleOmitEmpty:
				omitEmpty = true
			case "min", "gte", "gt", "len":
				if n, err := strconv.ParseFloat(arg, 64); err == nil && (n > 0 || name == "gt") {
					rejectsZero = true
				}
			}
		}
		return rejectsZero && !omitEmpty, true
	}
	return false, false
}

// constraintTagKeys 校验约束 tag 键（gin 用 binding，go-playground/validator 默认 validate）。
var constraintTagKeys = []string{"binding", "validate"}

// ruleRequired 校验约束中表示必填的规则名。
const ruleRequired = "required"

// ruleOmitEmpty 校验约束中表示「零值跳过后续校验」的规则名（字段可缺省）。
const ruleOmitEmpty = "omitempty"

// indexFieldDocs 索引包内全部结构体字段注释（含嵌套匿名结构体），键为字段标识符位置。
// 嵌入字段无标识符，以其类型表达式位置为键（与 types.Var.Pos() 一致）。
func (g *Graph) indexFieldDocs(p *packages.Package) {
	for _, f := range p.Syntax {
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, fld := range st.Fields.List {
				doc := commentText(fld.Doc)
				if doc == "" {
					doc = commentText(fld.Comment)
				}
				if doc == "" {
					continue
				}
				if len(fld.Names) == 0 {
					g.fieldDoc[embeddedPos(fld.Type)] = doc
					continue
				}
				for _, name := range fld.Names {
					g.fieldDoc[name.Pos()] = doc
				}
			}
			return true
		})
	}
}

// embeddedPos 嵌入字段的类型名标识符位置（*T / pkg.T / pkg.*T 取末段标识符）。
func embeddedPos(e ast.Expr) token.Pos {
	switch t := e.(type) {
	case *ast.StarExpr:
		return embeddedPos(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Pos()
	case *ast.IndexExpr:
		return embeddedPos(t.X)
	case *ast.IndexListExpr:
		return embeddedPos(t.X)
	}
	return e.Pos()
}

// commentText 注释组 → 单行文本（多行以空格拼接，便于作为 description）。
func commentText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.Join(strings.Fields(cg.Text()), " ")
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
	if key == "" {
		return ""
	}
	if g.docIndex == nil {
		g.docIndex = map[string]map[string]string{}
	}
	idx, ok := g.docIndex[p.PkgPath]
	if !ok {
		idx = g.buildDocIndex(p)
		g.docIndex[p.PkgPath] = idx
	}
	return idx[key]
}

// buildDocIndex 一次扫描包内全部声明，建「符号键 → doc 首段」索引（避免逐符号全包扫描的平方开销）。
func (g *Graph) buildDocIndex(p *packages.Package) map[string]string {
	idx := map[string]string{}
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Doc != nil {
					if key := g.funcDeclIDQuiet(p, decl); key != "" {
						if _, dup := idx[key]; !dup {
							idx[key] = firstParagraph(decl.Doc.Text())
						}
					}
				}
			case *ast.GenDecl:
				if decl.Doc == nil || len(decl.Specs) == 0 {
					continue
				}
				if ts, ok := decl.Specs[0].(*ast.TypeSpec); ok {
					key := "TYPE:" + p.PkgPath + "." + ts.Name.Name
					if _, dup := idx[key]; !dup {
						idx[key] = firstParagraph(decl.Doc.Text())
					}
				}
			}
		}
	}
	return idx
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
	g.implMu.Lock()
	defer g.implMu.Unlock()
	if impls, ok := g.implCache[ifaceMethod]; ok {
		return impls
	}
	impls := g.concreteImplsOf(ifaceMethod)
	g.implCache[ifaceMethod] = impls
	return impls
}

// concreteImplsOf ConcreteImplsOf 的无记忆实现。
func (g *Graph) concreteImplsOf(ifaceMethod string) []string {
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
	var real, doubles []string
	seen := map[string]bool{}
	// 只检查方法集里有同名方法的具体类型（预筛），再用 types.Implements 精确判定。
	for _, id := range g.ownersOf(name) {
		named := g.namedOf[id]
		if id == recvID {
			continue
		}
		if !types.Implements(named, iface) && !types.Implements(types.NewPointer(named), iface) {
			continue
		}
		implID, ok := declaredMethodID(named, id, name, ifaceNamed.Obj().Pkg())
		if !ok {
			continue // 经嵌入接口转发：只是再次分派，真实实现已由其它具体类型覆盖
		}
		if seen[implID] {
			continue // 多个类型嵌入同一实现：提升方法归并到其声明处
		}
		seen[implID] = true
		if isTestDouble(named) {
			doubles = append(doubles, implID)
		} else {
			real = append(real, implID)
		}
	}
	// 剔除测试替身实现（mock/fake）：只要还有真实实现就不让替身进入调用图，
	// 否则 mock 的「返回 mock.Arguments.Get(0)」会把值流污染成不可反推。全是替身时原样保留。
	out := real
	if len(out) == 0 {
		out = doubles
	}
	sort.Strings(out)
	return out
}

// declaredMethodID 具体类型方法集中名为 name 的方法的声明处符号 ID：经嵌入提升的方法归到被嵌入类型
// 的声明（有函数体可回溯），而非不存在声明的「外层类型.方法」。方法经嵌入的接口字段提升时返回 false。
// id 为 named 的类型 ID，pkg 用于未导出方法名的查找。
func declaredMethodID(named *types.Named, id, name string, pkg *types.Package) (string, bool) {
	fallback := named.Obj().Pkg().Path() + ".(*" + id + ")." + name
	sel := types.NewMethodSet(types.NewPointer(named)).Lookup(pkg, name)
	if sel == nil {
		return fallback, true
	}
	fn, ok := sel.Obj().(*types.Func)
	if !ok || fn.Signature().Recv() == nil {
		return fallback, true
	}
	if types.IsInterface(fn.Signature().Recv().Type()) {
		return "", false
	}
	return FuncID(fn), true
}

// ownersOf 方法集中含 name 方法的非接口命名类型 ID（有序）；索引首次调用时构建（调用方持有 implMu）。
func (g *Graph) ownersOf(name string) []string {
	if g.methodOwners == nil {
		g.methodOwners = map[string][]string{}
		for id, named := range g.namedOf {
			if _, isIface := named.Underlying().(*types.Interface); isIface {
				continue
			}
			ms := types.NewMethodSet(types.NewPointer(named)) // 指针方法集 ⊇ 值方法集（含嵌入提升）
			for i := 0; i < ms.Len(); i++ {
				m := ms.At(i).Obj().Name()
				g.methodOwners[m] = append(g.methodOwners[m], id)
			}
		}
		for _, ids := range g.methodOwners {
			sort.Strings(ids)
		}
	}
	return g.methodOwners[name]
}

// testDoublePkgSegs 测试替身惯用包名末段（mockery/gomock 生成代码目录）。
var testDoublePkgSegs = map[string]bool{"mocks": true, "mock": true, "fakes": true, "fake": true}

// testDoubleFieldPkgs 测试替身结构体惯用嵌入/持有的框架包（testify mock、gomock）。
var testDoubleFieldPkgs = map[string]bool{
	"github.com/stretchr/testify/mock": true,
	"github.com/golang/mock/gomock":    true,
	"go.uber.org/mock/gomock":          true,
}

// isTestDouble 命名类型是否为测试替身：位于 mocks/fakes 包，或字段来自 testify mock / gomock。
func isTestDouble(named *types.Named) bool {
	if named == nil || named.Obj().Pkg() == nil {
		return false
	}
	path := named.Obj().Pkg().Path()
	if testDoublePkgSegs[path[strings.LastIndex(path, "/")+1:]] {
		return true
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := 0; i < st.NumFields(); i++ {
		ft := stripPtr(st.Field(i).Type())
		if n, ok := ft.(*types.Named); ok && n.Obj().Pkg() != nil && testDoubleFieldPkgs[n.Obj().Pkg().Path()] {
			return true
		}
	}
	return false
}

// StructFields 匿名（内联）结构体的规范化字段视图，口径与命名结构体的 TypeInfo.Fields 一致。
func (g *Graph) StructFields(st *types.Struct) []Field {
	out := make([]Field, 0, st.NumFields())
	for i := 0; i < st.NumFields(); i++ {
		fv := st.Field(i)
		out = append(out, fieldOf(fv, st.Tag(i), g.fieldDoc[fv.Pos()]))
	}
	return out
}

// TypeIDOf 类型的规范 ID（命名类型为 包路径.名字，其余为类型串），与 Field.TypeID 同口径。
func TypeIDOf(t types.Type) string { return typeID(t) }

// KindOf 类型的归类（basic|slice|map|struct|any|iface|time|rawmsg|other），与 Field.Kind 同口径。
func KindOf(t types.Type) string { return kindOf(t) }

// recvTypeIDOf 从方法符号 ID 提取接收者类型 ID。
// "pkg.(*pkg.Recv).Method" → "pkg.Recv"；接口方法调用 ID "pkg.Iface.Method" → "pkg.Iface"。
func recvTypeIDOf(methodID string) string {
	i := strings.Index(methodID, ".(*")
	if i < 0 {
		if k := strings.LastIndex(methodID, "."); k > 0 {
			return methodID[:k]
		}
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

// MethodID 方法符号 ID：pkg.(*pkg.Recv).Method（值/指针接收者同口径）。框架原语表按此格式声明符号。
func MethodID(pkg, recv, method string) string {
	return fmt.Sprintf("%s.(*%s.%s).%s", pkg, pkg, recv, method)
}

// FuncID 函数/方法对象的符号 ID（与调用边、原语表的口径一致）。
func FuncID(fn *types.Func) string { return methodIDOf(fn) }

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

// FuncIDs 全部带函数体的符号 ID（排序，保证基于它的分析确定性）。
func (g *Graph) FuncIDs() []string {
	ids := make([]string, 0, len(g.funcDecls))
	for id := range g.funcDecls {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ResolveCallee 调用表达式 → 被调符号 ID；第二返回值为是否经接口分发（需再解析到具体实现）。
func ResolveCallee(call *ast.CallExpr, info *types.Info) (string, bool) {
	return resolveCallee(call, info)
}
