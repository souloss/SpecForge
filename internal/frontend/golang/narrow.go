package golang

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/schema"
	"github.com/specforge/specforge/internal/slicing"
	"github.com/specforge/specforge/internal/typeschema"
)

// 值级收窄（per-operation）：类型层面是 any 的响应（`fiber.Map`、`PageResult{List interface{}}`），
// 在具体 operation 里往往由字面量构造、字段被赋以具体类型。沿值流找到流入 data 槽的全部字面量，
// 按「这个 operation 里实际放进去了什么」合成一份专属 schema（组件名 <类型>For<Op>），
// 证据指向字面量位置。任一来源不是字面量、或同一字段出现多种具体类型时不收窄（宁缺毋滥）。

// narrowPrefix 收窄 schema 的类型 ID 前缀（编译器取末段作组件名）。
const narrowPrefix = "narrow/"

// narrowNameInfix 收窄组件名的连接词：<原类型名>For<OperationId>。
const narrowNameInfix = "For"

// errorBranchInfix 错误分支收窄组件名的中缀。
const errorBranchInfix = "Error"

// maxNarrowDepth map 字面量嵌套收窄的最大深度。
const maxNarrowDepth = 4

// narrowData 对成功写出点的 data 做值级收窄；返回收窄 schema 事实（ok=false 表示不收窄）。
func (b *ContractPayloadBuilder) narrowData(h slicing.SinkHit) (*facts.Fact, bool) {
	lits, ok := b.slicer.DataLiterals(h)
	if !ok {
		return nil, false
	}
	litType := lits[0].Info.TypeOf(lits[0].Lit)
	for _, l := range lits[1:] {
		if t := l.Info.TypeOf(l.Lit); t == nil || litType == nil || !types.Identical(t, litType) {
			return nil, false // 不同路径构造了不同类型：不收窄
		}
	}
	if litType == nil {
		return nil, false
	}
	var sc *typeschema.Schema
	if isStringAnyMap(litType) {
		sc = b.mapLiteralSchema(lits, 0)
	} else {
		sc = b.structLiteralSchema(lits, litType)
	}
	if sc == nil {
		return nil, false
	}
	typeName := lastSeg(codegraph.TypeIDOf(litType))
	if typeName == "" || strings.ContainsAny(typeName, "[]{} ") {
		typeName = anonShapeName
	}
	pos := b.g.Fset.Position(lits[0].Lit.Pos())
	return b.narrowFact(h, sc, typeName, pos.Filename, pos.Line, "narrowed from literal"), true
}

// anonShapeName 匿名类型（字面量或结构体类型无名字）收窄 schema 的组件名主干。
const anonShapeName = "Object"

// inlineDataFact data 槽经值流收窄到匿名结构体类型（如上游响应的 `Result struct{…}`）时，按类型合成专属 schema 事实；
// 证据指向写出点。
func (b *ContractPayloadBuilder) inlineDataFact(h slicing.SinkHit) *facts.Fact {
	sc := b.synth.SchemaOf(h.DataInline)
	return b.narrowFact(h, sc, anonShapeName, h.Site.File, h.Site.Line, "narrowed from inline struct type")
}

// narrowFact 收窄 schema 事实：ID 带形状指纹（同一 operation 内不同写出点的不同形状互不覆盖），
// 组件名 <typeName>[Error]For<Op>。
func (b *ContractPayloadBuilder) narrowFact(h slicing.SinkHit, sc *typeschema.Schema, typeName, file string, line int,
	quote string) *facts.Fact {
	branch := ""
	if h.Raw && h.ErrorBranch {
		branch = errorBranchInfix // 错误分支的形状单独命名（<类型>ErrorFor<Op>），不与成功体混名
	}
	opName := operationIDOf(b.route)
	if opName != "" {
		opName = strings.ToUpper(opName[:1]) + opName[1:]
	}
	return &facts.Fact{
		ID: "schema:" + narrowPrefix + shapeKey(sc) + "/" + opName + "." + typeName + branch + narrowNameInfix + opName, Kind: facts.KindSchema,
		Value: facts.SchemaPayload{Schema: sc}, Source: facts.SourceStatic, Confidence: schemaConfidence(sc),
		Evidence: []facts.Evidence{{File: file, StartLine: line, EndLine: line,
			BlobSHA: b.g.FileHashOf(file), Quote: quote}},
		Status: "verified",
	}
}

// isStringAnyMap 类型底层是否为 map[string]<接口>（fiber.Map / map[string]any）。
func isStringAnyMap(t types.Type) bool {
	m, ok := t.Underlying().(*types.Map)
	if !ok {
		return false
	}
	k, ok := m.Key().Underlying().(*types.Basic)
	return ok && k.Info()&types.IsString != 0 && types.IsInterface(m.Elem())
}

// mapLiteralSchema map[string]any 字面量 → object：属性为全部常量键（所有字面量都有的键为必填），
// 值取静态类型（嵌套 map 字面量递归收窄）；存在非常量键时放弃。
func (b *ContractPayloadBuilder) mapLiteralSchema(lits []slicing.DataLiteral, depth int) *typeschema.Schema {
	if depth > maxNarrowDepth {
		return nil
	}
	props := map[string]*typeschema.Schema{}
	seen := map[string]int{}
	for _, l := range lits {
		pairs := make([]slicing.KeyValue, 0, len(l.Lit.Elts)+len(l.Extra))
		for _, el := range l.Lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				return nil
			}
			tv, ok := l.Info.Types[kv.Key]
			if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
				return nil // 非常量键：结构取决于运行时
			}
			pairs = append(pairs, slicing.KeyValue{Key: constant.StringVal(tv.Value), Value: kv.Value})
		}
		pairs = append(pairs, l.Extra...) // m["k"] = v 追加的键
		keysHere := map[string]bool{}
		for _, kv := range pairs {
			key := kv.Key
			if !keysHere[key] {
				keysHere[key] = true
				seen[key]++
			}
			vs := b.literalValueSchema(kv.Value, l, depth)
			if old, dup := props[key]; dup && !sameShape(old, vs) {
				props[key] = &typeschema.Schema{Unknown: true, UnknownWhy: "value type differs across code paths"}
				continue
			}
			props[key] = vs
		}
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sc := &typeschema.Schema{Type: "object"}
	for _, k := range keys {
		req := seen[k] == len(lits)
		sc.Props = append(sc.Props, typeschema.Prop{Name: k, Schema: props[k], Required: req})
		if req {
			sc.Required = append(sc.Required, k)
		}
	}
	return sc
}

// literalValueSchema 字面量中一个值的 schema：嵌套 map 字面量递归，其余按静态类型。
func (b *ContractPayloadBuilder) literalValueSchema(v ast.Expr, l slicing.DataLiteral, depth int) *typeschema.Schema {
	if lit, ok := ast.Unparen(v).(*ast.CompositeLit); ok {
		if t := l.Info.TypeOf(lit); t != nil && isStringAnyMap(t) {
			if sc := b.mapLiteralSchema([]slicing.DataLiteral{{Lit: lit, Fn: l.Fn, Info: l.Info}}, depth+1); sc != nil {
				return sc
			}
		}
	}
	t := l.Info.TypeOf(v)
	if t == nil {
		return &typeschema.Schema{Unknown: true, UnknownWhy: "untyped literal value"}
	}
	if b, ok := t.(*types.Basic); ok && b.Kind() == types.UntypedNil {
		return &typeschema.Schema{Unknown: true, UnknownWhy: "nil literal value"}
	}
	return b.synth.SchemaOf(types.Default(t))
}

// structLiteralSchema 命名结构体字面量：对 any 字段，取字面量字段值与所在函数内 `x.F = rhs` 赋值的
// 具体静态类型；唯一且非接口时收窄。没有任何字段被收窄返回 nil。
func (b *ContractPayloadBuilder) structLiteralSchema(lits []slicing.DataLiteral, t types.Type) *typeschema.Schema {
	typeID := codegraph.TypeIDOf(t)
	ti := b.g.Type(typeID)
	if ti == nil || !ti.IsStruct {
		return nil
	}
	base := b.synth.Synthesize(typeID)
	narrowed := map[string]*typeschema.Schema{} // JSON 名 → 收窄后 schema
	for _, f := range ti.Fields {
		if f.Kind != "any" {
			continue
		}
		var cands []types.Type
		for _, l := range lits {
			for _, el := range l.Lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == f.Name {
						cands = append(cands, l.Info.TypeOf(kv.Value))
					}
				}
			}
			cands = append(cands, b.slicer.FieldAssignTypes(l.Fn, typeID, f.Name)...)
		}
		if ct := uniqueConcrete(cands); ct != nil {
			narrowed[f.JSONName] = b.synth.SchemaOf(ct)
		}
	}
	// 嵌入结构体的 any 字段（如 PageResult.List）：在声明者（typeID）的字面量里只以 `PageResult{List: …}` 出现，
	// 即便该字段自己未写进字面量，也应逐条字段赋值回溯（FieldAssignTypes 已覆盖 promoted 字段的 `result.List = …`）。
	for _, f := range ti.Fields {
		if f.TypeID == "" || f.Kind != "struct" {
			continue
		}
		inner := b.g.Type(f.TypeID)
		if inner == nil || !inner.IsStruct {
			continue
		}
		// 显式命名的嵌入字段按嵌套字段编码（不展平），路径不同，不在本处收窄。
		if typeschema.HasExplicitJSONName(f.Tag) {
			continue
		}
		for _, ef := range inner.Fields {
			if ef.Kind != "any" {
				continue
			}
			var cands []types.Type
			for _, l := range lits {
				// 只按外层类型回溯 promoted 字段赋值：内层类型的同名赋值属于无关变量，不参与。
				cands = append(cands, b.slicer.FieldAssignTypes(l.Fn, typeID, ef.Name)...)
			}
			if ct := uniqueConcrete(cands); ct != nil {
				narrowed[ef.JSONName] = b.synth.SchemaOf(ct)
			}
		}
	}
	sc := *base
	sc.Props = make([]typeschema.Prop, len(base.Props))
	for i, p := range base.Props {
		if ns, ok := narrowed[p.Name]; ok {
			p.Schema = ns
		}
		sc.Props[i] = p
	}
	// 字面量内嵌的复合字面量（`Template: MessageTempate{Params: map[string]interface{}{…}}`）逐层下钻收窄；
	// 多个字面量时须全部收窄出同一形状，否则某条路径的形状不能代表全部路径。
	var agreed *typeschema.Schema
	for i, l := range lits {
		ns, ok := b.nestedLiteralSchema(l.Lit, l, &sc, 0)
		if !ok || (i > 0 && shapeKey(ns) != shapeKey(agreed)) {
			agreed = nil
			break
		}
		agreed = ns
	}
	if agreed != nil {
		return agreed
	}
	if len(narrowed) == 0 {
		return nil
	}
	return &sc
}

// nestedLiteralSchema 结构体字面量 lit 中以复合字面量赋值的字段逐层收窄：map[string]any 字面量按常量键
// 合成 object，嵌套结构体字面量递归。sc 为 lit 对应的 schema（不修改，沿路径浅克隆）；无任何收窄返回 false。
func (b *ContractPayloadBuilder) nestedLiteralSchema(lit *ast.CompositeLit, l slicing.DataLiteral,
	sc *typeschema.Schema, depth int) (*typeschema.Schema, bool) {
	if sc == nil || sc.Ref != "" || depth > maxNarrowDepth {
		return sc, false
	}
	t := l.Info.TypeOf(lit)
	if t == nil {
		return sc, false
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return sc, false
	}
	fields := b.g.StructFields(st)
	out := *sc
	out.Props = append([]typeschema.Prop(nil), sc.Props...)
	changed := false
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		inner, ok := compositeOf(kv.Value)
		if !ok {
			continue
		}
		name := jsonFieldName(fields, key.Name)
		for i, p := range out.Props {
			if name == "" || p.Name != name {
				continue
			}
			var ns *typeschema.Schema
			if it := l.Info.TypeOf(inner); it != nil && isStringAnyMap(it) {
				ns = b.mapLiteralSchema([]slicing.DataLiteral{{Lit: inner, Fn: l.Fn, Info: l.Info}}, depth+1)
				if ns != nil && p.Schema != nil && !strings.HasPrefix(p.Schema.Description, typeschema.MapDescPrefix) {
					ns.Description = p.Schema.Description // 保留字段 doc；map 合成说明随收窄丢弃
				}
			} else if rs, ok := b.nestedLiteralSchema(inner, l, p.Schema, depth+1); ok {
				ns = rs
			}
			if ns != nil {
				out.Props[i].Schema = ns
				changed = true
			}
		}
	}
	return &out, changed
}

// compositeOf 表达式（去括号与取地址）是否为复合字面量。
func compositeOf(e ast.Expr) (*ast.CompositeLit, bool) {
	e = ast.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = ast.Unparen(u.X)
	}
	lit, ok := e.(*ast.CompositeLit)
	return lit, ok
}

// jsonFieldName 结构体中名为 goName 的非嵌入字段的序列化名；不存在或不序列化（`json:"-"`）返回空。
func jsonFieldName(fields []codegraph.Field, goName string) string {
	for _, f := range fields {
		if f.Name == goName && !f.Embedded && f.JSONName != jsonSkipName {
			return f.JSONName
		}
	}
	return ""
}

// uniqueConcrete 候选类型中「有信息量」的类型（非接口、非接口元素切片、非 nil）若唯一则返回之。
func uniqueConcrete(cands []types.Type) types.Type {
	var got types.Type
	for _, t := range cands {
		if t == nil || uninformative(t) {
			continue
		}
		if got == nil {
			got = t
			continue
		}
		if !types.Identical(got, t) {
			return nil // 多种具体类型：不收窄
		}
	}
	return got
}

// uninformative 类型不提供形状信息：接口、nil、元素为接口的切片/映射（`[]interface{}{}` 初始化）。
func uninformative(t types.Type) bool {
	if b, ok := t.(*types.Basic); ok && b.Kind() == types.UntypedNil {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Interface:
		return true
	case *types.Slice:
		return types.IsInterface(u.Elem())
	case *types.Map:
		return types.IsInterface(u.Elem())
	}
	return false
}

// sameShape 两个 schema 是否同形（只比较类型与引用，足够判断同一键的值在不同路径是否一致）。
func sameShape(a, b *typeschema.Schema) bool {
	return a != nil && b != nil && a.Type == b.Type && a.Ref == b.Ref && a.Unknown == b.Unknown
}

// shapeKeyLen 形状指纹的十六进制长度。
const shapeKeyLen = 8

// shapeKey schema 的形状指纹（JSON 序列化后哈希；字段有序，结果确定）。
func shapeKey(sc *schema.Schema) string {
	b, _ := json.Marshal(sc)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:shapeKeyLen]
}
