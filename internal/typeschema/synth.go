// Package typeschema 实现 Go 类型 → JSON Schema 合成
// （设计文档 §7.1 映射总表 + §7.2 疑难 case 清单）。
//
// 静态能定型的绝不问模型；定型不了的显式降级并标注
// （x-specforge-unknown），禁止静默编造。
package typeschema

import (
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/schema"
)

// Schema / Prop 语言无关的 schema IR（定义在 schema 包），Go 前端在此合成。
type (
	Schema = schema.Schema
	Prop   = schema.Prop
)

// Synthesizer 类型合成器（带缓存防递归）。
type Synthesizer struct {
	g       *codegraph.Graph
	cache   map[string]*Schema
	inStack map[string]bool
	// request 请求方向：必填只认校验约束（解码容忍缺省字段）；
	// 响应方向无 omitempty 的字段总会序列化输出，故视为必填。
	request bool
}

// New 创建合成器。
func New(g *codegraph.Graph) *Synthesizer {
	return &Synthesizer{
		g:       g,
		cache:   map[string]*Schema{},
		inStack: map[string]bool{},
	}
}

// NewRequest 创建请求方向合成器（独立缓存，必填语义见 Synthesizer.request）。
func NewRequest(g *codegraph.Graph) *Synthesizer {
	s := New(g)
	s.request = true
	return s
}

// MapDescPrefix map 字段合成 schema 的描述前缀（后接值类型串）：是合成说明而非字段 doc，
// 值级收窄把 map 换成具体 object 后应丢弃。
const MapDescPrefix = "map with value type "

// emptyStructID 匿名空结构体的类型 ID（codegraph.TypeIDOf 对非命名类型取类型串）。
const emptyStructID = "struct{}"

// Synthesize 按类型 ID 合成 schema。
func (s *Synthesizer) Synthesize(typeID string) *Schema {
	if sc, ok := s.cache[typeID]; ok {
		return sc
	}
	if s.inStack[typeID] {
		// 递归类型: 自引用（设计文档 §7.2.2）
		return &Schema{Ref: refNameOf(s.g, typeID)}
	}
	ti := s.g.Type(typeID)
	if ti == nil && typeID == emptyStructID {
		return &Schema{Type: "object"} // 空结构体恒序列化为 {}：静态可定型
	}
	if ti == nil {
		return &Schema{Unknown: true, UnknownWhy: "type not found: " + typeID}
	}
	s.inStack[typeID] = true
	defer delete(s.inStack, typeID)

	sc := s.synthType(ti, typeID)
	s.cache[typeID] = sc
	return sc
}

func (s *Synthesizer) synthType(ti *codegraph.TypeInfo, typeID string) *Schema {
	// time.Time / RawMessage 特判（§7.2.4 / §7.2.10）
	if ti.IsTime {
		return &Schema{Type: "string", Format: "date-time"}
	}
	if ti.IsRawMsg {
		return &Schema{NoBody: true}
	}
	if !ti.IsStruct {
		// 命名基础类型（别名/枚举载体）
		sc := basicSchema(ti.Underlying)
		if ti.Enumish {
			sc.Enum = enumValues(ti)
			sc.EnumSource = enumSource(ti)
		}
		return sc
	}
	if variants := s.unionVariantSchemas(typeID, ti); len(variants) > 0 {
		return &Schema{OneOf: variants}
	}
	// struct → object（嵌入字段展平，设计文档 §7.2.8）
	sc := &Schema{Type: "object"}
	var required []string
	seen := map[string]bool{}
	var embedded []*Schema
	for _, f := range ti.Fields {
		// encoding/json 忽略 `json:"-"` 与非嵌入的未导出字段（如 oapi-codegen union 的 `union json.RawMessage`）
		if f.JSONName == "-" || (!f.Embedded && !token.IsExported(f.Name)) {
			continue
		}
		if f.Embedded && !hasExplicitJSONName(f.Tag) {
			if inner := s.embeddedSchema(f); inner != nil {
				embedded = append(embedded, inner)
				continue
			}
		}
		fsc := s.synthField(f)
		if f.Doc != "" && fsc.Ref == "" {
			fsc.Description = f.Doc
		}
		seen[f.JSONName] = true
		req := f.Required
		if s.request {
			req, _ = codegraph.ConstraintRequired(f.Tag)
		}
		sc.Props = append(sc.Props, Prop{
			Name: f.JSONName, Schema: fsc, Required: req, OmitEmpty: hasOmitempty(f.Tag),
		})
		if req {
			required = append(required, f.JSONName)
		}
	}
	// encoding/json 语义：外层（浅层）同名字段遮蔽嵌入结构体的字段
	for _, inner := range embedded {
		for _, p := range inner.Props {
			if seen[p.Name] {
				continue
			}
			seen[p.Name] = true
			sc.Props = append(sc.Props, Prop{
				Name: p.Name, Schema: cloneSchema(p.Schema), Required: p.Required, OmitEmpty: p.OmitEmpty,
			})
			if p.Required {
				required = append(required, p.Name)
			}
		}
	}
	sort.Strings(required)
	sc.Required = required
	return sc
}

func (s *Synthesizer) unionVariantSchemas(typeID string, ti *codegraph.TypeInfo) []*Schema {
	if ti == nil || ti.Pkg == "" {
		return nil
	}
	for _, field := range ti.Fields {
		if field.Name != "union" || (field.Kind != "rawmsg" && !strings.Contains(field.TypeStr, "RawMessage")) {
			continue
		}
		prefix := lastTypeSegment(typeID)
		var ids []string
		for id, candidate := range s.g.Types {
			if candidate.Pkg != ti.Pkg || !strings.HasPrefix(lastTypeSegment(id), prefix) {
				continue
			}
			suffix := strings.TrimPrefix(lastTypeSegment(id), prefix)
			if suffix == "" || strings.Trim(suffix, "0123456789") != "" {
				continue
			}
			ids = append(ids, id)
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)
		variants := make([]*Schema, 0, len(ids))
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			variants = append(variants, s.Synthesize(id))
			seen[id] = true
		}
		for _, result := range s.g.ResultTypes(typeID) {
			key := types.TypeString(result, func(pkg *types.Package) string { return pkg.Path() })
			if seen[key] {
				continue
			}
			seen[key] = true
			if named, ok := types.Unalias(result).(*types.Named); ok && s.g.Type(codegraph.TypeIDOf(named)) != nil {
				variants = append(variants, s.Synthesize(codegraph.TypeIDOf(named)))
			} else {
				variants = append(variants, s.synthGoType(result, 0))
			}
		}
		return variants
	}
	return nil
}

func lastTypeSegment(id string) string {
	if i := strings.LastIndexByte(id, '.'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// embeddedSchema 嵌入字段指向的结构体 schema（供外层展平）；非结构体嵌入返回 nil，按普通字段处理。
func (s *Synthesizer) embeddedSchema(f codegraph.Field) *Schema {
	ti := s.g.Type(f.TypeID)
	if ti == nil || !ti.IsStruct || ti.IsTime {
		return nil
	}
	inner := s.Synthesize(f.TypeID)
	if inner == nil || inner.Type != "object" {
		return nil
	}
	return inner
}

// HasExplicitJSONName json tag 是否显式命名（显式命名的嵌入字段按普通嵌套字段编码，不展平）。
func HasExplicitJSONName(tag string) bool { return hasExplicitJSONName(tag) }

// hasExplicitJSONName 见 HasExplicitJSONName。
func hasExplicitJSONName(tag string) bool {
	name, _, _ := strings.Cut(tagValue(tag, "json"), ",")
	return name != ""
}

// synthField 单字段合成（§7.2.6 指针/omitempty、§7.2.3 any）。
func (s *Synthesizer) synthField(f codegraph.Field) *Schema {
	// 命名类型优先: 走 Synthesize（枚举/别名/time/RawMessage 正确处理）。
	// 必须克隆: 共享缓存可变性会污染同类型的其他引用（§7.3 shapeHash 边界）。
	if ti := s.g.Type(f.TypeID); ti != nil && (f.Kind == "basic" || f.Kind == "named" || f.Kind == "other") {
		if sc2 := s.Synthesize(f.TypeID); sc2 != nil {
			sc := cloneSchema(sc2)
			if f.Nullable {
				sc.Nullable = true
			}
			applyConstraints(sc, f.Tag)
			return sc
		}
	}
	var sc *Schema
	switch f.Kind {
	case "basic":
		sc = basicSchema(basicKindStr(f.TypeStr))
	case "time":
		sc = &Schema{Type: "string", Format: "date-time"}
	case "rawmsg":
		sc = &Schema{NoBody: true}
	case "slice":
		sc = &Schema{Type: "array", Items: s.synthElem(f)}
	case "map":
		sc = &Schema{Type: "object", Additional: true}
		sc.Description = MapDescPrefix + f.TypeStr
		if strings.Contains(f.TypeStr, "any") || strings.Contains(f.TypeStr, "interface{}") {
			sc.Unknown = true
			sc.UnknownWhy = "map value type any/interface{}: not statically resolvable"
		}
	case "any":
		sc = &Schema{Unknown: true, UnknownWhy: "interface{}/any: cannot be statically resolved"}
	case "struct":
		if ti := s.g.Type(f.TypeID); ti != nil {
			sc = cloneSchema(s.Synthesize(f.TypeID))
		} else if f.Type != nil {
			sc = s.synthGoType(f.Type, 0) // 匿名内联结构体：按字段类型直接合成
		} else {
			sc = &Schema{Unknown: true, UnknownWhy: "inline struct"}
		}
	default:
		if ti := s.g.Type(f.TypeID); ti != nil {
			sc = cloneSchema(s.Synthesize(f.TypeID))
		} else {
			sc = &Schema{Unknown: true, UnknownWhy: "unresolved: " + f.TypeStr}
		}
	}
	if f.Nullable {
		sc.Nullable = true
	}
	// 校验约束合并（binding tag，设计文档 §7.1 末表）
	applyConstraints(sc, f.Tag)
	return sc
}

// maxInlineDepth 匿名类型（内联结构体/多层切片）递归合成的深度上限，防病态嵌套。
const maxInlineDepth = 8

// synthGoType 按 go/types 类型合成 schema（切片元素、匿名结构体、指针包装）；
// 命名类型走 Synthesize，保留类型身份与枚举语义，不做名字后缀匹配。
func (s *Synthesizer) synthGoType(t types.Type, depth int) *Schema {
	if depth > maxInlineDepth {
		return &Schema{Unknown: true, UnknownWhy: "inline type nesting too deep: " + t.String()}
	}
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		return s.synthGoType(p.Elem(), depth+1)
	}
	switch codegraph.KindOf(t) {
	case "time":
		return &Schema{Type: "string", Format: "date-time"}
	case "rawmsg":
		return &Schema{NoBody: true}
	case "any", "iface":
		return &Schema{Unknown: true, UnknownWhy: "interface{}/any: cannot be statically resolved"}
	}
	if _, ok := t.(*types.Named); ok {
		if id := codegraph.TypeIDOf(t); s.g.Type(id) != nil {
			return cloneSchema(s.Synthesize(id))
		}
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return basicSchema(u.Name())
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Uint8 {
			return &Schema{Type: "string", Format: "byte"}
		}
		return &Schema{Type: "array", Items: s.synthGoType(u.Elem(), depth+1)}
	case *types.Array:
		return &Schema{Type: "array", Items: s.synthGoType(u.Elem(), depth+1)}
	case *types.Map:
		return &Schema{Type: "object", Additional: true, Description: MapDescPrefix + u.Elem().String()}
	case *types.Struct:
		return s.synthType(&codegraph.TypeInfo{IsStruct: true, Fields: s.g.StructFields(u)}, "")
	}
	return &Schema{Unknown: true, UnknownWhy: "unresolved: " + t.String()}
}

// synthElem 数组元素类型。
func (s *Synthesizer) synthElem(f codegraph.Field) *Schema {
	if f.Type != nil {
		t := types.Unalias(f.Type)
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		switch u := t.Underlying().(type) {
		case *types.Slice:
			return s.synthGoType(u.Elem(), 0)
		case *types.Array:
			return s.synthGoType(u.Elem(), 0)
		}
	}
	// 从类型串解析元素命名类型
	elem := strings.TrimSuffix(strings.TrimPrefix(f.TypeStr, "[]"), "*")
	if elem == "" {
		return &Schema{Unknown: true}
	}
	if id := findTypeID(s.g, elem); id != "" {
		return s.Synthesize(id)
	}
	return basicSchema(elem)
}

func findTypeID(g *codegraph.Graph, display string) string {
	// display 形如 "repo/pkg/mapping.OrderCheckResp"
	// 类型 ID 形如 "repo/pkg/mapping.OrderCheckResp"
	if g.Type(display) != nil {
		return display
	}
	// 带模块前缀: display = module + "/" + pkg + "." + T
	for id := range g.Types {
		if strings.HasSuffix(id, "."+lastSegment(display)) {
			return id
		}
	}
	return ""
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func basicKindStr(typeStr string) string {
	// 提取底层基础类型名
	t := strings.TrimPrefix(typeStr, "*")
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return t
}

func basicSchema(kind string) *Schema {
	switch strings.TrimSpace(kind) {
	case "string":
		return &Schema{Type: "string"}
	case "int", "int64", "uint", "uint64", "uintptr":
		return &Schema{Type: "integer", Format: "int64"}
	case "int8", "int16", "int32", "uint8", "uint16", "uint32":
		return &Schema{Type: "integer", Format: "int32"}
	case "float32", "float64":
		return &Schema{Type: "number", Format: "double"}
	case "bool":
		return &Schema{Type: "boolean"}
	case "byte":
		return &Schema{Type: "string", Format: "byte"}
	}
	return &Schema{Type: "string", Description: "assumed string: " + kind}
}

func enumValues(ti *codegraph.TypeInfo) []string {
	var out []string
	for _, c := range ti.Consts {
		out = append(out, constDisplay(c.Value))
	}
	return out
}

func enumSource(ti *codegraph.TypeInfo) string {
	if len(ti.Consts) > 0 {
		return ti.Consts[0].ID
	}
	return ""
}

func constDisplay(v string) string {
	v = strings.TrimPrefix(v, `"`)
	v = strings.TrimSuffix(v, `"`)
	return v
}

func hasOmitempty(tag string) bool {
	return parseTag(tag, "json", "omitempty")
}

// applyConstraints 合并校验约束（binding/validate tag，go-playground/validator 语义）：
// min/max/len 对 string/array 是长度约束，对数值是取值约束（gte/lte 同义；gt/lt 为开区间，OAS3.1 需 exclusive*，暂不映射）；oneof 为枚举。
func applyConstraints(sc *Schema, tag string) {
	val := tagValue(tag, "binding")
	if val == "" {
		val = tagValue(tag, "validate")
	}
	if val == "" {
		return
	}
	isLength := sc.Type == "string" || sc.Type == "array"
	isArray := sc.Type == "array"
	for _, part := range strings.Split(val, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			// required 在 codegraph.requiredOf 中判定；无参规则不影响 schema
			continue
		}
		rule, arg := kv[0], kv[1]
		if rule == "oneof" {
			sc.Enum = append(sc.Enum, strings.Fields(arg)...)
			continue
		}
		n, ok := parseFloat(arg)
		if !ok {
			continue
		}
		switch {
		case isLength && (rule == "min" || rule == "len" || rule == "minlength"):
			lo, hi := int(n), int(n)
			if isArray {
				sc.MinItems = &lo
			} else {
				sc.MinLen = &lo
			}
			if rule == "len" {
				if isArray {
					sc.MaxItems = &hi
				} else {
					sc.MaxLen = &hi
				}
			}
		case isLength && (rule == "max" || rule == "maxlength"):
			hi := int(n)
			if isArray {
				sc.MaxItems = &hi
			} else {
				sc.MaxLen = &hi
			}
		case !isLength && (rule == "min" || rule == "gte"):
			x := n
			sc.Min = &x
		case !isLength && (rule == "max" || rule == "lte"):
			x := n
			sc.Max = &x
		}
	}
}

func tagValue(tag, key string) string {
	// struct tag 语法: `key:"value" key2:"v2"`
	for _, part := range strings.Split(tag, "`") {
		_ = part
	}
	body := strings.Trim(tag, "`")
	for _, seg := range splitTags(body) {
		if strings.HasPrefix(seg, key+":") {
			return strings.Trim(strings.TrimPrefix(seg, key+":"), `"`)
		}
	}
	return ""
}

func splitTags(body string) []string {
	var out []string
	inQuote := false
	cur := strings.Builder{}
	for _, r := range body {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func parseFloat(s string) (float64, bool) {
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
		return f, true
	}
	return 0, false
}

func refNameOf(g *codegraph.Graph, typeID string) string {
	// $ref 命名规则 §8.3: 类型名 → 包路径 → +shapeHash
	name := lastSegment(typeID)
	return name
}

func parseTag(tag, key, want string) bool {
	v := tagValue(tag, key)
	for _, part := range strings.Split(v, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

// cloneSchema 浅克隆 schema 节点（防止字段级修饰污染共享缓存;
// 嵌套 Props/Items 保持共享——它们自身不被字段级修饰触碰）。
// cloneSchema 浅拷贝节点供字段级修改（nullable/约束/描述）；会被 append 的 Enum 单独复制，
// 避免与缓存中的共享底层数组互相污染。
func cloneSchema(sc *Schema) *Schema {
	if sc == nil {
		return nil
	}
	c := *sc
	c.Enum = append([]string(nil), sc.Enum...)
	return &c
}

// FieldSchema 单个结构体字段的独立 schema（含约束与 nullable），供请求参数绑定展开复用字段定型规则。
func (s *Synthesizer) FieldSchema(f codegraph.Field) *Schema {
	return s.synthField(f)
}

// SchemaOf 按 go/types 类型合成 schema（值级收窄用：字面量值、字段赋值右值的静态类型）。
func (s *Synthesizer) SchemaOf(t types.Type) *Schema {
	return s.synthGoType(t, 0)
}
