// Package loader 负责仓库摄入与服务拓扑发现（设计文档 §6.1 步骤 0–1、F1/F2）。
//
// 职责:
//   - 扫描 go.mod 定位模块
//   - 通过 cmd/ 目录与 main 函数定位服务入口
//   - 用 golang.org/x/tools/go/packages 加载带类型信息的全部包
package loader

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Service 描述一个可独立编译/运行的服务单元。
type Service struct {
	Name       string // 服务名，如 "service-ipo"
	Dir        string // 相对仓库根的目录
	Entrypoint string // main 函数所在包
}

// Loaded 仓库加载结果：包集合 + 服务清单。
type Loaded struct {
	Root     string // 绝对路径
	Module   string // module path
	Services []Service
	Pkgs     []*packages.Package
	Fset     *token.FileSet
}

// LoadRepo 加载整个仓库。dir 必须包含 go.mod。
func LoadRepo(dir string) (*Loaded, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		return nil, fmt.Errorf("no go.mod under %s: %w", abs, err)
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes |
			packages.NeedImports | packages.NeedDeps | packages.NeedModule,
		Dir:   abs,
		Env:   os.Environ(),
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	var errs []string
	for _, p := range pkgs {
		if len(p.Errors) > 0 && p.Name != "" {
			for _, e := range p.Errors {
				errs = append(errs, fmt.Sprintf("%s: %v", p.PkgPath, e))
			}
		}
	}
	// 容错：只要有类型信息就继续（设计文档 §10.1 tree-sitter 容错解析的对偶）。
	// 完全无法加载时才报错。
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages loaded: %v", strings.Join(errs, "; "))
	}

	l := &Loaded{Root: abs, Pkgs: pkgs, Fset: pkgs[0].Fset}
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Path != "" {
			l.Module = p.Module.Path
			break
		}
	}
	l.Services = detectServices(pkgs)
	return l, nil
}

// detectServices 定位 main 包 → 服务拓扑（F1）。
// 约定: cmd/<name>/ 或 cmd-<name>/ 或根 main 均视为服务。
func detectServices(pkgs []*packages.Package) []Service {
	var out []Service
	for _, p := range pkgs {
		if p.Name != "main" {
			continue
		}
		name := p.PkgPath
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		name = strings.TrimPrefix(strings.TrimPrefix(name, "cmd/"), "cmd-")
		if name == "" || name == "main" {
			name = p.PkgPath
		}
		out = append(out, Service{Name: name, Dir: p.PkgPath, Entrypoint: p.PkgPath})
	}
	return out
}

// ServiceFilter 按名称过滤服务涉及的包集合。
// 单模块多服务时，服务边界 = main 包可达闭包；简化实现：
// 全部包都属于该模块，交给路由提取阶段自然限定。
func (l *Loaded) ServiceFilter(name string) []*packages.Package {
	if name == "" {
		return l.Pkgs
	}
	for _, s := range l.Services {
		if s.Name == name {
			return l.Pkgs
		}
	}
	return nil
}

// ParseFile 单文件解析（指纹计算用，不依赖类型信息）。
func ParseFile(path string) (*ast.File, *token.FileSet, error) {
	fset := token.NewFileSet()
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	return f, fset, nil
}
