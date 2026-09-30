package slicing

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strings"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/profile"
)

// 框架原生响应写出器 + 响应包装器自动发现（函数摘要）。
//
// 原生写出器是框架 API 的固定语义（无需 profile）；包装器是仓库自定义的
// 「把形参装进信封再交给原生写出器」的函数（如 code.WriteRes），
// 通过扫描其函数体一次性算出摘要：哪个形参进了信封哪个字段、按哪个形参是否为 nil 分流。

// writeFlow 包装器函数体内一次原生写出的形参流向。
type writeFlow struct {
	envType   string          // 写出的信封结构体类型 ID（直接透传形参或 map 信封时为空）
	fieldOf   map[int]string  // 形参下标 → 信封字段 JSON 名（值即形参本身）
	derivedOf map[int]string  // 形参下标 → 信封字段 JSON 名（值由形参派生，如 err.Error()）
	direct    int             // 形参直接作为响应体时的下标；profile.NoSlot 表示否
	guardedBy int             // 写出位于 `if 形参 != nil {}` 分支内时该形参下标；否则 profile.NoSlot
	mapFields []EnvelopeField // map 字面量信封（gin.H{...}）的全部键；结构体信封为空
	code      *int            // 信封中「码」字段的整数常量（如 "code": 500）；无则 nil
}

// EnvelopeField 合成信封（map 字面量信封）的一个键。
type EnvelopeField struct {
	Key      string     // JSON 键
	Type     types.Type // 值的静态类型（承载 data 的键为形参类型，通常为 any）
	Role     string     // 语义：data / err / 空（普通字段）
	Required bool       // 是否在该包装器的每条写出路径上都出现
	JSONType string     // Type 未知时的 JSON 类型（LLM 包装器摘要给出，封闭集合）
}

// 合成信封字段语义。
const (
	RoleData = "data" // 承载业务数据的键
	RoleErr  = "err"  // 承载错误信息的键
)

// SyntheticEnvelope 包装器用 map 字面量构造的信封（无命名类型，由前端合成 schema）。
type SyntheticEnvelope struct {
	ID     string          // 合成类型 ID（wrapenv/<函数>.<函数名>Envelope）
	Fn     string          // 包装器函数符号 ID（证据）
	Fields []EnvelopeField // 键（按首次出现顺序）
}

// SyntheticEnvPrefix 合成信封类型 ID 前缀。
const SyntheticEnvPrefix = "wrapenv/"

// codeKeyNames 视为「业务码」的信封键（小写比较）：只采信整数常量值。
var codeKeyNames = map[string]bool{"code": true, "errcode": true, "err_code": true, "errno": true,
	"retcode": true, "ret_code": true, "statuscode": true, "status_code": true}

// DiscoverSinks 自动发现响应汇聚点：框架原生写出器（原语表）+ 仓库内的响应包装器。
// 包装器摘要让 handler 层调用点按「形参槽位」解析 data/err，与手写 profile 的 sink 同构。
// 同时返回 map 字面量信封的合成定义（按合成类型 ID 索引），供前端合成信封 schema。
func DiscoverSinks(g *codegraph.Graph, writers []adapter.Writer) ([]profile.SinkPattern, map[string]SyntheticEnvelope) {
	idx := writerIndex(writers)
	var out []profile.SinkPattern
	envs := map[string]SyntheticEnvelope{}
	for _, id := range g.FuncIDs() {
		if p, env, ok := summarizeWrapper(g, id, idx); ok {
			out = append(out, p)
			if env != nil {
				envs[env.ID] = *env
			}
		}
	}
	for _, w := range writers {
		out = append(out, profile.SinkPattern{
			Symbol: w.Symbol, DataSlot: w.BodyArg, ErrSlot: profile.NoSlot,
			Status: w.DefaultStatus, Raw: true, Auto: true,
		})
	}
	return out, envs
}

// writerIndex 写出器原语按符号索引。
func writerIndex(writers []adapter.Writer) map[string]adapter.Writer {
	idx := make(map[string]adapter.Writer, len(writers))
	for _, w := range writers {
		idx[w.Symbol] = w
	}
	return idx
}

// summarizeWrapper 计算单个函数的包装器摘要；函数体内无「形参 → 原生写出器」流向时返回 false。
func summarizeWrapper(g *codegraph.Graph, fnID string, writers map[string]adapter.Writer) (profile.SinkPattern, *SyntheticEnvelope, bool) {
	fd := g.FuncDeclOf(fnID)
	info := g.TypeInfoOfFunc(fnID)
	if fd == nil || fd.Body == nil || info == nil || fd.Type.Params == nil {
		return profile.SinkPattern{}, nil, false
	}
	params := paramIndex(fd, info)
	if len(params) == 0 {
		return profile.SinkPattern{}, nil, false
	}
	guards := nilGuards(fd.Body, info, params)
	var flows []writeFlow
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false // 闭包内的写出不属于本函数的同步摘要
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee, _ := codegraph.ResolveCallee(call, info)
		w, ok := writers[callee]
		if !ok || w.BodyArg >= len(call.Args) {
			return true
		}
		if f, ok := flowOf(g, call.Args[w.BodyArg], info, params, fd.Body); ok {
			f.guardedBy = guardOf(guards, call.Pos())
			flows = append(flows, f)
		}
		return true
	})
	return patternOf(fnID, flows, defaultStatusOf(writers))
}

// defaultStatusOf 包装器写出的默认 HTTP 状态（取原语表中写出器的默认值；无写出器时为 200）。
func defaultStatusOf(writers map[string]adapter.Writer) int {
	for _, w := range writers {
		return w.DefaultStatus
	}
	return fallbackStatus
}

// fallbackStatus 原语表为空时的默认 HTTP 状态。
const fallbackStatus = 200

// patternOf 由各写出点流向合成 sink 模式：无守卫的写出为成功路径（data 槽），
// `if p != nil` 守卫内的写出为失败路径（err 槽 = p）。信封里「码」字段的整数常量按分支记为
// 固定业务码；map 字面量信封额外合成一份信封定义。
func patternOf(fnID string, flows []writeFlow, status int) (profile.SinkPattern, *SyntheticEnvelope, bool) {
	p := profile.SinkPattern{
		Symbol: fnID, DataSlot: profile.NoSlot, ErrSlot: profile.NoSlot,
		Status: status, Auto: true,
	}
	hasSuccess, hasFailure := false, false
	var envFlows []writeFlow // 参与合成信封的写出（map 字面量）
	for _, f := range flows {
		if f.direct != profile.NoSlot {
			if f.guardedBy == profile.NoSlot {
				p.DataSlot, p.Raw, hasSuccess = f.direct, true, true
			}
			continue
		}
		if f.guardedBy != profile.NoSlot {
			name, raw := f.fieldOf[f.guardedBy]
			ok := raw
			if !ok {
				name, ok = f.derivedOf[f.guardedBy]
			}
			if ok {
				p.ErrSlot, p.ErrField, p.ErrRaw, hasFailure = f.guardedBy, name, raw, true
				p.ErrEnvelopeType, p.FailureCode = f.envType, f.code
				if f.mapFields != nil {
					envFlows = append(envFlows, f)
				}
			}
			continue
		}
		for idx, name := range f.fieldOf {
			if p.DataSlot == profile.NoSlot || idx < p.DataSlot {
				p.DataSlot, p.DataField = idx, name
			}
		}
		p.EnvelopeType, p.SuccessCode, hasSuccess = f.envType, f.code, true
		if f.mapFields != nil {
			envFlows = append(envFlows, f)
		}
	}
	if !hasSuccess && !hasFailure {
		return profile.SinkPattern{}, nil, false
	}
	if !hasSuccess {
		p.EnvelopeType = p.ErrEnvelopeType // 只有失败写出的包装器：信封即失败信封
	}
	p.BranchOnErr = hasSuccess && hasFailure
	var env *SyntheticEnvelope
	if len(envFlows) > 0 {
		for i := range envFlows {
			markRoles(&envFlows[i], p.DataSlot, p.ErrSlot)
		}
		env = synthesizeEnvelope(fnID, envFlows)
		p.EnvelopeType, p.ErrEnvelopeType = env.ID, env.ID
	}
	return p, env, true
}

// synthesizeEnvelope 合并各写出路径的 map 字面量键：键序按首次出现，每条路径都有的键为必填。
func synthesizeEnvelope(fnID string, flows []writeFlow) *SyntheticEnvelope {
	name := fnID[strings.LastIndex(fnID, ".")+1:]
	env := &SyntheticEnvelope{ID: SyntheticEnvPrefix + fnID + "." + name + "Envelope", Fn: fnID}
	count := map[string]int{}
	pos := map[string]int{}
	for _, f := range flows {
		for _, fld := range f.mapFields {
			count[fld.Key]++
			if i, seen := pos[fld.Key]; seen {
				if env.Fields[i].Role == "" {
					env.Fields[i].Role = fld.Role
				}
				continue
			}
			pos[fld.Key] = len(env.Fields)
			env.Fields = append(env.Fields, fld)
		}
	}
	for i := range env.Fields {
		env.Fields[i].Required = count[env.Fields[i].Key] == len(flows)
	}
	return env
}

// flowOf 解析写出实参：形参直接透传、信封复合字面量 `T{Field: 形参}`，
// 或先赋给局部变量再写出（`r := &T{Field: 形参}; c.JSON(r)`）。
func flowOf(g *codegraph.Graph, arg ast.Expr, info *types.Info, params map[types.Object]int, body *ast.BlockStmt) (writeFlow, bool) {
	f := writeFlow{direct: profile.NoSlot, guardedBy: profile.NoSlot, fieldOf: map[int]string{}, derivedOf: map[int]string{}}
	arg = stripAddr(arg)
	var holder types.Object // 承载字面量的局部变量（`m := gin.H{}; m["k"] = v` 的 m）
	if id, ok := arg.(*ast.Ident); ok {
		if idx, ok := params[info.Uses[id]]; ok {
			f.direct = idx
			return f, true
		}
		holder = info.Uses[id]
		init := localInitOf(body, holder, info)
		if init == nil {
			return f, false
		}
		arg = stripAddr(init)
	}
	lit, ok := arg.(*ast.CompositeLit)
	if !ok {
		return f, false
	}
	t := info.TypeOf(lit)
	if t == nil {
		return f, false
	}
	if isStringKeyMap(t) {
		return mapFlowOf(lit, MapAssignments(body, holder, info), info, params)
	}
	f.envType = typeIDOf(t)
	ti := g.Type(f.envType)
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		name := jsonFieldName(ti, key.Name)
		if idx, direct := paramOf(kv.Value, info, params); direct {
			f.fieldOf[idx] = name
		} else if idx, derived := derivedParamOf(kv.Value, info, params); derived {
			f.derivedOf[idx] = name
		} else if codeKeyNames[strings.ToLower(name)] {
			f.code = intConst(kv.Value, info)
		}
	}
	return f, len(f.fieldOf)+len(f.derivedOf) > 0
}

// mapFlowOf map 字面量信封（`gin.H{"code": 0, "data": data}`）：常量字符串键为信封字段，
// 值为形参 / 形参派生表达式 / 其它（记录静态类型）；「码」键的整数常量记为该分支的固定业务码。
// extra 为字面量之后对同一变量的常量键赋值（`m["data"] = data`），一并视为信封键。
func mapFlowOf(lit *ast.CompositeLit, extra []KeyValue, info *types.Info, params map[types.Object]int) (writeFlow, bool) {
	f := writeFlow{direct: profile.NoSlot, guardedBy: profile.NoSlot, fieldOf: map[int]string{}, derivedOf: map[int]string{},
		mapFields: []EnvelopeField{}}
	pairs := make([]KeyValue, 0, len(lit.Elts)+len(extra))
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return f, false
		}
		key, ok := constString(kv.Key, info)
		if !ok {
			return f, false // 非常量键：信封结构取决于运行时，交给形状缺口
		}
		pairs = append(pairs, KeyValue{Key: key, Value: kv.Value})
	}
	pairs = append(pairs, extra...)
	seen := map[string]bool{}
	for _, kv := range pairs {
		if idx, direct := paramOf(kv.Value, info, params); direct {
			f.fieldOf[idx] = kv.Key
		} else if idx, derived := derivedParamOf(kv.Value, info, params); derived {
			f.derivedOf[idx] = kv.Key
		} else if codeKeyNames[strings.ToLower(kv.Key)] {
			f.code = intConst(kv.Value, info)
		}
		if !seen[kv.Key] {
			seen[kv.Key] = true
			f.mapFields = append(f.mapFields, EnvelopeField{Key: kv.Key, Type: info.TypeOf(kv.Value)})
		}
	}
	return f, len(f.fieldOf)+len(f.derivedOf) > 0
}

// KeyValue map 的一个常量键及其值表达式。
type KeyValue struct {
	Key   string   // 常量字符串键
	Value ast.Expr // 值表达式
}

// MapAssignments 函数体内对 map 变量 obj 的常量键赋值（`m["k"] = v`），按出现顺序；非常量键跳过。
func MapAssignments(body *ast.BlockStmt, obj types.Object, info *types.Info) []KeyValue {
	if body == nil || obj == nil {
		return nil
	}
	var out []KeyValue
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, lhs := range as.Lhs {
			ix, ok := lhs.(*ast.IndexExpr)
			if !ok {
				continue
			}
			id, ok := ast.Unparen(ix.X).(*ast.Ident)
			if !ok || info.Uses[id] != obj {
				continue
			}
			if key, ok := constString(ix.Index, info); ok {
				out = append(out, KeyValue{Key: key, Value: as.Rhs[i]})
			}
		}
		return true
	})
	return out
}

// constString 常量字符串表达式的值。
func constString(e ast.Expr, info *types.Info) (string, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// markRoles 标注合成信封字段的语义（data/err），依据包装器模式的槽位绑定。
func markRoles(f *writeFlow, dataSlot, errSlot int) {
	for i := range f.mapFields {
		for idx, key := range f.fieldOf {
			if key == f.mapFields[i].Key && idx == dataSlot {
				f.mapFields[i].Role = RoleData
			}
		}
		for idx, key := range f.derivedOf {
			if key == f.mapFields[i].Key && idx == errSlot {
				f.mapFields[i].Role = RoleErr
			}
		}
		for idx, key := range f.fieldOf {
			if key == f.mapFields[i].Key && idx == errSlot {
				f.mapFields[i].Role = RoleErr
			}
		}
	}
}

// isStringKeyMap 类型底层是否为 map[string]V（gin.H、fiber.Map、map[string]any）。
func isStringKeyMap(t types.Type) bool {
	m, ok := t.Underlying().(*types.Map)
	if !ok {
		return false
	}
	k, ok := m.Key().Underlying().(*types.Basic)
	return ok && k.Info()&types.IsString != 0
}

// paramOf 表达式是否就是某个形参（返回下标）。
func paramOf(e ast.Expr, info *types.Info, params map[types.Object]int) (int, bool) {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return 0, false
	}
	idx, ok := params[info.Uses[id]]
	return idx, ok
}

// derivedParamOf 表达式是否由唯一一个形参派生（如 err.Error()、fmt.Sprint(err)），返回其下标。
func derivedParamOf(e ast.Expr, info *types.Info, params map[types.Object]int) (int, bool) {
	found := -1
	multi := false
	ast.Inspect(e, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if idx, isParam := params[info.Uses[id]]; isParam {
			if found >= 0 && found != idx {
				multi = true
			}
			found = idx
		}
		return true
	})
	return found, found >= 0 && !multi
}

// intConst 整数常量表达式的值；非常量返回 nil。
func intConst(e ast.Expr, info *types.Info) *int {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return nil
	}
	v, exact := constant.Int64Val(tv.Value)
	if !exact {
		return nil
	}
	n := int(v)
	return &n
}

// stripAddr 去掉括号与取地址：`&T{}` / `(x)` → 内层表达式。
func stripAddr(e ast.Expr) ast.Expr {
	e = ast.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = ast.Unparen(u.X)
	}
	return e
}

// localInitOf 局部变量 obj 在函数体内的唯一定义式右值（`x := expr` / `var x = expr`）；
// 被多次定义或找不到时返回 nil（值流不唯一时不猜）。
func localInitOf(body *ast.BlockStmt, obj types.Object, info *types.Info) ast.Expr {
	if body == nil || obj == nil {
		return nil
	}
	var init ast.Expr
	defs := 0
	ast.Inspect(body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.AssignStmt:
			if len(st.Lhs) != len(st.Rhs) {
				return true
			}
			for i, lhs := range st.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				if info.Defs[id] == obj || info.Uses[id] == obj {
					init, defs = st.Rhs[i], defs+1
				}
			}
		case *ast.ValueSpec:
			for i, name := range st.Names {
				if info.Defs[name] == obj && i < len(st.Values) {
					init, defs = st.Values[i], defs+1
				}
			}
		}
		return true
	})
	if defs != 1 {
		return nil
	}
	return init
}

// jsonFieldName 结构体字段 Go 名 → JSON 名（类型信息缺失时退回 Go 名）。
func jsonFieldName(ti *codegraph.TypeInfo, goName string) string {
	if ti != nil {
		for _, fl := range ti.Fields {
			if fl.Name == goName {
				return fl.JSONName
			}
		}
	}
	return goName
}

// paramIndex 形参对象 → 下标（不含接收者；匿名/空白形参不参与）。
func paramIndex(fd *ast.FuncDecl, info *types.Info) map[types.Object]int {
	out := map[types.Object]int{}
	idx := 0
	for _, field := range fd.Type.Params.List {
		if len(field.Names) == 0 {
			idx++
			continue
		}
		for _, name := range field.Names {
			if obj := info.Defs[name]; obj != nil && name.Name != "_" {
				out[obj] = idx
			}
			idx++
		}
	}
	return out
}

// nilGuard `if 形参 != nil { ... }` 分支的源码范围。
type nilGuard struct {
	param    int       // 被判空的形参下标
	from, to token.Pos // then 分支范围
}

// nilGuards 收集函数体内以形参判非空为条件的 then 分支。
func nilGuards(body *ast.BlockStmt, info *types.Info, params map[types.Object]int) []nilGuard {
	var out []nilGuard
	ast.Inspect(body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		be, ok := ast.Unparen(ifs.Cond).(*ast.BinaryExpr)
		if !ok || be.Op != token.NEQ {
			return true
		}
		for _, side := range [][2]ast.Expr{{be.X, be.Y}, {be.Y, be.X}} {
			id, ok := ast.Unparen(side[0]).(*ast.Ident)
			nilID, ok2 := ast.Unparen(side[1]).(*ast.Ident)
			if !ok || !ok2 || nilID.Name != "nil" {
				continue
			}
			if idx, ok := params[info.Uses[id]]; ok {
				out = append(out, nilGuard{param: idx, from: ifs.Body.Pos(), to: ifs.Body.End()})
			}
		}
		return true
	})
	return out
}

// guardOf 写出点所在的最内层判空分支形参；不在任何判空分支内返回 profile.NoSlot。
func guardOf(guards []nilGuard, pos token.Pos) int {
	best, span := profile.NoSlot, token.Pos(0)
	for _, gd := range guards {
		if pos >= gd.from && pos < gd.to && (best == profile.NoSlot || gd.to-gd.from < span) {
			best, span = gd.param, gd.to-gd.from
		}
	}
	return best
}
