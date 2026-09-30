package golang

import (
	"go/ast"
	"go/types"
	"sort"

	"github.com/specforge/specforge/internal/frontend"
)

// maxStructRefDepth 结构体字段类型递归引用的最大层数（与形状树深度同量级，防止大类型图膨胀）。
const maxStructRefDepth = 4

// jsonSkipName json tag 中表示「不序列化」的字段名。
const jsonSkipName = "-"

// JSONFieldsIn 实现 frontend.StructIndex：函数体内任一表达式的静态类型（剥去指针/切片/数组/map）
// 为结构体（命名或匿名）时，收录其序列化字段，并沿字段类型递归。
func (p *Program) JSONFieldsIn(funcIDs []string) []frontend.FieldSite {
	byName := map[string]frontend.FieldSite{}
	seen := map[*types.Struct]bool{}
	var visit func(t types.Type, depth int)
	visit = func(t types.Type, depth int) {
		st, ok := elemType(t).Underlying().(*types.Struct)
		if !ok || seen[st] {
			return
		}
		seen[st] = true
		for i, f := range p.g.StructFields(st) {
			pos := p.g.Fset.Position(st.Field(i).Pos())
			if _, dup := byName[f.JSONName]; !dup && f.JSONName != "" && f.JSONName != jsonSkipName && pos.IsValid() {
				byName[f.JSONName] = frontend.FieldSite{JSON: f.JSONName, File: pos.Filename, Line: pos.Line}
			}
			if depth < maxStructRefDepth {
				visit(f.Type, depth+1)
			}
		}
	}
	for _, fn := range funcIDs {
		fd, info := p.g.FuncDeclOf(fn), p.g.TypeInfoOfFunc(fn)
		if fd == nil || fd.Body == nil || info == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok {
				if t := info.TypeOf(e); t != nil {
					visit(t, 0)
				}
			}
			return true
		})
	}
	out := make([]frontend.FieldSite, 0, len(byName))
	for _, fs := range byName {
		out = append(out, fs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JSON < out[j].JSON })
	return out
}

// elemType 剥去指针、切片、数组与 map 值的包装，得到承载字段的元素类型。
func elemType(t types.Type) types.Type {
	for {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Slice:
			t = x.Elem()
		case *types.Array:
			t = x.Elem()
		case *types.Map:
			t = x.Elem()
		default:
			return t
		}
	}
}

// 编译期断言：Go 前端提供结构体字段索引。
var _ frontend.StructIndex = (*Program)(nil)
