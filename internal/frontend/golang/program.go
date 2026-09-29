package golang

import (
	"sort"
	"strings"
	"sync"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/slicing"
)

// Program 基于 Go 代码图的 frontend.Program 实现（只读、确定性、并发安全）。
type Program struct {
	g *codegraph.Graph // 代码图

	fieldOnce  sync.Once         // FieldTypes 惰性计算
	fieldTypes map[string]string // JSON 字段名 → 唯一基础类型
}

// NewProgram 用代码图构造程序视图（测试与其它前端组件复用）。
func NewProgram(g *codegraph.Graph) *Program { return &Program{g: g} }

// RelPath 实现 frontend.Program。
func (p *Program) RelPath(path string) string { return p.g.RelPath(path) }

// AbsPath 实现 frontend.Program。
func (p *Program) AbsPath(path string) string { return p.g.AbsPath(path) }

// FileHash 实现 frontend.Program。
func (p *Program) FileHash(path string) string { return p.g.FileHashOf(p.g.AbsPath(path)) }

// FileLines 实现 frontend.Program。
func (p *Program) FileLines(path string) []string { return p.g.FileLines(p.g.AbsPath(path)) }

// SourceRange 实现 frontend.Program。
func (p *Program) SourceRange(path string, from, to int) string {
	return p.g.SourceRange(p.g.AbsPath(path), from, to)
}

// ValidLine 实现 frontend.Program。
func (p *Program) ValidLine(path string, line int) bool {
	return p.g.ValidLine(p.g.AbsPath(path), line)
}

// FuncSource 实现 frontend.Program。
func (p *Program) FuncSource(id string) (string, int, int, string, bool) { return p.g.FuncSource(id) }

// Reach 实现 frontend.Program：handler 为接口方法时从全部具体实现出发，取最小深度。
func (p *Program) Reach(handler string) map[string]int {
	roots := []string{handler}
	if p.g.FuncDeclOf(handler) == nil {
		roots = p.g.ConcreteImplsOf(handler)
	}
	depth := map[string]int{}
	for _, root := range roots {
		for fn, d := range p.g.ForwardReach(root, slicing.MaxDepth) {
			if old, ok := depth[fn]; !ok || d < old {
				depth[fn] = d
			}
		}
	}
	return depth
}

// Callees 实现 frontend.Program（有序）。
func (p *Program) Callees(id string) []string {
	out := append([]string(nil), p.g.CalleesOf(id)...)
	sort.Strings(out)
	return out
}

// Symbol 实现 frontend.Program。
func (p *Program) Symbol(id string) (frontend.SymbolInfo, bool) {
	sym := p.g.Sym(id)
	if sym == nil {
		return frontend.SymbolInfo{}, false
	}
	return frontend.SymbolInfo{ID: sym.ID, Kind: string(sym.Kind), File: sym.File, Line: sym.Line, Doc: sym.Doc}, true
}

// SymbolExists 实现 frontend.Program（函数声明或符号表中存在）。
func (p *Program) SymbolExists(id string) bool {
	return p.g.FuncDeclOf(id) != nil || p.g.Sym(id) != nil
}

// TypeOf 实现 frontend.Program。
func (p *Program) TypeOf(id string) (frontend.TypeInfo, bool) {
	ti := p.g.Type(id)
	if ti == nil {
		return frontend.TypeInfo{}, false
	}
	out := frontend.TypeInfo{ID: ti.ID, IsStruct: ti.IsStruct, Fields: make([]frontend.FieldInfo, 0, len(ti.Fields))}
	for _, f := range ti.Fields {
		out.Fields = append(out.Fields, frontend.FieldInfo{Name: f.Name, JSON: f.JSONName, Type: f.TypeStr, Required: f.Required})
	}
	return out, true
}

// FindSymbols 实现 frontend.Program。
func (p *Program) FindSymbols(query string) []string {
	q := strings.ToLower(query)
	if q == "" {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if !seen[id] && strings.Contains(strings.ToLower(id), q) {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for id := range p.g.Syms {
		add(id)
	}
	for id := range p.g.Types {
		add(id)
	}
	sort.Strings(ids)
	return ids
}

// FieldTypes 实现 frontend.Program（惰性计算一次）。
func (p *Program) FieldTypes() map[string]string {
	p.fieldOnce.Do(func() { p.fieldTypes = uniqueFieldTypes(p.g) })
	return p.fieldTypes
}

// IsSourceFile 实现 frontend.Program：仓库内的 .go 文件。
func (p *Program) IsSourceFile(path string) bool {
	abs := p.g.AbsPath(path)
	return strings.HasSuffix(abs, ".go") && p.g.RelPath(abs) != abs
}

// Graph 底层代码图（Go 前端内部使用）。
func (p *Program) Graph() *codegraph.Graph { return p.g }

// uniqueFieldTypes JSON 字段名 → 代码图中该名字段的唯一基础类型；同名字段类型不一致的名字不收录。
// 按类型 ID 排序遍历，结果与 map 遍历序无关（确定性）。
func uniqueFieldTypes(g *codegraph.Graph) map[string]string {
	ids := make([]string, 0, len(g.Types))
	for id := range g.Types {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seen := map[string]string{}
	conflict := map[string]bool{}
	for _, id := range ids {
		ti := g.Types[id]
		if !ti.IsStruct {
			continue
		}
		for _, f := range ti.Fields {
			if f.JSONName == "-" {
				continue
			}
			typ := basicKindStrOf(f.TypeStr)
			if typ == "" {
				continue
			}
			if old, ok := seen[f.JSONName]; ok && old != typ {
				conflict[f.JSONName] = true
			}
			seen[f.JSONName] = typ
		}
	}
	for name := range conflict {
		delete(seen, name)
	}
	return seen
}

// basicKindStrOf 类型串 → JSON Schema 基础类型名（切片 array、映射 object；无法归类返回空）。
func basicKindStrOf(typeStr string) string {
	s := strings.TrimPrefix(typeStr, "*")
	if strings.HasPrefix(s, "[]") {
		return "array"
	}
	if strings.HasPrefix(s, "map[") {
		return "object"
	}
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	switch strings.TrimSpace(s) {
	case "string", "byte", "Time":
		return "string"
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return "integer"
	case "float32", "float64":
		return "number"
	case "bool":
		return "boolean"
	}
	return ""
}
