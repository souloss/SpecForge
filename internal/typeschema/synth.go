// Package typeschema 实现 Go 类型 → JSON Schema 合成
// （设计文档 §7.1 映射总表 + §7.2 疑难 case 清单）。
//
// 静态能定型的绝不问模型；定型不了的显式降级并标注
// （x-specforge-unknown），禁止静默编造。
package typeschema

import (
	"fmt"
	"sort"
	"strings"

	"github.com/specforge/specforge/internal/codegraph"
)

// Schema JSON Schema 节点（有序输出由编译器保证）。
type Schema struct {
	Type           string // object|array|string|integer|number|boolean|null
	Format         string // int64|int32|double|date-time
	Ref            string // $ref 名（命名类型引用）
	Items          *Schema
	Props          []Prop // object 属性（有序）
	Required       []string
	Enum           []string // 枚举值（字符串化）
	Additional     bool     // additionalProperties: true
	Nullable       bool     // 3.1: type 数组含 null
	Description    string
	Unknown        bool // x-specforge-unknown
	UnknownWhy     string
	EnumSource     string // 枚举证据符号
	Min, Max       *float64
	MinLen, MaxLen *int
	Pattern        string
	NoBody         bool // RawMessage 透传: 无 schema
}

// Prop object 的一个属性。
type Prop struct {
	Name      string
	Schema    *Schema
	Required  bool
	OmitEmpty bool
}

// Synthesizer 类型合成器（带缓存防递归）。
type Synthesizer struct {
	g       *codegraph.Graph
	cache   map[string]*Schema
	inStack map[string]bool
}

// New 创建合成器。
func New(g *codegraph.Graph) *Synthesizer {
	return &Synthesizer{
		g:       g,
		cache:   map[string]*Schema{},
		inStack: map[string]bool{},
	}
}

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
	// struct → object（嵌入字段展平，设计文档 §7.2.8）
	sc := &Schema{Type: "object"}
	var required []string
	for _, f := range ti.Fields {
		if f.JSONName == "-" {
			continue
		}
		fsc := s.synthField(f)
		sc.Props = append(sc.Props, Prop{
			Name: f.JSONName, Schema: fsc, Required: f.Required, OmitEmpty: hasOmitempty(f.Tag),
		})
		if f.Required {
			required = append(required, f.JSONName)
		}
	}
	sort.Strings(required)
	sc.Required = required
	return sc
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
		sc.Description = "map with value type " + f.TypeStr
		// map[string]any: 值不可定型 → 显式 unknown（设计文档 §7.2.3）
		if strings.Contains(f.TypeStr, "any") || strings.Contains(f.TypeStr, "interface{}") {
			sc.Unknown = true
			sc.UnknownWhy = "map value type any/interface{}: not statically resolvable"
		}
	case "any":
		// §7.2.3: 静态不可定型 → 显式 unknown，禁止编造
		sc = &Schema{Type: "object", Additional: true, Unknown: true,
			UnknownWhy: "interface{}/any: cannot be statically resolved"}
	case "struct":
		if ti := s.g.Type(f.TypeID); ti != nil {
			sc = s.Synthesize(f.TypeID)
		} else {
			sc = &Schema{Unknown: true, UnknownWhy: "inline struct"}
		}
	default:
		if ti := s.g.Type(f.TypeID); ti != nil {
			sc = s.Synthesize(f.TypeID)
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

// synthElem 数组元素类型。
func (s *Synthesizer) synthElem(f codegraph.Field) *Schema {
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

func applyConstraints(sc *Schema, tag string) {
	val := tagValue(tag, "binding")
	if val == "" {
		val = tagValue(tag, "validate")
	}
	if val == "" {
		return
	}
	for _, part := range strings.Split(val, ",") {
		kv := strings.SplitN(part, "=", 2)
		switch kv[0] {
		case "required":
			// required 由字段存在性决定（omitempty 反向）——binding required 强化必填
		case "min":
			if len(kv) == 2 {
				if n, ok := parseFloat(kv[1]); ok {
					x := n
					sc.Min = &x
				}
			}
		case "max":
			if len(kv) == 2 {
				if n, ok := parseFloat(kv[1]); ok {
					x := n
					sc.Max = &x
				}
			}
		case "minlength":
			if len(kv) == 2 {
				if n, ok := parseFloat(kv[1]); ok {
					x := int(n)
					sc.MinLen = &x
				}
			}
		case "maxlength":
			if len(kv) == 2 {
				if n, ok := parseFloat(kv[1]); ok {
					x := int(n)
					sc.MaxLen = &x
				}
			}
		case "oneof":
			if len(kv) == 2 {
				for _, v := range strings.Fields(kv[1]) {
					sc.Enum = append(sc.Enum, v)
				}
			}
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
func cloneSchema(sc *Schema) *Schema {
	if sc == nil {
		return nil
	}
	c := *sc
	return &c
}
