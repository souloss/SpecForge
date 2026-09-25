package engine

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/profile"
	"github.com/specforge/specforge/internal/slicing"
	"github.com/specforge/specforge/internal/typeschema"
)

// ContractPayloadBuilder 单 operation 的契约构造（档位 1 纯静态路径）。
type ContractPayloadBuilder struct {
	g       *codegraph.Graph
	prof    *profile.Profile
	synth   *typeschema.Synthesizer
	route   adapter.Route
	slicer  *slicing.Slicer
	catalog map[string]slicing.ErrorCodeEntry
	opKey   string
	evFile  string
	blob    string
}

// Build 产出 contract 事实 + 关联 schema/security 事实。
func (b *ContractPayloadBuilder) Build() (*facts.Fact, []*facts.Fact, []*facts.Fact) {
	cp := facts.ContractPayload{OperationID: operationIDOf(b.route)}
	conf := 1.0
	var unknowns []string

	// ---- 中间件 → security / header 参数（F7，profile 驱动） ----
	var securityNames []string
	var extra []*facts.Fact
	seenHeaders := map[string]bool{}
	for _, mw := range b.route.Middleware {
		mapping, ok := b.prof.AuthMiddleware[middlewareKey(mw)]
		if !ok {
			continue
		}
		if mapping.Header != "" && !seenHeaders[mapping.Header] {
			seenHeaders[mapping.Header] = true
			cp.Params = append(cp.Params, facts.ParamFact{
				In: "header", Name: mapping.Header, Required: mapping.Required,
				Type:   "string",
				Origin: "middleware:" + lastSeg(mw),
			})
		}
		if mapping.Scheme != "" {
			name := schemeNameOf(mapping)
			if !containsStr(securityNames, name) {
				securityNames = append(securityNames, name)
			}
			if b.routedScheme(name) == nil {
				extra = append(extra, &facts.Fact{
					ID: "security:" + name, Kind: facts.KindSecurity,
					Value: facts.SecurityPayload{
						MiddlewareSymbol: mw, SchemeType: mapping.Scheme,
						Header: mapping.Header, Required: mapping.Required,
						ScopeArg: scopeArgOf(b.route, mw),
					},
					Source: facts.SourceStatic, Confidence: 0.85,
					Evidence: []facts.Evidence{b.routeEvidence(mw)},
					Status:   "verified",
				})
			}
		}
	}
	cp.Security = securityNames
	cp.Tags = tagsOf(b.route)

	// ---- handler 函数体: 请求绑定 + 显式参数 ----
	// handler 可能是接口方法（真实仓库 handler 常经接口分发到 service 实现），
	// 此时 FuncDeclOf 返回 nil，需解析到具体实现再扫描其函数体。
	handlerBodies := map[string]*ast.FuncDecl{}
	if fd := b.g.FuncDeclOf(b.route.Handler); fd != nil {
		handlerBodies[b.route.Handler] = fd
	} else {
		for _, impl := range b.g.ConcreteImplsOf(b.route.Handler) {
			if fd := b.g.FuncDeclOf(impl); fd != nil {
				handlerBodies[impl] = fd
			}
		}
	}
	if len(handlerBodies) > 0 {
		for symID, fd := range handlerBodies {
			b.scanHandlerBody(fd, symID, &cp)
		}
	} else {
		unknowns = append(unknowns, "handler body not found")
		conf = 0.6
	}

	// 请求体 schema 合成（F4）
	var reqSchema *facts.Fact
	if cp.RequestBody != nil && cp.RequestBody.SchemaType != "" {
		reqSchema = b.schemaFact(cp.RequestBody.SchemaType)
	}

	// ---- 响应矩阵（调用图追踪，F5） ----
	hits := b.slicer.Trace(b.route.Handler)
	rows, schemaFacts, respConf, respUnknowns := b.responseMatrix(hits)
	cp.Responses = rows
	unknowns = append(unknowns, respUnknowns...)
	conf = minConf(conf, respConf)

	if len(cp.Responses) == 0 {
		unknowns = append(unknowns, "no response sink reached in call graph")
		conf = 0.4
	}

	cfact := &facts.Fact{
		ID: "contract:" + b.opKey, Kind: facts.KindContract, Value: cp,
		Source: facts.SourceStatic, Confidence: conf,
		Evidence: []facts.Evidence{b.routeEvidence("")},
		Status:   "verified",
	}

	if reqSchema != nil {
		schemaFacts = append([]*facts.Fact{reqSchema}, schemaFacts...)
	}
	// schema 置信度向 contract 传播（含 unknown 的 schema 拉低整体置信度）
	for _, sf := range schemaFacts {
		if sf.Confidence < conf {
			conf = sf.Confidence
		}
	}
	cfact.Confidence = conf
	return cfact, schemaFacts, extra
}

// scanHandlerBody 扫描 handler 函数体: BodyParser 绑定 + Query/Params/Get/Cookies。
//
// symID 是函数体所属符号的 ID（handler 自身或其具体实现），用于定位类型信息。
// 与旧实现的关键差异：BodyParser 的实参往往是上一行 `r := new(mapping.T)` 的
// 局部变量（*ast.Ident），此时 info.Types[ident] 因 go/packages 的 TypesInfo
// 可能缺失而返回空——改为用 info.TypeOf(ident) 兜底，并进一步沿局部声明
// `r := new(T)` 回溯类型。
func (b *ContractPayloadBuilder) scanHandlerBody(fd *ast.FuncDecl, symID string, cp *facts.ContractPayload) {
	info := b.g.TypeInfoOfFunc(symID)
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "BodyParser", "BodyParserWithLimit":
			if len(call.Args) > 0 && cp.RequestBody == nil {
				if t := bodyArgStaticType(call.Args[0], info, fd); t != "" {
					typeID := normalizeTypeString(t)
					if typeID != "" {
						cp.RequestBody = &facts.BodyFact{
							ContentType: "application/json", SchemaType: typeID,
						}
					}
				}
			}
		case "Query", "QueryInt", "QueryBool", "QueryFloat":
			if name := firstStringArg(call); name != "" {
				typ := "string"
				switch sel.Sel.Name {
				case "QueryInt":
					typ = "integer"
				case "QueryBool":
					typ = "boolean"
				case "QueryFloat":
					typ = "number"
				}
				if !paramExists(cp.Params, "query", name) {
					cp.Params = append(cp.Params, facts.ParamFact{
						In: "query", Name: name, Type: typ,
						Origin: "fiber:" + sel.Sel.Name,
					})
				}
			}
		case "Params":
			if name := firstStringArg(call); name != "" {
				if !paramExists(cp.Params, "path", name) {
					cp.Params = append(cp.Params, facts.ParamFact{
						In: "path", Name: name, Required: true, Type: "string",
						Origin: "fiber:Params",
					})
				}
			}
		case "Get", "GetReqHeaders":
			if name := firstStringArg(call); name != "" && len(call.Args) == 1 {
				if !paramExists(cp.Params, "header", name) {
					cp.Params = append(cp.Params, facts.ParamFact{
						In: "header", Name: name, Type: "string",
						Origin: "fiber:Get",
					})
				}
			}
		case "Cookies":
			if name := firstStringArg(call); name != "" {
				if !paramExists(cp.Params, "cookie", name) {
					cp.Params = append(cp.Params, facts.ParamFact{
						In: "cookie", Name: name, Type: "string",
						Origin: "fiber:Cookies",
					})
				}
			}
		}
		return true
	})
}

// responseMatrix 响应矩阵合成（成功信封 + 错误码行 + 未解析行）。
func (b *ContractPayloadBuilder) responseMatrix(hits []slicing.SinkHit) (
	[]facts.ResponseFact, []*facts.Fact, float64, []string) {

	var rows []facts.ResponseFact
	var schemaFacts []*facts.Fact
	conf := 1.0
	var unknowns []string

	envSpec := b.prof.ResponseEnvelope
	successCode := 0
	if envSpec != nil {
		successCode = envSpec.SuccessCode
	}

	seenSuccess := map[string]bool{}
	seenErrCode := map[string]bool{}
	var errSources []string
	hasUnresolvedErr := false

	for _, h := range hits {
		sinkLoc := fmt.Sprintf("%s:%d", shortFile(h.Site.File), h.Site.Line)
		switch {
		case h.Success:
			// 成功行: code=0 + data schema
			key := h.DataTypeID
			if key == "" {
				key = "no-body"
			}
			if seenSuccess[key] {
				// 合并 sink 证据到既有行
				for i := range rows {
					if rows[i].Envelope != nil && rows[i].Envelope.Code == successCode && rows[i].SchemaType == h.DataTypeID {
						rows[i].Sink = rows[i].Sink + "," + sinkLoc
					}
				}
				continue
			}
			seenSuccess[key] = true
			row := facts.ResponseFact{
				Status: h.Status, HasBody: h.HasBody,
				Envelope: &facts.Envelope{Code: successCode},
				Sink:     sinkLoc,
			}
			if h.DataTypeID != "" {
				row.SchemaType = h.DataTypeID
				if strings.HasPrefix(h.DataTypeID, "[]") {
					// 切片响应: 元素进 components，数组包装在编译期展开（§7.1）
					elem := strings.TrimPrefix(h.DataTypeID, "[]")
					if b.g.Type(elem) != nil {
						schemaFacts = append(schemaFacts, b.schemaFact(elem))
					}
				} else {
					schemaFacts = append(schemaFacts, b.schemaFact(h.DataTypeID))
				}
			}
			if h.DataUnknown {
				row.SchemaType = ""
				unknowns = append(unknowns, "success data: any/interface{} cannot be typed")
				conf = minConf(conf, 0.6)
			}
			rows = append(rows, row)
		case h.ErrConstID != "":
			// 具体错误码行（可溯源到常量）
			entry, ok := b.catalog[h.ErrConstID]
			if !ok {
				entry = slicing.ErrorCodeEntry{Symbol: h.ErrConstID, Code: -1, Msg: "unknown"}
			}
			if seenErrCode[h.ErrConstID] {
				for i := range rows {
					if rows[i].Envelope != nil && rows[i].Envelope.CodeRef == h.ErrConstID {
						rows[i].Sink = rows[i].Sink + "," + sinkLoc
					}
				}
				continue
			}
			seenErrCode[h.ErrConstID] = true
			rows = append(rows, facts.ResponseFact{
				Status: h.Status, HasBody: false,
				Envelope: &facts.Envelope{Code: entry.Code, CodeRef: h.ErrConstID, Msg: entry.Msg},
				Sink:     sinkLoc,
			})
		case h.ErrUnresolved:
			hasUnresolvedErr = true
			// 溯源成功但码不可静态定（如 errgroup.Wait）：把来源记进 unknown，供报告展示。
			if h.ErrSource != "" && !containsStr(errSources, h.ErrSource) {
				errSources = append(errSources, h.ErrSource)
			}
		}
	}
	if hasUnresolvedErr {
		msg := "error variable not statically traceable"
		if len(errSources) > 0 {
			msg += " (sources: " + strings.Join(errSources, ", ") + ")"
		}
		rows = append(rows, facts.ResponseFact{
			Status: hits[0].Status, HasBody: false,
			Envelope: &facts.Envelope{Code: -1, CodeRef: "unresolved", Msg: msg},
			Sink:     "(multiple)",
		})
		unknowns = append(unknowns, "error envelope: err variable not statically resolved")
		conf = minConf(conf, 0.75)
	}

	// 稳定排序: 成功在前，错误码升序，未解析最后
	sortRows(rows)
	return rows, schemaFacts, conf, unknowns
}

func (b *ContractPayloadBuilder) schemaFact(typeID string) *facts.Fact {
	sc := b.synth.Synthesize(typeID)
	ev := []facts.Evidence{}
	if ti := b.g.Type(typeID); ti != nil {
		ev = append(ev, facts.Evidence{
			File: ti.File, StartLine: ti.Line, EndLine: ti.Line,
			BlobSHA: b.g.FileHashOf(ti.File),
		})
	}
	return &facts.Fact{
		ID: "schema:" + typeID, Kind: facts.KindSchema, Value: sc,
		Source: facts.SourceStatic, Confidence: schemaConfidence(sc),
		Evidence: ev, Status: "verified",
	}
}

func (b *ContractPayloadBuilder) routeEvidence(mw string) facts.Evidence {
	ev := facts.Evidence{File: b.evFile, StartLine: b.route.Line, EndLine: b.route.Line, BlobSHA: b.blob}
	if mw != "" {
		if sym := b.g.Sym(mw); sym != nil {
			ev = facts.Evidence{File: sym.File, StartLine: sym.Line, EndLine: sym.Line,
				BlobSHA: b.g.FileHashOf(sym.File)}
		}
	}
	return ev
}

func (b *ContractPayloadBuilder) routedScheme(name string) *facts.Fact { return nil }

// ---- 辅助 --------------------------------------------------------------

func exprStaticType(e ast.Expr, info *types.Info) string {
	if info == nil {
		return ""
	}
	e = ast.Unparen(e)
	// new(T) → T
	if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 1 {
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "new" {
			return exprStaticType(call.Args[0], info)
		}
	}
	// &x → x
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op.String() == "&" {
		return exprStaticType(u.X, info)
	}
	if tv, ok := info.Types[e]; ok && tv.Type != nil {
		return tv.Type.String()
	}
	return ""
}

// bodyArgStaticType 求 BodyParser 实参的静态类型。
//
// 真实仓库 handler 的惯用写法是 `r := new(mapping.T); c.BodyParser(r)`，
// 实参是局部变量标识符。go/types 的 info.TypeOf(ident) 比 info.Types[ident]
// 更可靠（标识符在 Types map 里常缺失），故对 *ast.Ident 走 TypeOf 兜底；
// 若仍为空，再沿函数体回溯 `ident := new(T)` / `ident := &T{}` 声明。
func bodyArgStaticType(arg ast.Expr, info *types.Info, fd *ast.FuncDecl) string {
	if info == nil {
		return ""
	}
	arg = ast.Unparen(arg)
	switch e := arg.(type) {
	case *ast.Ident:
		if t := info.TypeOf(e); t != nil {
			return t.String()
		}
		// 兜底：回溯函数体里的局部声明。
		if fd != nil && fd.Body != nil {
			if t := findLocalDeclType(fd.Body, e.Name); t != "" {
				return t
			}
		}
		return ""
	case *ast.UnaryExpr:
		if e.Op.String() == "&" {
			return bodyArgStaticType(e.X, info, fd)
		}
	case *ast.CallExpr:
		// new(T)
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "new" && len(e.Args) == 1 {
			return bodyArgStaticType(e.Args[0], info, fd)
		}
	}
	if t := info.TypeOf(arg); t != nil {
		return t.String()
	}
	return ""
}

// findLocalDeclType 在函数体中查找 `name := new(T)` / `name := &T{}` / `name := T{}`
// 的局部声明，返回 T 的静态类型串。找不到返回空。
func findLocalDeclType(body *ast.BlockStmt, name string) string {
	var found string
	ast.Inspect(body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name || i >= len(as.Rhs) {
				continue
			}
			rhs := as.Rhs[i]
			// new(T)
			if call, ok := ast.Unparen(rhs).(*ast.CallExpr); ok && len(call.Args) == 1 {
				if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "new" {
					if tv, ok := typeOfTypeExpr(call.Args[0], nil); ok {
						found = tv
						return false
					}
				}
			}
			// &T{} / T{}
			expr := rhs
			if u, ok := rhs.(*ast.UnaryExpr); ok && u.Op.String() == "&" {
				expr = u.X
			}
			if cl, ok := expr.(*ast.CompositeLit); ok {
				if tv, ok := typeOfTypeExpr(cl.Type, nil); ok {
					found = tv
					return false
				}
			}
		}
		return true
	})
	return found
}

// typeOfTypeExpr 求一个「类型表达式」的静态类型串（无 *types.Info 时退化为
// 语法层解析：Ident → 名称，SelectorExpr → pkg.Name）。返回 (类型串, 是否成功)。
func typeOfTypeExpr(t ast.Expr, _ *types.Info) (string, bool) {
	switch e := ast.Unparen(t).(type) {
	case *ast.Ident:
		return e.Name, true
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok {
			return x.Name + "." + e.Sel.Name, true
		}
	}
	return "", false
}

func normalizeTypeString(t string) string {
	t = strings.TrimPrefix(t, "*")
	if i := strings.Index(t, "["); i > 0 && strings.HasSuffix(t, "]") {
		t = t[:i]
	}
	if strings.ContainsAny(t, " ()[]") || t == "" || isBasicType(t) {
		return ""
	}
	return t
}

func isBasicType(t string) bool {
	switch t {
	case "string", "int", "int64", "int32", "float64", "float32", "bool", "byte", "any", "interface{}":
		return true
	}
	return false
}

func firstStringArg(call *ast.CallExpr) string {
	if len(call.Args) == 0 {
		return ""
	}
	if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, err := strconv.Unquote(lit.Value)
		if err == nil {
			return s
		}
	}
	return ""
}

func paramExists(ps []facts.ParamFact, in, name string) bool {
	for _, p := range ps {
		if p.In == in && p.Name == name {
			return true
		}
	}
	return false
}

func middlewareKey(mwSymbol string) string {
	return lastSeg(mwSymbol)
}

func schemeNameOf(m profile.SecurityMapping) string {
	if m.Header != "" {
		return m.Header
	}
	return "auth"
}

func scopeArgOf(r adapter.Route, mw string) string {
	// PermissionCheck("0xff") 的字面量实参 → scope（引擎侧简化为包内解析）
	return ""
}

func tagsOf(r adapter.Route) []string {
	if r.Handler == "" {
		return nil
	}
	seg := lastSeg(handlerOwner(r.Handler))
	if seg != "" {
		return []string{seg}
	}
	return nil
}

func handlerOwner(handlerID string) string {
	// "pkg.(*Recv).Method" → "Recv"
	if i := strings.Index(handlerID, "(*"); i >= 0 {
		if j := strings.Index(handlerID[i:], ")."); j > 0 {
			return handlerID[i+2 : i+j]
		}
	}
	if i := strings.LastIndex(handlerID, "."); i > 0 {
		return handlerID[:i]
	}
	return ""
}

func operationIDOf(r adapter.Route) string {
	name := lastSeg(r.Handler)
	if name == "" {
		name = r.Method + strings.ReplaceAll(r.Path, "/", "")
	}
	if len(name) > 0 {
		name = strings.ToLower(name[:1]) + name[1:]
	}
	return name
}

func shortFile(f string) string {
	if i := strings.LastIndex(f, "/"); i >= 0 {
		return f[i+1:]
	}
	return f
}

func lastSeg(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func minConf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func schemaConfidence(sc *typeschema.Schema) float64 {
	if sc == nil {
		return 0.5
	}
	if sc.Unknown {
		return 0.6
	}
	// 递归检查内层 unknown
	var hasUnknown func(s *typeschema.Schema) bool
	hasUnknown = func(s *typeschema.Schema) bool {
		if s == nil {
			return false
		}
		if s.Unknown {
			return true
		}
		if s.Items != nil && hasUnknown(s.Items) {
			return true
		}
		for _, p := range s.Props {
			if hasUnknown(p.Schema) {
				return true
			}
		}
		return false
	}
	if hasUnknown(sc) {
		return 0.75
	}
	return 1.0
}

func sortRows(rows []facts.ResponseFact) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.Envelope == nil) != (b.Envelope == nil) {
			return a.Envelope != nil
		}
		if a.Envelope != nil && b.Envelope != nil {
			if a.Envelope.Code != b.Envelope.Code {
				// 成功(0)最前; -1(未解析)最后; 其余升序
				if a.Envelope.Code == 0 {
					return true
				}
				if b.Envelope.Code == 0 {
					return false
				}
				if a.Envelope.Code == -1 {
					return false
				}
				if b.Envelope.Code == -1 {
					return true
				}
				return a.Envelope.Code < b.Envelope.Code
			}
		}
		return a.Sink < b.Sink
	})
}

var _ = types.Typ

func containsStr(arr []string, v string) bool {
	for _, x := range arr {
		if x == v {
			return true
		}
	}
	return false
}
